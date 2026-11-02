package manager

import (
	"context"
	"fmt"
	"math/big"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vgropp/kea-roost/internal/kea"
	"github.com/vgropp/kea-roost/internal/oui"
)

// IPv6 is read from a kea-dhcp6 of its own. It is an addition: addresses, MACs and
// reservations of IPv4 stay the center, and IPv6 data is linked to them by MAC
// where Kea knows it.

// SetDHCP6 connects the manager to kea-dhcp6.
func (m *Manager) SetDHCP6(client *kea.Client) { m.kea6 = client }

// HasDHCP6 reports whether a kea-dhcp6 is configured.
func (m *Manager) HasDHCP6() bool { return m.kea6 != nil }

// AddPeer6 adds a kea-dhcp6 that gets every IPv6 change after the primary.
func (m *Manager) AddPeer6(name string, client *kea.Client) {
	m.peers6 = append(m.peers6, server{name, client})
}

// servers6 are the kea-dhcp6 servers, primary first. The primary carries the name
// of the IPv4 primary, as both usually run on the same host.
func (m *Manager) servers6() []server {
	return append([]server{{m.serverName, m.kea6}}, m.peers6...)
}

// isV6 reports whether s is one of the kea-dhcp6 servers.
func (m *Manager) isV6(s server) bool {
	return s.client == m.kea6 || slices.ContainsFunc(m.peers6, func(p server) bool { return p.client == s.client })
}

// activeLeases6 counts the active addresses; delegated prefixes are not counted.
func activeLeases6(leases []kea.Lease6, now int64) int {
	n := 0
	for _, l := range leases {
		if _, active := leaseState(l.State, l.CLTT+l.ValidLft-now); active && l.Type != "IA_PD" {
			n++
		}
	}
	return n
}

// Lease6View is an IPv6 lease prepared for display.
type Lease6View struct {
	Address string `json:"address"`
	// PrefixLen is 128 for an address and the delegated length for a prefix.
	PrefixLen int    `json:"prefixLen"`
	Prefix    bool   `json:"prefix"`
	DUID      string `json:"duid"`
	IAID      int64  `json:"iaid"`
	HWAddress string `json:"hwAddress,omitempty"`
	// MACSource says where a MAC that Kea did not report comes from: "duid" when it
	// is part of the DUID, "dhcpv4" when an IPv4 lease has the same DUID in its
	// client-id.
	MACSource string    `json:"macSource,omitempty"`
	Vendor    string    `json:"vendor,omitempty"`
	Hostname  string    `json:"hostname"`
	SubnetID  int64     `json:"subnetId"`
	Subnet    string    `json:"subnet,omitempty"`
	State     string    `json:"state"`
	Active    bool      `json:"active"`
	Started   time.Time `json:"started"`
	Expires   time.Time `json:"expires"`
	Remaining int64     `json:"remaining"`
	// Reserved is the hostname of the IPv6 reservation of the client, if it has one.
	Reserved *string `json:"reserved,omitempty"`
	// Device is the IPv4 reservation of the same MAC.
	Device *ReservationRef `json:"device,omitempty"`
}

// Lease6List is the answer of Leases6.
type Lease6List struct {
	Leases   []Lease6View `json:"leases"`
	Warnings []Warning    `json:"warnings"`
}

// Reservation6View is an IPv6 reservation prepared for display.
type Reservation6View struct {
	SubnetID       int64    `json:"subnetId"`
	Subnet         string   `json:"subnet,omitempty"`
	IdentifierType string   `json:"identifierType"`
	Identifier     string   `json:"identifier"`
	Vendor         string   `json:"vendor,omitempty"`
	Addresses      []string `json:"addresses"`
	Prefixes       []string `json:"prefixes"`
	Hostname       string   `json:"hostname"`
	Comment        string   `json:"comment,omitempty"`
	// HWAddress is the MAC of the client as far as it is known.
	HWAddress string `json:"hwAddress,omitempty"`
	// Device is the IPv4 reservation of the same MAC.
	Device *ReservationRef `json:"device,omitempty"`
	// Leased lists the addresses the client holds right now.
	Leased []string `json:"leased"`
}

// Reservation6List is the answer of Reservations6.
type Reservation6List struct {
	Reservations []Reservation6View `json:"reservations"`
	Warnings     []Warning          `json:"warnings"`
}

