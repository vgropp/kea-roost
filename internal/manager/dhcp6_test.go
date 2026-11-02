package manager

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vgropp/kea-roost/internal/kea"
	"github.com/vgropp/kea-roost/internal/keatest"
)

// setup6 adds a kea-dhcp6 to the test manager. The IPv4 side knows the printer by
// its MAC, so that IPv6 data can be linked to it.
func setup6(t *testing.T) (*env, *keatest.Server) {
	t.Helper()
	e := setup(t)
	e.fake.Hosts = append(e.fake.Hosts, map[string]any{"subnet-id": 1, "hw-address": "00:00:0c:00:00:01", "ip-address": "192.0.2.10", "hostname": "printer"})
	fake6 := keatest.New6()
	srv := httptest.NewServer(fake6)
	t.Cleanup(srv.Close)
	client, err := kea.NewClient(kea.Options{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	e.mgr.SetDHCP6(client)
	return e, fake6
}

func TestLeases6(t *testing.T) {
	e, _ := setup6(t)
	list, err := e.mgr.Leases6(context.Background())
	if err != nil || len(list.Leases) != 2 || len(list.Warnings) != 0 {
		t.Fatalf("%+v %v", list, err)
	}
	printer := list.Leases[0]
	if printer.Subnet != "fd00:db8:1::/64" || printer.State != "active" || printer.PrefixLen != 128 || printer.Prefix {
		t.Errorf("%+v", printer)
	}
	if printer.Reserved == nil || *printer.Reserved != "printer" {
		t.Error("the reservation by DUID was not found")
	}
	if printer.Device == nil || printer.Device.IPAddress != "192.0.2.10" {
		t.Errorf("not linked to the IPv4 reservation of its MAC: %+v", printer.Device)
	}
	if laptop := list.Leases[1]; laptop.Device != nil || laptop.Reserved != nil {
		t.Errorf("a client without MAC got a device: %+v", laptop)
	}
}

func TestReservations6AndSubnets6(t *testing.T) {
	e, _ := setup6(t)
	reservations, err := e.mgr.Reservations6(context.Background())
	if err != nil || len(reservations.Reservations) != 1 {
		t.Fatalf("%+v %v", reservations, err)
	}
	r := reservations.Reservations[0]
	if r.IdentifierType != "duid" || r.Addresses[0] != "fd00:db8:1::10" || len(r.Leased) != 1 || r.Leased[0] != "fd00:db8:1::1000" {
		t.Errorf("%+v", r)
	}
	subnets, err := e.mgr.Subnets6(context.Background())
	if err != nil || len(subnets.Subnets) != 1 {
		t.Fatalf("%+v %v", subnets, err)
	}
	s := subnets.Subnets[0]
	if s.Leases != 2 || s.Reservations != 1 || s.Pools[0].Size != "61440" || s.DNSServers[0] != "fd00:db8:1::1" {
		t.Errorf("%+v", s)
	}
}

func TestDHCP6InDashboardAndHealth(t *testing.T) {
	e, fake6 := setup6(t)
	d := e.mgr.Dashboard(context.Background())
	if d.DHCP6 == nil || !d.DHCP6.Reachable || d.DHCP6.ActiveLeases == nil || *d.DHCP6.ActiveLeases != 2 {
		t.Fatalf("%+v", d.DHCP6)
	}
	fake6.Lock()
	fake6.Fail["status-get"] = keatest.Failure{Drop: true}
	fake6.Unlock()
	if d := e.mgr.Dashboard(context.Background()); d.DHCP6.Reachable || !strings.Contains(d.DHCP6.Error, "unreachable") {
		t.Errorf("%+v", d.DHCP6)
	}
	report := e.mgr.Health(context.Background())
	found := false
	for _, c := range report.Checks {
		if c.Server == "DHCPv6" && c.Name == "Reachable" && c.Severity == SeverityCrit {
			found = true
		}
	}
	if !found {
		t.Errorf("no failed DHCPv6 check: %+v", report.Checks)
	}
}

func TestHostsFileHasIPv6(t *testing.T) {
	e, fake6 := setup6(t)
	// a reservation by MAC without a name takes the name of the IPv4 reservation
	fake6.Lock()
	fake6.Hosts = append(fake6.Hosts, map[string]any{"subnet-id": 1, "hw-address": "00:00:0c:00:00:01", "ip-addresses": []any{"fd00:db8:1::11"}})
	fake6.Unlock()
	dir := t.TempDir()
	e.mgr.SetHostsDir(dir, []string{"lan"})
	if err := e.mgr.RefreshHostsFile(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "reservations"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, line := range []string{"192.0.2.10 printer.lan printer", "fd00:db8:1::10 printer.lan printer", "fd00:db8:1::11 printer.lan printer"} {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("%q missing in\n%s", line, got)
		}
	}
}

func TestReservation6Lifecycle(t *testing.T) {
	e, fake6 := setup6(t)
	ctx := context.Background()
	in := Reservation6Input{SubnetID: 1, IdentifierType: "duid", Identifier: "00:04:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99",
		Addresses: []string{"FD00:DB8:1:0::20"}, Hostname: "laptop", Comment: "desk"}
	res, err := e.mgr.CreateReservation6(ctx, "admin", in)
	if err != nil || res.Status != StatusOK {
		t.Fatalf("%+v %v", res, err)
	}
	if !fake6.HasCall("config-write") {
		t.Error("kea-dhcp6 did not save")
	}
	if !strings.HasSuffix(res.Backup, "-reservation-add-dhcpv6.json") {
		t.Errorf("backup %q", res.Backup)
	}
	list, _ := e.mgr.Reservations6(ctx)
	if len(list.Reservations) != 2 {
		t.Fatalf("%+v", list.Reservations)
	}
	added := list.Reservations[1]
	if added.Addresses[0] != "fd00:db8:1::20" || added.Identifier != "00:04:aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99" || added.Comment != "desk" || len(added.Leased) != 1 {
		t.Errorf("not normalized or not linked to its lease: %+v", added)
	}

	// fields the form does not know survive an update
	fake6.Lock()
	fake6.Hosts[1]["client-classes"] = []any{"phones"}
	fake6.Unlock()
	in.Addresses, in.Hostname, in.Comment = []string{"fd00:db8:1::21"}, "laptop2", ""
	if res, err := e.mgr.UpdateReservation6(ctx, "admin", in); err != nil || res.Status != StatusOK {
		t.Fatalf("%+v %v", res, err)
	}
	fake6.Lock()
	h := fake6.Hosts[1]
	fake6.Unlock()
	if h["client-classes"] == nil || h["hostname"] != "laptop2" || h["user-context"] != nil {
		t.Errorf("%+v", h)
	}

	if res, err := e.mgr.DeleteReservation6(ctx, "admin", 1, "duid", in.Identifier); err != nil || res.Status != StatusOK {
		t.Fatalf("%+v %v", res, err)
	}
	entries, _ := e.mgr.Store().AuditTail(20)
	actions := []string{}
	for _, entry := range entries {
		actions = append(actions, entry.Action)
	}
	if got := strings.Join(actions, " "); !strings.Contains(got, "reservation6-add") || !strings.Contains(got, "reservation6-del") {
		t.Errorf("audit: %s", got)
	}
}

func TestReservation6Problems(t *testing.T) {
	e, _ := setup6(t)
	for _, in := range []Reservation6Input{
		{SubnetID: 1, IdentifierType: "duid", Identifier: "00:01", Addresses: []string{"fd00:db8:9::1"}},
		{SubnetID: 1, IdentifierType: "duid", Identifier: "00:02", Addresses: []string{"fd00:db8:1::10"}},
		{SubnetID: 1, IdentifierType: "duid", Identifier: "00:03", Addresses: []string{"192.0.2.1"}},
		{SubnetID: 1, IdentifierType: "client-id", Identifier: "00:04", Addresses: []string{"fd00:db8:1::30"}},
		{SubnetID: 9, IdentifierType: "duid", Identifier: "00:05", Addresses: []string{"fd00:db8:1::31"}},
		{SubnetID: 1, IdentifierType: "duid", Identifier: "00:06", Addresses: []string{"fd00:db8:1::32"}, Hostname: "two words"},
		{SubnetID: 1, IdentifierType: "duid", Identifier: "00:03:00:01:00:00:0c:00:00:01", Addresses: []string{"fd00:db8:1::33"}},
	} {
		var problems *ValidationError
		if _, err := e.mgr.CreateReservation6(context.Background(), "admin", in); !errors.As(err, &problems) {
			t.Errorf("%+v was accepted: %v", in, err)
		}
	}
}

func TestPrefixOnlyReservation6StaysEditable(t *testing.T) {
	e, fake6 := setup6(t)
	fake6.Lock()
	fake6.Hosts = append(fake6.Hosts, map[string]any{"subnet-id": 1, "duid": "00:07", "prefixes": []any{"fd00:db8:2:100::/56"}})
	fake6.Unlock()
	in := Reservation6Input{SubnetID: 1, IdentifierType: "duid", Identifier: "00:07", Prefixes: []string{"fd00:db8:2:100::/56"}, Hostname: "router"}
	if res, err := e.mgr.UpdateReservation6(context.Background(), "admin", in); err != nil || res.Status != StatusOK {
		t.Fatalf("%+v %v", res, err)
	}
	fake6.Lock()
	h := fake6.Hosts[1]
	fake6.Unlock()
	if fmt.Sprint(h["prefixes"]) != "[fd00:db8:2:100::/56]" || h["ip-addresses"] != nil || h["hostname"] != "router" {
		t.Errorf("%+v", h)
	}
}

func TestReservation6Prefixes(t *testing.T) {
	e, fake6 := setup6(t)
	ctx := context.Background()
	in := Reservation6Input{SubnetID: 1, IdentifierType: "duid", Identifier: "00:08", Prefixes: []string{"FD00:DB8:2:200::/56"}, Hostname: "router"}
	if res, err := e.mgr.CreateReservation6(ctx, "admin", in); err != nil || res.Status != StatusOK {
		t.Fatalf("%+v %v", res, err)
	}
	fake6.Lock()
	got := fmt.Sprint(fake6.Hosts[len(fake6.Hosts)-1]["prefixes"])
	fake6.Unlock()
	if got != "[fd00:db8:2:200::/56]" {
		t.Errorf("prefixes: %s", got)
	}
	for _, c := range []struct{ prefix, want string }{
		{"fd00:db8:2:280::/57", "overlaps"},
		{"fd00:db8:2:201::/56", "bits set"},
		{"192.0.2.0/24", "no IPv6 prefix"},
	} {
		bad := Reservation6Input{SubnetID: 1, IdentifierType: "duid", Identifier: "00:09", Prefixes: []string{c.prefix}}
		_, err := e.mgr.CreateReservation6(ctx, "admin", bad)
		var v *ValidationError
		if !errors.As(err, &v) || !strings.Contains(v.Error(), c.want) {
			t.Errorf("%s: %v", c.prefix, err)
		}
	}
}

func TestMACFromDUID(t *testing.T) {
	for duid, mac := range map[string]string{
		"00:03:00:01:00:00:0c:aa:01:70":                         "00:00:0c:aa:01:70",
		"00:01:00:01:2c:5a:00:00:00:00:0c:00:00:01":             "00:00:0c:00:00:01",
		"00:04:aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99": "",
		"00:02:00:00:ff:ff:01:02:03:04:05:06:07:08":             "",
		"00:03:00:06:00:00:0c:00:00:01":                         "",
		"nonsense":                                              "",
	} {
		if got := macFromDUID(duid); got != mac {
			t.Errorf("%s: %q, want %q", duid, got, mac)
		}
	}
}

func TestLease6MACFromDUIDNamesTheDevice(t *testing.T) {
	e, fake6 := setup6(t)
	fake6.Lock()
	fake6.Leases6[0]["hw-address"] = ""
	fake6.Leases6[0]["duid"] = "00:03:00:01:00:00:0c:00:00:01"
	fake6.Unlock()
	list, err := e.mgr.Leases6(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	l := list.Leases[0]
	if l.HWAddress != "00:00:0c:00:00:01" || l.MACSource != "duid" || l.Device == nil || l.Device.Hostname != "printer" {
		t.Errorf("%+v", l)
	}
}

func TestLease6MACFromIPv4ClientID(t *testing.T) {
	e, _ := setup6(t)
	// the IPv4 lease of the printer carries the DUID of the laptop lease in its client-id
	e.fake.Lock()
	e.fake.Leases = append(e.fake.Leases, map[string]any{"ip-address": "192.0.2.10", "hw-address": "00:00:0C:00:00:01",
		"client-id": "ff:00:00:00:07:00:04:aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99", "subnet-id": 1, "state": 0, "cltt": 1, "valid-lft": 4000})
	e.fake.Unlock()
	list, err := e.mgr.Leases6(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	l := list.Leases[1]
	if l.HWAddress != "00:00:0c:00:00:01" || l.MACSource != "dhcpv4" || l.Device == nil {
		t.Errorf("%+v", l)
	}
	if duidFromClientID("01:00:00:0c:00:00:01") != "" {
		t.Error("a MAC client-id was taken for a DUID")
	}
}

func TestIPv4ReservationShowsIPv6(t *testing.T) {
	e, _ := setup6(t)
	list, err := e.mgr.Reservations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range list.Reservations {
		if r.Identifier != "00:00:0c:00:00:01" {
			continue
		}
		if r.IPv6 == nil || len(r.IPv6.Leased) != 1 || r.IPv6.Leased[0] != "fd00:db8:1::1000" || len(r.IPv6.Reserved) != 1 || r.IPv6.Reserved[0] != "fd00:db8:1::10" {
			t.Errorf("%+v", r.IPv6)
		}
		return
	}
	t.Fatal("printer reservation missing")
}

func TestReservation6RefusesShortPrefixes(t *testing.T) {
	e, _ := setup6(t)
	for _, prefix := range []string{"::/0", "fd00::/8"} {
		in := Reservation6Input{SubnetID: 1, IdentifierType: "duid", Identifier: "00:0a", Prefixes: []string{prefix}}
		_, err := e.mgr.CreateReservation6(context.Background(), "admin", in)
		var v *ValidationError
		if !errors.As(err, &v) || !strings.Contains(v.Error(), "too short") {
			t.Errorf("%s: %v", prefix, err)
		}
	}
}

func TestCheckReservation6SuggestsAnAddressOutsideThePools(t *testing.T) {
	e, _ := setup6(t)
	check, err := e.mgr.CheckReservation6(context.Background(), Reservation6Input{SubnetID: 1, Addresses: []string{"fd00:db8:1::1001"}})
	if err != nil || len(check.Warnings) != 1 || !strings.Contains(check.Warnings[0], "inside the dynamic pool") || check.Suggestion != "fd00:db8:1::2" {
		t.Fatalf("%+v %v", check, err)
	}
	check, err = e.mgr.CheckReservation6(context.Background(), Reservation6Input{SubnetID: 1, Addresses: []string{"fd00:db8:1::20"}})
	if err != nil || len(check.Warnings) != 0 || check.Suggestion != "" {
		t.Errorf("outside of the pool: %+v %v", check, err)
	}
}

func TestWithPrefix(t *testing.T) {
	p := netip.MustParsePrefix("fd00:db8:1::/64")
	for in, want := range map[string]string{
		"::20":           "fd00:db8:1::20",
		" ::1:2 ":        "fd00:db8:1::1:2",
		"fd00:db8:1::30": "fd00:db8:1::30",
		"::1:0:0:0:5":    "::1:0:0:0:5", // reaches into the prefix
		"::zz":           "::zz",
		"fd00:db8:9::1":  "fd00:db8:9::1",
	} {
		if got := withPrefix(in, p); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	if got := withPrefix("::5", netip.MustParsePrefix("fd00:db8:1:2:3:4:5:0/120")); got != "fd00:db8:1:2:3:4:5:5" {
		t.Errorf("/120: %s", got)
	}
}

func TestReservation6RefusesTheNetworkAddress(t *testing.T) {
	e, _ := setup6(t)
	in := Reservation6Input{SubnetID: 1, IdentifierType: "duid", Identifier: "00:0b", Addresses: []string{"::"}}
	_, err := e.mgr.CreateReservation6(context.Background(), "admin", in)
	var v *ValidationError
	if !errors.As(err, &v) || !strings.Contains(v.Error(), "anycast") {
		t.Errorf("%v", err)
	}
}
