package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"github.com/vgropp/kea-roost/internal/kea"
)

// HealthCheck is the result of one check.
type HealthCheck struct {
	Name     string   `json:"name"`
	Server   string   `json:"server,omitempty"`
	Severity Severity `json:"severity"`
	Detail   string   `json:"detail"`
}

// PacketStats shows the DHCP traffic of a server since Kea started.
type PacketStats struct {
	Server      string `json:"server"`
	Uptime      int64  `json:"uptime"`
	Standby     bool   `json:"standby"`
	Received    int64  `json:"received"`
	Sent        int64  `json:"sent"`
	Discover    int64  `json:"discover"`
	Offer       int64  `json:"offer"`
	Request     int64  `json:"request"`
	Ack         int64  `json:"ack"`
	Nak         int64  `json:"nak"`
	Release     int64  `json:"release"`
	Decline     int64  `json:"decline"`
	Inform      int64  `json:"inform"`
	Dropped     int64  `json:"dropped"`
	ParseFailed int64  `json:"parseFailed"`
	AllocFailed int64  `json:"allocFailed"`
	Conflicts   int64  `json:"conflicts"`
	Reused      int64  `json:"reused"`
}

// Finding is something the configuration review noticed.
type Finding struct {
	Rule     string   `json:"rule"`
	Severity Severity `json:"severity"`
	Area     string   `json:"area"`
	Target   string   `json:"target"`
	Message  string   `json:"message"`
	// Page and Query lead to the entry: a page, or "client" for the client popup.
	Page  string `json:"page,omitempty"`
	Query string `json:"query,omitempty"`
	// Hidden marks a finding the operator accepted.
	Hidden bool `json:"hidden,omitempty"`
}

// ReviewRule is one check of the review and what came of it.
type ReviewRule struct {
	Rule     string `json:"rule"`
	Title    string `json:"title"`
	Findings int    `json:"findings"`
	Hidden   int    `json:"hidden,omitempty"`
	// Skipped says why its source of data is not available, if so.
	Skipped string `json:"skipped,omitempty"`
	network bool
}

// HealthReport is the answer of Health.
type HealthReport struct {
	Severity Severity      `json:"severity"`
	Checks   []HealthCheck `json:"checks"`
	Traffic  []PacketStats `json:"traffic"`
	Findings []Finding     `json:"findings"`
	Rules    []ReviewRule  `json:"rules"`
	network  networkData
}

func packetStats(name string, status *kea.Status, stats map[string]int64) PacketStats {
	return PacketStats{
		Server: name, Uptime: status.Uptime,
		Received: stats["pkt4-received"], Sent: stats["pkt4-sent"],
		Discover: stats["pkt4-discover-received"], Offer: stats["pkt4-offer-sent"],
		Request: stats["pkt4-request-received"], Ack: stats["pkt4-ack-sent"], Nak: stats["pkt4-nak-sent"],
		Release: stats["pkt4-release-received"], Decline: stats["pkt4-decline-received"], Inform: stats["pkt4-inform-received"],
		Dropped: stats["pkt4-receive-drop"], ParseFailed: stats["pkt4-parse-failed"],
		AllocFailed: stats["v4-allocation-fail"], Conflicts: stats["v4-reservation-conflicts"], Reused: stats["v4-lease-reuses"],
	}
}

// trafficChecks reads the counters. A standby drops what it does not serve on
// purpose, so drops only count on a server that is meant to answer.
func trafficChecks(p PacketStats, cfg Settings) []HealthCheck {
	var out []HealthCheck
	add := func(severity Severity, name, format string, args ...any) {
		out = append(out, HealthCheck{name, p.Server, severity, fmt.Sprintf(format, args...)})
	}
	// below some number of packets a share says nothing
	minPackets := int64(cfg.MinPackets)
	if !p.Standby && p.Received >= minPackets {
		if share := 100 * float64(p.Dropped) / float64(p.Received); share >= float64(cfg.DropWarnPercent) {
			add(SeverityWarn, "Dropped packets", "%d of %d packets (%.0f %%)", p.Dropped, p.Received, share)
		}
		if answers := p.Ack + p.Nak; answers >= minPackets {
			if share := 100 * float64(p.Nak) / float64(answers); share >= float64(cfg.NakWarnPercent) {
				add(SeverityWarn, "Refused requests", "%d NAKs on %d answers (%.0f %%)", p.Nak, answers, share)
			}
		}
	}
	if p.ParseFailed > 0 {
		add(SeverityWarn, "Malformed packets", "%d packets could not be parsed", p.ParseFailed)
	}
	if p.AllocFailed > 0 {
		add(SeverityCrit, "Address allocation", "%d clients got no address", p.AllocFailed)
	}
	if p.Decline > 0 {
		add(SeverityWarn, "Declined addresses", "%d times a client found its address already in use", p.Decline)
	}
	if p.Conflicts > 0 {
		add(SeverityWarn, "Reservation conflicts", "%d times a reserved address was held by another client", p.Conflicts)
	}
	return out
}