// IPv6Ref is what IPv6 knows of the device behind an IPv4 reservation.
type IPv6Ref struct {
	Reserved []string `json:"reserved"`
	Leased   []string `json:"leased"`
}

// ipv6ByMAC collects the IPv6 reservations and active leases by MAC, for the
// IPv4 side. Without kea-dhcp6 it returns nil.
func (m *Manager) ipv6ByMAC(ctx context.Context) (map[string]*IPv6Ref, error) {
	if m.kea6 == nil {
		return nil, nil
	}
	out := map[string]*IPv6Ref{}
	ref := func(mac string) *IPv6Ref {
		mac = strings.ToLower(mac)
		if out[mac] == nil {
			out[mac] = &IPv6Ref{Reserved: []string{}, Leased: []string{}}
		}
		return out[mac]
	}
	leases, err := m.Leases6(ctx)
	if err != nil {
		return nil, err
	}
	reservations, err := m.reservations6(ctx, leases, nil)
	if err != nil {
		return nil, err
	}
	for _, r := range reservations.Reservations {
		if r.HWAddress != "" {
			ref(r.HWAddress).Reserved = append(ref(r.HWAddress).Reserved, r.Addresses...)
		}
	}
	for _, l := range leases.Leases {
		if l.Active && !l.Prefix && l.HWAddress != "" {
			ref(l.HWAddress).Leased = append(ref(l.HWAddress).Leased, l.Address)
		}
	}
	return out, nil
}

// Subnet6View is an IPv6 subnet with its use.
type Subnet6View struct {
	kea.Subnet6
	DNSServers   []string `json:"dnsServers"`
	Leases       int      `json:"leases"`
	Delegated    int      `json:"delegated"`
	Reservations int      `json:"reservations"`
	// Map draws the pools with what is used in them.
	Map *SubnetMap6 `json:"map"`
}

// Subnet6List is the answer of Subnets6.
type Subnet6List struct {
	Subnets  []Subnet6View `json:"subnets"`
	Warnings []Warning     `json:"warnings"`
	// Editable is set when kea-dhcp6 has the subnet commands.
	Editable bool `json:"editable"`
}

// DHCP6Status is the state of kea-dhcp6 for the dashboard.
type DHCP6Status struct {
	URL          string `json:"url"`
	Reachable    bool   `json:"reachable"`
	Error        string `json:"error,omitempty"`
	Version      string `json:"version,omitempty"`
	Uptime       int64  `json:"uptime"`
	ActiveLeases *int   `json:"activeLeases"`
	HAState      string `json:"haState,omitempty"`
	warnings     []Warning
}

func (m *Manager) subnets6(ctx context.Context) ([]kea.Subnet6, error) {
	config, err := m.kea6.ConfigGet(ctx)
	if err != nil {
		return nil, err
	}
	return kea.ParseSubnets6(config)
}

func (m *Manager) hosts6(ctx context.Context, subnets []kea.Subnet6) ([]kea.Host, error) {
	return hostsIn(ctx, m.kea6, subnetIDs6(subnets))
}

func subnetIDs6(subnets []kea.Subnet6) []int64 {
	ids := make([]int64, 0, len(subnets))
	for _, s := range subnets {
		ids = append(ids, s.ID)
	}
	return ids
}

// devicesByMAC returns the IPv4 reservations by MAC, to name IPv6 clients. Without
// them IPv6 is still shown, only unnamed.
func (m *Manager) devicesByMAC(ctx context.Context) (map[string]kea.Host, *Warning) {
	subnets, err := m.subnets(ctx)
	if err == nil {
		var hosts []kea.Host
		if hosts, err = m.allHosts(ctx, subnets); err == nil {
			_, byMAC := indexHosts(hosts)
			return byMAC, nil
		}
	}
	return nil, &Warning{SeverityWarn, "IPv4 reservations: " + err.Error()}
}

