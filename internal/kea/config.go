package kea

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
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
	// Source is the level that sets the option, such as global or subnet, where known.
	Source string `json:"source,omitempty"`
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
	// AdditionalClasses are the classes the subnet itself has Kea evaluate after
	// the reservation lookup (evaluate-additional-classes).
	AdditionalClasses []string `json:"additionalClasses"`
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

// OwnGateways and OwnDNSServers are the values the subnet sets itself, without
// what it inherits.
func (s Subnet) OwnGateways() []string { return ownOption(s.Options, "routers", 3) }

// OwnDNSServers is the domain-name-servers option the subnet sets itself.
func (s Subnet) OwnDNSServers() []string { return ownOption(s.Options, "domain-name-servers", 6) }

func ownOption(options []Option, name string, code int64) []string {
	for _, o := range options {
		if (o.Name == name || o.Code == code) && o.Source == SourceSubnet {
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

// The levels an option of a subnet can come from.
const (
	SourceGlobal        = "global"
	SourceSharedNetwork = "shared network"
	SourceSubnet        = "subnet"
)

var subnetLevels = []string{SourceGlobal, SourceSharedNetwork, SourceSubnet}

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
	Pool       string      `json:"pool"`
	PoolID     *int64      `json:"pool-id"`
	OptionData []rawOption `json:"option-data"`
	rawAdditional
}

type rawSubnet struct {
	ID            int64       `json:"id"`
	Subnet        string      `json:"subnet"`
	Pools         []rawPool   `json:"pools"`
	OptionData    []rawOption `json:"option-data"`
	ValidLifetime *int64      `json:"valid-lifetime"`
	rawAdditional
}

// rawAdditional reads the additional classes of a pool, subnet or shared network.
// Kea 3.0 renamed require-client-classes and still accepts the old name.
type rawAdditional struct {
	Evaluate []string `json:"evaluate-additional-classes"`
	Require  []string `json:"require-client-classes"`
}

func (r rawAdditional) list() []string {
	if r.Evaluate != nil {
		return r.Evaluate
	}
	return r.Require
}

type rawScope struct {
	Name          string      `json:"name"`
	OptionData    []rawOption `json:"option-data"`
	ValidLifetime *int64      `json:"valid-lifetime"`
	Subnets       []rawSubnet `json:"subnet4"`
	rawAdditional
}

// Class is a configured client class.
type Class struct {
	Name    string
	Test    string
	Options []Option
	// Additional classes are only evaluated where a pool, subnet or shared network
	// lists them, after the reservation lookup (only-in-additional-list).
	Additional bool
}

type rawClass struct {
	Name           string      `json:"name"`
	Test           string      `json:"test"`
	OptionData     []rawOption `json:"option-data"`
	OnlyInList     bool        `json:"only-in-additional-list"`
	OnlyIfRequired bool        `json:"only-if-required"`
}

// OptionLayer is what one level of the configuration sets.
type OptionLayer struct {
	Source  string
	Options []Option
}

// OptionLayers returns the levels that set options for a client, from the weakest
// to the strongest: global, classes, shared network, subnet, pool. Of two classes
// the first one wins. A reservation is stronger than all of these.
func OptionLayers(config json.RawMessage, subnetID int64, ip netip.Addr, classes []string) ([]OptionLayer, error) {
	var doc struct {
		Dhcp4 struct {
			rawScope
			SharedNetworks []rawScope `json:"shared-networks"`
			Classes        []rawClass `json:"client-classes"`
		} `json:"Dhcp4"`
	}
	if err := json.Unmarshal(config, &doc); err != nil {
		return nil, fmt.Errorf("cannot read configuration: %w", err)
	}
	options := func(raw []rawOption) []Option { return mergeOptions(raw) }
	layers := []OptionLayer{{"global", options(doc.Dhcp4.OptionData)}}
	for _, name := range slices.Backward(classes) {
		for _, c := range doc.Dhcp4.Classes {
			if c.Name == name {
				layers = append(layers, OptionLayer{"class " + name, options(c.OptionData)})
			}
		}
	}
	add := func(raw rawSubnet) {
		layers = append(layers, OptionLayer{"subnet " + raw.Subnet, options(raw.OptionData)})
		for _, rp := range raw.Pools {
			if p, err := ParsePool(rp.Pool); err == nil && ip.IsValid() && p.Contains(ip) {
				layers = append(layers, OptionLayer{"pool " + rp.Pool, options(rp.OptionData)})
			}
		}
	}
	for _, raw := range doc.Dhcp4.Subnets {
		if raw.ID == subnetID {
			add(raw)
		}
	}
	for _, network := range doc.Dhcp4.SharedNetworks {
		for _, raw := range network.Subnets {
			if raw.ID == subnetID {
				layers = append(layers, OptionLayer{"shared network " + network.Name, options(network.OptionData)})
				add(raw)
			}
		}
	}
	return layers, nil
}

// ParseClasses extracts the client classes from a config-get answer.
func ParseClasses(config json.RawMessage) ([]Class, error) {
	var doc struct {
		Dhcp4 struct {
			Classes []rawClass `json:"client-classes"`
		} `json:"Dhcp4"`
	}
	if err := json.Unmarshal(config, &doc); err != nil {
		return nil, fmt.Errorf("cannot read configuration: %w", err)
	}
	out := make([]Class, 0, len(doc.Dhcp4.Classes))
	for _, c := range doc.Dhcp4.Classes {
		out = append(out, Class{c.Name, c.Test, mergeOptions(c.OptionData), c.OnlyInList || c.OnlyIfRequired})
	}
	return out, nil
}

// AdditionalClasses returns the additional classes Kea evaluates for a client of
// the subnet with the address ip: those of its pool, of the subnet and of its
// shared network, in this order.
func AdditionalClasses(config json.RawMessage, subnetID int64, ip netip.Addr) ([]string, error) {
	var doc struct {
		Dhcp4 struct {
			rawScope
			SharedNetworks []rawScope `json:"shared-networks"`
		} `json:"Dhcp4"`
	}
	if err := json.Unmarshal(config, &doc); err != nil {
		return nil, fmt.Errorf("cannot read configuration: %w", err)
	}
	var out []string
	add := func(raw rawSubnet, network rawScope) {
		for _, rp := range raw.Pools {
			if p, err := ParsePool(rp.Pool); err == nil && ip.IsValid() && p.Contains(ip) {
				out = append(out, rp.list()...)
			}
		}
		out = append(out, raw.list()...)
		out = append(out, network.list()...)
	}
	for _, raw := range doc.Dhcp4.Subnets {
		if raw.ID == subnetID {
			add(raw, rawScope{})
		}
	}
	for _, network := range doc.Dhcp4.SharedNetworks {
		for _, raw := range network.Subnets {
			if raw.ID == subnetID {
				add(raw, network)
			}
		}
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
		ID:                raw.ID,
		Subnet:            raw.Subnet,
		Prefix:            prefix.Masked(),
		SharedNetwork:     network,
		Pools:             []Pool{},
		Options:           mergeSourced("dhcp4", subnetLevels, global.OptionData, shared.OptionData, raw.OptionData),
		ValidLifetime:     firstSet(raw.ValidLifetime, shared.ValidLifetime, global.ValidLifetime),
		AdditionalClasses: append([]string{}, raw.list()...),
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
func mergeOptions(scopes ...[]rawOption) []Option { return mergeOptionsIn("dhcp4", scopes...) }

// mergeOptionsIn resolves inheritance for the options of one space; an option
// without space belongs to the standard one.
func mergeOptionsIn(space string, scopes ...[]rawOption) []Option {
	return mergeSourced(space, nil, scopes...)
}

// standardOptions are the codes of the options Kea Roost edits or suggests, so
// that an option given only by name and the same one given only by code are
// recognized as one. Kea itself reports both in config-get.
var standardOptions = map[string]map[string]int64{
	"dhcp4": {"routers": 3, "domain-name-servers": 6, "domain-name": 15, "interface-mtu": 26,
		"ntp-servers": 42, "tftp-server-name": 66, "boot-file-name": 67, "domain-search": 119, "time-offset": 2},
	"dhcp6": {"dns-servers": 23, "domain-search": 24, "sntp-servers": 31, "information-refresh-time": 32},
}

// StandardCode returns the code of a well-known DHCPv4 option name, or 0.
func StandardCode(name string) int64 { return standardOptions["dhcp4"][name] }

// withStandardName fills in the name or code of a standard option that has only one.
func withStandardName(space string, o Option) Option {
	for name, code := range standardOptions[space] {
		switch {
		case o.Name == "" && o.Code == code:
			o.Name = name
		case o.Code == 0 && o.Name == name:
			o.Code = code
		}
	}
	return o
}

// mergeSourced is mergeOptionsIn that names the level of each option; levels
// matches scopes.
func mergeSourced(space string, levels []string, scopes ...[]rawOption) []Option {
	out := []Option{}
	// an option can be named by its name, its code or both, so either one finds it
	byName, byCode := map[string]int{}, map[int64]int{}
	for level, scope := range scopes {
		source := ""
		if level < len(levels) {
			source = levels[level]
		}
		for _, o := range scope {
			if o.Space != "" && o.Space != space {
				continue
			}
			i, ok := byName[o.Name]
			if !ok || o.Name == "" {
				i, ok = byCode[o.Code]
				ok = ok && o.Code != 0
			}
			entry := withStandardName(space, Option{o.Name, o.Code, o.Data, source})
			if ok {
				// keep what the weaker level knew of the name or code
				if entry.Name == "" {
					entry.Name = out[i].Name
				}
				if entry.Code == 0 {
					entry.Code = out[i].Code
				}
				out[i] = entry
			} else {
				i = len(out)
				out = append(out, entry)
			}
			if entry.Name != "" {
				byName[entry.Name] = i
			}
			if entry.Code != 0 {
				byCode[entry.Code] = i
			}
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
