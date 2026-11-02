package manager

import (
	"context"
	"slices"
	"testing"
)

func TestReview6(t *testing.T) {
	e, fake6 := setup6(t)
	ctx := context.Background()
	rules := func(r *HealthReport) []string {
		var out []string
		for _, f := range r.Findings {
			if f.IPv6 {
				out = append(out, f.Rule)
			}
		}
		slices.Sort(out)
		return out
	}
	if got := rules(e.mgr.Health(ctx)); len(got) != 0 {
		t.Fatalf("the clean configuration has findings: %v", got)
	}

	fake6.Lock()
	s := fake6.Config["subnet6"].([]any)[0].(map[string]any)
	delete(s, "interface")
	delete(s, "option-data")
	s["pools"] = []any{map[string]any{"pool": "fd00:db8:1::1-fd00:db8:1::ff"}, map[string]any{"pool": "fd00:db8:1::f0-fd00:db8:1::1ff"}}
	fake6.Hosts = append(fake6.Hosts,
		map[string]any{"subnet-id": 1, "duid": "00:03:00:01:00:00:0c:00:00:02", "ip-addresses": []any{"fd00:db8:1::10"}, "hostname": "Printer."},
		map[string]any{"subnet-id": 1, "duid": "00:03:00:01:00:00:0c:00:00:03", "ip-addresses": []any{"fd00:db8:2::5"}})
	fake6.Unlock()
	r := e.mgr.Health(ctx)
	want := []string{"v6-dns-missing", "v6-duplicate-address", "v6-duplicate-name", "v6-pool-overlap",
		"v6-reservation-in-pool", "v6-reservation-in-pool", "v6-reservation-wrong-subnet", "v6-subnet-interface"}
	if got := rules(r); !slices.Equal(got, want) {
		t.Errorf("findings: %v", got)
	}
	for _, f := range r.Findings {
		if f.IPv6 && f.Area == "Reservation" && f.Page != "reservations" || f.IPv6 && f.Area == "Subnet" && f.Page != "subnets" {
			t.Errorf("link: %+v", f)
		}
	}
	if !slices.ContainsFunc(r.Rules, func(rule ReviewRule) bool { return rule.Rule == "v6-pool-overlap" && rule.Findings == 1 }) {
		t.Errorf("rules: %+v", r.Rules)
	}
	// a relay is enough to choose the subnet
	fake6.Lock()
	s["relay"] = map[string]any{"ip-addresses": []any{"fd00:db8:9::1"}}
	fake6.Unlock()
	if got := rules(e.mgr.Health(ctx)); slices.Contains(got, "v6-subnet-interface") {
		t.Errorf("relayed subnet: %v", got)
	}
}

func TestReviewWithoutDHCP6HasNoIPv6Rules(t *testing.T) {
	e := setup(t)
	for _, rule := range e.mgr.Health(context.Background()).Rules {
		if rule.v6 {
			t.Fatalf("%+v", rule)
		}
	}
}
