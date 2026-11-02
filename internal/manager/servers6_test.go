package manager

import (
	"context"
	"errors"
	"maps"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vgropp/kea-roost/internal/kea"
	"github.com/vgropp/kea-roost/internal/keatest"
)

// setupPair6 adds a second kea-dhcp6 as peer "standby". The IPv4 side stays a
// single server.
func setupPair6(t *testing.T) (*env, *keatest.Server, *keatest.Server) {
	t.Helper()
	e, fake6 := setup6(t)
	peer := keatest.New6()
	srv := httptest.NewServer(peer)
	t.Cleanup(srv.Close)
	client, err := kea.NewClient(kea.Options{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	e.mgr.AddPeer6("standby", client)
	return e, fake6, peer
}

// withHA gives both kea-dhcp6 fakes the HA state of a hot-standby pair.
func withHA(primary, standby *keatest.Server) {
	state := func(role, name, partner string) []any {
		return []any{map[string]any{
			"ha-mode": "hot-standby",
			"ha-servers": map[string]any{
				"local": map[string]any{"role": role, "scopes": []any{}, "server-name": name, "state": "hot-standby"},
				"remote": map[string]any{"age": 3, "clock-skew": 0, "communication-interrupted": false, "in-touch": true,
					"last-scopes": []any{}, "last-state": "hot-standby", "role": "standby", "server-name": partner},
			},
		}}
	}
	primary.Lock()
	primary.HA = state("primary", "server1", "server2")
	primary.Unlock()
	standby.Lock()
	standby.HA = state("standby", "server2", "server1")
	standby.Unlock()
}

func TestReservation6ReachesEveryDHCP6Server(t *testing.T) {
	e, fake6, peer := setupPair6(t)
	in := Reservation6Input{SubnetID: 1, IdentifierType: "duid", Identifier: "00:04:aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99",
		Addresses: []string{"fd00:db8:1::20"}, Hostname: "laptop"}
	res, err := e.mgr.CreateReservation6(context.Background(), "admin", in)
	if err != nil || res.Status != StatusOK || !strings.Contains(res.Message, "primary, standby") {
		t.Fatalf("%+v %v", res, err)
	}
	if len(fake6.Hosts) != 2 || len(peer.Hosts) != 2 || !peer.HasCall("config-write") {
		t.Fatalf("hosts: %d and %d", len(fake6.Hosts), len(peer.Hosts))
	}
	if e.fake.HasCall("reservation-add") || e.fake.HasCall("config-write") {
		t.Error("an IPv6 change reached kea-dhcp4")
	}
	backups, err := e.mgr.Store().Backups()
	if err != nil || len(backups) != 2 {
		t.Fatalf("backups: %v %v", backups, err)
	}
	names := backups[0].Name + " " + backups[1].Name
	if !strings.Contains(names, "-reservation-add-dhcpv6.json") || !strings.Contains(names, "-reservation-add-dhcpv6-standby.json") {
		t.Errorf("backups: %s", names)
	}
}

func TestFailureOnTheDHCP6PeerIsPartial(t *testing.T) {
	e, fake6, peer := setupPair6(t)
	peer.Lock()
	peer.Fail["reservation-add"] = keatest.Failure{Result: 1, Text: "no"}
	peer.Unlock()
	in := Reservation6Input{SubnetID: 1, IdentifierType: "duid", Identifier: "00:04:aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99",
		Addresses: []string{"fd00:db8:1::20"}}
	res, err := e.mgr.CreateReservation6(context.Background(), "admin", in)
	if err != nil || res.Status != StatusPartial || len(fake6.Hosts) != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	if !strings.Contains(res.Message, "stopped at standby") || !strings.HasPrefix(res.Steps[len(res.Steps)-1].Name, "standby: ") {
		t.Errorf("%+v", res)
	}
}

func TestServers6ComparesAndSyncs(t *testing.T) {
	e, fake6, peer := setupPair6(t)
	ctx := context.Background()
	peer.Lock()
	peer.Hosts = nil
	subnet := maps.Clone(peer.Config["subnet6"].([]any)[0].(map[string]any))
	subnet["valid-lifetime"] = 600
	subnet["interface"] = "br0" // differs per server and does not count
	peer.Config["subnet6"] = []any{subnet}
	peer.Unlock()

	list, err := e.mgr.Servers6(ctx)
	if err != nil || len(list.Servers) != 2 || len(list.Rows) != 2 {
		t.Fatalf("%+v %v", list, err)
	}
	diff := list.Servers[1].Differences
	if len(diff) != 2 {
		t.Fatalf("differences: %+v", diff)
	}
	for _, d := range diff {
		if d.Manual || d.Change != map[string]string{"subnet": "different", "reservation": "missing"}[d.Kind] {
			t.Errorf("%+v", d)
		}
	}
	if d := e.mgr.Dashboard(ctx); len(d.Servers6) != 2 || !d.Servers6[1].IPv6 || d.Servers6[1].Differences == nil || *d.Servers6[1].Differences != 2 {
		t.Errorf("dashboard: %+v", d.Servers6)
	} else if !strings.Contains(warningText(d.Warnings), "standby (DHCPv6) differs") {
		t.Errorf("warnings: %+v", d.Warnings)
	}

	res, err := e.mgr.SyncServer6(ctx, "admin", "standby")
	if err != nil || res.Status != StatusOK || len(res.Warnings) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(peer.Hosts) != 1 || fake6.HasCall("reservation-add") {
		t.Errorf("peer hosts: %+v", peer.Hosts)
	}
	synced := peer.Config["subnet6"].([]any)[0].(map[string]any)
	if synced["valid-lifetime"] != nil || synced["interface"] != "br0" { // the primary inherits the lifetime
		t.Errorf("synced subnet: %+v", synced)
	}
	entries, _ := e.mgr.Store().AuditTail(1)
	if entries[0].Action != "server-sync" || entries[0].Target != "standby (DHCPv6)" {
		t.Errorf("audit: %+v", entries[0])
	}
	if _, err := e.mgr.SyncServer6(ctx, "admin", "primary"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the primary was synced with itself: %v", err)
	}
	if _, err := e.mgr.SyncServer(ctx, "admin", "standby"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an IPv6 peer was taken for an IPv4 one: %v", err)
	}
}

func TestHA6(t *testing.T) {
	e, fake6, peer := setupPair6(t)
	withHA(fake6, peer)
	ctx := context.Background()
	list, err := e.mgr.Servers6(ctx)
	if err != nil || len(list.HA) != 1 || list.Servers[1].HA == nil || list.Servers[1].HA.State != "hot-standby" {
		t.Fatalf("%+v %v", list, err)
	}
	res, err := e.mgr.RunHA6(ctx, "admin", "standby", "maintenance")
	if err != nil || res.Status != StatusOK || !fake6.HasCall("ha-maintenance-start") || peer.HasCall("ha-maintenance-start") || e.fake.HasCall("ha-maintenance-start") {
		t.Fatalf("%+v %v", res, err)
	}
	entries, _ := e.mgr.Store().AuditTail(1)
	if entries[0].Action != "ha-maintenance-start" || entries[0].Target != "standby (DHCPv6)" {
		t.Errorf("audit: %+v", entries[0])
	}
	if _, err := e.mgr.RunHA(ctx, "admin", "standby", "maintenance"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an IPv6 peer was taken for an IPv4 one: %v", err)
	}

	peer.Lock()
	peer.HA[0].(map[string]any)["ha-servers"].(map[string]any)["local"].(map[string]any)["state"] = "waiting"
	peer.Unlock()
	found := false
	for _, c := range e.mgr.Health(ctx).Checks {
		if c.Server == "standby (DHCPv6)" && c.Name == "HA state" && c.Severity == SeverityWarn {
			found = true
		}
	}
	if !found {
		t.Error("the waiting standby is not in the health report")
	}
	fake6.Lock()
	fake6.HA[0].(map[string]any)["ha-servers"].(map[string]any)["local"].(map[string]any)["state"] = "waiting"
	fake6.Unlock()
	if d := e.mgr.Dashboard(ctx); d.DHCP6.HAState != "waiting" || !strings.Contains(warningText(d.Warnings), "DHCPv6 HA state of server1: waiting") {
		t.Errorf("%+v %+v", d.DHCP6, d.Warnings)
	}
}

func TestSingleDHCP6HasNoServerRows(t *testing.T) {
	e, _ := setup6(t)
	if d := e.mgr.Dashboard(context.Background()); len(d.Servers6) != 0 {
		t.Errorf("%+v", d.Servers6)
	}
	list, err := e.mgr.Servers6(context.Background())
	if err != nil || len(list.Servers) != 1 || len(list.Rows) != 1 || list.Rows[0].Role != "primary" {
		t.Errorf("%+v %v", list, err)
	}
}

func warningText(list []Warning) string {
	var b strings.Builder
	for _, w := range list {
		b.WriteString(w.Message + "\n")
	}
	return b.String()
}

func TestDeleteLease6ReachesEveryDHCP6Server(t *testing.T) {
	e, fake6, peer := setupPair6(t)
	ctx := context.Background()
	if err := e.mgr.DeleteLease6(ctx, "admin", "FD00:DB8:1:0::1000", false); err != nil {
		t.Fatal(err)
	}
	if len(fake6.Leases6) != 1 || len(peer.Leases6) != 1 || e.fake.HasCall("lease4-del") {
		t.Fatalf("leases left: %d and %d", len(fake6.Leases6), len(peer.Leases6))
	}
	entries, _ := e.mgr.Store().AuditTail(1)
	if entries[0].Action != "lease6-del" || entries[0].Target != "fd00:db8:1::1000" {
		t.Errorf("audit: %+v", entries[0])
	}
	// an address is not a prefix
	if err := e.mgr.DeleteLease6(ctx, "admin", "fd00:db8:1::1001", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted as a prefix: %v", err)
	}
	for _, bad := range []string{"192.0.2.10", "::ffff:192.0.2.10", "fe80::1%eth0", "nonsense"} {
		var invalid *ValidationError
		if err := e.mgr.DeleteLease6(ctx, "admin", bad, false); !errors.As(err, &invalid) {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

func TestDeleteLease6WithoutDHCP6(t *testing.T) {
	e := setup(t)
	if err := e.mgr.DeleteLease6(context.Background(), "admin", "fd00:db8:1::1000", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("%v", err)
	}
}

func TestDashboardHasIPv6LeasesAndPools(t *testing.T) {
	e, _ := setup6(t)
	d := e.mgr.Dashboard(context.Background())
	if len(d.RecentLeases6) != 2 || d.RecentLeases6[0].Address != "fd00:db8:1::1000" {
		t.Fatalf("recent: %+v", d.RecentLeases6)
	}
	if len(d.Subnets6) != 1 || d.Subnets6[0].Assigned != 2 || d.Subnets6[0].Total != "61440" || d.Subnets6[0].Percent == nil {
		t.Fatalf("pools: %+v", d.Subnets6)
	}
	if without := setup(t).mgr.Dashboard(context.Background()); without.RecentLeases6 == nil || len(without.Subnets6) != 0 {
		t.Errorf("%+v %+v", without.RecentLeases6, without.Subnets6)
	}
}
