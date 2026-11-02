package kea

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/netip"
	"strings"
)

// Lease6 is an entry of lease6-get-all. Type is IA_NA for an address and IA_PD for
// a delegated prefix, whose length is PrefixLen.
type Lease6 struct {
	IPAddress    string `json:"ip-address"`
	PrefixLen    int    `json:"prefix-len"`
	Type         string `json:"type"`
	DUID         string `json:"duid"`
	IAID         int64  `json:"iaid"`
	HWAddress    string `json:"hw-address"`
	Hostname     string `json:"hostname"`
	SubnetID     int64  `json:"subnet-id"`
	State        int    `json:"state"`
	CLTT         int64  `json:"cltt"`
	ValidLft     int64  `json:"valid-lft"`
	PreferredLft int64  `json:"preferred-lft"`
}

// Leases6 returns all IPv6 leases.
func (c *Client) Leases6(ctx context.Context) ([]Lease6, error) {
	r, err := c.call(ctx, "lease6-get-all", nil, ResultEmpty)
	if err != nil {
		return nil, err
	}
	var out struct {
		Leases []Lease6 `json:"leases"`
	}
	if err := decodeArguments(r, "lease6-get-all", &out); err != nil {
		return nil, err
	}
	return out.Leases, nil
}

// DeleteLease6 removes the lease of an address, or with prefix of a delegated prefix,
// and reports whether Kea had it.
func (c *Client) DeleteLease6(ctx context.Context, ip string, prefix bool) (bool, error) {
	leaseType := "IA_NA"
	if prefix {
		leaseType = "IA_PD"
	}
	r, err := c.call(ctx, "lease6-del", map[string]any{"ip-address": ip, "type": leaseType}, ResultEmpty)
	if err != nil {
		return false, err
	}
	return r.Result == ResultSuccess, nil
}

// Pool6 is a dynamic address range of an IPv6 subnet. Its size can exceed every
// integer type, so it is kept as decimal text.
type Pool6 struct {
	Text  string     `json:"pool"`
	First netip.Addr `json:"-"`
	Last  netip.Addr `json:"-"`
	Size  string     `json:"size"`
}

// Contains reports whether ip is part of the pool.
func (p Pool6) Contains(ip netip.Addr) bool {
	return p.First.Compare(ip) <= 0 && ip.Compare(p.Last) <= 0
}

// PDPool is a range from which Kea delegates prefixes to routers.
type PDPool struct {
	Prefix       string `json:"prefix"`
	PrefixLen    int    `json:"prefixLen"`
	DelegatedLen int    `json:"delegatedLen"`
}

// Subnet6 is a configured IPv6 subnet.
type Subnet6 struct {
	ID            int64        `json:"id"`
	Subnet        string       `json:"subnet"`
	Prefix        netip.Prefix `json:"-"`
	SharedNetwork string       `json:"sharedNetwork,omitempty"`
	Interface     string       `json:"interface,omitempty"`
	// Relayed is set when the subnet is chosen by a relay address or interface-id,
	// for clients that are not on a local link.
	Relayed       bool     `json:"relayed,omitempty"`
	Pools         []Pool6  `json:"pools"`
	PDPools       []PDPool `json:"pdPools"`
	Options       []Option `json:"options"`
	ValidLifetime *int64   `json:"validLifetime"`
	// PreferredLifetime is how long a client prefers its address for new connections.
	PreferredLifetime *int64 `json:"preferredLifetime"`
}

// DNSServers returns the dns-servers option of the subnet.
func (s Subnet6) DNSServers() []string {
	return Subnet{Options: s.Options}.option("dns-servers", 23)
}

type rawPDPool struct {
	Prefix       string `json:"prefix"`
	PrefixLen    int    `json:"prefix-len"`
	DelegatedLen int    `json:"delegated-len"`
}

type rawSubnet6 struct {
	ID            int64       `json:"id"`
	Subnet        string      `json:"subnet"`
	Interface     string      `json:"interface"`
	Pools         []rawPool   `json:"pools"`
	PDPools       []rawPDPool `json:"pd-pools"`
	OptionData    []rawOption `json:"option-data"`
	ValidLifetime *int64      `json:"valid-lifetime"`
	Preferred     *int64      `json:"preferred-lifetime"`
	rawRelay
}

// rawRelay is how Kea picks a subnet for relayed clients.
type rawRelay struct {
	InterfaceID string `json:"interface-id"`
	Relay       struct {
		IPAddresses []string `json:"ip-addresses"`
	} `json:"relay"`
}

func (r rawRelay) relayed() bool { return r.InterfaceID != "" || len(r.Relay.IPAddresses) > 0 }

type rawScope6 struct {
	Name          string       `json:"name"`
	Interface     string       `json:"interface"`
	OptionData    []rawOption  `json:"option-data"`
	ValidLifetime *int64       `json:"valid-lifetime"`
	Preferred     *int64       `json:"preferred-lifetime"`
	Subnets       []rawSubnet6 `json:"subnet6"`
	rawRelay
}

