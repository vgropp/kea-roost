package kea

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// Pool is a dynamic address range of a subnet.
type Pool struct {
	Text  string     `json:"pool"`
	ID    int64      `json:"id"`
	First netip.Addr `json:"-"`
	Last  netip.Addr `json:"-"`
}

// Contains reports whether ip is part of the pool.
func (p Pool) Contains(ip netip.Addr) bool {
	return p.First.Compare(ip) <= 0 && ip.Compare(p.Last) <= 0
}

// Size returns the number of addresses in the pool.
func (p Pool) Size() int64 {
	first, last := p.First.As4(), p.Last.As4()
	return int64(binary.BigEndian.Uint32(last[:])) - int64(binary.BigEndian.Uint32(first[:])) + 1
}

// Option is one DHCP option of a subnet with inheritance already applied.
type Option struct {
	Name string `json:"name"`
	Code int64  `json:"code"`
	Data string `json:"data"`
}

// Subnet is a configured IPv4 subnet.
type Subnet struct {
	ID            int64        `json:"id"`
	Subnet        string       `json:"subnet"`
	Prefix        netip.Prefix `json:"-"`
	SharedNetwork string       `json:"sharedNetwork,omitempty"`
	Pools         []Pool       `json:"pools"`
	Options       []Option     `json:"options"`
	ValidLifetime *int64       `json:"validLifetime"`
}

// option returns the values of a comma separated option, or nil.
func (s Subnet) option(name string, code int64) []string {
	for _, o := range s.Options {
		if o.Name == name || o.Code == code {
			var out []string
			for _, v := range strings.Split(o.Data, ",") {
				if v = strings.TrimSpace(v); v != "" {
					out = append(out, v)
				}
			}
			return out
		}
	}
	return nil
}

// Gateways returns the routers option.
func (s Subnet) Gateways() []string { return s.option("routers", 3) }

// DNSServers returns the domain-name-servers option.
func (s Subnet) DNSServers() []string { return s.option("domain-name-servers", 6) }

// PoolContaining returns the pool holding ip, or nil.
func (s Subnet) PoolContaining(ip netip.Addr) *Pool {
	for i := range s.Pools {
		if s.Pools[i].Contains(ip) {
			return &s.Pools[i]
		}
	}
	return nil
}

type rawOption struct {
	Name  string `json:"name"`
	Code  int64  `json:"code"`
	Space string `json:"space"`
	Data  string `json:"data"`
}

type rawPool struct {
	Pool   string `json:"pool"`
	PoolID *int64 `json:"pool-id"`
}

type rawSubnet struct {
	ID            int64       `json:"id"`
	Subnet        string      `json:"subnet"`
	Pools         []rawPool   `json:"pools"`
	OptionData    []rawOption `json:"option-data"`
	ValidLifetime *int64      `json:"valid-lifetime"`
}

type rawScope struct {
	Name          string      `json:"name"`
	OptionData    []rawOption `json:"option-data"`
	ValidLifetime *int64      `json:"valid-lifetime"`
	Subnets       []rawSubnet `json:"subnet4"`
}

// Class is a configured client class.
type Class struct {
	Name    string
	Test    string
	Options []Option
}

// ParseClasses extracts the client classes from a config-get answer.
func ParseClasses(config json.RawMessage) ([]Class, error) {
	var doc struct {
		Dhcp4 struct {
			Classes []struct {
				Name       string      `json:"name"`
				Test       string      `json:"test"`
				OptionData []rawOption `json:"option-data"`
			} `json:"client-classes"`
		} `json:"Dhcp4"`
	}
	if err := json.Unmarshal(config, &doc); err != nil {
		return nil, fmt.Errorf("cannot read configuration: %w", err)
	}
	out := make([]Class, 0, len(doc.Dhcp4.Classes))
	for _, c := range doc.Dhcp4.Classes {
		out = append(out, Class{c.Name, c.Test, mergeOptions(c.OptionData)})
	}
	return out, nil
}

