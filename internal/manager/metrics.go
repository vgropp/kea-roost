package manager

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/vgropp/kea-roost/internal/kea"
)

// The Kea metrics keep the names of the common exporters, so that existing dashboards
// work. Both report them as gauges despite the suffix, because Kea can reset them.
var (
	packetsSent = map[string]string{
		"pkt4-ack-sent": "ack", "pkt4-nak-sent": "nak", "pkt4-offer-sent": "offer",
	}
	packetsReceived = map[string]string{
		"pkt4-ack-received": "ack", "pkt4-decline-received": "decline", "pkt4-discover-received": "discover",
		"pkt4-inform-received": "inform", "pkt4-nak-received": "nak", "pkt4-offer-received": "offer",
		"pkt4-parse-failed": "parse-failed", "pkt4-receive-drop": "drop", "pkt4-release-received": "release",
		"pkt4-request-received": "request", "pkt4-unknown-received": "unknown",
	}
	globalStats = map[string]string{
		"cumulative-assigned-addresses":     "cumulative_addresses_assigned_total",
		"declined-addresses":                "addresses_declined_total",
		"reclaimed-declined-addresses":      "addresses_declined_reclaimed_total",
		"reclaimed-leases":                  "addresses_reclaimed_total",
		"v4-allocation-fail":                "allocation_fail_total",
		"v4-allocation-fail-shared-network": "allocation_fail_shared_network_total",
		"v4-allocation-fail-subnet":         "allocation_fail_subnet_total",
		"v4-allocation-fail-no-pools":       "allocation_fail_no_pools_total",
		"v4-allocation-fail-classes":        "allocation_fail_classes_total",
		"v4-lease-reuses":                   "lease_reuses_total",
		"v4-reservation-conflicts":          "reservation_conflicts_total",
	}
	subnetStats = map[string]string{
		"assigned-addresses":                "addresses_assigned_total",
		"declined-addresses":                "addresses_declined_total",
		"reclaimed-declined-addresses":      "addresses_declined_reclaimed_total",
		"reclaimed-leases":                  "addresses_reclaimed_total",
		"total-addresses":                   "addresses_total",
		"cumulative-assigned-addresses":     "cumulative_addresses_assigned_total",
		"v4-allocation-fail":                "allocation_fail_total",
		"v4-allocation-fail-shared-network": "allocation_fail_shared_network_total",
		"v4-allocation-fail-subnet":         "allocation_fail_subnet_total",
		"v4-allocation-fail-no-pools":       "allocation_fail_no_pools_total",
		"v4-allocation-fail-classes":        "allocation_fail_classes_total",
		"v4-lease-reuses":                   "lease_reuses_total",
		"v4-reservation-conflicts":          "reservation_conflicts_total",
	}
	failedContexts = map[string]string{
		"v4-allocation-fail-subnet":         "subnet",
		"v4-allocation-fail-shared-network": "shared-network",
		"v4-allocation-fail-no-pools":       "no-pools",
		"v4-allocation-fail-classes":        "classes",
	}
	poolStats = map[string]string{
		"assigned-addresses":            "pool_addresses_assigned_total",
		"declined-addresses":            "pool_addresses_declined_total",
		"reclaimed-leases":              "pool_addresses_reclaimed_total",
		"reclaimed-declined-addresses":  "pool_addresses_declined_reclaimed_total",
		"cumulative-assigned-addresses": "pool_addresses_cumulative_assigned_total",
		"total-addresses":               "pool_addresses_total",
	}
	subnetStat = regexp.MustCompile(`^subnet\[(\d+)\]\.(?:pool\[(\d+)\]\.)?(.+)$`)

	// kea-dhcp6 as kea-exporter names it. Prefix pools have no statistics there.
	packetsSent6     = map[string]string{"pkt6-advertise-sent": "advertise", "pkt6-reply-sent": "reply"}
	packetsReceived6 = map[string]string{
		"pkt6-receive-drop": "drop", "pkt6-parse-failed": "parse-failed", "pkt6-solicit-received": "solicit",
		"pkt6-advertise-received": "advertise", "pkt6-request-received": "request", "pkt6-reply-received": "reply",
		"pkt6-renew-received": "renew", "pkt6-rebind-received": "rebind", "pkt6-release-received": "release",
		"pkt6-decline-received": "decline", "pkt6-infrequest-received": "infrequest", "pkt6-unknown-received": "unknown",
	}
	packetsSent4over6     = map[string]string{"pkt6-dhcpv4-response-sent": "response"}
	packetsReceived4over6 = map[string]string{"pkt6-dhcpv4-query-received": "query", "pkt6-dhcpv4-response-received": "response"}
	subnetStats6          = map[string]string{
		"assigned-nas":                 "na_assigned_total",
		"total-nas":                    "na_total",
		"assigned-pds":                 "pd_assigned_total",
		"total-pds":                    "pd_total",
		"declined-addresses":           "addresses_declined_total",
		"reclaimed-declined-addresses": "addresses_declined_reclaimed_total",
		"reclaimed-leases":             "addresses_reclaimed_total",
		"v6-reservation-conflicts":     "reservation_conflicts_total",
		"v6-ia-na-lease-reuses":        "na_reuses_total",
		"v6-ia-pd-lease-reuses":        "pd_reuses_total",
	}
	failedContexts6 = map[string]string{
		"v6-allocation-fail-subnet":         "subnet",
		"v6-allocation-fail-shared-network": "shared-network",
		"v6-allocation-fail-no-pools":       "no-pools",
		"v6-allocation-fail-classes":        "classes",
	}
)

