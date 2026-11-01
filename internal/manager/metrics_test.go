package manager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

var metricLine = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*(\{[a-zA-Z_][a-zA-Z0-9_]*="(\\.|[^"\\])*"(,[a-zA-Z_][a-zA-Z0-9_]*="(\\.|[^"\\])*")*\})? -?[0-9.]+$`)

func checkFormat(t *testing.T, text string) {
	t.Helper()
	seen := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimSpace(text), "\n") {
		if strings.HasPrefix(line, "# TYPE ") {
			name := strings.Fields(line)[2]
			if seen[name] {
				t.Errorf("family %s appears twice", name)
			}
			seen[name] = true
			continue
		}
		if strings.HasPrefix(line, "# HELP ") {
			continue
		}
		if !metricLine.MatchString(line) {
			t.Errorf("invalid line %q", line)
		}
	}
}

func metricsText(t *testing.T, p *pair) string {
	t.Helper()
	text := string(p.mgr.Metrics(context.Background()))
	checkFormat(t, text)
	return text
}

func TestMetricsKeepTheNamesOfTheExporters(t *testing.T) {
	p := setupPair(t)
	p.fake.Stats["pkt4-ack-sent"] = [][]any{{7, "2026-10-30 12:00:00.000000"}}
	p.fake.Stats["pkt4-receive-drop"] = [][]any{{2, "2026-10-30 12:00:00.000000"}}
	p.fake.Stats["v4-reservation-conflicts"] = [][]any{{3, "2026-10-30 12:00:00.000000"}}
	p.fake.Stats["subnet[1].v4-allocation-fail-no-pools"] = [][]any{{4, "2026-10-30 12:00:00.000000"}}
	p.fake.Stats["subnet[1].v4-lease-reuses"] = [][]any{{5, "2026-10-30 12:00:00.000000"}}
	text := metricsText(t, p)
	for _, want := range []string{
		`kea_dhcp4_packets_sent_total{server="primary",operation="ack"} 7`,
		`kea_dhcp4_packets_received_total{server="primary",operation="drop"} 2`,
		`kea_dhcp4_global4_reservation_conflicts_total{server="primary"} 3`,
		`kea_dhcp4_addresses_assigned_total{server="primary",subnet="192.0.2.0/24",subnet_id="1",prefix="192.0.2.0/24",shared_network="",pool=""} 42`,
		`kea_dhcp4_addresses_total{server="primary",subnet="192.0.2.0/24",subnet_id="1",prefix="192.0.2.0/24",shared_network="",pool=""} 100`,
		`kea_dhcp4_allocation_fail_no_pools_total{server="primary",subnet="192.0.2.0/24"`,
		`kea_dhcp4_allocations_failed_total{server="primary",subnet="192.0.2.0/24",subnet_id="1",context="no-pools"} 4`,
		`kea_dhcp4_leases_reused_total{server="primary",subnet="192.0.2.0/24",subnet_id="1"} 5`,
		`kea_dhcp4_pool_addresses_assigned_total{server="primary",subnet="192.0.2.0/24",subnet_id="1",prefix="192.0.2.0/24",shared_network="",pool="192.0.2.100 - 192.0.2.199",pool_id="0"} 42`,
		`kea_dhcp4_addresses_assigned_total{server="standby",`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestPoolsDoNotCountTwice(t *testing.T) {
	text := metricsText(t, setupPair(t))
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, "kea_dhcp4_addresses_assigned_total{") && !strings.Contains(line, `pool=""`) {
			t.Errorf("pool in the subnet metric: %s", line)
		}
	}
}

func TestMetricsOfKeaRoost(t *testing.T) {
	p := setupPair(t)
	p.mgr.SetVersion("v1.2.3")
	p.peer.Hosts = []map[string]any{{"subnet-id": 0, "hw-address": macA, "ip-address": "192.0.2.19"}}
	p.fake.Config["client-classes"] = []any{map[string]any{"name": "one"}}
	monitor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`[{"mac":"` + macA + `","ipv4":["192.0.2.20"],"last_seen":"2026-10-30T11:59:00Z"}]`))
	}))
	t.Cleanup(monitor.Close)
	p.mgr.SetARPMonitor(monitor.URL)
	text := metricsText(t, p)
	for _, want := range []string{
		`roost_info{version="v1.2.3"} 1`,
		`roost_server_up{server="primary"} 1`,
		`roost_server_up{server="standby"} 1`,
		`roost_ha_state{server="primary",state="hot-standby",role="primary"} 1`,
		`roost_ha_partner_in_touch{server="primary",partner="server2"} 1`,
		`roost_findings{severity="info"} `,
		`roost_rule_findings{rule="class-unused"} 1`,
		`roost_findings_hidden 0`,
		`roost_health_checks{severity="ok"} `,
		`roost_config_differences{server="standby"} 2`,
		`roost_reservations 0`,
		`roost_leases_active 0`,
		`roost_arp_monitor_up 1`,
		`roost_devices_known 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestDownServerStillGivesMetrics(t *testing.T) {
	p := setupPair(t)
	p.peerSrv.Close()
	text := metricsText(t, p)
	if !strings.Contains(text, `roost_server_up{server="standby"} 0`) || strings.Contains(text, `kea_dhcp4_addresses_total{server="standby"`) {
		t.Fatalf("metrics:\n%s", text)
	}
}

func TestLabelValuesAreEscaped(t *testing.T) {
	set := metricSet{}
	set.add("x", "help", 1, "a", "b\"c\\d\ne")
	got := string(set.render())
	if !strings.Contains(got, `x{a="b\"c\\d\ne"} 1`) {
		t.Fatalf("got %s", got)
	}
	checkFormat(t, got)
}
