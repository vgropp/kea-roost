package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vgropp/kea-roost/internal/keatest"
)

func TestConfigTestSafe(t *testing.T) {
	for version, want := range map[string]bool{"3.0.3": false, "3.0.4": true, "3.1.0": true, "4.0.0": true, "2.6.1": false, "": false, "nonsense": false} {
		if got := configTestSafe(version); got != want {
			t.Errorf("%q: %v", version, got)
		}
	}
}

func TestGlobalOptionsReachEveryServer(t *testing.T) {
	p := setupPair(t)
	p.fake.Version, p.peer.Version = "3.0.4", "3.0.3"
	p.fake.Config["option-data"] = []any{map[string]any{"name": "domain-name-servers", "code": 6, "data": "192.0.2.53", "csv-format": true, "always-send": true}}
	ctx := context.Background()

	list, err := p.mgr.GlobalOptions(ctx, false)
	if err != nil || len(list.Options) != 1 || len(list.Blockers) != 0 || len(list.Notes) != 1 || !strings.Contains(list.Notes[0], "standby runs Kea 3.0.3") {
		t.Fatalf("%+v %v", list, err)
	}
	in := []ReservationOption{{Name: "domain-name-servers", Data: "192.0.2.54, 192.0.2.55"}, {Name: "domain-name", Data: "lan"}}
	res, err := p.mgr.UpdateGlobalOptions(ctx, "admin", false, in)
	if err != nil || res.Status != StatusOK {
		t.Fatalf("%+v %v", res, err)
	}
	// the primary runs 3.0.4 and is tested first; the peer runs 3.0.3 and is not
	if !p.fake.HasCall("config-test") || p.peer.HasCall("config-test") || p.fake.Reconfigured != 1 || p.peer.Reconfigured != 1 {
		t.Errorf("config-test %v/%v, config-set %d/%d", p.fake.HasCall("config-test"), p.peer.HasCall("config-test"), p.fake.Reconfigured, p.peer.Reconfigured)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "standby runs Kea 3.0.3") {
		t.Errorf("warnings: %v", res.Warnings)
	}
	got := fmt.Sprint(p.fake.Config["option-data"])
	if !strings.Contains(got, "192.0.2.54, 192.0.2.55") || !strings.Contains(got, "always-send:true") || !strings.Contains(got, "domain-name data:lan") && !strings.Contains(got, "data:lan") {
		t.Errorf("primary options: %s", got)
	}
	if !strings.Contains(fmt.Sprint(p.peer.Config["option-data"]), "data:lan") || p.peer.Config["subnet4"] == nil {
		t.Errorf("peer: %+v", p.peer.Config)
	}
	entries, _ := p.mgr.Store().AuditTail(1)
	if entries[0].Action != "config-set" || !strings.Contains(entries[0].Target, "domain-name-servers, domain-name") {
		t.Errorf("audit: %+v", entries[0])
	}
}

func TestGlobalOptionsOfDHCP6(t *testing.T) {
	e, fake6 := setup6(t)
	fake6.Lock()
	fake6.Version = "3.0.4"
	fake6.Unlock()
	res, err := e.mgr.UpdateGlobalOptions(context.Background(), "admin", true, []ReservationOption{{Name: "dns-servers", Data: "fd00:db8:1::53"}})
	if err != nil || res.Status != StatusOK || e.fake.Reconfigured != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if got := fmt.Sprint(fake6.Config["option-data"]); !strings.Contains(got, "fd00:db8:1::53") {
		t.Errorf("%s", got)
	}
}

func TestGlobalOptionsProblems(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	for _, in := range [][]ReservationOption{
		{{Name: "", Data: "x"}},
		{{Name: "domain-name", Data: "a"}, {Name: "Domain-Name", Data: "b"}},
	} {
		var v *ValidationError
		if _, err := e.mgr.UpdateGlobalOptions(ctx, "admin", false, in); !errors.As(err, &v) {
			t.Errorf("%+v: %v", in, err)
		}
	}
	if _, err := e.mgr.UpdateGlobalOptions(ctx, "admin", true, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("without kea-dhcp6: %v", err)
	}
	// Kea refusing the configuration leaves it as it was
	e.fake.Version = "3.0.4"
	e.fake.Fail["config-test"] = keatest.Failure{Result: 1, Text: "option data does not match"}
	res, err := e.mgr.UpdateGlobalOptions(ctx, "admin", false, []ReservationOption{{Name: "domain-name-servers", Data: "nonsense"}})
	if err != nil || res.Status != StatusFailed || e.fake.Reconfigured != 0 || !strings.Contains(res.Message, "config-test") {
		t.Errorf("%+v %v", res, err)
	}
}