// readableSeconds picks the unit a person would use for a span of time.
func readableSeconds(sec int64) string {
	switch {
	case sec <= 90:
		return fmt.Sprintf("%d s", sec)
	case sec < 3600:
		return fmt.Sprintf("%d min", sec/60)
	case sec < 86400:
		return fmt.Sprintf("%d h %d min", sec/3600, sec%3600/60)
	}
	return fmt.Sprintf("%d d %d h", sec/86400, sec%86400/3600)
}

func majorMinor(version string) string {
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return version
	}
	return parts[0] + "." + parts[1]
}

// Hooks and what is lost without them.
var hookChecks = []struct {
	command, name, loss string
	severity            Severity
}{
	{"lease4-get-all", "libdhcp_lease_cmds", "no leases, and HA cannot work", SeverityCrit},
	{"reservation-add", "libdhcp_host_cmds", "reservations cannot be changed", SeverityCrit},
	{"subnet4-update", "libdhcp_subnet_cmds", "subnets cannot be edited", SeverityInfo},
	{"class-update", "libdhcp_class_cmds", "classes cannot be edited", SeverityInfo},
}

func (m *Manager) serverChecks(ctx context.Context, s server) ([]HealthCheck, *PacketStats, string) {
	ctx, cancel := context.WithTimeout(ctx, serverCheckTimeout)
	defer cancel()
	checks := []HealthCheck{}
	add := func(severity Severity, name, format string, args ...any) {
		checks = append(checks, HealthCheck{name, s.name, severity, fmt.Sprintf(format, args...)})
	}
	status, err := s.client.Status(ctx)
	if err != nil {
		add(SeverityCrit, "Reachable", "%v", err)
		return checks, nil, ""
	}
	version, err := s.client.Version(ctx)
	if err != nil {
		version = ""
	}
	add(SeverityOK, "Reachable", "Kea %s, up for %s", version, readableSeconds(status.Uptime))
	if status.Sockets.Status != "" && status.Sockets.Status != "ready" {
		add(SeverityCrit, "DHCP sockets", "%s", status.Sockets.Status)
	} else {
		add(SeverityOK, "DHCP sockets", "ready")
	}

	standby := false
	for _, rel := range status.HA {
		local := rel.Servers.Local
		standby = local.Role == "standby" && local.State == "hot-standby"
		add(HAStateSeverity(local.State), "HA state", "%s as %s", local.State, local.Role)
		if r := rel.Servers.Remote; r != nil {
			switch {
			case r.CommunicationInterrupted:
				add(SeverityCrit, "HA partner", "communication with %s is interrupted", r.ServerName)
			case !r.InTouch:
				add(SeverityCrit, "HA partner", "%s has never answered", r.ServerName)
			default:
				add(SeverityOK, "HA partner", "%s is %s", r.ServerName, r.LastState)
			}
			if r.ClockSkew != nil {
				severity := SeverityOK
				if skewed(*r.ClockSkew, m.cfg().ClockSkewSeconds) {
					severity = SeverityWarn
				}
				add(severity, "Clock", "%d s from %s", *r.ClockSkew, r.ServerName)
			}
		}
		if hb, err := s.client.HAHeartbeat(ctx, local.ServerName); err == nil && hb.UnsentUpdates > 0 {
			// the counter only tells the past: it keeps its value after the partner returns
			add(SeverityInfo, "Lease updates", "%d were held back while the partner was away", hb.UnsentUpdates)
		}
	}

	if commands, err := s.client.Commands(ctx); err != nil {
		add(SeverityWarn, "Hooks", "%v", err)
	} else {
		missing := false
		for _, h := range hookChecks {
			if !slices.Contains(commands, h.command) {
				missing = true
				add(h.severity, "Hooks", "%s is not loaded: %s", h.name, h.loss)
			}
		}
		if !missing {
			add(SeverityOK, "Hooks", "all hooks loaded")
		}
	}
	if config, err := s.client.ConfigGet(ctx); err == nil {
		if lossy := kea.LossyOptions(config); len(lossy) > 0 {
			add(SeverityCrit, "Writable configuration", "changes are locked: config-write would erase %s", strings.Join(lossy, ", "))
		}
	}

	var traffic *PacketStats
	if stats, err := s.client.Statistics(ctx); err != nil {
		add(SeverityWarn, "Statistics", "%v", err)
	} else {
		p := packetStats(s.name, status, stats)
		p.Standby = standby
		traffic = &p
		checks = append(checks, trafficChecks(p, m.cfg())...)
	}
	return checks, traffic, version
}

