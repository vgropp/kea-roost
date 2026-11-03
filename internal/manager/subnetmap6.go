package manager

import (
	"maps"
	"math/big"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/vgropp/kea-roost/internal/kea"
)

// An IPv6 pool is far too large for one square per address, so it is drawn as up to
// map6Blocks squares of equal share. A prefix pool hands out few enough prefixes to
// draw each one, up to map6Prefixes.
const (
	map6Blocks   = 256
	map6Prefixes = 1024
)

// Address6 is a used address or prefix on the map.
type Address6 struct {
	Address string `json:"address"`
	// State is reserved or leased; reserved wins.
	State string `json:"state"`
	Name  string `json:"name,omitempty"`
}

// Block6 is one square of an address pool: a range and what is used in it.
type Block6 struct {
	First string     `json:"first"`
	Last  string     `json:"last"`
	State string     `json:"state"`
	Used  []Address6 `json:"used"`
}

// PoolMap6 draws one address pool.
type PoolMap6 struct {
	Pool string `json:"pool"`
	// Size is the number of addresses, as decimal text, and Each how many one
	// block stands for.
	Size   string   `json:"size"`
	Each   string   `json:"each"`
	Used   int      `json:"used"`
	Blocks []Block6 `json:"blocks"`
}

// PDMap6 draws one prefix pool, a square per delegated prefix.
type PDMap6 struct {
	Pool     string     `json:"pool"`
	Count    int        `json:"count"`
	Used     int        `json:"used"`
	Prefixes []Address6 `json:"prefixes"`
	// TooLarge is set when the pool hands out more prefixes than are drawn.
	TooLarge bool `json:"tooLarge,omitempty"`
}

// SubnetMap6 is the picture of an IPv6 subnet.
type SubnetMap6 struct {
	Pools   []PoolMap6 `json:"pools"`
	PDPools []PDMap6   `json:"pdPools"`
	// Outside lists used addresses that lie in no pool, such as reservations.
	Outside []Address6 `json:"outside"`
}

func toInt(ip netip.Addr) *big.Int {
	b := ip.As16()
	return new(big.Int).SetBytes(b[:])
}

func fromInt(n *big.Int) netip.Addr {
	var b [16]byte
	n.FillBytes(b[:])
	return netip.AddrFrom16(b)
}

// mark records a use; a reservation outranks a lease.
func mark(used map[string]Address6, a Address6) {
	if prev, ok := used[a.Address]; ok && prev.State == "reserved" {
		if prev.Name == "" {
			prev.Name = a.Name
			used[a.Address] = prev
		}
		return
	}
	used[a.Address] = a
}