// macFromDUID reads the MAC out of a DUID of type LLT (1) or LL (3) with hardware
// type Ethernet. Other DUIDs carry no MAC.
func macFromDUID(duid string) string {
	var b []byte
	for _, part := range strings.Split(strings.ReplaceAll(duid, "-", ":"), ":") {
		n, err := strconv.ParseUint(part, 16, 8)
		if err != nil || len(part) != 2 {
			return ""
		}
		b = append(b, byte(n))
	}
	var mac []byte
	switch {
	case len(b) == 14 && b[0] == 0 && b[1] == 1 && b[2] == 0 && b[3] == 1:
		mac = b[8:]
	case len(b) == 10 && b[0] == 0 && b[1] == 3 && b[2] == 0 && b[3] == 1:
		mac = b[4:]
	default:
		return ""
	}
	parts := make([]string, len(mac))
	for i, x := range mac {
		parts[i] = fmt.Sprintf("%02x", x)
	}
	return strings.Join(parts, ":")
}

// duidFromClientID returns the DUID inside an IPv4 client-id of RFC 4361: type
// 255, then four bytes IAID, then the DUID. Linux clients often send one.
func duidFromClientID(clientID string) string {
	parts := strings.Split(strings.ToLower(clientID), ":")
	if len(parts) < 7 || parts[0] != "ff" {
		return ""
	}
	return strings.Join(parts[5:], ":")
}

func subnet6Text(subnets []kea.Subnet6, id int64) string {
	for _, s := range subnets {
		if s.ID == id {
			return s.Subnet
		}
	}
	return ""
}

// Leases6 returns all IPv6 leases, named by the IPv6 reservation of the client or
// the IPv4 reservation of its MAC.
func (m *Manager) Leases6(ctx context.Context) (*Lease6List, error) {
	leases, err := m.kea6.Leases6(ctx)
	if err != nil {
		return nil, err
	}
	list := &Lease6List{Leases: make([]Lease6View, 0, len(leases)), Warnings: []Warning{}}
	subnets, err := m.subnets6(ctx)
	if err != nil {
		list.Warnings = append(list.Warnings, Warning{SeverityWarn, "IPv6 subnets: " + err.Error()})
	}
	byDUID := map[string]kea.Host{}
	if hosts, err := m.hosts6(ctx, subnets); err != nil {
		list.Warnings = append(list.Warnings, Warning{SeverityWarn, "IPv6 reservations: " + err.Error()})
	} else {
		for _, h := range hosts {
			idType, id := h.Identifier()
			byDUID[idType+" "+strings.ToLower(id)] = h
		}
	}
	devices, warning := m.devicesByMAC(ctx)
	if warning != nil {
		list.Warnings = append(list.Warnings, *warning)
	}
	macByDUID := map[string]string{}
	if leases4, err := m.source(ctx).client.Leases(ctx); err == nil {
		for _, l := range leases4 {
			if duid := duidFromClientID(l.ClientID); duid != "" && l.HWAddress != "" {
				macByDUID[duid] = strings.ToLower(l.HWAddress)
			}
		}
	}
	now := m.now()
	for _, l := range leases {
		expires := time.Unix(l.CLTT+l.ValidLft, 0)
		remaining := max(int64(expires.Sub(now).Seconds()), 0)
		v := Lease6View{
			Address:   l.IPAddress,
			PrefixLen: 128,
			Prefix:    l.Type == "IA_PD",
			DUID:      l.DUID,
			IAID:      l.IAID,
			HWAddress: l.HWAddress,
			Vendor:    oui.Lookup(l.HWAddress),
			Hostname:  l.Hostname,
			SubnetID:  l.SubnetID,
			Subnet:    subnet6Text(subnets, l.SubnetID),
			Started:   time.Unix(l.CLTT, 0),
			Expires:   expires,
			Remaining: remaining,
		}
		if v.Prefix {
			v.PrefixLen = l.PrefixLen
		}
		if v.HWAddress == "" {
			if mac := macFromDUID(l.DUID); mac != "" {
				v.HWAddress, v.MACSource = mac, "duid"
			} else if mac := macByDUID[strings.ToLower(l.DUID)]; mac != "" {
				v.HWAddress, v.MACSource = mac, "dhcpv4"
			}
			v.Vendor = oui.Lookup(v.HWAddress)
		}
		v.State, v.Active = leaseState(l.State, remaining)
		h, ok := byDUID["duid "+strings.ToLower(l.DUID)]
		if !ok && l.HWAddress != "" {
			h, ok = byDUID["hw-address "+strings.ToLower(l.HWAddress)]
		}
		if ok {
			name := h.Hostname()
			v.Reserved = &name
		}
		if d, ok := devices[strings.ToLower(v.HWAddress)]; ok && v.HWAddress != "" {
			v.Device = reservationRef(d, "", "")
		}
		list.Leases = append(list.Leases, v)
	}
	return list, nil
}