// Health runs all checks. It only reads.
func (m *Manager) Health(ctx context.Context) *HealthReport {
	r := &HealthReport{Checks: []HealthCheck{}, Traffic: []PacketStats{}, Findings: []Finding{}, Rules: []ReviewRule{}}
	versions := map[string]string{}
	for _, s := range m.servers() {
		checks, traffic, version := m.serverChecks(ctx, s)
		r.Checks = append(r.Checks, checks...)
		if traffic != nil {
			r.Traffic = append(r.Traffic, *traffic)
		}
		if version != "" {
			versions[s.name] = version
		}
	}
	if len(versions) > 1 {
		lines := map[string]bool{}
		for _, v := range versions {
			lines[majorMinor(v)] = true
		}
		if len(lines) > 1 {
			r.Checks = append(r.Checks, HealthCheck{"Versions", "", SeverityWarn, "the servers run different Kea releases"})
		} else {
			r.Checks = append(r.Checks, HealthCheck{"Versions", "", SeverityOK, "the servers run the same Kea release"})
		}
	}
	if m.HasPeers() {
		list := m.Servers(ctx).Servers
		for _, v := range list[1:] {
			switch {
			case !v.Reachable || !list[0].Reachable:
			case len(v.Differences) > 0:
				r.Checks = append(r.Checks, HealthCheck{"Configuration in step", v.Name, SeverityWarn, fmt.Sprintf("%d entries differ from the primary", len(v.Differences))})
			default:
				r.Checks = append(r.Checks, HealthCheck{"Configuration in step", v.Name, SeverityOK, "same as the primary"})
			}
		}
	}
	network := m.fetchNetwork(ctx)
	r.network = network
	if m.ARPMonitorEnabled() {
		if network.arpErr != nil {
			r.Checks = append(r.Checks, HealthCheck{"ARP monitor", "", SeverityWarn, network.arpErr.Error()})
		} else {
			r.Checks = append(r.Checks, HealthCheck{"ARP monitor", "", SeverityOK, "reachable"})
		}
	}
	if m.apURL != "" {
		if network.wifiErr != nil {
			r.Checks = append(r.Checks, HealthCheck{"AP dashboard", "", SeverityWarn, network.wifiErr.Error()})
		} else {
			r.Checks = append(r.Checks, HealthCheck{"AP dashboard", "", SeverityOK, "reachable"})
		}
	}

	if c := m.hostsCheck(); c != nil {
		r.Checks = append(r.Checks, *c)
	}

	if err := m.review(ctx, r, network); err != nil {
		r.Checks = append(r.Checks, HealthCheck{"Configuration review", "", SeverityWarn, "not possible: " + err.Error()})
	}

	r.Severity = SeverityOK
	rank := map[Severity]int{SeverityWarn: 1, SeverityCrit: 2}
	for _, c := range r.Checks {
		if rank[c.Severity] > rank[r.Severity] {
			r.Severity = c.Severity
		}
	}
	for _, f := range r.Findings {
		if !f.Hidden && rank[f.Severity] > rank[r.Severity] {
			r.Severity = f.Severity
		}
	}
	return r
}

