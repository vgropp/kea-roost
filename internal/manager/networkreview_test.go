package manager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vgropp/kea-roost/internal/kea"
	"github.com/vgropp/kea-roost/internal/keatest"
)

const (
	macA = "02:00:00:00:00:0a"
	macB = "02:00:00:00:00:0b"
)

var testCfg = defaultSettings()

var reviewNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func reviewSubnets(t *testing.T) []kea.Subnet {
	t.Helper()
	subnets, err := kea.ParseSubnets(json.RawMessage(reviewedConfig))
	if err != nil {
		t.Fatal(err)
	}
	return subnets
}

func onlineDevice(mac string, ips ...string) arpDevice {
	return arpDevice{MAC: mac, IPv4: ips, LastSeen: reviewNow.Add(-time.Minute)}
}

func reserved(mac, ip string) kea.Host {
	return kea.Host{"subnet-id": int64(0), "hw-address": mac, "ip-address": ip}
}

func has(list []Finding, rule string, severity Severity, part string) bool {
	return slices.ContainsFunc(list, func(f Finding) bool {
		return f.Rule == rule && f.Severity == severity && strings.Contains(f.Target+" "+f.Message, part)
	})
}

func TestDeviceOnlineWithAnotherAddress(t *testing.T) {
	list := reviewNetwork(reviewSubnets(t), []kea.Host{reserved(macA, "192.0.2.10")}, nil,
		[]arpDevice{onlineDevice(macA, "192.0.2.20")}, reviewNow, testCfg)
	if !has(list, "network-moved", SeverityWarn, "192.0.2.20") {
		t.Fatalf("list: %+v", list)
	}
}

func TestForeignDeviceOnReservedAddress(t *testing.T) {
	list := reviewNetwork(reviewSubnets(t), []kea.Host{reserved(macA, "192.0.2.10")}, nil,
		[]arpDevice{onlineDevice(macB, "192.0.2.10")}, reviewNow, testCfg)
	if !has(list, "network-foreign-reservation", SeverityCrit, macB) {
		t.Fatalf("list: %+v", list)
	}
}

func TestTwoDevicesOnOneAddress(t *testing.T) {
	list := reviewNetwork(reviewSubnets(t), nil, nil,
		[]arpDevice{onlineDevice(macB, "192.0.2.20"), onlineDevice(macA, "192.0.2.20")}, reviewNow, testCfg)
	if !has(list, "network-duplicate", SeverityCrit, macA+", "+macB) {
		t.Fatalf("list: %+v", list)
	}
}

func TestOldSightingIsNoConflict(t *testing.T) {
	old := onlineDevice(macB, "192.0.2.20")
	old.LastSeen = reviewNow.Add(-time.Hour)
	list := reviewNetwork(reviewSubnets(t), nil, nil, []arpDevice{onlineDevice(macA, "192.0.2.20"), old}, reviewNow, testCfg)
	if has(list, "network-duplicate", SeverityCrit, "192.0.2.20") {
		t.Fatalf("list: %+v", list)
	}
}

func TestLeaseAgainstTheNetwork(t *testing.T) {
	lease := LeaseView{IPAddress: "192.0.2.150", HWAddress: macA, Active: true}
	list := reviewNetwork(reviewSubnets(t), nil, []LeaseView{lease}, []arpDevice{onlineDevice(macA, "192.0.2.151")}, reviewNow, testCfg)
	if !has(list, "network-lease-moved", SeverityWarn, "192.0.2.150") {
		t.Fatalf("moved: %+v", list)
	}
	list = reviewNetwork(reviewSubnets(t), nil, []LeaseView{lease}, []arpDevice{onlineDevice(macA, "192.0.2.150"), onlineDevice(macB, "192.0.2.150")}, reviewNow, testCfg)
	if !has(list, "network-foreign-lease", SeverityWarn, macB) {
		t.Fatalf("foreign: %+v", list)
	}
}

func TestOnlineInsideThePoolWithoutLease(t *testing.T) {
	list := reviewNetwork(reviewSubnets(t), nil, nil, []arpDevice{onlineDevice(macA, "192.0.2.150")}, reviewNow, testCfg)
	if !has(list, "network-pool-no-lease", SeverityWarn, "192.0.2.150") {
		t.Fatalf("list: %+v", list)
	}
	list = reviewNetwork(reviewSubnets(t), nil, nil, []arpDevice{onlineDevice(macA, "192.0.2.20")}, reviewNow, testCfg)
	if len(list) != 0 {
		t.Fatalf("outside of the pool is fine: %+v", list)
	}
}

