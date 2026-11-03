package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/vgropp/kea-roost/internal/kea"
)

func subnet6Input() Subnet6Input {
	return Subnet6Input{Pools: []string{"fd00:db8:1::1000-fd00:db8:1::ffff"}, DNSServers: []string{"fd00:db8:1::1"}, ValidLifetime: 86400, PreferredLifetime: 43200}
}

func TestUpdateSubnet6(t *testing.T) {
	e, fake6, peer := setupPair6(t)
	in := subnet6Input()
	in.Pools = []string{"fd00:db8:1::2000 - fd00:db8:1::2fff", "fd00:db8:1::1000/116"}
	in.DNSServers = []string{"FD00:DB8:1::53"}
	res, err := e.mgr.UpdateSubnet6(context.Background(), "admin", 1, in)
	if err != nil || res.Status != StatusOK {
		t.Fatalf("%+v %v", res, err)
	}
	for _, f := range []map[string]any{fake6.Config, peer.Config} {
		s := f["subnet6"].([]any)[0].(map[string]any)
		pools := fmt.Sprint(s["pools"])
		if !strings.Contains(pools, "fd00:db8:1::1000-fd00:db8:1::1fff") || !strings.Contains(pools, "fd00:db8:1::2000-fd00:db8:1::2fff") {
			t.Errorf("pools: %s", pools)
		}
		if !strings.Contains(fmt.Sprint(s["option-data"]), "fd00:db8:1::53") || s["pd-pools"] == nil || s["interface"] != "lan" {
			t.Errorf("subnet: %+v", s)
		}
		// unchanged lifetimes stay inherited
		if s["valid-lifetime"] != nil {
			t.Errorf("valid lifetime pinned: %v", s["valid-lifetime"])
		}
	}
	if len(fake6.Hosts) != 1 || e.fake.HasCall("subnet4-update") {
		t.Error("reservation lost or IPv4 touched")
	}
	entries, _ := e.mgr.Store().AuditTail(1)
	if entries[0].Action != "subnet6-update" {
		t.Errorf("audit: %+v", entries[0])
	}
}

func TestSubnet6Problems(t *testing.T) {
	e, _ := setup6(t)
	for _, c := range []struct {
		change func(*Subnet6Input)
		want   string
	}{
		{func(in *Subnet6Input) { in.Pools = []string{"fd00:db8:2::1-fd00:db8:2::9"} }, "not inside"},
		{func(in *Subnet6Input) { in.Pools = []string{"192.0.2.1-192.0.2.9"} }, "invalid pool"},
		{func(in *Subnet6Input) {
			in.Pools = []string{"fd00:db8:1::1000-fd00:db8:1::1fff", "fd00:db8:1::1800-fd00:db8:1::2fff"}
		}, "overlap"},
		{func(in *Subnet6Input) { in.DNSServers = []string{"192.0.2.53"} }, "invalid DNS server"},
		{func(in *Subnet6Input) { in.PreferredLifetime = in.ValidLifetime + 1 }, "preferred lifetime"},
		{func(in *Subnet6Input) { in.ValidLifetime = 0 }, "between 1 and"},
		{func(in *Subnet6Input) { in.ValidLifetime = 1 << 32 }, "between 1 and"},
	} {
		in := subnet6Input()
		c.change(&in)
		_, err := e.mgr.UpdateSubnet6(context.Background(), "admin", 1, in)
		var v *ValidationError
		if !errors.As(err, &v) || !strings.Contains(v.Error(), c.want) {
			t.Errorf("%s: %v", c.want, err)
		}
	}
	if _, err := e.mgr.UpdateSubnet6(context.Background(), "admin", 9, subnet6Input()); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown subnet: %v", err)
	}
}

func TestSubnet6CheckWarns(t *testing.T) {
	e, _ := setup6(t)
	in := subnet6Input()
	in.Pools = []string{"fd00:db8:1::5-fd00:db8:1::20"}
	warnings, err := e.mgr.CheckSubnet6(context.Background(), 1, in)
	got := strings.Join(warnings, "\n")
	// both leases leave the pool; the reserved address fd00:db8:1::10 is now inside it
	if err != nil || !strings.Contains(got, "2 active leases") || !strings.Contains(got, "1 reservations now lie inside a pool: fd00:db8:1::10") {
		t.Fatalf("%v %v", warnings, err)
	}
}

func TestSubnet6LifetimesMoveTheirBounds(t *testing.T) {
	e, fake6 := setup6(t)
	fake6.Lock()
	s := fake6.Config["subnet6"].([]any)[0].(map[string]any)
	s["min-valid-lifetime"], s["max-valid-lifetime"] = 86400, 86400
	fake6.Unlock()
	in := subnet6Input()
	in.ValidLifetime, in.PreferredLifetime = 7200, 3600
	if res, err := e.mgr.UpdateSubnet6(context.Background(), "admin", 1, in); err != nil || res.Status != StatusOK {
		t.Fatalf("%+v %v", res, err)
	}
	s = fake6.Config["subnet6"].([]any)[0].(map[string]any)
	// the minimum moves down to the new value, the maximum stays
	if fmt.Sprint(s["valid-lifetime"], s["min-valid-lifetime"], s["max-valid-lifetime"], s["preferred-lifetime"]) != "7200 7200 86400 3600" {
		t.Errorf("%+v", s)
	}
	if list, _ := e.mgr.Subnets6(context.Background()); !list.Editable || *list.Subnets[0].PreferredLifetime != 3600 {
		t.Errorf("%+v", list)
	}
}