// review fills in findings and rules. Without a result it fails loudly: an empty
// review must not look like a clean one.
func (m *Manager) review(ctx context.Context, r *HealthReport, network networkData) error {
	src := m.source(ctx).client
	config, err := src.ConfigGet(ctx)
	if err != nil {
		return err
	}
	subnets, err := kea.ParseSubnets(config)
	if err != nil {
		return err
	}
	hosts, err := hostsOn(ctx, src, subnets)
	if err != nil {
		return err
	}
	r.Findings = reviewConfig(config, subnets, hosts)
	found, skipped := m.networkFindings(ctx, subnets, hosts, network)
	r.Findings = append(r.Findings, found...)
	linkFindings(r.Findings)
	m.markHidden(r.Findings)
	sortFindings(r.Findings)
	r.Rules = reviewSummary(r.Findings, skipped)
	return nil
}

// classMember finds references to a class inside a test expression.
var classMember = regexp.MustCompile(`member\('([^']+)'\)`)

// usedClasses collects every class the configuration refers to outside of a
// reservation.
func usedClasses(node any, used map[string]bool) {
	switch v := node.(type) {
	case map[string]any:
		for key, child := range v {
			switch key {
			case "client-class":
				if name, ok := child.(string); ok {
					used[name] = true
				}
			case "client-classes", "require-client-classes", "evaluate-additional-classes":
				if list, ok := child.([]any); ok {
					for _, item := range list {
						if name, ok := item.(string); ok {
							used[name] = true
						}
					}
				}
			case "test":
				if text, ok := child.(string); ok {
					for _, m := range classMember.FindAllStringSubmatch(text, -1) {
						used[m[1]] = true
					}
				}
			}
			if key != "client-classes" {
				usedClasses(child, used)
			}
		}
	case []any:
		for _, child := range v {
			usedClasses(child, used)
		}
	}
}

