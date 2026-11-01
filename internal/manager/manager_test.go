package manager

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vgropp/kea-roost/internal/kea"
	"github.com/vgropp/kea-roost/internal/keatest"
	"github.com/vgropp/kea-roost/internal/oui"
)

type env struct {
	fake *keatest.Server
	mgr  *Manager
	srv  *httptest.Server
}

func setup(t *testing.T) *env {
	t.Helper()
	fake := keatest.New()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	client, err := kea.NewClient(kea.Options{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &env{fake, New(client, store, "primary"), srv}
}

func input() ReservationInput {
	return ReservationInput{
		SubnetID:       1,
		IdentifierType: "hw-address",
		Identifier:     "AA-BB-CC-00-00-01",
		IPAddress:      "192.0.2.10",
		Hostname:       "nas",
		Comment:        "basement",
	}
}

func (e *env) seed() {
	e.fake.Hosts = []map[string]any{{
		"subnet-id": 1, "hw-address": "aa:bb:cc:00:00:01", "ip-address": "192.0.2.10", "hostname": "nas",
		"boot-file-name": "pxe.efi", "client-classes": []any{"printers"},
		"user-context": map[string]any{"comment": "old", "owner": "alice"},
		"option-data":  []any{map[string]any{"name": "ntp-servers", "code": 42, "space": "dhcp4", "csv-format": true, "data": "192.0.2.1"}},
	}}
}

func (e *env) audit(t *testing.T) AuditEntry {
	t.Helper()
	entries, err := e.mgr.Store().AuditTail(1)
	if err != nil || len(entries) != 1 {
		t.Fatalf("audit: %v %v", entries, err)
	}
	return entries[0]
}

func TestCreatePersistsAndVerifies(t *testing.T) {
	e := setup(t)
	res, err := e.mgr.CreateReservation(context.Background(), "alice", input())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusOK {
		t.Fatalf("got %+v", res)
	}
	if len(e.fake.Persisted) != 1 || e.fake.Persisted[0]["hw-address"] != "aa:bb:cc:00:00:01" {
		t.Fatalf("not persisted: %+v", e.fake.Persisted)
	}
	want := []string{"config-get", "reservation-add", "config-write", "reservation-get"}
	var got []string
	for _, c := range e.fake.Calls {
		for _, w := range want {
			if c == w && (len(got) == 0 || got[len(got)-1] != c) {
				got = append(got, c)
			}
		}
	}
	tail := strings.Join(got, ",")
	if !strings.HasSuffix(tail, strings.Join(want, ",")) {
		t.Errorf("order: %s", tail)
	}

	data, err := os.ReadFile(e.backupFile(t, res.Backup))
	if err != nil || !strings.Contains(string(data), "subnet4") {
		t.Errorf("backup: %v", err)
	}
	info, _ := os.Stat(e.backupFile(t, res.Backup))
	if info.Mode().Perm() != 0o600 {
		t.Errorf("backup holds credentials but has mode %v", info.Mode())
	}
	if a := e.audit(t); a.User != "alice" || a.Status != StatusOK || a.Backup != res.Backup {
		t.Errorf("audit %+v", a)
	}
}

func (e *env) backupFile(t *testing.T, name string) string {
	t.Helper()
	path, err := e.mgr.Store().BackupPath(name)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCreateValidation(t *testing.T) {
	cases := map[string]struct {
		change func(*ReservationInput)
		want   string
	}{
		"bad mac":         {func(i *ReservationInput) { i.Identifier = "xx" }, "MAC"},
		"unknown type":    {func(i *ReservationInput) { i.IdentifierType = "foo" }, "identifier type"},
		"unknown subnet":  {func(i *ReservationInput) { i.SubnetID = 9 }, "does not exist"},
		"foreign ip":      {func(i *ReservationInput) { i.IPAddress = "10.0.0.1" }, "is not in subnet"},
		"ipv6":            {func(i *ReservationInput) { i.IPAddress = "2001:db8::1" }, "IPv4"},
		"network address": {func(i *ReservationInput) { i.IPAddress = "192.0.2.0" }, "network or broadcast"},
		"broadcast":       {func(i *ReservationInput) { i.IPAddress = "192.0.2.255" }, "network or broadcast"},
		"bad hostname":    {func(i *ReservationInput) { i.Hostname = "a b<script>" }, "invalid hostname"},
		"nameless option": {func(i *ReservationInput) { i.Options = []ReservationOption{{Data: "x"}} }, "option"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			in := input()
			c.change(&in)
			_, err := e.mgr.CreateReservation(context.Background(), "u", in)
			var v *ValidationError
			if !errors.As(err, &v) || !strings.Contains(v.Error(), c.want) {
				t.Fatalf("got %v", err)
			}
			if e.fake.HasCall("reservation-add") || e.fake.HasCall("config-write") {
				t.Error("rejected input reached Kea")
			}
		})
	}
}

func TestDuplicates(t *testing.T) {
	e := setup(t)
	e.seed()

	_, err := e.mgr.CreateReservation(context.Background(), "u", ReservationInput{
		SubnetID: 1, IdentifierType: "hw-address", Identifier: "AA:BB:CC:00:00:01", IPAddress: "192.0.2.11",
	})
	if err == nil || !strings.Contains(err.Error(), "already reserved") {
		t.Errorf("duplicate MAC: %v", err)
	}
	_, err = e.mgr.CreateReservation(context.Background(), "u", ReservationInput{
		SubnetID: 1, IdentifierType: "hw-address", Identifier: "aa:bb:cc:00:00:02", IPAddress: "192.0.2.10",
	})
	if err == nil || !strings.Contains(err.Error(), "already reserved for") {
		t.Errorf("duplicate IP: %v", err)
	}
}

func TestAddressIsCheckedAcrossScopes(t *testing.T) {
	e := setup(t)
	e.fake.Hosts = []map[string]any{{"subnet-id": 0, "hw-address": "aa:bb:cc:00:00:02", "ip-address": "192.0.2.10", "hostname": "printer"}}
	_, err := e.mgr.CreateReservation(context.Background(), "u", input())
	if err == nil || !strings.Contains(err.Error(), "already reserved for printer (aa:bb:cc:00:00:02)") {
		t.Fatalf("got %v", err)
	}
}

func TestPoolAndLeaseOverlapWarn(t *testing.T) {
	e := setup(t)
	e.fake.Leases = []map[string]any{{"ip-address": "192.0.2.150", "hw-address": "de:ad:be:ef:00:01", "subnet-id": 1, "state": 0, "cltt": 1, "valid-lft": 1}}
	in := input()
	in.IPAddress = "192.0.2.150"
	res, err := e.mgr.CreateReservation(context.Background(), "u", in)
	if err != nil || res.Status != StatusOK {
		t.Fatalf("got %+v %v", res, err)
	}
	all := strings.Join(res.Warnings, "|")
	if !strings.Contains(all, "pool") || !strings.Contains(all, "leased to") {
		t.Errorf("warnings: %s", all)
	}
}

func TestUpdateKeepsUnknownFields(t *testing.T) {
	e := setup(t)
	e.seed()
	in := input()
	in.IPAddress = "192.0.2.11"
	in.Comment = "new"
	in.Options = []ReservationOption{{Name: "ntp-servers", Code: 42, Data: "192.0.2.2"}}
	res, err := e.mgr.UpdateReservation(context.Background(), "u", in)
	if err != nil || res.Status != StatusOK {
		t.Fatalf("got %+v %v", res, err)
	}
	h := e.fake.Hosts[0]
	if h["ip-address"] != "192.0.2.11" || h["boot-file-name"] != "pxe.efi" || h["client-classes"] == nil {
		t.Errorf("host %+v", h)
	}
	ctx := h["user-context"].(map[string]any)
	if ctx["comment"] != "new" || ctx["owner"] != "alice" {
		t.Errorf("user-context %+v", ctx)
	}
	opt := h["option-data"].([]any)[0].(map[string]any)
	if opt["data"] != "192.0.2.2" || opt["csv-format"] != true || opt["space"] != "dhcp4" {
		t.Errorf("option %+v", opt)
	}
}

func TestUpdateOwnValuesAreNoDuplicate(t *testing.T) {
	e := setup(t)
	e.seed()
	res, err := e.mgr.UpdateReservation(context.Background(), "u", input())
	if err != nil || res.Status != StatusOK {
		t.Fatalf("got %+v %v", res, err)
	}
}

func TestUpdateClearsComment(t *testing.T) {
	e := setup(t)
	e.seed()
	in := input()
	in.Comment = ""
	if _, err := e.mgr.UpdateReservation(context.Background(), "u", in); err != nil {
		t.Fatal(err)
	}
	ctx := e.fake.Hosts[0]["user-context"].(map[string]any)
	if _, ok := ctx["comment"]; ok || ctx["owner"] != "alice" {
		t.Errorf("user-context %+v", ctx)
	}
}

func TestUpdateAndDeleteUnknown(t *testing.T) {
	e := setup(t)
	if _, err := e.mgr.UpdateReservation(context.Background(), "u", input()); !errors.Is(err, ErrNotFound) {
		t.Errorf("update: %v", err)
	}
	if _, err := e.mgr.DeleteReservation(context.Background(), "u", 1, "hw-address", "aa:bb:cc:00:00:01"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete: %v", err)
	}
}

func TestDelete(t *testing.T) {
	e := setup(t)
	e.seed()
	res, err := e.mgr.DeleteReservation(context.Background(), "u", 1, "hw-address", "AA:BB:CC:00:00:01")
	if err != nil || res.Status != StatusOK {
		t.Fatalf("got %+v %v", res, err)
	}
	if len(e.fake.Hosts) != 0 || len(e.fake.Persisted) != 0 || !e.fake.HasCall("config-write") {
		t.Errorf("hosts %v persisted %v", e.fake.Hosts, e.fake.Persisted)
	}
}

func TestFailureHandling(t *testing.T) {
	cases := []struct {
		name     string
		command  string
		failure  keatest.Failure
		status   Status
		message  string
		noChange bool
	}{
		{"kea rejects", "reservation-add", keatest.Failure{Result: 1, Text: "conflict"}, StatusFailed, "rejected", true},
		{"no answer before applying", "reservation-add", keatest.Failure{Drop: true}, StatusPartial, "unclear", true},
		{"no answer after applying", "reservation-add", keatest.Failure{After: true}, StatusPartial, "unclear", false},
		{"config-write fails", "config-write", keatest.Failure{Result: 1, Text: "read-only fs"}, StatusPartial, "NOT saved", false},
		{"verify fails", "reservation-get", keatest.Failure{Drop: true}, StatusPartial, "verification", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t)
			e.fake.Fail[c.command] = c.failure
			res, err := e.mgr.CreateReservation(context.Background(), "u", input())
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != c.status || !strings.Contains(res.Message, c.message) {
				t.Fatalf("got %+v", res)
			}
			e.fake.Lock()
			hosts := len(e.fake.Hosts)
			e.fake.Unlock()
			if (hosts == 0) != c.noChange {
				t.Errorf("hosts: %d", hosts)
			}
			if a := e.audit(t); a.Status != c.status {
				t.Errorf("audit %+v", a)
			}
		})
	}
}

