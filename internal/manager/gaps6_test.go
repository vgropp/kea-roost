package manager

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/vgropp/kea-roost/internal/kea"
	"github.com/vgropp/kea-roost/internal/keatest"
)

func TestReview6RulesOfSettingsAndSubnets(t *testing.T) {
	e, fake6 := setup6(t)
	fake6.Lock()
	fake6.Config["reservations-in-subnet"] = false
	fake6.Config["subnet6"] = append(fake6.Config["subnet6"].([]any),
		map[string]any{"id": 2, "subnet": "fd00:db8:5::/64", "interface": "lan", "option-data": []any{map[string]any{"name": "dns-servers", "data": "fd00:db8:5::1"}}},
		map[string]any{"id": 3, "subnet": "fd00:db8:6::/64", "interface": "lan", "pools": []any{map[string]any{"pool": "fd00:db8:6::/63"}},
			"option-data": []any{map[string]any{"name": "dns-servers", "data": "fd00:db8:6::1"}}})
	fake6.Unlock()
	var got []string
	for _, f := range e.mgr.Health(context.Background()).Findings {
		if f.IPv6 {
			got = append(got, f.Rule+" "+f.Target)
		}
	}
	for _, want := range []string{"v6-setting-in-subnet reservations-in-subnet", "v6-subnet-unused fd00:db8:5::/64", "v6-pool-outside fd00:db8:6::/64"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s missing in %v", want, got)
		}
	}
}

func TestSubnet6WarnsAboutPrefixesLeavingThePools(t *testing.T) {
	e, fake6 := setup6(t)
	fake6.Lock()
	fake6.Leases6 = append(fake6.Leases6, map[string]any{"ip-address": "fd00:db8:2:100::", "prefix-len": 56, "type": "IA_PD", "duid": "00:07", "iaid": 1,
		"subnet-id": 1, "state": 0, "cltt": fake6.Leases6[0]["cltt"], "valid-lft": 86400})
	fake6.Unlock()
	in := subnet6Input()
	in.PDPools = []kea.PDPool{{Prefix: "fd00:db8:3::", PrefixLen: 48, DelegatedLen: 56}}
	warnings, err := e.mgr.CheckSubnet6(context.Background(), 1, in)
	if err != nil || !strings.Contains(strings.Join(warnings, "\n"), "1 delegated prefixes without a reservation end up outside the prefix pools, so their routers get new ones: fd00:db8:2:100::/56") {
		t.Errorf("%v %v", warnings, err)
	}
	// the same prefix inside the pools is fine
	in.PDPools = []kea.PDPool{{Prefix: "fd00:db8:2::", PrefixLen: 48, DelegatedLen: 56}}
	if warnings, _ := e.mgr.CheckSubnet6(context.Background(), 1, in); strings.Contains(strings.Join(warnings, "\n"), "delegated prefixes") {
		t.Errorf("%v", warnings)
	}
}

func TestDashboardWarnsAboutFullIPv6Pools(t *testing.T) {
	e, fake6 := setup6(t)
	fake6.Lock()
	// two active leases in a pool of two addresses
	fake6.Config["subnet6"].([]any)[0].(map[string]any)["pools"] = []any{map[string]any{"pool": "fd00:db8:1::1000-fd00:db8:1::1001"}}
	fake6.Unlock()
	d := e.mgr.Dashboard(context.Background())
	if !strings.Contains(warningText(d.Warnings), "Subnet fd00:db8:1::/64 is 100 % utilized") {
		t.Errorf("%+v", d.Warnings)
	}
}

func TestHealthNamesMissingHooksOfDHCP6(t *testing.T) {
	e, fake6 := setup6(t)
	fake6.Lock()
	fake6.Unknown["subnet6-update"] = true
	fake6.Unlock()
	found := false
	for _, c := range e.mgr.Health(context.Background()).Checks {
		if c.Server == "DHCPv6" && c.Name == "Hooks" && c.Severity == SeverityInfo && strings.Contains(c.Detail, "libdhcp_subnet_cmds") {
			found = true
		}
	}
	if !found {
		t.Error("the missing subnet hook of kea-dhcp6 is not reported")
	}
}

func TestSyncStoppedHalfwayIsPartial(t *testing.T) {
	p := setupPair(t)
	p.fake.Hosts = []map[string]any{
		{"subnet-id": 1, "hw-address": "aa:bb:cc:00:00:01", "ip-address": "192.0.2.10"},
		{"subnet-id": 1, "hw-address": "aa:bb:cc:00:00:02", "ip-address": "192.0.2.11"},
	}
	p.peer.Fail["reservation-add"] = keatest.Failure{Result: 1, Text: "no room"}
	res, err := p.mgr.SyncServer(context.Background(), "alice", "standby")
	if err != nil || res.Status != StatusPartial || !strings.Contains(res.Message, "mix of both states") {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestLease6ShowsTheAccessPoint(t *testing.T) {
	e, _ := setup6(t)
	// the printer lease carries the MAC 00:00:0c:00:00:01
	withDashboard(t, e, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"mac":"00:00:0C:00:00:01","name":"attic","station_id":"ap-1","iface":"home5","signal":-55,"last_seen":"2099-01-01T00:00:00Z","device_info":{}}]`)
	})
	list, err := e.mgr.Leases6(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range list.Leases {
		if l.Address == "fd00:db8:1::1000" && (l.Wifi == nil || l.Wifi.AccessPoint != "attic") {
			t.Errorf("%+v", l.Wifi)
		}
	}
}

func TestSubnetMap6KeepsClientChosenAddressesApart(t *testing.T) {
	e, fake6 := setup6(t)
	fake6.Lock()
	fake6.Leases6 = append(fake6.Leases6, map[string]any{"ip-address": "fd00:db8:1:0:aaaa:bbbb:cccc:dddd", "type": "IA_NA", "duid": "00:09", "iaid": 1,
		"subnet-id": 1, "state": 4, "cltt": fake6.Leases6[0]["cltt"], "valid-lft": 86400, "hostname": "tablet"})
	fake6.Unlock()
	list, err := e.mgr.Subnets6(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := list.Subnets[0].Map
	if len(m.Registered) != 1 || m.Registered[0].State != "registered" || m.Registered[0].Name != "tablet" {
		t.Errorf("registered: %+v", m.Registered)
	}
	for _, a := range m.Outside {
		if a.State == "registered" || a.Address == "fd00:db8:1:0:aaaa:bbbb:cccc:dddd" {
			t.Errorf("outside: %+v", m.Outside)
		}
	}
}

// A reservation of an address a client also reported says more than the report.
func TestSubnetMap6PrefersTheReservation(t *testing.T) {
	e, fake6 := setup6(t)
	fake6.Lock()
	fake6.Leases6 = append(fake6.Leases6, map[string]any{"ip-address": "fd00:db8:1::10", "type": "IA_NA", "duid": "00:09", "iaid": 1,
		"subnet-id": 1, "state": 4, "cltt": fake6.Leases6[0]["cltt"], "valid-lft": 86400})
	fake6.Unlock()
	list, err := e.mgr.Subnets6(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := list.Subnets[0].Map
	if len(m.Registered) != 0 || len(m.Outside) != 1 || m.Outside[0].State != "reserved" {
		t.Errorf("outside %+v, registered %+v", m.Outside, m.Registered)
	}
}
