package manager

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

func hiddenSetup(t *testing.T) *env {
	t.Helper()
	e := setup(t)
	if err := e.mgr.LoadSettings(); err != nil {
		t.Fatal(err)
	}
	return e
}

func withUnusedClass(t *testing.T, e *env) {
	t.Helper()
	e.fake.Config["client-classes"] = []any{map[string]any{"name": "one"}, map[string]any{"name": "two"}}
	e.fake.Config["subnet4"].([]any)[0].(map[string]any)["option-data"] = []any{
		map[string]any{"name": "routers", "data": "198.51.100.1"},
	}
}

func finding(r *HealthReport, rule, target string) *Finding {
	i := slices.IndexFunc(r.Findings, func(f Finding) bool { return f.Rule == rule && f.Target == target })
	if i < 0 {
		return nil
	}
	return &r.Findings[i]
}

func TestHiddenFindingStopsCountingButStaysListed(t *testing.T) {
	e := hiddenSetup(t)
	withUnusedClass(t, e)
	ctx := context.Background()
	if r := e.mgr.Health(ctx); r.Severity != SeverityWarn {
		t.Fatalf("severity: %s", r.Severity)
	}
	if err := e.mgr.HideFinding("alice", "gateway-outside", "192.0.2.0/24", true); err != nil {
		t.Fatal(err)
	}
	r := e.mgr.Health(ctx)
	if f := finding(r, "gateway-outside", "192.0.2.0/24"); f == nil || !f.Hidden {
		t.Fatalf("finding: %+v", f)
	}
	if r.Severity != SeverityOK {
		t.Fatalf("a hidden finding still counts: %s", r.Severity)
	}
	i := slices.IndexFunc(r.Rules, func(x ReviewRule) bool { return x.Rule == "gateway-outside" })
	if r.Rules[i].Findings != 0 || r.Rules[i].Hidden != 1 {
		t.Fatalf("rule: %+v", r.Rules[i])
	}
	if f := finding(r, "class-unused", "one"); f == nil || f.Hidden {
		t.Fatalf("other findings stay: %+v", f)
	}
	if a := e.audit(t); a.Action != "finding-hide" || a.User != "alice" {
		t.Fatalf("audit: %+v", a)
	}
}

func TestHiddenFindingsSurviveARestart(t *testing.T) {
	e := hiddenSetup(t)
	if err := e.mgr.HideFinding("alice", "class-unused", "one", true); err != nil {
		t.Fatal(err)
	}
	again := New(e.mgr.kea, e.mgr.Store(), "primary")
	if err := again.LoadSettings(); err != nil {
		t.Fatal(err)
	}
	if !(*again.hidden.Load())[hiddenKey("class-unused", "one")] {
		t.Fatal("forgotten")
	}
	if err := again.HideFinding("alice", "class-unused", "one", false); err != nil {
		t.Fatal(err)
	}
	if (*again.hidden.Load())[hiddenKey("class-unused", "one")] {
		t.Fatal("still hidden")
	}
}

func TestOnlyKnownFindingsCanBeHidden(t *testing.T) {
	e := hiddenSetup(t)
	var invalid *ValidationError
	for _, c := range [][2]string{{"nonsense", "x"}, {"class-unused", ""}} {
		if err := e.mgr.HideFinding("alice", c[0], c[1], true); !errors.As(err, &invalid) {
			t.Errorf("%v: %v", c, err)
		}
	}
}

func TestNetworkFindingCanBeHidden(t *testing.T) {
	e := hiddenSetup(t)
	e.fake.Hosts = []map[string]any{{"subnet-id": 0, "hw-address": macA, "ip-address": "192.0.2.10"}}
	monitor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`[{"mac":"` + macA + `","ipv4":["192.0.2.20","192.0.2.10"],"last_seen":"2026-10-30T11:59:00Z"}]`))
	}))
	t.Cleanup(monitor.Close)
	e.mgr.SetARPMonitor(monitor.URL)
	e.mgr.now = func() time.Time { return reviewNow }
	if err := e.mgr.HideFinding("alice", "network-moved", macA, true); err != nil {
		t.Fatal(err)
	}
	if f := finding(e.mgr.Health(context.Background()), "network-moved", macA); f == nil || !f.Hidden {
		t.Fatalf("finding: %+v", f)
	}
}