func TestStopsAfterFailureWithoutFurtherChanges(t *testing.T) {
	e := setup(t)
	e.fake.Fail["reservation-add"] = keatest.Failure{Result: 1, Text: "conflict"}
	if _, err := e.mgr.CreateReservation(context.Background(), "u", input()); err != nil {
		t.Fatal(err)
	}
	if e.fake.HasCall("config-write") {
		t.Error("config-write after rejected change")
	}

	e = setup(t)
	e.fake.Fail["config-write"] = keatest.Failure{Result: 1, Text: "disk"}
	if _, err := e.mgr.CreateReservation(context.Background(), "u", input()); err != nil {
		t.Fatal(err)
	}
	if e.fake.HasCall("reservation-del") {
		t.Error("automatic rollback attempted")
	}
}

func TestNoChangeWithoutBackup(t *testing.T) {
	e := setup(t)
	if err := os.RemoveAll(e.mgr.store.dir); err != nil {
		t.Fatal(err)
	}
	res, err := e.mgr.CreateReservation(context.Background(), "u", input())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusFailed || e.fake.HasCall("reservation-add") {
		t.Fatalf("got %+v", res)
	}
	if !strings.Contains(strings.Join(res.Warnings, " "), "audit log") {
		t.Errorf("audit failure not reported: %v", res.Warnings)
	}
}