func TestSubnet6PrefixPools(t *testing.T) {
	e, fake6 := setup6(t)
	ctx := context.Background()
	in := subnet6Input()
	in.PDPools = []kea.PDPool{{Prefix: "fd00:db8:2::", PrefixLen: 48, DelegatedLen: 56}, {Prefix: "FD00:DB8:3::", PrefixLen: 48, DelegatedLen: 60}}
	if res, err := e.mgr.UpdateSubnet6(ctx, "admin", 1, in); err != nil || res.Status != StatusOK {
		t.Fatalf("%+v %v", res, err)
	}
	fake6.Lock()
	got := fmt.Sprint(fake6.Config["subnet6"].([]any)[0].(map[string]any)["pd-pools"])
	fake6.Unlock()
	if !strings.Contains(got, "fd00:db8:3::") || !strings.Contains(got, "fd00:db8:2::") {
		t.Errorf("pd-pools: %s", got)
	}
	// without the field the prefix pools stay
	in.PDPools = nil
	if res, err := e.mgr.UpdateSubnet6(ctx, "admin", 1, in); err != nil || res.Status != StatusOK {
		t.Fatalf("%+v %v", res, err)
	}
	if list, _ := e.mgr.Subnets6(ctx); len(list.Subnets[0].PDPools) != 2 {
		t.Errorf("%+v", list.Subnets[0].PDPools)
	}
	for _, c := range []struct {
		pool kea.PDPool
		want string
	}{
		{kea.PDPool{Prefix: "fd00:db8:2::1", PrefixLen: 48, DelegatedLen: 56}, "bits set"},
		{kea.PDPool{Prefix: "fd00:db8:2::", PrefixLen: 48, DelegatedLen: 40}, "cannot hand out"},
		{kea.PDPool{Prefix: "192.0.2.0", PrefixLen: 24, DelegatedLen: 28}, "no IPv6 prefix"},
	} {
		bad := subnet6Input()
		bad.PDPools = []kea.PDPool{c.pool}
		_, err := e.mgr.UpdateSubnet6(ctx, "admin", 1, bad)
		var v *ValidationError
		if !errors.As(err, &v) || !strings.Contains(v.Error(), c.want) {
			t.Errorf("%s: %v", c.want, err)
		}
	}
	overlap := subnet6Input()
	overlap.PDPools = []kea.PDPool{{Prefix: "fd00:db8:2::", PrefixLen: 48, DelegatedLen: 56}, {Prefix: "fd00:db8:2:100::", PrefixLen: 56, DelegatedLen: 64}}
	var v *ValidationError
	if _, err := e.mgr.UpdateSubnet6(ctx, "admin", 1, overlap); !errors.As(err, &v) || !strings.Contains(v.Error(), "overlap") {
		t.Errorf("%v", err)
	}
}

func TestSubnet6WithoutPreferredLifetime(t *testing.T) {
	e, fake6 := setup6(t)
	in := subnet6Input()
	in.PreferredLifetime = 0
	if res, err := e.mgr.UpdateSubnet6(context.Background(), "admin", 1, in); err != nil || res.Status != StatusOK {
		t.Fatalf("%+v %v", res, err)
	}
	fake6.Lock()
	defer fake6.Unlock()
	if v, ok := fake6.Config["subnet6"].([]any)[0].(map[string]any)["preferred-lifetime"]; ok {
		t.Errorf("preferred lifetime set to %v", v)
	}
}

// A Kea that dropped the reservations of the subnet is caught by the check.
func TestSubnetEditNoticesLostReservations(t *testing.T) {
	e, fake6 := setup6(t)
	fake6.Lock()
	fake6.DropHostsOnSubnetUpdate = true
	fake6.Unlock()
	res, err := e.mgr.UpdateSubnet6(context.Background(), "admin", 1, subnet6Input())
	if err != nil || res.Status != StatusPartial || !strings.Contains(res.Steps[len(res.Steps)-1].Message, "1 reservations before and 0 after") {
		t.Fatalf("%+v %v", res, err)
	}
	e.fake.Hosts = []map[string]any{{"subnet-id": 1, "hw-address": "aa:bb:cc:00:00:01", "ip-address": "192.0.2.10"}}
	e.fake.DropHostsOnSubnetUpdate = true
	in := SubnetInput{Pools: []string{"192.0.2.100-192.0.2.199"}, Gateways: []string{"192.0.2.1"}, DNSServers: []string{"192.0.2.53"}, ValidLifetime: 7200}
	if res, err := e.mgr.UpdateSubnet(context.Background(), "admin", 1, in); err != nil || res.Status != StatusPartial {
		t.Fatalf("IPv4: %+v %v", res, err)
	}
}