func TestKeaKnowsWhatTheNetworkDoesNot(t *testing.T) {
	lease := LeaseView{IPAddress: "192.0.2.150", HWAddress: macA, Active: true}
	silent := onlineDevice(macA, "192.0.2.150")
	silent.LastSeen = reviewNow.Add(-3 * time.Hour)
	list := reviewNetwork(reviewSubnets(t), []kea.Host{reserved(macB, "192.0.2.10")}, []LeaseView{lease}, []arpDevice{silent}, reviewNow, testCfg)
	if !has(list, "network-lease-silent", SeverityInfo, "3 h 0 min") {
		t.Fatalf("silent: %+v", list)
	}
	if !has(list, "network-reservation-unseen", SeverityInfo, "365 days") {
		t.Fatalf("unseen: %+v", list)
	}
}

func TestEverythingInStepFindsNothing(t *testing.T) {
	lease := LeaseView{IPAddress: "192.0.2.10", HWAddress: macA, Active: true}
	list := reviewNetwork(reviewSubnets(t), []kea.Host{reserved(macA, "192.0.2.10")}, []LeaseView{lease},
		[]arpDevice{onlineDevice(macA, "192.0.2.10", "0.0.0.0")}, reviewNow, testCfg)
	if len(list) != 0 {
		t.Fatalf("list: %+v", list)
	}
}

func TestReviewListsItsRules(t *testing.T) {
	e := setup(t)
	r := e.mgr.Health(context.Background())
	if len(r.Rules) != len(reviewRules) {
		t.Fatalf("rules: %d", len(r.Rules))
	}
	i := slices.IndexFunc(r.Rules, func(x ReviewRule) bool { return x.Rule == "network-duplicate" })
	if i < 0 || r.Rules[i].Skipped != "needs the ARP monitor" {
		t.Fatalf("without a monitor the rule must say so: %+v", r.Rules[i])
	}

	answer := "[]"
	monitor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(answer))
	}))
	t.Cleanup(monitor.Close)
	e.mgr.SetARPMonitor(monitor.URL)
	if r = e.mgr.Health(context.Background()); r.Rules[i].Skipped != "ARP monitor reports no devices" {
		t.Fatalf("an empty answer is no clean result: %+v", r.Rules[i])
	}

	answer = `[{"mac":"02:00:00:00:00:0a","ipv4":["192.0.2.20"],"last_seen":"2026-10-30T12:00:00Z"}]`
	if r = e.mgr.Health(context.Background()); r.Rules[i].Skipped != "" {
		t.Fatalf("rule: %+v", r.Rules[i])
	}

	dashboard := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(dashboard.Close)
	e.mgr.SetAPDashboard(dashboard.URL)
	if r = e.mgr.Health(context.Background()); r.Rules[i].Skipped != "AP dashboard unreachable" {
		t.Fatalf("rule: %+v", r.Rules[i])
	}

	monitor.Close()
	if r = e.mgr.Health(context.Background()); r.Rules[i].Skipped != "ARP monitor unreachable" {
		t.Fatalf("rule: %+v", r.Rules[i])
	}
}

func TestFailedReviewDoesNotLookClean(t *testing.T) {
	e := setup(t)
	e.fake.Fail["config-get"] = keatest.Failure{Result: 1, Text: "no"}
	r := e.mgr.Health(context.Background())
	if !slices.ContainsFunc(r.Checks, func(c HealthCheck) bool {
		return c.Name == "Configuration review" && c.Severity == SeverityWarn
	}) {
		t.Fatalf("checks: %+v", r.Checks)
	}
}

func TestDeviceListedTwiceIsNoDuplicate(t *testing.T) {
	list := reviewNetwork(reviewSubnets(t), nil, nil,
		[]arpDevice{onlineDevice(macA, "192.0.2.20"), onlineDevice(macA, "192.0.2.20")}, reviewNow, testCfg)
	if has(list, "network-duplicate", SeverityCrit, macA) {
		t.Fatalf("list: %+v", list)
	}
}

func TestDeviceWithoutTimeIsNoAge(t *testing.T) {
	lease := LeaseView{IPAddress: "192.0.2.150", HWAddress: macA, Active: true}
	list := reviewNetwork(reviewSubnets(t), nil, []LeaseView{lease}, []arpDevice{{MAC: macA, IPv4: []string{"192.0.2.150"}}}, reviewNow, testCfg)
	if !has(list, "network-lease-silent", SeverityInfo, "did not show") {
		t.Fatalf("list: %+v", list)
	}
}