func TestVerifyDetectsDifferentResult(t *testing.T) {
	e := setup(t)
	e.seed()
	e.fake.SilentUpdate = true
	in := input()
	in.IPAddress = "192.0.2.11"
	res, err := e.mgr.UpdateReservation(context.Background(), "u", in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusPartial {
		t.Fatalf("got %+v", res)
	}
}

func TestPersistRetry(t *testing.T) {
	e := setup(t)
	e.fake.Fail["config-write"] = keatest.Failure{Result: 1, Text: "disk"}
	if res := e.mgr.Persist(context.Background(), "u"); res.Status != StatusFailed {
		t.Fatalf("got %+v", res)
	}
	delete(e.fake.Fail, "config-write")
	if res := e.mgr.Persist(context.Background(), "u"); res.Status != StatusOK {
		t.Fatalf("got %+v", res)
	}
}

func TestChangeSurvivesCancelledRequest(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := e.mgr.CreateReservation(ctx, "u", input())
	if err != nil || res.Status != StatusOK {
		t.Fatalf("got %+v %v", res, err)
	}
}

func TestDeleteLease(t *testing.T) {
	e := setup(t)
	e.fake.Leases = []map[string]any{{"ip-address": "192.0.2.100"}}
	if err := e.mgr.DeleteLease(context.Background(), "u", "192.0.2.100"); err != nil {
		t.Fatal(err)
	}
	if err := e.mgr.DeleteLease(context.Background(), "u", "192.0.2.100"); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v", err)
	}
	var v *ValidationError
	if err := e.mgr.DeleteLease(context.Background(), "u", "../x"); !errors.As(err, &v) {
		t.Errorf("got %v", err)
	}
}