// LossyOptions names options config-get reports as binary without a value. Kea
// shows vendor-encapsulated-options like that even when it has one, and
// config-write stores what config-get shows, which erases the value on disk.
func LossyOptions(config json.RawMessage) []string {
	var doc any
	if json.Unmarshal(config, &doc) != nil {
		return nil
	}
	seen := map[string]bool{}
	var walk func(node any)
	walk = func(node any) {
		switch v := node.(type) {
		case map[string]any:
			if list, ok := v["option-data"].([]any); ok {
				for _, item := range list {
					o, ok := item.(map[string]any)
					if !ok {
						continue
					}
					name, _ := o["name"].(string)
					if csv, ok := o["csv-format"].(bool); ok && !csv && o["data"] == "" && name != "" && !hasSubOption(list, name) {
						seen[name] = true
					}
				}
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(doc)
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// hasSubOption reports whether an option is an empty container of defined
// sub-options, which keep their values on their own.
func hasSubOption(options []any, name string) bool {
	return slices.ContainsFunc(options, func(item any) bool {
		o, ok := item.(map[string]any)
		return ok && o["space"] == name+"-space"
	})
}

// SubnetFor returns the subnet a host belongs to. Global reservations (subnet 0)
// apply wherever their address fits, so those are matched by address.
func SubnetFor(subnets []Subnet, h Host) *Subnet {
	id := h.SubnetID()
	ip, err := netip.ParseAddr(h.IP())
	for i := range subnets {
		if id != 0 && subnets[i].ID == id {
			return &subnets[i]
		}
		if id == 0 && err == nil && subnets[i].Prefix.Contains(ip) {
			return &subnets[i]
		}
	}
	return nil
}

// ParseSubnets extracts the subnets from a config-get answer, including those
// nested in shared networks. Options and lifetimes that Kea inherits from the
// shared network or global scope are resolved here.
func ParseSubnets(config json.RawMessage) ([]Subnet, error) {
	var doc struct {
		Dhcp4 *struct {
			rawScope
			SharedNetworks []rawScope `json:"shared-networks"`
		} `json:"Dhcp4"`
	}
	if err := json.Unmarshal(config, &doc); err != nil {
		return nil, fmt.Errorf("cannot read configuration: %w", err)
	}
	if doc.Dhcp4 == nil {
		return nil, fmt.Errorf("configuration has no Dhcp4 section")
	}
	global := doc.Dhcp4.rawScope
	var subnets []Subnet
	for _, raw := range global.Subnets {
		s, err := buildSubnet(raw, "", global, rawScope{})
		if err != nil {
			return nil, err
		}
		subnets = append(subnets, s)
	}
	for _, network := range doc.Dhcp4.SharedNetworks {
		for _, raw := range network.Subnets {
			s, err := buildSubnet(raw, network.Name, global, network)
			if err != nil {
				return nil, err
			}
			subnets = append(subnets, s)
		}
	}
	return subnets, nil
}

func buildSubnet(raw rawSubnet, network string, global, shared rawScope) (Subnet, error) {
	prefix, err := netip.ParsePrefix(raw.Subnet)
	if err != nil {
		return Subnet{}, fmt.Errorf("subnet %d: %w", raw.ID, err)
	}
	s := Subnet{
		ID:            raw.ID,
		Subnet:        raw.Subnet,
		Prefix:        prefix.Masked(),
		SharedNetwork: network,
		Pools:         []Pool{},
		Options:       mergeOptions(global.OptionData, shared.OptionData, raw.OptionData),
		ValidLifetime: firstSet(raw.ValidLifetime, shared.ValidLifetime, global.ValidLifetime),
	}
	for i, rp := range raw.Pools {
		p, err := ParsePool(rp.Pool)
		if err != nil {
			return Subnet{}, fmt.Errorf("subnet %d: %w", raw.ID, err)
		}
		p.ID = int64(i)
		if rp.PoolID != nil {
			p.ID = *rp.PoolID
		}
		s.Pools = append(s.Pools, p)
	}
	return s, nil
}

func firstSet(values ...*int64) *int64 {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

// mergeOptions applies option scopes from outermost to innermost.
func mergeOptions(scopes ...[]rawOption) []Option {
	out := []Option{}
	index := map[string]int{}
	for _, scope := range scopes {
		for _, o := range scope {
			if o.Space != "" && o.Space != "dhcp4" {
				continue
			}
			key := o.Name
			if key == "" {
				key = strconv.FormatInt(o.Code, 10)
			}
			if i, ok := index[key]; ok {
				out[i] = Option{o.Name, o.Code, o.Data}
				continue
			}
			index[key] = len(out)
			out = append(out, Option{o.Name, o.Code, o.Data})
		}
	}
	return out
}

// ParsePool parses both pool notations of Kea: "a - b" and prefix.
func ParsePool(text string) (Pool, error) {
	if first, last, ok := strings.Cut(text, "-"); ok {
		a, err := netip.ParseAddr(strings.TrimSpace(first))
		if err != nil {
			return Pool{}, fmt.Errorf("Pool %q: %w", text, err)
		}
		b, err := netip.ParseAddr(strings.TrimSpace(last))
		if err != nil {
			return Pool{}, fmt.Errorf("Pool %q: %w", text, err)
		}
		return Pool{Text: text, First: a, Last: b}, nil
	}
	prefix, err := netip.ParsePrefix(strings.TrimSpace(text))
	if err != nil {
		return Pool{}, fmt.Errorf("Pool %q: %w", text, err)
	}
	prefix = prefix.Masked()
	first := prefix.Addr().As4()
	last := first
	for i := prefix.Bits(); i < 32; i++ {
		last[i/8] |= 1 << (7 - i%8)
	}
	return Pool{Text: text, First: prefix.Addr(), Last: netip.AddrFrom4(last)}, nil
}