// Reservations6 lists all IPv6 reservations with the addresses their clients hold.
func (m *Manager) Reservations6(ctx context.Context) (*Reservation6List, error) {
	leases, err := m.Leases6(ctx)
	return m.reservations6(ctx, leases, err)
}

// reservations6 builds the list from leases fetched already, so that a caller that
// needs both reads them once.
func (m *Manager) reservations6(ctx context.Context, leases *Lease6List, leasesErr error) (*Reservation6List, error) {
	subnets, err := m.subnets6(ctx)
	if err != nil {
		return nil, err
	}
	hosts, err := m.hosts6(ctx, subnets)
	if err != nil {
		return nil, err
	}
	list := &Reservation6List{Reservations: make([]Reservation6View, 0, len(hosts)), Warnings: []Warning{}}
	// the lease views know the MAC of more clients than Kea does
	leased := map[string][]string{}
	macByDUID := map[string]string{}
	if leasesErr != nil {
		list.Warnings = append(list.Warnings, Warning{SeverityWarn, "IPv6 leases: " + leasesErr.Error()})
	} else {
		for _, l := range leases.Leases {
			if l.HWAddress != "" {
				macByDUID[strings.ToLower(l.DUID)] = strings.ToLower(l.HWAddress)
			}
			if !l.Active {
				continue
			}
			for _, key := range []string{"duid " + l.DUID, "hw-address " + l.HWAddress} {
				leased[strings.ToLower(key)] = append(leased[strings.ToLower(key)], l.Address)
			}
		}
	}
	devices, warning := m.devicesByMAC(ctx)
	if warning != nil {
		list.Warnings = append(list.Warnings, *warning)
	}
	for _, h := range hosts {
		idType, id := h.Identifier()
		v := Reservation6View{
			SubnetID:       h.SubnetID(),
			Subnet:         subnet6Text(subnets, h.SubnetID()),
			IdentifierType: idType,
			Identifier:     id,
			Addresses:      h.IPs(),
			Prefixes:       h.Prefixes(),
			Hostname:       h.Hostname(),
			Comment:        h.Comment(),
			Leased:         leased[strings.ToLower(idType+" "+id)],
		}
		if v.Leased == nil {
			v.Leased = []string{}
		}
		mac := id
		if idType == "duid" {
			if mac = macFromDUID(id); mac == "" {
				mac = macByDUID[strings.ToLower(id)]
			}
		} else if idType != "hw-address" {
			mac = ""
		}
		if mac != "" {
			v.HWAddress = strings.ToLower(mac)
			v.Vendor = oui.Lookup(mac)
			if d, ok := devices[strings.ToLower(mac)]; ok {
				v.Device = reservationRef(d, "", "")
			}
		}
		list.Reservations = append(list.Reservations, v)
	}
	slices.SortFunc(list.Reservations, func(a, b Reservation6View) int {
		return compareFirstAddress(a.Addresses, b.Addresses)
	})
	return list, nil
}

func compareFirstAddress(a, b []string) int {
	first := func(list []string) netip.Addr {
		if len(list) == 0 {
			return netip.IPv6Unspecified()
		}
		ip, _ := netip.ParseAddr(list[0])
		return ip
	}
	return first(a).Compare(first(b))
}