func TestNetworkRulesAreListed(t *testing.T) {
	lease := LeaseView{IPAddress: "192.0.2.150", HWAddress: macA, Active: true}
	list := reviewNetwork(reviewSubnets(t), []kea.Host{reserved(macA, "192.0.2.10"), reserved(macB, "192.0.2.11")}, []LeaseView{lease},
		[]arpDevice{onlineDevice(macA, "192.0.2.150"), onlineDevice("02:00:00:00:00:0c", "192.0.2.150", "192.0.2.10")}, reviewNow, testCfg)
	if len(list) == 0 {
		t.Fatal("no findings")
	}
	for _, f := range list {
		if !slices.ContainsFunc(reviewRules, func(r ReviewRule) bool { return r.Rule == f.Rule }) {
			t.Errorf("no rule for %+v", f)
		}
	}
}

func TestEveryFindingBelongsToARule(t *testing.T) {
	a := host("192.0.2.150", "direct-dns", "ghost")
	a["hostname"] = "same"
	b := host("192.0.2.150")
	b["hw-address"], b["hostname"] = macB, "same"
	list := findings(t, reviewedConfig, []kea.Host{a, b})
	for _, f := range list {
		if !slices.ContainsFunc(reviewRules, func(r ReviewRule) bool { return r.Rule == f.Rule }) {
			t.Errorf("no rule for %+v", f)
		}
	}
}

func TestDeviceWithSeveralAddressesIsOnlyANote(t *testing.T) {
	list := reviewNetwork(reviewSubnets(t), []kea.Host{reserved(macA, "192.0.2.10")}, nil,
		[]arpDevice{onlineDevice(macA, "192.0.2.20", "192.0.2.10")}, reviewNow, testCfg)
	if !has(list, "network-moved", SeverityInfo, "also seen") || has(list, "network-moved", SeverityWarn, "192.0.2.20") {
		t.Fatalf("list: %+v", list)
	}
}

func TestReviewLooksBackFurtherThanTheDevicesPage(t *testing.T) {
	e := setup(t)
	asked := make(chan string, 4)
	monitor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked <- r.URL.Query().Get("days")
		w.Write([]byte("[]"))
	}))
	t.Cleanup(monitor.Close)
	e.mgr.SetARPMonitor(monitor.URL)
	ctx := context.Background()
	e.mgr.fetchNetwork(ctx)
	if got := <-asked; got != "365" {
		t.Fatalf("review asked for %s days", got)
	}
	if _, err := e.mgr.arpDevices(ctx); err != nil {
		t.Fatal(err)
	}
	if got := <-asked; got != "7" {
		t.Fatalf("devices asked for %s days", got)
	}
}

func TestLatestSightingCounts(t *testing.T) {
	lease := LeaseView{IPAddress: "192.0.2.150", HWAddress: macA, Active: true}
	old := onlineDevice(macA, "192.0.2.150")
	old.LastSeen = reviewNow.Add(-3 * time.Hour)
	list := reviewNetwork(reviewSubnets(t), nil, []LeaseView{lease}, []arpDevice{onlineDevice(macA, "192.0.2.150"), old}, reviewNow, testCfg)
	if has(list, "network-lease-silent", SeverityInfo, macA) {
		t.Fatalf("list: %+v", list)
	}
}

func TestFindingsLeadToTheirEntry(t *testing.T) {
	list := reviewNetwork(reviewSubnets(t), []kea.Host{reserved(macB, "192.0.2.10")}, nil, []arpDevice{onlineDevice(macA, "192.0.2.10")}, reviewNow, testCfg)
	list = append(list, Finding{Area: "Reservation", Target: "nas"}, Finding{Area: "Class", Target: "phones"}, Finding{Area: "Subnet", Target: "192.0.2.0/24"}, Finding{Area: "Setting", Target: "x"})
	linkFindings(list)
	want := map[string][2]string{
		"network-foreign-reservation": {"client", "192.0.2.10"},
		"network-reservation-unseen":  {"reservations", macB},
	}
	for _, f := range list {
		if w, ok := want[f.Rule]; ok && (f.Page != w[0] || f.Query != w[1]) {
			t.Errorf("%s: %s %s", f.Rule, f.Page, f.Query)
		}
		switch f.Area {
		case "Reservation":
			if f.Page != "reservations" || f.Query != "nas" {
				t.Errorf("%+v", f)
			}
		case "Class":
			if f.Page != "classes" {
				t.Errorf("%+v", f)
			}
		case "Subnet":
			if f.Page != "subnets" || f.Query != "" {
				t.Errorf("%+v", f)
			}
		case "Setting":
			if f.Page != "" {
				t.Errorf("%+v", f)
			}
		}
	}
}
