package manager

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/vgropp/kea-roost/internal/kea"
)

const healthARPTimeout = 10 * time.Second

var reviewRules = []ReviewRule{
	{Rule: "setting-global", Title: "Global reservations are switched on in Kea"},
	{Rule: "setting-in-subnet", Title: "Subnet reservations are switched on in Kea"},
	{Rule: "duplicate-address", Title: "No address is reserved twice"},
	{Rule: "duplicate-name", Title: "No hostname is used by two reservations"},
	{Rule: "reservation-wrong-subnet", Title: "Reservations lie inside their subnet"},
	{Rule: "reservation-no-subnet", Title: "Reserved addresses lie in a configured subnet"},
	{Rule: "reservation-in-pool", Title: "Reserved addresses lie outside of the pools"},
	{Rule: "reservation-inherited", Title: "Reservations set no option to the value they inherit"},
	{Rule: "class-missing", Title: "Reservations only use classes that exist"},
	{Rule: "class-unused", Title: "Every class can apply"},
	{Rule: "pool-overlap", Title: "Pools do not overlap"},
	{Rule: "pool-outside", Title: "Pools lie inside their subnet"},
	{Rule: "subnet-unused", Title: "Every subnet has a pool or a reservation"},
	{Rule: "gateway-outside", Title: "Gateways lie inside their subnet"},
	{Rule: "gateway-missing", Title: "Every subnet offers a gateway"},
	{Rule: "dns-missing", Title: "Every subnet offers a DNS server"},
	{Rule: "network-duplicate", Title: "No address is online on two devices", network: true},
	{Rule: "network-foreign-reservation", Title: "Reserved addresses are only online on their device", network: true},
	{Rule: "network-foreign-lease", Title: "Leased addresses are only online on their device", network: true},
	{Rule: "network-moved", Title: "Reserved devices are online with their reserved address", network: true},
	{Rule: "network-lease-moved", Title: "Leased devices are online with their leased address", network: true},
	{Rule: "network-pool-no-lease", Title: "Online addresses inside a pool have a lease", network: true},
	{Rule: "network-lease-silent", Title: "Devices with an active lease are seen on the network", network: true},
	{Rule: "network-reservation-unseen", Title: "Reserved devices are seen on the network", network: true},
}

// reviewSummary counts the findings per rule. skipped tells why the rules that
// compare with the network did not run, so that they do not look fine.
func reviewSummary(findings []Finding, skipped string) []ReviewRule {
	out := slices.Clone(reviewRules)
	for i := range out {
		if out[i].network {
			out[i].Skipped = skipped
		}
		for _, f := range findings {
			switch {
			case f.Rule != out[i].Rule:
			case f.Hidden:
				out[i].Hidden++
			default:
				out[i].Findings++
			}
		}
	}
	return out
}

// networkData is what the integrations report, fetched once per health check.
type networkData struct {
	devices []arpDevice
	arpErr  error
	wifi    map[string]apClient
	wifiErr error
}

// fetchNetwork gives the ARP monitor longer than the dashboard does, because the
// review is worth little without it.
func (m *Manager) fetchNetwork(ctx context.Context) networkData {
	var n networkData
	if m.ARPMonitorEnabled() {
		short, cancel := context.WithTimeout(ctx, healthARPTimeout)
		defer cancel()
		n.devices, n.arpErr = m.arpDevicesWithin(short, m.cfg().ReviewDays)
	}
	if m.apURL != "" {
		n.wifi, n.wifiErr = m.wifiClients(ctx)
	}
	return n
}

// networkFindings compares Kea with the network. Instead of findings it returns why
// that is not possible: comparing with missing data would look like a clean result.
func (m *Manager) networkFindings(ctx context.Context, subnets []kea.Subnet, hosts []kea.Host, n networkData) ([]Finding, string) {
	switch {
	case !m.ARPMonitorEnabled():
		return nil, "needs the ARP monitor"
	case n.arpErr != nil:
		return nil, "ARP monitor unreachable"
	case len(n.devices) == 0:
		return nil, "ARP monitor reports no devices"
	case n.wifiErr != nil:
		return nil, "AP dashboard unreachable"
	}
	leases, err := m.Leases(ctx)
	if err != nil {
		return nil, "leases unavailable: " + err.Error()
	}
	return reviewNetwork(subnets, hosts, leases, mergeWifiClients(n.devices, n.wifi), m.now(), m.cfg()), ""
}

func hostLabel(h kea.Host) string {
	_, id := h.Identifier()
	if name := h.Hostname(); name != "" {
		return name + " (" + id + ")"
	}
	return id
}