type metricFamily struct {
	help    string
	samples []string
}

// metricSet collects samples, because a family has to stay together in the output.
type metricSet map[string]*metricFamily

var labelEscape = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// add records one sample. labels are pairs of name and value.
func (s metricSet) add(name, help string, value float64, labels ...string) {
	f := s[name]
	if f == nil {
		f = &metricFamily{help: help}
		s[name] = f
	}
	parts := make([]string, 0, len(labels)/2)
	for i := 0; i+1 < len(labels); i += 2 {
		parts = append(parts, labels[i]+`="`+labelEscape.Replace(labels[i+1])+`"`)
	}
	sample := name
	if len(parts) > 0 {
		sample += "{" + strings.Join(parts, ",") + "}"
	}
	f.samples = append(f.samples, sample+" "+strconv.FormatFloat(value, 'f', -1, 64))
}

func (s metricSet) render() []byte {
	var out bytes.Buffer
	for _, name := range slices.Sorted(maps.Keys(s)) {
		f := s[name]
		fmt.Fprintf(&out, "# HELP %s %s\n# TYPE %s gauge\n", name, f.help, name)
		for _, sample := range f.samples {
			out.WriteString(sample + "\n")
		}
	}
	return out.Bytes()
}

func boolValue(ok bool) float64 {
	if ok {
		return 1
	}
	return 0
}

func subnetLabels(server string, id string, subnets map[string]kea.Subnet) []string {
	name, network := id, ""
	prefix := ""
	if s, ok := subnets[id]; ok {
		name, prefix, network = s.Subnet, s.Subnet, s.SharedNetwork
	}
	return []string{"server", server, "subnet", name, "subnet_id", id, "prefix", prefix, "shared_network", network, "pool", ""}
}

func addKeaStats(set metricSet, server string, stats map[string]int64, subnets map[string]kea.Subnet) {
	for _, stat := range slices.Sorted(maps.Keys(stats)) {
		value := float64(stats[stat])
		if operation, ok := packetsSent[stat]; ok {
			set.add("kea_dhcp4_packets_sent_total", "Packets sent", value, "server", server, "operation", operation)
		}
		if operation, ok := packetsReceived[stat]; ok {
			set.add("kea_dhcp4_packets_received_total", "Packets received", value, "server", server, "operation", operation)
		}
		if name, ok := globalStats[stat]; ok {
			set.add("kea_dhcp4_global4_"+name, "Kea statistic "+stat, value, "server", server)
		}
		match := subnetStat.FindStringSubmatch(stat)
		if match == nil {
			continue
		}
		id, pool, key := match[1], match[2], match[3]
		labels := subnetLabels(server, id, subnets)
		if pool != "" {
			if name, ok := poolStats[key]; ok {
				set.add("kea_dhcp4_"+name, "Kea statistic "+key+" of a pool", value, append(labels[:10:10], "pool", poolText(subnets[id], pool), "pool_id", pool)...)
			}
			continue
		}
		if name, ok := subnetStats[key]; ok {
			set.add("kea_dhcp4_"+name, "Kea statistic "+key, value, labels...)
		}
		short := []string{"server", server, "subnet", labels[3], "subnet_id", id}
		if context, ok := failedContexts[key]; ok {
			set.add("kea_dhcp4_allocations_failed_total", "Allocation fail count", value, append(short, "context", context)...)
		}
		if key == "v4-lease-reuses" {
			set.add("kea_dhcp4_leases_reused_total", "Times a lease was renewed in memory", value, short...)
		}
	}
}