func TestViewsCarryVendor(t *testing.T) {
	e := setup(t)
	e.fake.Hosts = []map[string]any{{"subnet-id": 1, "hw-address": "00:00:0c:00:00:01", "ip-address": "192.0.2.10"}}
	e.fake.Leases = []map[string]any{{"ip-address": "192.0.2.100", "hw-address": "7e:00:00:00:00:01", "subnet-id": 1}}
	leases, err := e.mgr.Leases(context.Background())
	if err != nil || leases[0].Vendor != oui.Private {
		t.Fatalf("lease %+v %v", leases, err)
	}
	list, err := e.mgr.Reservations(context.Background())
	if err != nil || !strings.Contains(list.Reservations[0].Vendor, "Cisco") {
		t.Fatalf("reservation %+v %v", list, err)
	}
}

func TestLeaseStates(t *testing.T) {
	e := setup(t)
	now := time.Unix(1000, 0)
	e.mgr.now = func() time.Time { return now }
	e.fake.Leases = []map[string]any{
		{"ip-address": "192.0.2.1", "state": 0, "cltt": 900, "valid-lft": 300},
		{"ip-address": "192.0.2.2", "state": 0, "cltt": 100, "valid-lft": 300},
		{"ip-address": "192.0.2.3", "state": 1, "cltt": 900, "valid-lft": 300},
		{"ip-address": "192.0.2.4", "state": 2, "cltt": 900, "valid-lft": 300},
		{"ip-address": "192.0.2.5", "state": 9, "cltt": 900, "valid-lft": 300},
	}
	leases, err := e.mgr.Leases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		state     string
		active    bool
		remaining int64
	}{{"active", true, 200}, {"expired", false, 0}, {"declined", false, 200}, {"expired", false, 200}, {"unknown (9)", false, 200}}
	for i, w := range want {
		l := leases[i]
		if l.State != w.state || l.Active != w.active || l.Remaining != w.remaining || l.Server != "primary" {
			t.Errorf("%d: got %+v", i, l)
		}
	}
}

func TestReservationListJoinsLeaseAndPool(t *testing.T) {
	e := setup(t)
	e.fake.Hosts = []map[string]any{
		{"subnet-id": 1, "hw-address": "aa:bb:cc:00:00:01", "ip-address": "192.0.2.150"},
		{"subnet-id": 0, "hw-address": "aa:bb:cc:00:00:02", "hostname": "global"},
	}
	e.fake.Leases = []map[string]any{{"ip-address": "192.0.2.150", "subnet-id": 1, "state": 0, "cltt": time.Now().Unix(), "valid-lft": 100}}
	list, err := e.mgr.Reservations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Reservations) != 2 {
		t.Fatalf("got %+v", list)
	}
	byID := map[string]ReservationView{}
	for _, r := range list.Reservations {
		byID[r.Identifier] = r
	}
	r := byID["aa:bb:cc:00:00:01"]
	if !r.InPool || r.Lease == nil || !r.Lease.Active {
		t.Errorf("got %+v", r)
	}
	if g := byID["aa:bb:cc:00:00:02"]; g.Subnet != "global" || g.InPool {
		t.Errorf("got %+v", g)
	}
}

