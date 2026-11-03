package manager

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/vgropp/kea-roost/internal/kea"
)

// Client6View is what the client popup shows for an IPv6 address.
type Client6View struct {
	Address     string            `json:"address"`
	Name        string            `json:"name"`
	Lease       *Lease6View       `json:"lease"`
	Reservation *Reservation6View `json:"reservation"`
	// Device is what the ARP monitor saw of the client, by its MAC or this address.
	Device      *ClientDevice `json:"device"`
	Explanation *Explanation  `json:"explanation"`
	History     []AuditEntry  `json:"history"`
	Warnings    []string      `json:"warnings"`
}

// matchesClient tells whether a reservation is the one of a lease: by address,
// DUID or MAC.
func matchesClient(r Reservation6View, l *Lease6View, ip string) bool {
	if slices.Contains(r.Addresses, ip) {
		return true
	}
	if l == nil {
		return false
	}
	return r.IdentifierType == "duid" && strings.EqualFold(r.Identifier, l.DUID) ||
		r.IdentifierType == "hw-address" && l.HWAddress != "" && strings.EqualFold(r.Identifier, l.HWAddress)
}

// Client6 explains an IPv6 address: its lease, its reservation, and why Kea
// hands it out.
func (m *Manager) Client6(ctx context.Context, address string) (*Client6View, error) {
	if m.kea6 == nil {
		return nil, ErrNotFound
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(address))
	if err != nil || !addr.Is6() || addr.Is4In6() || addr.Zone() != "" {
		return nil, &ValidationError{Problems: []string{fmt.Sprintf("invalid IPv6 address %q", address)}}
	}
	ip := addr.String()
	v := &Client6View{Address: ip, History: []AuditEntry{}, Warnings: []string{}}

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
	// one answer of the ARP monitor serves the leases and the network card
	arp, arpErr := m.arpDevices6(ctx)
	if arpErr != nil {
		v.Warnings = append(v.Warnings, "Network data unavailable: "+arpErr.Error())
	}
	leases, err := m.leases6(ctx, arp)
	if err != nil {
		v.Warnings = append(v.Warnings, "Lease data unavailable: "+err.Error())
	} else {
		for i := range leases.Leases {
			if leases.Leases[i].Address == ip {
				v.Lease = &leases.Leases[i]
			}
		}
	}
	// the leases fetched above link the reservations, no second round to Kea
	if reservations, err := m.reservations6(ctx, leases, err); err != nil {
		v.Warnings = append(v.Warnings, "Reservations unavailable: "+err.Error())
	} else {
		for i := range reservations.Reservations {
			if matchesClient(reservations.Reservations[i], v.Lease, ip) {
				v.Reservation = &reservations.Reservations[i]
				break
			}
		}
	}
	if v.Lease == nil && v.Reservation == nil {
		return nil, ErrNotFound
	}
	switch {
	case v.Reservation != nil && v.Reservation.Hostname != "":
		v.Name = trimName(v.Reservation.Hostname)
	case v.Lease != nil && v.Lease.Hostname != "":
		v.Name = trimName(v.Lease.Hostname)
	}

	mac := ""
	if v.Lease != nil {
		mac = strings.ToLower(v.Lease.HWAddress)
	}
	for _, d := range arp {
		if (mac != "" && strings.EqualFold(d.MAC, mac)) || slices.Contains(routableIPv6(d.IPv6), ip) {
			v.Device = clientDevice(d)
			break
		}
	}

	var host kea.Host
	if v.Reservation != nil {
		for _, h := range hosts {
			idType, id := h.Identifier()
			if h.SubnetID() == v.Reservation.SubnetID && idType == v.Reservation.IdentifierType && strings.EqualFold(id, v.Reservation.Identifier) {
				host = h
			}
		}
	}
	v.Explanation = explain6(config, subnets, v, addr, host)

	entries, err := m.store.AuditTail(clientAuditScan)
	if err != nil {
		v.Warnings = append(v.Warnings, "Audit log unavailable: "+err.Error())
	}
	duid := ""
	if v.Lease != nil {
		duid = strings.ToLower(v.Lease.DUID)
	} else if v.Reservation.IdentifierType == "duid" {
		duid = strings.ToLower(v.Reservation.Identifier)
	}
	for _, a := range entries {
		text := strings.ToLower(a.Target)
		if duid != "" && strings.Contains(text, duid) || containsAddress(text, ip) {
			if v.History = append(v.History, a); len(v.History) == clientAuditLimit {
				break
			}
		}
	}
	return v, nil
}