// Subnets6 lists the IPv6 subnets with their leases and reservations.
func (m *Manager) Subnets6(ctx context.Context) (*Subnet6List, error) {
	subnets, err := m.subnets6(ctx)
	if err != nil {
		return nil, err
	}
	list := &Subnet6List{Subnets: make([]Subnet6View, 0, len(subnets)), Warnings: []Warning{}, Editable: m.subnets6Editable(ctx)}
	leases, err := m.kea6.Leases6(ctx)
	if err != nil {
		list.Warnings = append(list.Warnings, Warning{SeverityWarn, "IPv6 leases: " + err.Error()})
	}
	hosts, err := m.hosts6(ctx, subnets)
	if err != nil {
		list.Warnings = append(list.Warnings, Warning{SeverityWarn, "IPv6 reservations: " + err.Error()})
	}
	now := m.now()
	for _, s := range subnets {
		v := Subnet6View{Subnet6: s, DNSServers: s.DNSServers()}
		if v.DNSServers == nil {
			v.DNSServers = []string{}
		}
		for _, l := range leases {
			if l.SubnetID != s.ID {
				continue
			}
			if _, active := leaseState(l.State, l.CLTT+l.ValidLft-now.Unix()); !active {
				continue
			}
			if l.Type == "IA_PD" {
				v.Delegated++
			} else {
				v.Leases++
			}
		}
		for _, h := range hosts {
			if h.SubnetID() == s.ID {
				v.Reservations++
			}
		}
		v.Map = subnetMap6(s, leases, hosts, now)
		list.Subnets = append(list.Subnets, v)
	}
	return list, nil
}

// DHCP6 returns the state of kea-dhcp6, or nil without one.
func (m *Manager) DHCP6(ctx context.Context) *DHCP6Status {
	if m.kea6 == nil {
		return nil
	}
	d := &DHCP6Status{URL: m.kea6.URL()}
	status, err := m.kea6.Status(ctx)
	if err != nil {
		d.Error = err.Error()
		return d
	}
	d.Reachable, d.Uptime = true, status.Uptime
	if v, err := m.kea6.Version(ctx); err == nil {
		d.Version = v
	}
	if leases, err := m.kea6.Leases6(ctx); err == nil {
		n := activeLeases6(leases, m.now().Unix())
		d.ActiveLeases = &n
	}
	views, warnings := m.haViews(ctx, m.kea6, status)
	if len(views) > 0 {
		d.HAState = views[0].Servers.Local.State
	}
	for _, w := range warnings {
		d.warnings = append(d.warnings, Warning{w.Severity, "DHCPv6 " + w.Message})
	}
	return d
}

// Subnet6Usage is the use of the pools of an IPv6 subnet. Pools can be larger than
// any integer, so the total is decimal text.
type Subnet6Usage struct {
	ID       int64    `json:"id"`
	Subnet   string   `json:"subnet"`
	Assigned int      `json:"assigned"`
	Total    string   `json:"total"`
	Percent  *float64 `json:"percent"`
}

// usage6 relates the active addresses of a subnet to the size of its pools.
func usage6(s Subnet6View) Subnet6Usage {
	total := new(big.Int)
	for _, p := range s.Pools {
		if n, ok := new(big.Int).SetString(p.Size, 10); ok {
			total.Add(total, n)
		}
	}
	u := Subnet6Usage{ID: s.ID, Subnet: s.Subnet, Assigned: s.Leases, Total: total.String()}
	if total.Sign() > 0 {
		percent, _ := new(big.Float).Quo(new(big.Float).SetInt64(int64(s.Leases)*100), new(big.Float).SetInt(total)).Float64()
		u.Percent = &percent
	}
	return u
}

// fillDHCP6 adds the latest IPv6 leases and the use of the IPv6 pools.
func (m *Manager) fillDHCP6(ctx context.Context, d *Dashboard) {
	if list, err := m.Leases6(ctx); err != nil {
		d.warn(err, "IPv6 leases")
	} else {
		recent := []Lease6View{}
		for _, l := range list.Leases {
			if l.Active {
				recent = append(recent, l)
			}
		}
		slices.SortFunc(recent, func(a, b Lease6View) int { return b.Started.Compare(a.Started) })
		d.RecentLeases6 = recent[:min(len(recent), recentLeaseCount)]
	}
	subnets, err := m.Subnets6(ctx)
	if err != nil {
		d.warn(err, "IPv6 subnets")
		return
	}
	for _, s := range subnets.Subnets {
		u := usage6(s)
		d.Subnets6 = append(d.Subnets6, u)
		if u.Percent != nil && *u.Percent >= float64(m.cfg().PoolWarnPercent) {
			severity := SeverityWarn
			if *u.Percent >= float64(m.cfg().PoolCritPercent) {
				severity = SeverityCrit
			}
			d.Warnings = append(d.Warnings, Warning{severity, fmt.Sprintf("Subnet %s is %.0f %% utilized", s.Subnet, *u.Percent)})
		}
	}
}
