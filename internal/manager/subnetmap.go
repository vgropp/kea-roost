package manager

import (
	"context"
	"net/netip"
	"slices"
	"strings"

	"github.com/vgropp/kea-roost/internal/kea"
)

// States of an address on the map, strongest first: a gateway stays one even if it
// also has a reservation.
const (
	addressNetwork  = "network"
	addressGateway  = "gateway"
	addressReserved = "reserved"
	addressLeased   = "leased"
	addressDevice   = "device"
	addressFree     = "free"
)

const (
	// A larger subnet gets a block per /24 instead of a square per address.
	maxMapCells = 1024
	// Below this prefix length the walk over all addresses is not worth it.
	minMapPrefix = 16
)

// AddressCell is one address on the map.
type AddressCell struct {
	IP    string `json:"ip"`
	State string `json:"state"`
	Pool  bool   `json:"pool"`
	Name  string `json:"name,omitempty"`
	MAC   string `json:"mac,omitempty"`
}

// AddressBlock sums up a /24 of a subnet too large for single squares.
type AddressBlock struct {
	First string `json:"first"`
	Last  string `json:"last"`
	Used  int    `json:"used"`
	Free  int    `json:"free"`
	Pool  int    `json:"pool"`
}

// FreeRange is a run of addresses nothing claims.
type FreeRange struct {
	First string `json:"first"`
	Last  string `json:"last"`
	Count int    `json:"count"`
	// Pool says that Kea hands these out; the others are free for static addresses.
	Pool bool `json:"pool"`
}

// SubnetMap shows what happens to every address of a subnet.
type SubnetMap struct {
	Subnet   string         `json:"subnet"`
	TooLarge bool           `json:"tooLarge,omitempty"`
	Cells    []AddressCell  `json:"cells"`
	Blocks   []AddressBlock `json:"blocks"`
	Free     []FreeRange    `json:"free"`
	Counts   map[string]int `json:"counts"`
	// FreePool and FreeStatic count the free addresses inside and outside the pools.
	FreePool   int      `json:"freePool"`
	FreeStatic int      `json:"freeStatic"`
	Warnings   []string `json:"warnings"`
	// Incomplete says that a source is missing, so the free ranges can hold used addresses.
	Incomplete bool `json:"incomplete,omitempty"`
}

type addressClaim struct {
	state, name, mac string
}

// claims collects who uses which address. The first claim on an address wins.
func (m *Manager) claims(ctx context.Context, subnet kea.Subnet, hosts []kea.Host, result *SubnetMap) map[netip.Addr]addressClaim {
	out := map[netip.Addr]addressClaim{}
	missing := func(text string) {
		result.Warnings = append(result.Warnings, text)
		result.Incomplete = true
	}
	claim := func(text, state, name, mac string) {
		ip, err := netip.ParseAddr(text)
		if err != nil || !subnet.Prefix.Contains(ip) {
			return
		}
		name, mac = strings.TrimSuffix(name, "."), strings.ToLower(mac)
		found, taken := out[ip]
		if !taken {
			out[ip] = addressClaim{state, name, mac}
			return
		}
		// a reservation without a name can still learn it from its lease
		if found.name == "" {
			found.name = name
		}
		if found.mac == "" {
			found.mac = mac
		}
		out[ip] = found
	}
	for _, g := range subnet.Gateways() {
		claim(g, addressGateway, "gateway", "")
	}
	for _, h := range hosts {
		mac := ""
		if idType, id := h.Identifier(); idType == "hw-address" {
			mac = id
		}
		claim(h.IP(), addressReserved, h.Hostname(), mac)
	}
	leases, err := m.Leases(ctx)
	if err != nil {
		missing("Lease data unavailable: " + err.Error())
	}
	for _, l := range leases {
		if l.occupied() {
			claim(l.IPAddress, addressLeased, l.Hostname, l.HWAddress)
		}
	}
	if !m.DevicesEnabled() {
		missing("Without an ARP monitor, devices with static addresses are not known and can lie in the free ranges.")
		return out
	}
	var devices []arpDevice
	if m.ARPMonitorEnabled() {
		short, cancel := context.WithTimeout(ctx, healthARPTimeout)
		defer cancel()
		if devices, err = m.arpDevices(short); err != nil {
			missing(err.Error())
		}
	}
	wifi, err := m.wifiClients(ctx)
	if err != nil {
		missing(err.Error())
	}
	for _, d := range mergeWifiClients(devices, wifi) {
		online := m.now().Sub(d.LastSeen) <= m.cfg().online()
		// a device without an address yet reports 0.0.0.0, which is no sighting
		addresses := slices.DeleteFunc(slices.Clone(d.IPv4), func(ip string) bool { return ip == "0.0.0.0" })
		for i, text := range addresses {
			// Addresses in a pool change hands all the time, so an old sighting says
			// nothing there. Only the address a device uses right now counts.
			ip, err := netip.ParseAddr(text)
			if err == nil && subnet.PoolContaining(ip) != nil && (i > 0 || !online) {
				continue
			}
			claim(text, addressDevice, d.Hostname, d.MAC)
		}
	}
	return out
}