// addKeaStats6 reports the statistics of kea-dhcp6. The labels are those of the
// IPv4 metrics, a superset of what kea-exporter uses.
func addKeaStats6(set metricSet, server string, stats map[string]float64, subnets map[string]kea.Subnet6) {
	for _, stat := range slices.Sorted(maps.Keys(stats)) {
		value := stats[stat]
		for _, p := range []struct {
			names        map[string]string
			metric, help string
		}{
			{packetsSent6, "kea_dhcp6_packets_sent_total", "Packets sent"},
			{packetsReceived6, "kea_dhcp6_packets_received_total", "Packets received"},
			{packetsSent4over6, "kea_dhcp6_packets_sent_dhcp4_total", "DHCPv4-over-DHCPv6 packets sent"},
			{packetsReceived4over6, "kea_dhcp6_packets_received_dhcp4_total", "DHCPv4-over-DHCPv6 packets received"},
		} {
			if operation, ok := p.names[stat]; ok {
				set.add(p.metric, p.help, value, "server", server, "operation", operation)
			}
		}
		match := subnetStat.FindStringSubmatch(stat)
		if match == nil {
			continue
		}
		id, pool, key := match[1], match[2], match[3]
		name, network, poolLabel := id, "", ""
		s, known := subnets[id]
		if known {
			name, network = s.Subnet, s.SharedNetwork
		}
		if pool != "" {
			poolLabel = pool
			if i, err := strconv.Atoi(pool); err == nil && known && i < len(s.Pools) {
				poolLabel = s.Pools[i].Text
			}
		}
		labels := []string{"server", server, "subnet", name, "subnet_id", id, "prefix", name, "shared_network", network, "pool", poolLabel}
		if metric, ok := subnetStats6[key]; ok {
			set.add("kea_dhcp6_"+metric, "Kea statistic "+key, value, labels...)
		}
		if context, ok := failedContexts6[key]; ok && pool == "" {
			set.add("kea_dhcp6_allocations_failed_total", "Allocation fail count", value, "server", server, "subnet", name, "subnet_id", id, "context", context)
		}
	}
}

func poolText(s kea.Subnet, position string) string {
	i, err := strconv.Atoi(position)
	if err != nil || i >= len(s.Pools) {
		return position
	}
	return s.Pools[i].Text
}

// addServerMetrics reports one server. The metrics of kea-dhcp6 servers start with
// roost_dhcp6_, because they carry the names of the kea-dhcp4 servers.
func (m *Manager) addServerMetrics(ctx context.Context, set metricSet, s server, v6 bool) {
	ctx, cancel := context.WithTimeout(ctx, serverCheckTimeout)
	defer cancel()
	prefix := "roost_"
	if v6 {
		prefix = "roost_dhcp6_"
	}
	status, err := s.client.Status(ctx)
	set.add(prefix+"server_up", "Whether the Kea server answers", boolValue(err == nil), "server", s.name)
	if err != nil {
		return
	}
	set.add(prefix+"server_uptime_seconds", "Uptime of Kea", float64(status.Uptime), "server", s.name)
	for _, rel := range status.HA {
		local := rel.Servers.Local
		set.add(prefix+"ha_state", "HA state of the server, always 1", 1, "server", s.name, "state", local.State, "role", local.Role)
		if r := rel.Servers.Remote; r != nil {
			set.add(prefix+"ha_partner_in_touch", "Whether the HA partner answers", boolValue(r.InTouch && !r.CommunicationInterrupted), "server", s.name, "partner", r.ServerName)
			if r.ClockSkew != nil {
				set.add(prefix+"ha_clock_skew_seconds", "Clock difference to the HA partner", float64(*r.ClockSkew), "server", s.name, "partner", r.ServerName)
			}
		}
	}
	config, configErr := s.client.ConfigGet(ctx)
	if v6 {
		stats, err := s.client.StatisticValues(ctx)
		if err != nil {
			return
		}
		subnets := map[string]kea.Subnet6{}
		if list, err := kea.ParseSubnets6(config); configErr == nil && err == nil {
			for _, subnet := range list {
				subnets[strconv.FormatInt(subnet.ID, 10)] = subnet
			}
		}
		addKeaStats6(set, s.name, stats, subnets)
		return
	}
	stats, err := s.client.Statistics(ctx)
	if err != nil {
		return
	}
	subnets := map[string]kea.Subnet{}
	if list, err := kea.ParseSubnets(config); configErr == nil && err == nil {
		for _, subnet := range list {
			subnets[strconv.FormatInt(subnet.ID, 10)] = subnet
		}
	}
	addKeaStats(set, s.name, stats, subnets)
}

