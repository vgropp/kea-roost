package manager

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vgropp/kea-roost/internal/kea"
)

const reviewedConfig = `{"Dhcp4": {
  "reservations-global": true,
  "option-data": [ { "name": "domain-name-servers", "data": "192.0.2.53" }, { "name": "domain-name", "data": "lan" } ],
  "client-classes": [
    { "name": "direct-dns", "option-data": [ { "name": "domain-name-servers", "data": "192.0.2.2" } ] },
    { "name": "phones", "test": "substring(option[60].hex,0,3) == 'abc'" },
    { "name": "unused", "option-data": [] },
    { "name": "inner" },
    { "name": "outer", "test": "member('inner')" }
  ],
  "subnet4": [ {
    "id": 1, "subnet": "192.0.2.0/24", "valid-lifetime": 7200,
    "pools": [ { "pool": "192.0.2.100 - 192.0.2.199", "option-data": [ { "name": "domain-name", "data": "pool.lan" } ] } ],
    "option-data": [ { "name": "routers", "data": "192.0.2.1" }, { "name": "domain-name", "data": "subnet.lan" } ]
  } ]
}}`

func explainFor(t *testing.T, config string, h kea.Host, lease *LeaseView, other string) *Explanation {
	t.Helper()
	return explainSeen(t, config, h, lease, other, "")
}