func TestGlobalReservationsBelongToTheirSubnet(t *testing.T) {
	e := setup(t)
	e.fake.Hosts = []map[string]any{{"subnet-id": 0, "hw-address": "aa:bb:cc:00:00:01", "ip-address": "192.0.2.150"}}
	list, err := e.mgr.Reservations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := list.Reservations[0]
	if r.Subnet != "192.0.2.0/24" || !r.Global || !r.InPool {
		t.Errorf("got %+v", r)
	}
	subnets, err := e.mgr.Subnets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if *subnets.Subnets[0].Reservations != 1 || *subnets.Reservations != 1 {
		t.Errorf("counts %d %d", *subnets.Subnets[0].Reservations, *subnets.Reservations)
	}

	in := input()
	in.SubnetID, in.Identifier, in.IPAddress = 0, "aa:bb:cc:00:00:02", "192.0.2.151"
	res, err := e.mgr.CreateReservation(context.Background(), "u", in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(res.Warnings, "|"), "pool") {
		t.Errorf("global reservation in pool not reported: %v", res.Warnings)
	}
}

func TestLeasesLinkToReservations(t *testing.T) {
	e := setup(t)
	e.fake.Hosts = []map[string]any{
		{"subnet-id": 0, "hw-address": "aa:bb:cc:00:00:01", "ip-address": "192.0.2.10", "hostname": "nas"},
		{"subnet-id": 1, "hw-address": "aa:bb:cc:00:00:02", "ip-address": "192.0.2.11", "hostname": "cam"},
	}
	e.fake.Leases = []map[string]any{
		{"ip-address": "192.0.2.10", "hw-address": "de:ad:be:ef:00:01", "subnet-id": 1},
		{"ip-address": "192.0.2.150", "hw-address": "AA:BB:CC:00:00:02"},
		{"ip-address": "192.0.2.151", "hw-address": "de:ad:be:ef:00:02"},
	}
	list, err := e.mgr.LeasesWithReservations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if list.Leases[0].Subnet != "192.0.2.0/24" {
		t.Errorf("subnet %q", list.Leases[0].Subnet)
	}
	if r := list.Leases[0].Reservation; r == nil || r.Hostname != "nas" {
		t.Errorf("match by IP: %+v", r)
	}
	if r := list.Leases[1].Reservation; r == nil || r.IPAddress != "192.0.2.11" {
		t.Errorf("match by MAC: %+v", r)
	}
	if r := list.Leases[0].Reservation; !strings.Contains(r.Conflict, "de:ad:be:ef:00:01") && r.Conflict != "" {
		t.Errorf("conflict %q", r.Conflict)
	}
	if r := list.Leases[1].Reservation; r.Conflict != "" {
		t.Errorf("same MAC, other address: %q", r.Conflict)
	}
	if r := list.Leases[2].Reservation; r != nil {
		t.Errorf("no match expected: %+v", r)
	}

	e.fake.Unknown["reservation-get-all"] = true
	list, err = e.mgr.LeasesWithReservations(context.Background())
	if err != nil || len(list.Leases) != 3 || len(list.Warnings) != 1 {
		t.Fatalf("without host hook: %+v %v", list, err)
	}
}

func TestReservationListWithoutLeaseHook(t *testing.T) {
	e := setup(t)
	e.seed()
	e.fake.Unknown["lease4-get-all"] = true
	list, err := e.mgr.Reservations(context.Background())
	if err != nil || len(list.Reservations) != 1 || len(list.Warnings) != 1 {
		t.Fatalf("got %+v %v", list, err)
	}
}