// Metrics renders the state in the text format of Prometheus. It only reads.
func (m *Manager) Metrics(ctx context.Context) []byte {
	set := metricSet{}
	set.add("roost_info", "Version of Kea Roost, always 1", 1, "version", m.version)
	for _, s := range m.servers() {
		m.addServerMetrics(ctx, set, s, false)
	}
	for _, s := range m.serversOf(true) {
		m.addServerMetrics(ctx, set, s, true)
	}

	report := m.Health(ctx)
	severities := map[Severity]float64{SeverityCrit: 0, SeverityWarn: 0, SeverityInfo: 0}
	hidden := 0.0
	for _, f := range report.Findings {
		if f.Hidden {
			hidden++
		} else {
			severities[f.Severity]++
		}
	}
	for _, severity := range []Severity{SeverityCrit, SeverityWarn, SeverityInfo} {
		set.add("roost_findings", "Open findings of the review", severities[severity], "severity", string(severity))
	}
	set.add("roost_findings_hidden", "Findings hidden by the operator", hidden)
	for _, r := range report.Rules {
		if r.Skipped == "" {
			set.add("roost_rule_findings", "Open findings per rule of the review", float64(r.Findings), "rule", r.Rule)
		}
	}
	set.add("roost_rules_skipped", "Review rules without data", float64(len(slices.DeleteFunc(slices.Clone(report.Rules), func(r ReviewRule) bool { return r.Skipped == "" }))))
	checks := map[Severity]float64{SeverityOK: 0, SeverityInfo: 0, SeverityWarn: 0, SeverityCrit: 0}
	for _, c := range report.Checks {
		checks[c.Severity]++
	}
	for _, severity := range []Severity{SeverityOK, SeverityInfo, SeverityWarn, SeverityCrit} {
		set.add("roost_health_checks", "Results of the health checks", checks[severity], "severity", string(severity))
	}
	for _, v := range m.Servers(ctx).Servers[1:] {
		if v.Reachable {
			set.add("roost_config_differences", "Entries that differ from the primary", float64(len(v.Differences)), "server", v.Name)
		}
	}
	if m.kea6 != nil && len(m.peers6) > 0 {
		for _, v := range m.serverList(ctx, true).Servers[1:] {
			if v.Reachable {
				set.add("roost_dhcp6_config_differences", "Entries that differ from the primary kea-dhcp6", float64(len(v.Differences)), "server", v.Name)
			}
		}
	}
	if m.kea6 != nil {
		if list, err := m.Reservations6(ctx); err == nil {
			set.add("roost_dhcp6_reservations", "IPv6 reservations", float64(len(list.Reservations)))
		}
		if leases, err := m.kea6.Leases6(ctx); err == nil {
			set.add("roost_dhcp6_leases_active", "Active IPv6 addresses, without delegated prefixes", float64(activeLeases6(leases, m.now().Unix())))
		}
	}
	if m.hostsPath != "" {
		set.add("roost_dns_hosts_file_ok", "Whether the DNS hosts file is up to date", boolValue(m.hostsCheck().Severity == SeverityOK))
	}

	subnets, err := m.subnets(ctx)
	if err == nil {
		if hosts, err := m.allHosts(ctx, subnets); err == nil {
			set.add("roost_reservations", "Reservations", float64(len(hosts)))
		}
	}
	if leases, err := m.Leases(ctx); err == nil {
		active := 0.0
		for _, l := range leases {
			if l.Active {
				active++
			}
		}
		set.add("roost_leases_active", "Active leases", active)
	}
	network := report.network
	set.add("roost_arp_monitor_up", "Whether the ARP monitor answers", boolValue(m.ARPMonitorEnabled() && network.arpErr == nil))
	if m.ARPMonitorEnabled() && network.arpErr == nil {
		online := 0.0
		for _, d := range mergeWifiClients(slices.Clone(network.devices), network.wifi) {
			if m.now().Sub(d.LastSeen) <= m.cfg().online() {
				online++
			}
		}
		set.add("roost_devices_online", "Devices seen within the online window", online)
		set.add("roost_devices_known", "Devices within the review window", float64(len(network.devices)))
	}
	if m.apURL != "" {
		set.add("roost_ap_dashboard_up", "Whether the AP dashboard answers", boolValue(network.wifiErr == nil))
		if network.wifiErr == nil {
			set.add("roost_wifi_clients", "Clients connected to the access points", float64(len(network.wifi)))
		}
	}
	return set.render()
}