// explain6 retraces from the configuration why the client has its address.
func explain6(config []byte, subnets []kea.Subnet6, v *Client6View, addr netip.Addr, host kea.Host) *Explanation {
	e := &Explanation{Steps: []ExplainStep{}, Options: []EffectiveOption{}, Unknown: []string{}}
	step := func(severity Severity, title, format string, args ...any) {
		e.Steps = append(e.Steps, ExplainStep{Title: title, Detail: fmt.Sprintf(format, args...), Severity: severity})
	}
	var subnet *kea.Subnet6
	for i := range subnets {
		s := &subnets[i]
		if v.Lease != nil && s.ID == v.Lease.SubnetID || v.Lease == nil && s.ID == v.Reservation.SubnetID || subnet == nil && s.Prefix.Contains(addr) {
			subnet = s
		}
	}
	if subnet == nil {
		step(SeverityCrit, "Subnet", "%s lies in no configured subnet", addr)
		return e
	}
	switch {
	case subnet.Relayed:
		step(SeverityOK, "Subnet", "%s, chosen by the relay the request comes through", subnet.Subnet)
	case subnet.Interface != "":
		step(SeverityOK, "Subnet", "%s, chosen by the interface %s the request arrives on", subnet.Subnet, subnet.Interface)
	default:
		step(SeverityWarn, "Subnet", "%s names neither an interface nor a relay, so Kea cannot choose it for clients on a local link", subnet.Subnet)
	}

	registered := v.Lease != nil && v.Lease.State == "registered"
	prefix := v.Lease != nil && v.Lease.Prefix
	reserved := false
	switch r := v.Reservation; {
	case r == nil:
		step(SeverityInfo, "Reservation", "none for this client")
	case r.SubnetID != subnet.ID:
		step(SeverityWarn, "Reservation", "the reservation of this client is in subnet %d, not in this one", r.SubnetID)
	case !parseFlags6(config).inSubnet():
		step(SeverityCrit, "Reservation", "reservations-in-subnet is off, so Kea ignores the reservation by %s", r.IdentifierType)
	default:
		reserved = slices.Contains(r.Addresses, v.Address)
		detail := fmt.Sprintf("by %s %s", r.IdentifierType, r.Identifier)
		if len(r.Addresses) > 0 {
			detail += ": " + strings.Join(r.Addresses, ", ")
		}
		step(SeverityOK, "Reservation", "%s", detail)
		if !reserved && !registered && !prefix && len(r.Addresses) > 0 {
			step(SeverityWarn, "Moves", "Kea reserves %s for this client, so it changes address at its next renewal", strings.Join(r.Addresses, ", "))
		}
	}

	switch {
	case registered:
		step(SeverityInfo, "Address", "the client chose %s itself and reported it to Kea (RFC 9686); Kea did not hand it out", addr)
	case prefix:
		step(SeverityOK, "Prefix", "%s/%d is delegated from a prefix pool of the subnet", addr, v.Lease.PrefixLen)
	case reserved:
		step(SeverityOK, "Address", "%s comes from the reservation", addr)
	case inPools6(subnet.Pools, addr):
		for _, p := range subnet.Pools {
			if p.Contains(addr) {
				step(SeverityOK, "Address", "%s comes from the pool %s", addr, p.Text)
			}
		}
	default:
		step(SeverityWarn, "Address", "%s lies outside of all pools and is not reserved, probably from an earlier configuration; the client gets another one at its next renewal", addr)
	}
	if !registered {
		lifetime := func(p *int64) string {
			if p == nil {
				return "Kea's default"
			}
			return readableSeconds(*p)
		}
		step(SeverityInfo, "Lifetime", "valid for %s, preferred for %s", lifetime(subnet.ValidLifetime), lifetime(subnet.PreferredLifetime))
	}

	layers, err := kea.OptionLayers6(config, subnet.ID, addr)
	if err == nil {
		if host != nil {
			layers = append(layers, kea.OptionLayer{Source: "reservation", Options: hostOptions(host)})
		}
		e.Options = effectiveOptions(layers)
	}
	e.Unknown = append(e.Unknown, "Client classes are not evaluated for IPv6.")
	return e
}