// ParseSubnets6 extracts the subnets from the config-get answer of kea-dhcp6,
// including those in shared networks, with inherited options resolved.
func ParseSubnets6(config json.RawMessage) ([]Subnet6, error) {
	var doc struct {
		Dhcp6 *struct {
			rawScope6
			SharedNetworks []rawScope6 `json:"shared-networks"`
		} `json:"Dhcp6"`
	}
	if err := json.Unmarshal(config, &doc); err != nil {
		return nil, fmt.Errorf("cannot read configuration: %w", err)
	}
	if doc.Dhcp6 == nil {
		return nil, fmt.Errorf("configuration has no Dhcp6 section")
	}
	global := doc.Dhcp6.rawScope6
	var subnets []Subnet6
	for _, raw := range global.Subnets {
		s, err := buildSubnet6(raw, global, rawScope6{})
		if err != nil {
			return nil, err
		}
		subnets = append(subnets, s)
	}
	for _, network := range doc.Dhcp6.SharedNetworks {
		for _, raw := range network.Subnets {
			s, err := buildSubnet6(raw, global, network)
			if err != nil {
				return nil, err
			}
			subnets = append(subnets, s)
		}
	}
	return subnets, nil
}

func buildSubnet6(raw rawSubnet6, global, shared rawScope6) (Subnet6, error) {
	prefix, err := netip.ParsePrefix(raw.Subnet)
	if err != nil || !prefix.Addr().Is6() {
		return Subnet6{}, fmt.Errorf("subnet %d: %q is no IPv6 network", raw.ID, raw.Subnet)
	}
	s := Subnet6{
		ID:                raw.ID,
		Subnet:            raw.Subnet,
		Prefix:            prefix.Masked(),
		SharedNetwork:     shared.Name,
		Interface:         raw.Interface,
		Pools:             []Pool6{},
		PDPools:           []PDPool{},
		Options:           mergeOptionsIn("dhcp6", global.OptionData, shared.OptionData, raw.OptionData),
		ValidLifetime:     firstSet(raw.ValidLifetime, shared.ValidLifetime, global.ValidLifetime),
		PreferredLifetime: firstSet(raw.Preferred, shared.Preferred, global.Preferred),
	}
	if s.Interface == "" {
		s.Interface = shared.Interface
	}
	s.Relayed = raw.relayed() || shared.relayed()
	for _, rp := range raw.Pools {
		p, err := ParsePool6(rp.Pool)
		if err != nil {
			return Subnet6{}, fmt.Errorf("subnet %d: %w", raw.ID, err)
		}
		s.Pools = append(s.Pools, p)
	}
	for _, pd := range raw.PDPools {
		s.PDPools = append(s.PDPools, PDPool(pd))
	}
	return s, nil
}

// ParsePool6 parses both pool notations of Kea for IPv6: "a - b" and prefix.
func ParsePool6(text string) (Pool6, error) {
	var first, last netip.Addr
	if a, b, ok := strings.Cut(text, "-"); ok {
		var err error
		if first, err = netip.ParseAddr(strings.TrimSpace(a)); err != nil {
			return Pool6{}, fmt.Errorf("pool %q: %w", text, err)
		}
		if last, err = netip.ParseAddr(strings.TrimSpace(b)); err != nil {
			return Pool6{}, fmt.Errorf("pool %q: %w", text, err)
		}
	} else {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(text))
		if err != nil {
			return Pool6{}, fmt.Errorf("pool %q: %w", text, err)
		}
		prefix = prefix.Masked()
		first = prefix.Addr()
		bytes := first.As16()
		for i := prefix.Bits(); i < 128; i++ {
			bytes[i/8] |= 1 << (7 - i%8)
		}
		last = netip.AddrFrom16(bytes)
	}
	if !first.Is6() || !last.Is6() || last.Less(first) {
		return Pool6{}, fmt.Errorf("pool %q is no IPv6 range", text)
	}
	a, b := first.As16(), last.As16()
	size := new(big.Int).Sub(new(big.Int).SetBytes(b[:]), new(big.Int).SetBytes(a[:]))
	size.Add(size, big.NewInt(1))
	return Pool6{Text: text, First: first, Last: last, Size: size.String()}, nil
}

// IPs returns the addresses of an IPv6 reservation.
func (h Host) IPs() []string { return h.strings("ip-addresses") }

// Prefixes returns the delegated prefixes of an IPv6 reservation.
func (h Host) Prefixes() []string { return h.strings("prefixes") }

func (h Host) strings(key string) []string {
	list, _ := h[key].([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// OptionLayers6 returns the levels of kea-dhcp6 that set options for an address,
// from the weakest to the strongest: global, shared network, subnet, pool. Client
// classes are not read for IPv6.
func OptionLayers6(config json.RawMessage, subnetID int64, ip netip.Addr) ([]OptionLayer, error) {
	var doc struct {
		Dhcp6 struct {
			rawScope6
			SharedNetworks []rawScope6 `json:"shared-networks"`
		} `json:"Dhcp6"`
	}
	if err := json.Unmarshal(config, &doc); err != nil {
		return nil, fmt.Errorf("cannot read configuration: %w", err)
	}
	options := func(raw []rawOption) []Option { return mergeOptionsIn("dhcp6", raw) }
	layers := []OptionLayer{{"global", options(doc.Dhcp6.OptionData)}}
	add := func(raw rawSubnet6) {
		layers = append(layers, OptionLayer{"subnet " + raw.Subnet, options(raw.OptionData)})
		for _, rp := range raw.Pools {
			if p, err := ParsePool6(rp.Pool); err == nil && ip.IsValid() && p.Contains(ip) {
				layers = append(layers, OptionLayer{"pool " + rp.Pool, options(rp.OptionData)})
			}
		}
	}
	for _, raw := range doc.Dhcp6.Subnets {
		if raw.ID == subnetID {
			add(raw)
		}
	}
	for _, network := range doc.Dhcp6.SharedNetworks {
		for _, raw := range network.Subnets {
			if raw.ID == subnetID {
				layers = append(layers, OptionLayer{"shared network " + network.Name, options(network.OptionData)})
				add(raw)
			}
		}
	}
	return layers, nil
}