// The peer reloads only once the primary serves normally again.
func TestGlobalOptionsWaitForHA(t *testing.T) {
	p := setupPair(t)
	haSettlePoll, haSettleTimeout = 10*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { haSettlePoll, haSettleTimeout = time.Second, 45*time.Second })
	p.fake.HAStateAfterReload = "syncing"
	res, err := p.mgr.UpdateGlobalOptions(context.Background(), "admin", false, []ReservationOption{{Name: "domain-name", Data: "lan"}})
	if err != nil || res.Status != StatusPartial || p.peer.Reconfigured != 0 || !strings.Contains(res.Steps[len(res.Steps)-1].Message, "HA state is still syncing") {
		t.Fatalf("%+v %v", res, err)
	}
	// a terminated pair is reported at once instead of waited for
	p.fake.SetHAState("hot-standby")
	p.fake.HAStateAfterReload = "terminated"
	later := time.Now().Add(time.Second)
	p.mgr.now = func() time.Time { return later }
	res, err = p.mgr.UpdateGlobalOptions(context.Background(), "admin", false, []ReservationOption{{Name: "domain-name", Data: "home"}})
	if err != nil || res.Status != StatusPartial || p.peer.Reconfigured != 0 || !strings.Contains(res.Steps[len(res.Steps)-1].Message, "terminated") {
		t.Fatalf("terminated: %+v %v", res, err)
	}
	// with the partner away, partner-down is as settled as it gets
	p.fake.HAStateAfterReload = "partner-down"
	p.fake.SetHAState("partner-down")
	later = later.Add(time.Second)
	res, err = p.mgr.UpdateGlobalOptions(context.Background(), "admin", false, []ReservationOption{{Name: "domain-name", Data: "lan"}})
	if err != nil || res.Status != StatusOK || p.peer.Reconfigured != 1 {
		t.Fatalf("partner down: %+v %v", res, err)
	}
}

// A reload starts only from a steady HA state.
func TestGlobalOptionsBlockedWhileHAIsUnsettled(t *testing.T) {
	p := setupPair(t)
	p.peer.SetHAState("communication-recovery")
	list, err := p.mgr.GlobalOptions(context.Background(), false)
	if err != nil || len(list.Blockers) != 1 || !strings.Contains(list.Blockers[0], "standby is in the state communication-recovery") {
		t.Fatalf("%+v %v", list, err)
	}
	if _, err := p.mgr.UpdateGlobalOptions(context.Background(), "admin", false, []ReservationOption{{Name: "domain-name", Data: "lan"}}); err == nil || p.fake.Reconfigured != 0 {
		t.Fatalf("not blocked: %v", err)
	}
}

func TestGlobalOptionsBlockedByOtherSpaces(t *testing.T) {
	e := setup(t)
	e.fake.Config["option-data"] = []any{map[string]any{"name": "vendor-option", "code": 2, "space": "vendor-4491", "data": "x"}}
	list, err := e.mgr.GlobalOptions(context.Background(), false)
	if err != nil || len(list.Blockers) != 1 || !strings.Contains(list.Blockers[0], "space vendor-4491") {
		t.Fatalf("%+v %v", list, err)
	}
	var v *ValidationError
	if _, err := e.mgr.UpdateGlobalOptions(context.Background(), "admin", false, nil); !errors.As(err, &v) {
		t.Errorf("%v", err)
	}
	long := []ReservationOption{{Name: "domain-name", Data: strings.Repeat("x", maxOptionText+1)}}
	e.fake.Config["option-data"] = []any{}
	if _, err := e.mgr.UpdateGlobalOptions(context.Background(), "admin", false, long); !errors.As(err, &v) || !strings.Contains(v.Error(), "too long") {
		t.Errorf("%v", err)
	}
}

// Saving the options a server already has costs no reload.
func TestUnchangedGlobalOptionsSkipTheReload(t *testing.T) {
	e := setup(t)
	res, err := e.mgr.UpdateGlobalOptions(context.Background(), "u", false, []ReservationOption{{Name: "domain-name-servers", Data: "192.0.2.53"}})
	if err != nil || res.Status != StatusOK || !strings.Contains(res.Message, "Nothing changed") {
		t.Fatalf("got %+v %v", res, err)
	}
	if e.fake.HasCall("config-set") {
		t.Error("reloaded without a change")
	}
	res, err = e.mgr.UpdateGlobalOptions(context.Background(), "u", false, []ReservationOption{{Name: "domain-name-servers", Data: "192.0.2.53,192.0.2.54"}})
	if err != nil || res.Status != StatusOK || !e.fake.HasCall("config-set") {
		t.Fatalf("got %+v %v", res, err)
	}
}

func TestGlobalOptionsComparedStrictly(t *testing.T) {
	e := setup(t)
	for _, in := range [][]ReservationOption{
		{{Name: "domain-name-servers", Data: "192.0.2.53,192.0.2.53"}},
		{{Name: "domain-name-servers", Data: "192.0.2.53"}, {Name: "domain-name", Data: "lan"}},
		{{Name: "domain-name-servers", Code: 99, Data: "192.0.2.53"}},
	} {
		if e.mgr.globalsUnchanged(context.Background(), e.mgr.serversOf(false), false, in) {
			t.Errorf("%+v counts as unchanged", in)
		}
	}
	if !e.mgr.globalsUnchanged(context.Background(), e.mgr.serversOf(false), false, []ReservationOption{{Name: "domain-name-servers", Code: 6, Data: " 192.0.2.53 "}}) {
		t.Error("the same option counts as changed")
	}
}
