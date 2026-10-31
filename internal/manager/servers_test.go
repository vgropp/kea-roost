package manager

import (
	"context"
	"errors"
	"maps"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/vgropp/kea-roost/internal/kea"
	"github.com/vgropp/kea-roost/internal/keatest"
)

type pair struct {
	*env
	peer    *keatest.Server
	peerSrv *httptest.Server
}

func setupPair(t *testing.T) *pair {
	t.Helper()
	e := setup(t)
	peer := keatest.New()
	srv := httptest.NewServer(peer)
	t.Cleanup(srv.Close)
	client, err := kea.NewClient(kea.Options{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	e.mgr.AddPeer("standby", client)
	return &pair{e, peer, srv}
}

func (p *pair) differences(t *testing.T) []Difference {
	t.Helper()
	list := p.mgr.Servers(context.Background())
	if len(list.Servers) != 2 || !list.Servers[1].Reachable {
		t.Fatalf("servers: %+v", list.Servers)
	}
	return list.Servers[1].Differences
}

func TestChangeReachesEveryServer(t *testing.T) {
	p := setupPair(t)
	res, err := p.mgr.CreateReservation(context.Background(), "alice", input())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusOK || !strings.Contains(res.Message, "primary, standby") {
		t.Fatalf("result: %+v", res)
	}
	if len(p.fake.Hosts) != 1 || len(p.peer.Hosts) != 1 || !p.peer.HasCall("config-write") {
		t.Fatalf("hosts: %d and %d", len(p.fake.Hosts), len(p.peer.Hosts))
	}
	backups, err := p.mgr.Store().Backups()
	if err != nil || len(backups) != 2 {
		t.Fatalf("backups: %v %v", backups, err)
	}
	if !strings.Contains(backups[0].Name+backups[1].Name, "reservation-add-standby") {
		t.Fatalf("backups: %+v", backups)
	}
	if d := p.differences(t); len(d) != 0 {
		t.Fatalf("differences: %+v", d)
	}
}

func TestFailureOnThePrimaryLeavesPeersAlone(t *testing.T) {
	p := setupPair(t)
	p.fake.Fail["reservation-add"] = keatest.Failure{Result: 1, Text: "no"}
	res, err := p.mgr.CreateReservation(context.Background(), "alice", input())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusFailed || p.peer.HasCall("reservation-add") {
		t.Fatalf("result: %+v", res)
	}
}

func TestFailureOnAPeerIsPartialAndKeepsThePrimary(t *testing.T) {
	p := setupPair(t)
	p.peer.Fail["reservation-add"] = keatest.Failure{Result: 1, Text: "no"}
	res, err := p.mgr.CreateReservation(context.Background(), "alice", input())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusPartial || !strings.Contains(res.Message, "stopped at standby") {
		t.Fatalf("result: %+v", res)
	}
	if len(p.fake.Hosts) != 1 {
		t.Fatal("primary lost the change")
	}
	if p.audit(t).Status != StatusPartial {
		t.Fatal("audit does not show the partial change")
	}
}

func TestPeerBeingDownDoesNotBlockThePrimary(t *testing.T) {
	p := setupPair(t)
	p.peerSrv.Close()
	res, err := p.mgr.CreateReservation(context.Background(), "alice", input())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusPartial || len(p.fake.Hosts) != 1 {
		t.Fatalf("result: %+v", res)
	}
	list := p.mgr.Servers(context.Background())
	if list.Servers[1].Reachable || list.Servers[1].Error == "" {
		t.Fatalf("servers: %+v", list.Servers[1])
	}
}

func TestSyncMakesAPeerMatch(t *testing.T) {
	p := setupPair(t)
	p.seed()
	p.fake.Hosts[0]["client-classes"] = []any{}
	p.fake.Hosts = append(p.fake.Hosts,
		map[string]any{"subnet-id": 1, "hw-address": "aa:bb:cc:00:00:02", "ip-address": "192.0.2.11", "hostname": "new"})
	p.peer.Hosts = []map[string]any{
		{"subnet-id": 1, "hw-address": "aa:bb:cc:00:00:01", "ip-address": "192.0.2.10", "hostname": "old"},
		{"subnet-id": 1, "hw-address": "aa:bb:cc:00:00:09", "ip-address": "192.0.2.19", "hostname": "stray"},
	}
	found := map[string]string{}
	for _, d := range p.differences(t) {
		found[d.Change] = d.Target
	}
	if !strings.Contains(found["missing"], ":02") || !strings.Contains(found["extra"], ":09") || !strings.Contains(found["different"], ":01") {
		t.Fatalf("differences: %v", found)
	}

	res, err := p.mgr.SyncServer(context.Background(), "alice", "standby")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusOK {
		t.Fatalf("result: %+v", res)
	}
	if d := p.differences(t); len(d) != 0 {
		t.Fatalf("left: %+v", d)
	}
	if !p.peer.HasCall("config-write") || p.fake.HasCall("reservation-del") {
		t.Fatal("sync must write the peer and leave the primary alone")
	}
	if a := p.audit(t); a.Action != "server-sync" || a.Target != "standby" {
		t.Fatalf("audit: %+v", a)
	}
}

func TestSyncCopiesClassesAndSubnets(t *testing.T) {
	p := setupPair(t)
	p.fake.Config["client-classes"] = []any{map[string]any{"name": "direct", "option-data": []any{}}}
	p.peer.Config["client-classes"] = []any{map[string]any{"name": "stray"}}
	p.fake.Config["subnet4"].([]any)[0].(map[string]any)["pools"] = []any{map[string]any{"pool": "192.0.2.50 - 192.0.2.60"}}
	res, err := p.mgr.SyncServer(context.Background(), "alice", "standby")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusOK {
		t.Fatalf("result: %+v", res)
	}
	if d := p.differences(t); len(d) != 0 {
		t.Fatalf("left: %+v", d)
	}
}

func TestSyncLeavesMissingSubnetsToTheOperator(t *testing.T) {
	p := setupPair(t)
	p.peer.Config["subnet4"] = []any{}
	res, err := p.mgr.SyncServer(context.Background(), "alice", "standby")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusOK || len(res.Warnings) != 1 || p.peer.HasCall("config-write") {
		t.Fatalf("result: %+v", res)
	}
	if d := p.differences(t); len(d) != 1 || !d[0].Manual {
		t.Fatalf("differences: %+v", d)
	}
}

func TestSyncUnknownServer(t *testing.T) {
	p := setupPair(t)
	if _, err := p.mgr.SyncServer(context.Background(), "alice", "primary"); err != ErrNotFound {
		t.Fatalf("got %v", err)
	}
}

func TestLeaseDeleteReachesPeers(t *testing.T) {
	p := setupPair(t)
	lease := map[string]any{"ip-address": "192.0.2.100", "hw-address": "aa:bb:cc:00:00:03", "subnet-id": 1, "state": 0, "cltt": 1, "valid-lft": 7200}
	p.fake.Leases = []map[string]any{lease}
	p.peer.Leases = []map[string]any{lease}
	if err := p.mgr.DeleteLease(context.Background(), "alice", "192.0.2.100"); err != nil {
		t.Fatal(err)
	}
	if len(p.peer.Leases) != 0 {
		t.Fatal("lease is still on the peer")
	}
	p.fake.Leases = []map[string]any{lease}
	if err := p.mgr.DeleteLease(context.Background(), "alice", "192.0.2.100"); err != nil {
		t.Fatalf("a lease the peer does not know: %v", err)
	}
}

func TestPersistTriesEveryServer(t *testing.T) {
	p := setupPair(t)
	p.fake.Fail["config-write"] = keatest.Failure{Result: 1, Text: "no"}
	res := p.mgr.Persist(context.Background(), "alice")
	if res.Status != StatusFailed || len(res.Steps) != 2 || !p.peer.HasCall("config-write") {
		t.Fatalf("result: %+v", res)
	}
}

func TestReplaceRunsOnEveryServer(t *testing.T) {
	p := setupPair(t)
	p.seed()
	p.peer.Hosts = cloneHosts(p.fake.Hosts)
	in := input()
	in.NewIdentifierType, in.NewIdentifier = "hw-address", "aa:bb:cc:00:00:77"
	res, err := p.mgr.UpdateReservation(context.Background(), "alice", in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusOK {
		t.Fatalf("result: %+v", res)
	}
	if got := p.peer.Hosts[0]["hw-address"]; got != "aa:bb:cc:00:00:77" {
		t.Fatalf("peer holds %v", got)
	}
}

func TestDeleteRunsOnEveryServer(t *testing.T) {
	p := setupPair(t)
	p.seed()
	p.peer.Hosts = cloneHosts(p.fake.Hosts)
	if _, err := p.mgr.DeleteReservation(context.Background(), "alice", 1, "hw-address", "aa:bb:cc:00:00:01"); err != nil {
		t.Fatal(err)
	}
	if len(p.peer.Hosts) != 0 {
		t.Fatal("reservation is still on the peer")
	}
}

func cloneHosts(in []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, h := range in {
		out = append(out, maps.Clone(h))
	}
	return out
}

func TestDashboardShowsEveryServer(t *testing.T) {
	p := setupPair(t)
	d := p.mgr.Dashboard(context.Background())
	if len(d.Servers) != 2 || d.Servers[1].Name != "standby" || d.Servers[1].HAState != "hot-standby" {
		t.Fatalf("servers: %+v", d.Servers)
	}
	if d.Servers[1].Differences == nil || *d.Servers[1].Differences != 0 {
		t.Fatalf("peer: %+v", d.Servers[1])
	}

	p.peer.Hosts = []map[string]any{{"subnet-id": 0, "hw-address": "aa:bb:cc:00:00:09", "ip-address": "192.0.2.19"}}
	d = p.mgr.Dashboard(context.Background())
	if len(d.Warnings) != 1 || !strings.Contains(d.Warnings[0].Message, "differs") {
		t.Fatalf("warnings: %+v", d.Warnings)
	}

	p.peerSrv.Close()
	d = p.mgr.Dashboard(context.Background())
	if d.Servers[1].Reachable || !d.Server.Reachable || len(d.Warnings) != 1 || d.Warnings[0].Severity != SeverityCrit {
		t.Fatalf("dashboard: %+v", d)
	}
}

func TestDashboardWithoutPeersHasNoServerList(t *testing.T) {
	e := setup(t)
	if d := e.mgr.Dashboard(context.Background()); len(d.Servers) != 0 {
		t.Fatalf("servers: %+v", d.Servers)
	}
}

func TestMaintenanceGoesToThePartner(t *testing.T) {
	p := setupPair(t)
	res, err := p.mgr.RunHA(context.Background(), "alice", "standby", "maintenance")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusOK || !p.fake.HasCall("ha-maintenance-start") || p.peer.HasCall("ha-maintenance-start") {
		t.Fatalf("result: %+v", res)
	}
	if a := p.audit(t); a.Action != "ha-maintenance-start" || a.Target != "standby" || a.User != "alice" {
		t.Fatalf("audit: %+v", a)
	}
}

func TestEndingMaintenanceGoesToTheServerThatTookOver(t *testing.T) {
	p := setupPair(t)
	p.fake.SetHAState("partner-in-maintenance")
	p.peer.SetHAState("in-maintenance")
	if _, err := p.mgr.RunHA(context.Background(), "alice", "standby", "maintenance-cancel"); err != nil {
		t.Fatal(err)
	}
	if !p.fake.HasCall("ha-maintenance-cancel") || p.peer.HasCall("ha-maintenance-cancel") {
		t.Fatal("wrong server")
	}
}

func TestSyncNamesThePartner(t *testing.T) {
	p := setupPair(t)
	res, err := p.mgr.RunHA(context.Background(), "alice", "primary", "sync")
	if err != nil || res.Status != StatusOK {
		t.Fatalf("%+v %v", res, err)
	}
	if !p.fake.HasCall("ha-sync") {
		t.Fatal("no sync")
	}
}

func TestHAActionsFollowTheState(t *testing.T) {
	cases := map[string][]string{
		"hot-standby":            {"maintenance", "sync"},
		"in-maintenance":         {"maintenance-cancel"},
		"partner-in-maintenance": {"maintenance-cancel"},
		"terminated":             {"reset", "sync"},
		"waiting":                {"continue"},
	}
	for state, want := range cases {
		p := setupPair(t)
		p.fake.SetHAState(state)
		got := p.mgr.Servers(context.Background()).Servers[0].HA.Actions
		if !slices.Equal(got, want) {
			t.Errorf("%s: %v", state, got)
		}
	}
}

func TestHAActionIsRefusedInTheWrongState(t *testing.T) {
	p := setupPair(t)
	_, err := p.mgr.RunHA(context.Background(), "alice", "primary", "reset")
	var invalid *ValidationError
	if !errors.As(err, &invalid) || p.fake.HasCall("ha-reset") {
		t.Fatalf("got %v", err)
	}
	if _, err := p.mgr.RunHA(context.Background(), "alice", "nobody", "reset"); err != ErrNotFound {
		t.Fatalf("got %v", err)
	}
	if _, err := p.mgr.RunHA(context.Background(), "alice", "primary", "shutdown"); !errors.As(err, &invalid) {
		t.Fatalf("got %v", err)
	}
}

func TestRejectedHAActionIsReported(t *testing.T) {
	p := setupPair(t)
	p.fake.Fail["ha-maintenance-start"] = keatest.Failure{Result: 1, Text: "no"}
	res, err := p.mgr.RunHA(context.Background(), "alice", "standby", "maintenance")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusFailed || p.audit(t).Status != StatusFailed {
		t.Fatalf("result: %+v", res)
	}
}