// SubnetMap walks through a subnet. An address is free when it is neither reserved nor
// leased and no device uses it: in a pool right now, outside of one within the window of
// the devices page. In a subnet too large for single squares, block names a /24 to show
// them for anyway.
func (m *Manager) SubnetMap(ctx context.Context, id int64, block string) (*SubnetMap, error) {
	subnets, err := m.subnets(ctx)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(subnets, func(s kea.Subnet) bool { return s.ID == id })
	if i < 0 {
		return nil, ErrNotFound
	}
	subnet := subnets[i]
	out := &SubnetMap{Subnet: subnet.Subnet, Cells: []AddressCell{}, Blocks: []AddressBlock{}, Free: []FreeRange{}, Counts: map[string]int{}, Warnings: []string{}}
	if subnet.Prefix.Bits() < minMapPrefix {
		out.TooLarge = true
		return out, nil
	}
	var zoom netip.Prefix
	if block != "" {
		first, err := netip.ParseAddr(block)
		if err != nil || !first.Is4() || !subnet.Prefix.Contains(first) {
			return nil, &ValidationError{Problems: []string{"block is not inside the subnet"}}
		}
		zoom = netip.PrefixFrom(first, 24).Masked()
	}
	hosts, err := m.allHosts(ctx, subnets)
	if err != nil {
		return nil, err
	}
	var own []kea.Host
	for _, h := range hosts {
		if s := kea.SubnetFor(subnets, h); s != nil && s.ID == id {
			own = append(own, h)
		}
	}
	claimed := m.claims(ctx, subnet, own, out)
	walkSubnet(subnet, claimed, zoom, out)
	return out, nil
}

func addToBlock(out *SubnetMap, ip netip.Addr, state string, pool bool) {
	octets := ip.As4()
	if octets[3] == 0 || len(out.Blocks) == 0 {
		out.Blocks = append(out.Blocks, AddressBlock{First: ip.String()})
	}
	b := &out.Blocks[len(out.Blocks)-1]
	b.Last = ip.String()
	if pool {
		b.Pool++
	}
	switch state {
	case addressFree:
		b.Free++
	case addressNetwork:
	default:
		b.Used++
	}
}

func walkSubnet(subnet kea.Subnet, claimed map[netip.Addr]addressClaim, zoom netip.Prefix, out *SubnetMap) {
	bounds, err := kea.ParsePool(subnet.Prefix.String())
	if err != nil {
		return
	}
	size := 1 << (32 - subnet.Prefix.Bits())
	reservedEnds := subnet.Prefix.Bits() < 31
	var run *FreeRange
	flush := func() {
		if run != nil {
			out.Free = append(out.Free, *run)
			run = nil
		}
	}
	for ip := bounds.First; ; ip = ip.Next() {
		c, used := claimed[ip]
		pool := subnet.PoolContaining(ip) != nil
		state := addressFree
		switch {
		case reservedEnds && (ip == bounds.First || ip == bounds.Last):
			state = addressNetwork
		case used:
			state = c.state
		}
		out.Counts[state]++
		if size <= maxMapCells || zoom.IsValid() && zoom.Contains(ip) {
			out.Cells = append(out.Cells, AddressCell{ip.String(), state, pool, c.name, c.mac})
		}
		if size > maxMapCells {
			addToBlock(out, ip, state, pool)
		}
		if state == addressFree {
			if pool {
				out.FreePool++
			} else {
				out.FreeStatic++
			}
			if run != nil && run.Pool == pool {
				run.Last = ip.String()
				run.Count++
			} else {
				flush()
				run = &FreeRange{ip.String(), ip.String(), 1, pool}
			}
		} else {
			flush()
		}
		if ip == bounds.Last {
			break
		}
	}
	flush()
}
