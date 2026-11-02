package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/vgropp/kea-roost/internal/kea"
)

// reviewRules6 are the rules for kea-dhcp6. They only show with one configured.
var reviewRules6 = []ReviewRule{
	{Rule: "v6-setting-in-subnet", Title: "IPv6: subnet reservations are switched on in Kea", v6: true},
	{Rule: "v6-duplicate-address", Title: "IPv6: no address is reserved twice", v6: true},
	{Rule: "v6-duplicate-name", Title: "IPv6: no hostname is used by two reservations", v6: true},
	{Rule: "v6-reservation-wrong-subnet", Title: "IPv6: reservations lie inside their subnet", v6: true},
	{Rule: "v6-reservation-in-pool", Title: "IPv6: reserved addresses lie outside of the pools", v6: true},
	{Rule: "v6-pool-overlap", Title: "IPv6: pools do not overlap", v6: true},
	{Rule: "v6-pool-outside", Title: "IPv6: pools lie inside their subnet", v6: true},
	{Rule: "v6-subnet-interface", Title: "IPv6: every subnet names its interface or relay", v6: true},
	{Rule: "v6-subnet-unused", Title: "IPv6: every subnet has a pool or a reservation", v6: true},
	{Rule: "v6-dns-missing", Title: "IPv6: every subnet offers a DNS server", v6: true},
}

func parseFlags6(config json.RawMessage) reservationFlags {
	var doc struct {
		Dhcp6 reservationFlags `json:"Dhcp6"`
	}
	if json.Unmarshal(config, &doc) != nil {
		return reservationFlags{}
	}
	return doc.Dhcp6
}

// reviewConfig6 looks for mistakes in the configuration of kea-dhcp6 that Kea
// accepts without complaint.
func reviewConfig6(config json.RawMessage, subnets []kea.Subnet6, hosts []kea.Host) []Finding {
	out := []Finding{}
	add := func(rule string, severity Severity, area, target, format string, args ...any) {
		f := Finding{Rule: rule, Severity: severity, Area: area, Target: target, IPv6: true}
		f.Message = fmt.Sprintf(format, args...)
		f.Page, f.Query = "subnets", ""
		if area == "Reservation" {
			f.Page, f.Query = "reservations", target
		}
		out = append(out, f)
	}
	byID := map[int64]*kea.Subnet6{}
	for i := range subnets {
		byID[subnets[i].ID] = &subnets[i]
	}

	byAddress := map[string][]string{}
	byName := map[string][]string{}
	inSubnet := 0
	served := map[int64]bool{}
	for _, h := range hosts {
		_, id := h.Identifier()
		label := id
		if name := h.Hostname(); name != "" {
			label = name
			key := strings.ToLower(strings.TrimSuffix(name, "."))
			byName[key] = append(byName[key], id)
		}
		s := byID[h.SubnetID()]
		if h.SubnetID() != 0 {
			inSubnet++
			served[h.SubnetID()] = true
		}
		for _, text := range h.IPs() {
			ip, err := netip.ParseAddr(text)
			if err != nil {
				continue
			}
			byAddress[ip.String()] = append(byAddress[ip.String()], label)
			switch {
			case s == nil:
			case !s.Prefix.Contains(ip):
				add("v6-reservation-wrong-subnet", SeverityCrit, "Reservation", label, "%s is outside of its subnet %s", ip, s.Subnet)
			case inPools6(s.Pools, ip):
				for _, p := range s.Pools {
					if p.Contains(ip) {
						add("v6-reservation-in-pool", SeverityInfo, "Reservation", label, "%s lies inside the dynamic pool %s, so another client may hold it first; outside of the pool it stays free", ip, p.Text)
					}
				}
			}
		}
	}
	for _, ip := range sortedKeys(byAddress) {
		if labels := byAddress[ip]; len(labels) > 1 {
			add("v6-duplicate-address", SeverityCrit, "Reservation", ip, "is reserved %d times: %s", len(labels), strings.Join(labels, ", "))
		}
	}
	for _, name := range sortedKeys(byName) {
		if ids := byName[name]; len(ids) > 1 {
			add("v6-duplicate-name", SeverityWarn, "Reservation", name, "is used by %d reservations: %s", len(ids), strings.Join(ids, ", "))
		}
	}
	if inSubnet > 0 && !parseFlags6(config).inSubnet() {
		add("v6-setting-in-subnet", SeverityCrit, "Setting", "reservations-in-subnet", "is off, so Kea ignores %d subnet reservations", inSubnet)
	}

	for _, s := range subnets {
		if s.Interface == "" && !s.Relayed {
			add("v6-subnet-interface", SeverityWarn, "Subnet", s.Subnet, "names neither an interface nor a relay, so Kea cannot tell that clients on a local link belong to it")
		}
		if len(s.Pools) == 0 && len(s.PDPools) == 0 && !served[s.ID] {
			add("v6-subnet-unused", SeverityWarn, "Subnet", s.Subnet, "has neither a pool nor a reservation, so it serves nobody")
		}
		pools := slices.Clone(s.Pools)
		slices.SortFunc(pools, func(a, b kea.Pool6) int { return a.First.Compare(b.First) })
		for i, p := range pools {
			if !s.Prefix.Contains(p.First) || !s.Prefix.Contains(p.Last) {
				add("v6-pool-outside", SeverityCrit, "Subnet", s.Subnet, "pool %s reaches outside of the subnet", p.Text)
			}
			if i > 0 && p.First.Compare(pools[i-1].Last) <= 0 {
				add("v6-pool-overlap", SeverityCrit, "Subnet", s.Subnet, "pools %s and %s overlap", pools[i-1].Text, p.Text)
			}
		}
		if len(s.DNSServers()) == 0 {
			add("v6-dns-missing", SeverityInfo, "Subnet", s.Subnet, "offers no DNS server")
		}
	}
	sortFindings(out)
	return out
}

func (m *Manager) review6(ctx context.Context) ([]Finding, error) {
	config, err := m.kea6.ConfigGet(ctx)
	if err != nil {
		return nil, err
	}
	subnets, err := kea.ParseSubnets6(config)
	if err != nil {
		return nil, err
	}
	hosts, err := m.hosts6(ctx, subnets)
	if err != nil {
		return nil, err
	}
	return reviewConfig6(config, subnets, hosts), nil
}