// reviewNetwork finds where Kea and the network disagree.
func reviewNetwork(subnets []kea.Subnet, hosts []kea.Host, leases []LeaseView, devices []arpDevice, now time.Time, cfg Settings) []Finding {
	out := []Finding{}
	add := func(rule string, severity Severity, target, format string, args ...any) {
		out = append(out, Finding{Rule: rule, Severity: severity, Area: "Network", Target: target, Message: fmt.Sprintf(format, args...)})
	}
	byIP, byMAC := indexHosts(hosts)
	leaseByIP, leaseByMAC := map[string]LeaseView{}, map[string]LeaseView{}
	for _, l := range leases {
		if l.Active {
			leaseByIP[l.IPAddress] = l
			leaseByMAC[strings.ToLower(l.HWAddress)] = l
		}
	}

	online := map[string][]string{}
	lastSeen := map[string]time.Time{}
	for _, d := range devices {
		mac := strings.ToLower(d.MAC)
		if d.LastSeen.After(lastSeen[mac]) {
			lastSeen[mac] = d.LastSeen
		}
		ip, subnet := deviceAddress(d, subnets)
		if subnet == nil || now.Sub(d.LastSeen) > cfg.online() {
			continue
		}
		address := ip.String()
		if !slices.Contains(online[address], mac) {
			online[address] = append(online[address], mac)
		}

		h, reserved := byMAC[mac]
		if reserved && h.IP() != "" && h.IP() != address {
			// a device can have several addresses, or may just have left the reserved one
			if slices.Contains(d.IPv4, h.IP()) {
				add("network-moved", SeverityInfo, mac, "is online with %s and was also seen with its reserved %s", address, h.IP())
			} else {
				add("network-moved", SeverityWarn, mac, "is online with %s, its reservation says %s", address, h.IP())
			}
		}
		if owner, ok := byIP[address]; ok {
			if idType, id := owner.Identifier(); idType == "hw-address" && !strings.EqualFold(id, mac) {
				add("network-foreign-reservation", SeverityCrit, address, "is reserved for %s, but %s is online with it", hostLabel(owner), mac)
			}
		}
		lease, leased := leaseByMAC[mac]
		if leased && lease.IPAddress != address && !(reserved && h.IP() != "") {
			add("network-lease-moved", SeverityWarn, mac, "is online with %s, but its lease is for %s", address, lease.IPAddress)
		}
		other, leasedIP := leaseByIP[address]
		if leasedIP && other.HWAddress != "" && !strings.EqualFold(other.HWAddress, mac) {
			add("network-foreign-lease", SeverityWarn, address, "is leased to %s, but %s is online with it", other.HWAddress, mac)
		}
		if _, isReserved := byIP[address]; !leasedIP && !isReserved && subnet.PoolContaining(ip) != nil {
			add("network-pool-no-lease", SeverityWarn, address, "is online on %s without a lease, so Kea can give it to another client", mac)
		}
	}
	for _, address := range sortedAddresses(online) {
		if macs := online[address]; len(macs) > 1 {
			slices.Sort(macs)
			add("network-duplicate", SeverityCrit, address, "is online on %d devices: %s", len(macs), strings.Join(macs, ", "))
		}
	}

	for _, l := range leases {
		mac := strings.ToLower(l.HWAddress)
		if !l.Active || mac == "" {
			continue
		}
		if seen, ok := lastSeen[mac]; !ok || seen.IsZero() {
			add("network-lease-silent", SeverityInfo, l.IPAddress, "has an active lease for %s, which the network did not show", mac)
		} else if age := now.Sub(seen); age > cfg.leaseSilent() {
			add("network-lease-silent", SeverityInfo, l.IPAddress, "has an active lease for %s, last seen %s ago", mac, readableSeconds(int64(age.Seconds())))
		}
	}
	for _, h := range hosts {
		idType, id := h.Identifier()
		if idType != "hw-address" {
			continue
		}
		if _, ok := lastSeen[strings.ToLower(id)]; !ok {
			out = append(out, Finding{
				Rule: "network-reservation-unseen", Severity: SeverityInfo, Area: "Network", Target: hostLabel(h),
				Message: fmt.Sprintf("was not seen in the last %d days", cfg.ReviewDays),
				Page:    "reservations", Query: id,
			})
		}
	}
	return out
}

func sortedAddresses(online map[string][]string) []string {
	out := slices.Collect(maps.Keys(online))
	slices.SortFunc(out, func(a, b string) int {
		x, _ := netip.ParseAddr(a)
		y, _ := netip.ParseAddr(b)
		return x.Compare(y)
	})
	return out
}