// subnetMap6 lays out the pools of a subnet with its active leases and reservations.
func subnetMap6(s kea.Subnet6, leases []kea.Lease6, hosts []kea.Host, now time.Time) *SubnetMap6 {
	addresses, prefixes := map[string]Address6{}, map[string]Address6{}
	for _, l := range leases {
		if _, active := leaseState(l.State, l.CLTT+l.ValidLft-now.Unix()); !active || l.SubnetID != s.ID {
			continue
		}
		ip, err := netip.ParseAddr(l.IPAddress)
		if err != nil || l.Type == "IA_PD" && (l.PrefixLen < 1 || l.PrefixLen > 128) {
			continue
		}
		name := trimName(l.Hostname)
		if l.Type == "IA_PD" {
			mark(prefixes, Address6{netip.PrefixFrom(ip, l.PrefixLen).String(), "leased", name})
		} else {
			mark(addresses, Address6{ip.String(), "leased", name})
		}
	}
	for _, h := range hosts {
		if h.SubnetID() != s.ID {
			continue
		}
		name := trimName(h.Hostname())
		for _, text := range h.IPs() {
			if ip, err := netip.ParseAddr(text); err == nil {
				mark(addresses, Address6{ip.String(), "reserved", name})
			}
		}
		for _, text := range h.Prefixes() {
			if p, err := netip.ParsePrefix(text); err == nil {
				mark(prefixes, Address6{p.Masked().String(), "reserved", name})
			}
		}
	}

	m := &SubnetMap6{Pools: []PoolMap6{}, PDPools: []PDMap6{}, Outside: []Address6{}}
	inPool := map[string]bool{}
	for _, p := range s.Pools {
		size, _ := new(big.Int).SetString(p.Size, 10)
		count := int64(map6Blocks)
		if size.IsInt64() && size.Int64() < count {
			count = size.Int64()
		}
		// each block holds the same share, rounded up; the last one may be short
		each := new(big.Int).Div(new(big.Int).Add(size, big.NewInt(count-1)), big.NewInt(count))
		first, last := toInt(p.First), toInt(p.Last)
		pm := PoolMap6{Pool: p.Text, Size: p.Size, Each: each.String(), Blocks: []Block6{}}
		for start := new(big.Int).Set(first); start.Cmp(last) <= 0; start.Add(start, each) {
			end := new(big.Int).Add(start, each)
			end.Sub(end, big.NewInt(1))
			if end.Cmp(last) > 0 {
				end.Set(last)
			}
			pm.Blocks = append(pm.Blocks, Block6{First: fromInt(start).String(), Last: fromInt(end).String(), State: "free", Used: []Address6{}})
		}
		for text, a := range addresses {
			ip := netip.MustParseAddr(text)
			if !p.Contains(ip) {
				continue
			}
			inPool[text] = true
			pm.Used++
			i := new(big.Int).Div(new(big.Int).Sub(toInt(ip), first), each).Int64()
			b := &pm.Blocks[i]
			b.Used = append(b.Used, a)
			if b.State != "reserved" {
				b.State = a.State
			}
		}
		for i := range pm.Blocks {
			slices.SortFunc(pm.Blocks[i].Used, func(a, b Address6) int {
				return netip.MustParseAddr(a.Address).Compare(netip.MustParseAddr(b.Address))
			})
		}
		m.Pools = append(m.Pools, pm)
	}
	for text, a := range addresses {
		if !inPool[text] {
			m.Outside = append(m.Outside, a)
		}
	}
	slices.SortFunc(m.Outside, func(a, b Address6) int {
		return netip.MustParseAddr(a.Address).Compare(netip.MustParseAddr(b.Address))
	})

	for _, p := range s.PDPools {
		addr, err := netip.ParseAddr(p.Prefix)
		if err != nil || p.PrefixLen < 1 || p.PrefixLen > 128 || p.DelegatedLen < p.PrefixLen || p.DelegatedLen > 128 {
			continue
		}
		pool := netip.PrefixFrom(addr, p.PrefixLen).Masked()
		pd := PDMap6{Pool: pool.String() + ", each /" + itoa(p.DelegatedLen), Prefixes: []Address6{}}
		bits := p.DelegatedLen - p.PrefixLen
		if bits >= 31 || 1<<bits > map6Prefixes {
			pd.TooLarge = true
		} else {
			pd.Count = 1 << bits
		}
		step := new(big.Int).Lsh(big.NewInt(1), uint(128-p.DelegatedLen))
		start := toInt(pool.Addr())
		for i := 0; i < pd.Count; i++ {
			prefix := netip.PrefixFrom(fromInt(new(big.Int).Add(start, new(big.Int).Mul(step, big.NewInt(int64(i))))), p.DelegatedLen).String()
			cell := Address6{Address: prefix, State: "free"}
			if used, ok := prefixes[prefix]; ok {
				cell = used
				pd.Used++
			}
			pd.Prefixes = append(pd.Prefixes, cell)
		}
		if pd.TooLarge {
			// sorted first, so that a cut keeps the same prefixes on every call
			for _, text := range slices.Sorted(maps.Keys(prefixes)) {
				used := prefixes[text]
				if q, err := netip.ParsePrefix(text); err == nil && pool.Contains(q.Addr()) {
					pd.Used++
					if len(pd.Prefixes) < map6Prefixes {
						pd.Prefixes = append(pd.Prefixes, used)
					}
				}
			}
			slices.SortFunc(pd.Prefixes, func(a, b Address6) int { return strings.Compare(a.Address, b.Address) })
		}
		m.PDPools = append(m.PDPools, pd)
	}
	return m
}

func itoa(n int) string { return big.NewInt(int64(n)).String() }