func TestDashboard(t *testing.T) {
	e := setup(t)
	e.seed()
	e.fake.Leases = []map[string]any{
		{"ip-address": "192.0.2.100", "subnet-id": 1, "state": 0, "cltt": time.Now().Unix(), "valid-lft": 100},
		{"ip-address": "192.0.2.101", "subnet-id": 1, "state": 0, "cltt": 1, "valid-lft": 100},
	}
	d := e.mgr.Dashboard(context.Background())
	if !d.Server.Reachable || d.Server.Version != "3.0.3" || d.Server.LastContact == nil {
		t.Fatalf("server %+v", d.Server)
	}
	if *d.ActiveLeases != 1 || *d.TotalLeases != 2 || *d.Reservations != 1 || *d.SubnetCount != 1 {
		t.Errorf("counts %d %d %d %d", *d.ActiveLeases, *d.TotalLeases, *d.Reservations, *d.SubnetCount)
	}
	if *d.Subnets[0].Percent != 1 || *d.Subnets[0].Pools[0].Assigned != 1 || *d.Subnets[0].Pools[0].Total != 100 {
		t.Errorf("subnet %+v", d.Subnets[0])
	}
	if len(d.HA) != 1 || d.HA[0].Severity != SeverityOK || *d.HA[0].UnsentUpdates != 7 {
		t.Errorf("ha %+v", d.HA)
	}
	if len(d.Warnings) != 0 {
		t.Errorf("warnings %+v", d.Warnings)
	}
}

func TestDashboardUnreachable(t *testing.T) {
	e := setup(t)
	if d := e.mgr.Dashboard(context.Background()); !d.Server.Reachable {
		t.Fatal("expected reachable")
	}
	e.srv.Close()
	d := e.mgr.Dashboard(context.Background())
	if d.Server.Reachable || d.Server.Error == "" || d.Server.LastContact == nil {
		t.Fatalf("server %+v", d.Server)
	}
	if len(d.Warnings) != 1 || d.Warnings[0].Severity != SeverityCrit {
		t.Fatalf("warnings %+v", d.Warnings)
	}
}

func TestDashboardReportsMissingHooks(t *testing.T) {
	e := setup(t)
	e.fake.Unknown["lease4-get-all"] = true
	e.fake.Unknown["reservation-get-all"] = true
	d := e.mgr.Dashboard(context.Background())
	if !d.Server.Reachable || d.ActiveLeases != nil || d.Reservations != nil || *d.SubnetCount != 1 {
		t.Fatalf("got %+v", d)
	}
	if len(d.Warnings) != 2 {
		t.Fatalf("warnings %+v", d.Warnings)
	}
}

func TestHAWarnings(t *testing.T) {
	skew := int64(120)
	w := remoteWarnings(&kea.HAServerRemote{ServerName: "b", InTouch: false, CommunicationInterrupted: true, LastState: "partner-down", ClockSkew: &skew}, defaultSettings().ClockSkewSeconds)
	if len(w) != 4 {
		t.Fatalf("got %+v", w)
	}
	if HAStateSeverity("terminated") != SeverityCrit || HAStateSeverity("partner-down") != SeverityWarn || HAStateSeverity("hot-standby") != SeverityOK {
		t.Error("severity mapping")
	}
}

func TestUtilizationCountsOnlyPoolLeases(t *testing.T) {
	e := setup(t)
	now := time.Now().Unix()
	lease := func(ip string, state int) map[string]any {
		return map[string]any{"ip-address": ip, "subnet-id": 1, "state": state, "cltt": now, "valid-lft": 100}
	}
	e.fake.Leases = []map[string]any{
		lease("192.0.2.10", 0),  // reservation outside the pool
		lease("192.0.2.11", 0),  // reservation outside the pool
		lease("192.0.2.100", 0), // in pool
		lease("192.0.2.101", 1), // declined, still blocks the address
		lease("192.0.2.102", 3), // released
	}
	list, err := e.mgr.Subnets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s := list.Subnets[0]
	if *s.Assigned != 2 || *s.Total != 100 || *s.OutsidePools != 2 || *s.Percent != 2 {
		t.Errorf("subnet: %d/%d, outside %d, %v", *s.Assigned, *s.Total, *s.OutsidePools, *s.Percent)
	}
	if *s.Pools[0].Assigned != 2 {
		t.Errorf("pool: %d", *s.Pools[0].Assigned)
	}
}

func TestUtilizationFallsBackToStatistics(t *testing.T) {
	e := setup(t)
	e.fake.Unknown["lease4-get-all"] = true
	list, err := e.mgr.Subnets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s := list.Subnets[0]; *s.Assigned != 42 || s.OutsidePools != nil || *s.Pools[0].Assigned != 42 {
		t.Errorf("subnet %+v", s)
	}
}