func explainSeen(t *testing.T, config string, h kea.Host, lease *LeaseView, other, seen string) *Explanation {
	t.Helper()
	raw := json.RawMessage(config)
	subnets, err := kea.ParseSubnets(raw)
	if err != nil {
		t.Fatal(err)
	}
	classes, err := kea.ParseClasses(raw)
	if err != nil {
		t.Fatal(err)
	}
	e, err := explain(explainInput{raw, subnets, classes, h, lease, other, seen})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func option(t *testing.T, e *Explanation, name string) EffectiveOption {
	t.Helper()
	i := slices.IndexFunc(e.Options, func(o EffectiveOption) bool { return o.Name == name })
	if i < 0 {
		t.Fatalf("option %s missing in %+v", name, e.Options)
	}
	return e.Options[i]
}

func step(t *testing.T, e *Explanation, title string) ExplainStep {
	t.Helper()
	i := slices.IndexFunc(e.Steps, func(s ExplainStep) bool { return s.Title == title })
	if i < 0 {
		t.Fatalf("step %s missing in %+v", title, e.Steps)
	}
	return e.Steps[i]
}

func host(ip string, classes ...any) kea.Host {
	return kea.Host{"subnet-id": int64(0), "hw-address": "02:00:00:00:00:01", "ip-address": ip, "client-classes": classes}
}

func TestOptionsFollowThePrecedenceOfKea(t *testing.T) {
	h := host("192.0.2.150", "direct-dns")
	h["option-data"] = []any{map[string]any{"name": "routers", "data": "192.0.2.9"}}
	e := explainFor(t, reviewedConfig, h, nil, "")

	dns := option(t, e, "domain-name-servers")
	if dns.Data != "192.0.2.2" || dns.Source != "class direct-dns" || dns.Overrides[0].Source != "global" {
		t.Errorf("class over global: %+v", dns)
	}
	name := option(t, e, "domain-name")
	if name.Data != "pool.lan" || len(name.Overrides) != 2 || name.Overrides[0].Source != "subnet 192.0.2.0/24" {
		t.Errorf("pool over subnet over global: %+v", name)
	}
	routers := option(t, e, "routers")
	if routers.Data != "192.0.2.9" || routers.Source != "reservation" {
		t.Errorf("reservation over subnet: %+v", routers)
	}
}

func TestExplanationSaysWhatItCannotKnow(t *testing.T) {
	e := explainFor(t, reviewedConfig, nil, nil, "")
	if !slices.ContainsFunc(e.Unknown, func(u string) bool { return strings.Contains(u, "phones") }) {
		t.Fatalf("unknown: %+v", e.Unknown)
	}
}

func TestExplanationFindsIgnoredReservations(t *testing.T) {
	config := strings.Replace(reviewedConfig, `"reservations-global": true`, `"reservations-global": false`, 1)
	e := explainFor(t, config, host("192.0.2.10"), nil, "")
	if step(t, e, "Reservation").Severity != SeverityCrit {
		t.Fatalf("steps: %+v", e.Steps)
	}
	if strings.Contains(step(t, e, "Address").Detail, "from the reservation") {
		t.Fatalf("an ignored reservation gives no address: %+v", e.Steps)
	}
}

func TestExplanationSeesAddressTrouble(t *testing.T) {
	e := explainFor(t, reviewedConfig, host("192.0.2.10"), nil, "02:00:00:00:00:02")
	if s := step(t, e, "Address"); s.Severity != SeverityCrit || !strings.Contains(s.Detail, "02:00:00:00:00:02") {
		t.Fatalf("taken: %+v", s)
	}
	lease := &LeaseView{IPAddress: "192.0.2.150", SubnetID: 1, Active: true}
	e = explainFor(t, reviewedConfig, host("192.0.2.10"), lease, "")
	if s := step(t, e, "Address"); s.Severity != SeverityWarn {
		t.Fatalf("moved: %+v", s)
	}
}

func TestClientIsFoundByMACAddressAndName(t *testing.T) {
	e := setup(t)
	e.seed()
	e.fake.Config["reservations-global"] = true
	e.fake.Leases = []map[string]any{{"ip-address": "192.0.2.10", "hw-address": "aa:bb:cc:00:00:01", "hostname": "nas", "subnet-id": 1, "state": 0, "cltt": time.Now().Unix(), "valid-lft": 7200}}
	for _, id := range []string{"AA-BB-CC-00-00-01", "aa:bb:cc:00:00:01", "192.0.2.10", "NAS"} {
		c, err := e.mgr.Client(context.Background(), id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if c.HWAddress != "aa:bb:cc:00:00:01" || c.Lease == nil || c.Reservation == nil || c.Name != "nas" {
			t.Fatalf("%s: %+v", id, c)
		}
	}
	if _, err := e.mgr.Client(context.Background(), "nobody"); err != ErrNotFound {
		t.Fatalf("got %v", err)
	}
	// a MAC nobody knows yet can still be explained
	if c, err := e.mgr.Client(context.Background(), "aa:bb:cc:00:00:99"); err != nil || c.Lease != nil {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestClientShowsItsChanges(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	res, err := e.mgr.CreateReservation(ctx, "alice", input())
	if err != nil || res.Status != StatusOK {
		t.Fatalf("%+v %v", res, err)
	}
	other := input()
	other.Identifier, other.IPAddress = "aa:bb:cc:00:00:02", "192.0.2.11"
	if _, err := e.mgr.CreateReservation(ctx, "bob", other); err != nil {
		t.Fatal(err)
	}
	c, err := e.mgr.Client(ctx, "aa:bb:cc:00:00:01")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.History) != 1 || c.History[0].User != "alice" {
		t.Fatalf("history: %+v", c.History)
	}
}

func findings(t *testing.T, config string, hosts []kea.Host) []Finding {
	t.Helper()
	raw := json.RawMessage(config)
	subnets, err := kea.ParseSubnets(raw)
	if err != nil {
		t.Fatal(err)
	}
	return reviewConfig(raw, subnets, hosts)
}

func found(list []Finding, severity Severity, part string) bool {
	return slices.ContainsFunc(list, func(f Finding) bool {
		return f.Severity == severity && strings.Contains(f.Target+" "+f.Message, part)
	})
}

func TestReviewFindsMistakes(t *testing.T) {
	a := host("192.0.2.150", "direct-dns", "ghost")
	a["hostname"] = "same"
	b := host("192.0.2.150")
	b["hw-address"], b["hostname"] = "02:00:00:00:00:02", "same"
	c := host("198.51.100.1")
	c["hw-address"] = "02:00:00:00:00:03"
	d := host("192.0.2.20")
	d["hw-address"] = "02:00:00:00:00:04"
	d["option-data"] = []any{map[string]any{"name": "routers", "data": "192.0.2.1"}}
	list := findings(t, reviewedConfig, []kea.Host{a, b, c, d})
	for _, want := range []struct {
		severity Severity
		part     string
	}{
		{SeverityInfo, "unused has no test"},
		{SeverityWarn, "ghost"},
		{SeverityCrit, "reserved 2 times"},
		{SeverityWarn, "used by 2 reservations"},
		{SeverityWarn, "no configured subnet"},
		{SeverityInfo, "inside the dynamic pool"},
		{SeverityInfo, "inherits anyway"},
	} {
		if !found(list, want.severity, want.part) {
			t.Errorf("%q missing in %+v", want.part, list)
		}
	}
	for _, used := range []string{"direct-dns has", "inner has", "phones has"} {
		if found(list, SeverityInfo, used) {
			t.Errorf("%q is used: %+v", used, list)
		}
	}
	if list[0].Severity != SeverityCrit {
		t.Errorf("the worst comes first: %+v", list)
	}
}

func TestReviewFindsBrokenSettingsAndSubnets(t *testing.T) {
	config := `{"Dhcp4": {
	  "subnet4": [
	    { "id": 1, "subnet": "192.0.2.0/24",
	      "pools": [ { "pool": "192.0.2.10 - 192.0.2.20" }, { "pool": "192.0.2.15 - 192.0.2.30" } ],
	      "option-data": [ { "name": "routers", "data": "198.51.100.1" } ] },
	    { "id": 2, "subnet": "198.51.100.0/24" }
	  ] }}`
	list := findings(t, config, []kea.Host{host("192.0.2.5")})
	for _, part := range []string{"reservations-global", "overlap"} {
		if !found(list, SeverityCrit, part) {
			t.Errorf("%q missing in %+v", part, list)
		}
	}
	for _, part := range []string{"is not inside the subnet", "serves nobody"} {
		if !found(list, SeverityWarn, part) {
			t.Errorf("%q missing in %+v", part, list)
		}
	}
}

func TestTrafficChecks(t *testing.T) {
	busy := PacketStats{Server: "a", Received: 1000, Dropped: 200, Ack: 100, Nak: 50, AllocFailed: 3, Decline: 1}
	got := trafficChecks(busy, defaultSettings())
	for _, name := range []string{"Dropped packets", "Refused requests", "Address allocation", "Declined addresses"} {
		if !slices.ContainsFunc(got, func(c HealthCheck) bool { return c.Name == name }) {
			t.Errorf("%s missing in %+v", name, got)
		}
	}
	busy = PacketStats{Server: "a", Standby: true, Received: 1000, Dropped: 900}
	if got := trafficChecks(busy, defaultSettings()); len(got) != 0 {
		t.Errorf("a standby drops on purpose: %+v", got)
	}
	if got := trafficChecks(PacketStats{Server: "a", Received: 4, Dropped: 2}, defaultSettings()); len(got) != 0 {
		t.Errorf("too few packets: %+v", got)
	}
}

func TestHealthCoversEveryServer(t *testing.T) {
	p := setupPair(t)
	p.peer.Hosts = []map[string]any{{"subnet-id": 0, "hw-address": "aa:bb:cc:00:00:09", "ip-address": "192.0.2.19"}}
	p.peer.Unknown["subnet4-update"] = true
	r := p.mgr.Health(context.Background())
	if r.Severity != SeverityWarn || len(r.Traffic) != 2 {
		t.Fatalf("report: %+v", r)
	}
	has := func(server, name string, severity Severity) bool {
		return slices.ContainsFunc(r.Checks, func(c HealthCheck) bool {
			return c.Server == server && c.Name == name && c.Severity == severity
		})
	}
	if !has("standby", "Configuration in step", SeverityWarn) || !has("primary", "HA state", SeverityOK) || !has("standby", "Hooks", SeverityInfo) {
		t.Fatalf("checks: %+v", r.Checks)
	}

	p.peerSrv.Close()
	r = p.mgr.Health(context.Background())
	if r.Severity != SeverityCrit || !slices.ContainsFunc(r.Checks, func(c HealthCheck) bool {
		return c.Server == "standby" && c.Name == "Reachable" && c.Severity == SeverityCrit
	}) {
		t.Fatalf("report: %+v", r)
	}
}

func TestHealthWithThePrimaryDownClaimsNoSync(t *testing.T) {
	p := setupPair(t)
	p.srv.Close()
	r := p.mgr.Health(context.Background())
	if slices.ContainsFunc(r.Checks, func(c HealthCheck) bool { return c.Name == "Configuration in step" }) {
		t.Fatalf("checks: %+v", r.Checks)
	}
}

func TestReadableSeconds(t *testing.T) {
	for sec, want := range map[int64]string{0: "0 s", 80: "80 s", 91: "1 min", 1124: "18 min", 3600: "1 h 0 min", 7380: "2 h 3 min", 86400: "1 d 0 h", 183600: "2 d 3 h"} {
		if got := readableSeconds(sec); got != want {
			t.Errorf("%d: %s", sec, got)
		}
	}
}

func TestStaticAddressOutsideOfThePools(t *testing.T) {
	e := explainSeen(t, reviewedConfig, nil, nil, "", "192.0.2.20")
	s := step(t, e, "Address")
	if !e.Static || s.Severity != SeverityInfo || !strings.Contains(s.Detail, "set on the device") {
		t.Fatalf("explanation: %+v", e)
	}
	if strings.Contains(step(t, e, "Reservation").Detail, "pool") {
		t.Fatalf("no talk of a pool: %+v", e.Steps)
	}
}

func TestAddressInsideThePoolWithoutLeaseIsAWarning(t *testing.T) {
	e := explainSeen(t, reviewedConfig, nil, nil, "", "192.0.2.150")
	if s := step(t, e, "Address"); e.Static || s.Severity != SeverityWarn || !strings.Contains(s.Detail, "another client") {
		t.Fatalf("explanation: %+v", e)
	}
}

func TestLeaseGoesBeforeWhatTheNetworkSaw(t *testing.T) {
	lease := &LeaseView{IPAddress: "192.0.2.150", SubnetID: 1, Active: true}
	e := explainSeen(t, reviewedConfig, nil, lease, "", "192.0.2.150")
	if s := step(t, e, "Address"); e.Static || s.Severity != SeverityOK {
		t.Fatalf("explanation: %+v", e)
	}
}

func TestDeviceAwayFromItsReservation(t *testing.T) {
	e := explainSeen(t, reviewedConfig, host("192.0.2.10"), nil, "", "192.0.2.20")
	if s := step(t, e, "Network"); s.Severity != SeverityWarn {
		t.Fatalf("step: %+v", s)
	}
}

func TestContainsAddress(t *testing.T) {
	for text, want := range map[string]bool{
		"192.0.2.1":                          true,
		"subnet 0, x (nas, 192.0.2.1)":       true,
		"x (nas, 192.0.2.1 -> b, 192.0.2.2)": true,
		"x (nas, 192.0.2.10)":                false,
		"x (nas, 10.192.0.2.1)":              false,
	} {
		if got := containsAddress(text, "192.0.2.1"); got != want {
			t.Errorf("%q: %v", text, got)
		}
	}
	if containsAddress("x", "") {
		t.Error("an empty address matches nothing")
	}
}

func TestClientHistoryFindsAReservationByItsAddress(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	in := ReservationInput{SubnetID: 1, IdentifierType: "client-id", Identifier: "01:02:03:04", IPAddress: "192.0.2.10", Hostname: "nas"}
	if res, err := e.mgr.CreateReservation(ctx, "alice", in); err != nil || res.Status != StatusOK {
		t.Fatalf("%+v %v", res, err)
	}
	c, err := e.mgr.Client(ctx, "192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.History) != 1 {
		t.Fatalf("history: %+v", c.History)
	}
}