// reviewConfig looks for mistakes that Kea accepts without complaint.
func reviewConfig(config json.RawMessage, subnets []kea.Subnet, hosts []kea.Host) []Finding {
	out := []Finding{}
	add := func(rule string, severity Severity, area, target, format string, args ...any) {
		out = append(out, Finding{Rule: rule, Severity: severity, Area: area, Target: target, Message: fmt.Sprintf(format, args...)})
	}
	flags := parseFlags(config)

	var doc struct {
		Dhcp4 map[string]any `json:"Dhcp4"`
	}
	classes, err := kea.ParseClasses(config)
	if err != nil || json.Unmarshal(config, &doc) != nil {
		return out
	}
	used := map[string]bool{}
	usedClasses(map[string]any{"subnet4": doc.Dhcp4["subnet4"], "shared-networks": doc.Dhcp4["shared-networks"], "client-classes-tests": classTests(doc.Dhcp4)}, used)
	for _, h := range hosts {
		for _, name := range h.Classes() {
			used[name] = true
		}
	}
	known := map[string]bool{}
	for _, c := range classes {
		known[c.Name] = true
		if c.Test == "" && !used[c.Name] {
			add("class-unused", SeverityInfo, "Class", c.Name, "has no test and nothing assigns it, so it never applies")
		}
	}

	byAddress := map[string][]string{}
	byName := map[string][]string{}
	global, inSubnet := 0, 0
	for _, h := range hosts {
		_, id := h.Identifier()
		label := id
		if name := h.Hostname(); name != "" {
			label = name
			byName[strings.ToLower(name)] = append(byName[strings.ToLower(name)], id)
		}
		if h.SubnetID() == 0 {
			global++
		} else {
			inSubnet++
		}
		for _, name := range h.Classes() {
			if !known[name] && !slices.Contains(builtinClasses, name) {
				add("class-missing", SeverityWarn, "Reservation", label, "uses the class %s, which does not exist", name)
			}
		}
		ip, err := netip.ParseAddr(h.IP())
		if err != nil {
			continue
		}
		byAddress[ip.String()] = append(byAddress[ip.String()], label)
		s := kea.SubnetFor(subnets, h)
		switch {
		case s == nil:
			add("reservation-no-subnet", SeverityWarn, "Reservation", label, "%s lies in no configured subnet", ip)
		case !s.Prefix.Contains(ip):
			add("reservation-wrong-subnet", SeverityCrit, "Reservation", label, "%s is outside of its subnet %s", ip, s.Subnet)
		case s.PoolContaining(ip) != nil:
			add("reservation-in-pool", SeverityInfo, "Reservation", label, "%s lies inside the dynamic pool, so another client may hold it first", ip)
		}
		if s != nil {
			inherited := map[string]string{}
			for _, o := range s.Options {
				inherited[o.Name] = o.Data
			}
			for _, o := range hostOptions(h) {
				if o.Name != "" && inherited[o.Name] == o.Data {
					add("reservation-inherited", SeverityInfo, "Reservation", label, "sets %s to the value it inherits anyway", o.Name)
				}
			}
		}
	}
	for _, ip := range sortedKeys(byAddress) {
		if labels := byAddress[ip]; len(labels) > 1 {
			add("duplicate-address", SeverityCrit, "Reservation", ip, "is reserved %d times: %s", len(labels), strings.Join(labels, ", "))
		}
	}
	for _, name := range sortedKeys(byName) {
		if ids := byName[name]; len(ids) > 1 {
			add("duplicate-name", SeverityWarn, "Reservation", name, "is used by %d reservations: %s", len(ids), strings.Join(ids, ", "))
		}
	}
	if global > 0 && !flags.global() {
		add("setting-global", SeverityCrit, "Setting", "reservations-global", "is off, so Kea ignores %d global reservations", global)
	}
	if inSubnet > 0 && !flags.inSubnet() {
		add("setting-in-subnet", SeverityCrit, "Setting", "reservations-in-subnet", "is off, so Kea ignores %d subnet reservations", inSubnet)
	}

	for _, s := range subnets {
		if len(s.Pools) == 0 && !slices.ContainsFunc(hosts, func(h kea.Host) bool {
			found := kea.SubnetFor(subnets, h)
			return found != nil && found.ID == s.ID
		}) {
			add("subnet-unused", SeverityWarn, "Subnet", s.Subnet, "has neither a pool nor a reservation, so it serves nobody")
		}
		pools := slices.Clone(s.Pools)
		slices.SortFunc(pools, func(a, b kea.Pool) int { return a.First.Compare(b.First) })
		for i, p := range pools {
			if !s.Prefix.Contains(p.First) || !s.Prefix.Contains(p.Last) {
				add("pool-outside", SeverityCrit, "Subnet", s.Subnet, "pool %s reaches outside of the subnet", p.Text)
			}
			if i > 0 && p.First.Compare(pools[i-1].Last) <= 0 {
				add("pool-overlap", SeverityCrit, "Subnet", s.Subnet, "pools %s and %s overlap", pools[i-1].Text, p.Text)
			}
		}
		gateways := s.Gateways()
		if len(gateways) == 0 {
			add("gateway-missing", SeverityInfo, "Subnet", s.Subnet, "offers no gateway")
		}
		for _, g := range gateways {
			if ip, err := netip.ParseAddr(g); err != nil || !s.Prefix.Contains(ip) {
				add("gateway-outside", SeverityWarn, "Subnet", s.Subnet, "gateway %s is not inside the subnet", g)
			}
		}
		if len(s.DNSServers()) == 0 {
			add("dns-missing", SeverityInfo, "Subnet", s.Subnet, "offers no DNS server")
		}
	}
	sortFindings(out)
	return out
}

// linkFindings says where the entry of a finding can be opened.
func linkFindings(list []Finding) {
	pages := map[string]string{"Reservation": "reservations", "Class": "classes", "Network": "client"}
	for i := range list {
		f := &list[i]
		if f.Page != "" {
			continue
		}
		if f.Area == "Subnet" {
			f.Page = "subnets"
			continue
		}
		f.Page, f.Query = pages[f.Area], f.Target
	}
}

func sortFindings(list []Finding) {
	rank := map[Severity]int{SeverityCrit: 0, SeverityWarn: 1, SeverityInfo: 2}
	slices.SortStableFunc(list, func(a, b Finding) int { return rank[a.Severity] - rank[b.Severity] })
}

// classTests keeps only the tests of the classes: a class does not count as used
// because of its own entry.
func classTests(dhcp4 map[string]any) []any {
	list, _ := dhcp4["client-classes"].([]any)
	out := make([]any, 0, len(list))
	for _, item := range list {
		if c, ok := item.(map[string]any); ok {
			out = append(out, map[string]any{"test": c["test"]})
		}
	}
	return out
}