func TestDashboardUtilizationWarning(t *testing.T) {
	e := setup(t)
	for i := range 97 {
		e.fake.Leases = append(e.fake.Leases, map[string]any{
			"ip-address": fmt.Sprintf("192.0.2.%d", 100+i), "subnet-id": 1, "state": 0, "cltt": time.Now().Unix(), "valid-lft": 100,
		})
	}
	d := e.mgr.Dashboard(context.Background())
	if len(d.Warnings) != 1 || d.Warnings[0].Severity != SeverityCrit {
		t.Fatalf("got %+v", d.Warnings)
	}
}

func TestNormalizeIdentifier(t *testing.T) {
	good := map[[2]string]string{
		{"hw-address", "AA-BB-CC-DD-EE-FF"}: "aa:bb:cc:dd:ee:ff",
		{"client-id", "01:AA"}:              "01:aa",
		{"circuit-id", "'port 1'"}:          "'port 1'",
		{"duid", "0102"}:                    "0102",
	}
	for in, want := range good {
		got, err := NormalizeIdentifier(in[0], in[1])
		if err != nil || got != want {
			t.Errorf("%v: %q %v", in, got, err)
		}
	}
	for _, in := range [][2]string{{"hw-address", "aa:bb"}, {"client-id", "xyz"}, {"nope", "aa"}, {"duid", ""}} {
		if _, err := NormalizeIdentifier(in[0], in[1]); err == nil {
			t.Errorf("%v: expected error", in)
		}
	}
}

func TestBackupNames(t *testing.T) {
	e := setup(t)
	name, err := e.mgr.Store().SaveBackup("reservation-add", []byte("{}"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.mgr.Store().BackupPath(name); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"../audit.log", "kea-config-x.json", "/etc/passwd", ""} {
		if _, err := e.mgr.Store().BackupPath(bad); !errors.Is(err, ErrInvalidBackup) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	list, err := e.mgr.Store().Backups()
	if err != nil || len(list) != 1 {
		t.Fatalf("got %v %v", list, err)
	}
}

func TestLeaseDeleteWaitsForRunningChange(t *testing.T) {
	e := setup(t)
	e.fake.Leases = []map[string]any{{"ip-address": "192.0.2.100", "subnet-id": 1, "state": 0, "cltt": 1, "valid-lft": 7200}}
	e.mgr.writeMu.Lock()
	done := make(chan error, 1)
	go func() { done <- e.mgr.DeleteLease(context.Background(), "alice", "192.0.2.100") }()
	select {
	case <-done:
		t.Fatal("did not wait")
	case <-time.After(50 * time.Millisecond):
	}
	e.mgr.writeMu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAuditNamesTheReservation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	in := input()
	if _, err := e.mgr.CreateReservation(ctx, "alice", in); err != nil {
		t.Fatal(err)
	}
	if a := e.audit(t); !strings.HasSuffix(a.Target, "(nas, 192.0.2.10)") {
		t.Errorf("add: %s", a.Target)
	}
	in.Hostname, in.IPAddress = "nas2", "192.0.2.11"
	if _, err := e.mgr.UpdateReservation(ctx, "alice", in); err != nil {
		t.Fatal(err)
	}
	if a := e.audit(t); !strings.HasSuffix(a.Target, "(nas, 192.0.2.10 -> nas2, 192.0.2.11)") {
		t.Errorf("update: %s", a.Target)
	}
	in.Comment = "only a note"
	if _, err := e.mgr.UpdateReservation(ctx, "alice", in); err != nil {
		t.Fatal(err)
	}
	if a := e.audit(t); !strings.HasSuffix(a.Target, "(nas2, 192.0.2.11)") {
		t.Errorf("no address change: %s", a.Target)
	}
	in.NewIdentifierType, in.NewIdentifier = "hw-address", "aa:bb:cc:00:00:77"
	if _, err := e.mgr.UpdateReservation(ctx, "alice", in); err != nil {
		t.Fatal(err)
	}
	if a := e.audit(t); !strings.HasSuffix(a.Target, "(nas2, 192.0.2.11)") || !strings.Contains(a.Target, "aa:bb:cc:00:00:77") {
		t.Errorf("replace: %s", a.Target)
	}
	if _, err := e.mgr.DeleteReservation(ctx, "alice", 1, "hw-address", "aa:bb:cc:00:00:77"); err != nil {
		t.Fatal(err)
	}
	if a := e.audit(t); a.Action != "reservation-del" || !strings.HasSuffix(a.Target, "(nas2, 192.0.2.11)") {
		t.Errorf("delete: %s", a.Target)
	}
}
