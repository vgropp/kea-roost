package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"slices"
	"strings"

	"github.com/vgropp/kea-roost/internal/kea"
)

// Subnet6Input is the editable part of an IPv6 subnet. Prefix pools, the interface
// and everything else stay as they are.
type Subnet6Input struct {
	Pools             []string `json:"pools"`
	DNSServers        []string `json:"dnsServers"`
	ValidLifetime     int64    `json:"validLifetime"`
	PreferredLifetime int64    `json:"preferredLifetime"`
	// PDPools are the prefix pools; nil leaves them as they are.
	PDPools []kea.PDPool `json:"pdPools"`
}

func pdKey(p kea.PDPool) string {
	return fmt.Sprintf("%s/%d each /%d", p.Prefix, p.PrefixLen, p.DelegatedLen)
}

// checkPDPools validates prefix pools and brings their prefixes into canonical form.
func checkPDPools(pools []kea.PDPool) []string {
	var problems []string
	if len(pools) > maxPrefixes {
		return []string{fmt.Sprintf("at most %d prefix pools per subnet", maxPrefixes)}
	}
	var seen []netip.Prefix
	for i, p := range pools {
		addr, err := netip.ParseAddr(strings.TrimSpace(p.Prefix))
		if err != nil || !addr.Is6() || addr.Is4In6() || addr.Zone() != "" || p.PrefixLen < 1 || p.PrefixLen > 128 {
			problems = append(problems, fmt.Sprintf("prefix pool %s/%d is no IPv6 prefix", p.Prefix, p.PrefixLen))
			continue
		}
		if p.PrefixLen < minPrefixLen {
			problems = append(problems, fmt.Sprintf("prefix pool %s/%d is too short, at least /%d", addr, p.PrefixLen, minPrefixLen))
			continue
		}
		prefix := netip.PrefixFrom(addr, p.PrefixLen)
		if prefix.Masked() != prefix {
			problems = append(problems, fmt.Sprintf("prefix pool %s has bits set after its length, %s is meant", prefix, prefix.Masked()))
			continue
		}
		if p.DelegatedLen < p.PrefixLen || p.DelegatedLen > 128 {
			problems = append(problems, fmt.Sprintf("prefix pool %s cannot hand out /%d prefixes", prefix, p.DelegatedLen))
		}
		for _, other := range seen {
			if other.Overlaps(prefix) {
				problems = append(problems, fmt.Sprintf("prefix pools %s and %s overlap", other, prefix))
			}
		}
		seen = append(seen, prefix)
		pools[i].Prefix = addr.String()
	}
	return problems
}

func inPDPools(pools []kea.PDPool, prefix netip.Prefix) bool {
	return slices.ContainsFunc(pools, func(p kea.PDPool) bool {
		addr, err := netip.ParseAddr(p.Prefix)
		return err == nil && prefix.Bits() == p.DelegatedLen && netip.PrefixFrom(addr, p.PrefixLen).Contains(prefix.Addr())
	})
}

// poolKey6 is the "first-last" form of a pool. Kea writes a range that fits a
// prefix as that prefix, so pools are compared by their bounds.
func poolKey6(p kea.Pool6) string { return p.First.String() + "-" + p.Last.String() }

func canonicalPools6(texts []string, prefix netip.Prefix) ([]kea.Pool6, []string) {
	var problems []string
	pools := make([]kea.Pool6, 0, len(texts))
	for _, text := range texts {
		p, err := kea.ParsePool6(strings.TrimSpace(text))
		switch {
		case err != nil || p.First.Is4In6() || p.First.Zone() != "" || p.Last.Zone() != "":
			problems = append(problems, fmt.Sprintf("invalid pool %q", text))
		case !prefix.Contains(p.First) || !prefix.Contains(p.Last):
			problems = append(problems, fmt.Sprintf("pool %s is not inside %s", text, prefix))
		default:
			p.Text = poolKey6(p)
			pools = append(pools, p)
		}
	}
	slices.SortFunc(pools, func(a, b kea.Pool6) int { return a.First.Compare(b.First) })
	for i := 1; i < len(pools); i++ {
		if pools[i].First.Compare(pools[i-1].Last) <= 0 {
			problems = append(problems, fmt.Sprintf("pools %s and %s overlap", pools[i-1].Text, pools[i].Text))
		}
	}
	return pools, problems
}

func inPools6(pools []kea.Pool6, ip netip.Addr) bool {
	return slices.ContainsFunc(pools, func(p kea.Pool6) bool { return p.Contains(ip) })
}

// checkSubnet6 validates a change and returns the subnet, the new pools and what
// the operator should know before applying it.
func (m *Manager) checkSubnet6(ctx context.Context, id int64, in *Subnet6Input) (*kea.Subnet6, []kea.Pool6, []string, error) {
	if m.kea6 == nil {
		return nil, nil, nil, ErrNotFound
	}
	subnets, err := m.subnets6(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	i := slices.IndexFunc(subnets, func(s kea.Subnet6) bool { return s.ID == id })
	if i < 0 {
		return nil, nil, nil, ErrNotFound
	}
	subnet := &subnets[i]

	pools, problems := canonicalPools6(in.Pools, subnet.Prefix)
	if len(in.Pools) > maxPrefixes || len(in.DNSServers) > maxPrefixes {
		problems = append(problems, fmt.Sprintf("at most %d pools and %d DNS servers", maxPrefixes, maxPrefixes))
	}
	problems = append(problems, checkPDPools(in.PDPools)...)
	if subnet.SharedNetwork != "" {
		problems = append(problems, fmt.Sprintf("subnet belongs to the shared network %s, which cannot be edited here", subnet.SharedNetwork))
	}
	for i, d := range in.DNSServers {
		ip, err := netip.ParseAddr(strings.TrimSpace(d))
		if err != nil || !ip.Is6() || ip.Is4In6() || ip.Zone() != "" {
			problems = append(problems, fmt.Sprintf("invalid DNS server %q", d))
			continue
		}
		in.DNSServers[i] = ip.String()
	}
	// an empty preferred lifetime leaves Kea's default, if the subnet has none yet
	unsetPreferred := in.PreferredLifetime == 0 && subnet.PreferredLifetime == nil
	switch {
	case in.ValidLifetime <= 0 || in.ValidLifetime > maxLifetime || !unsetPreferred && (in.PreferredLifetime <= 0 || in.PreferredLifetime > maxLifetime):
		problems = append(problems, fmt.Sprintf("lifetimes must be between 1 and %d seconds", int64(maxLifetime)))
	case in.PreferredLifetime > in.ValidLifetime:
		problems = append(problems, "the preferred lifetime must not be longer than the valid lifetime")
	}
	if len(problems) > 0 {
		return nil, nil, nil, &ValidationError{Problems: problems}
	}

	var warnings []string
	if len(pools) == 0 {
		warnings = append(warnings, "without a pool, only clients with a reservation get an address")
	}
	hosts, err := m.hosts6(ctx, subnets)
	if err != nil {
		return nil, nil, nil, err
	}
	reserved := map[netip.Addr]bool{}
	for _, h := range hosts {
		for _, text := range h.IPs() {
			if ip, err := netip.ParseAddr(text); err == nil {
				reserved[ip] = true
			}
		}
	}
	reservedPrefixes := map[netip.Prefix]bool{}
	for _, h := range hosts {
		for _, text := range h.Prefixes() {
			if p, err := netip.ParsePrefix(text); err == nil {
				reservedPrefixes[p] = true
			}
		}
	}
	var leaving, leavingPrefixes []string
	if leases, err := m.kea6.Leases6(ctx); err != nil {
		warnings = append(warnings, "Lease check not possible: "+err.Error())
	} else {
		now := m.now().Unix()
		for _, l := range leases {
			ip, err := netip.ParseAddr(l.IPAddress)
			state, active := leaseState(l.State, l.CLTT+l.ValidLft-now)
			if err == nil && active && state != "registered" && l.Type != "IA_PD" && l.SubnetID == id && !reserved[ip] && !inPools6(pools, ip) {
				leaving = append(leaving, ip.String())
			}
			prefix := netip.PrefixFrom(ip, l.PrefixLen)
			if err == nil && active && l.Type == "IA_PD" && l.SubnetID == id && in.PDPools != nil && !reservedPrefixes[prefix] && !inPDPools(in.PDPools, prefix) {
				leavingPrefixes = append(leavingPrefixes, prefix.String())
			}
		}
	}
	if len(leaving) > 0 {
		slices.Sort(leaving)
		warnings = append(warnings, fmt.Sprintf("%d active leases without a reservation end up outside the pools and will get new addresses: %s", len(leaving), listAddresses(leaving)))
	}
	if len(leavingPrefixes) > 0 {
		slices.Sort(leavingPrefixes)
		warnings = append(warnings, fmt.Sprintf("%d delegated prefixes without a reservation end up outside the prefix pools, so their routers get new ones: %s", len(leavingPrefixes), listAddresses(leavingPrefixes)))
	}
	var inside []string
	for ip := range reserved {
		if subnet.Prefix.Contains(ip) && inPools6(pools, ip) && !inPools6(subnet.Pools, ip) {
			inside = append(inside, ip.String())
		}
	}
	if len(inside) > 0 {
		slices.Sort(inside)
		warnings = append(warnings, fmt.Sprintf("%d reservations now lie inside a pool: %s", len(inside), listAddresses(inside)))
	}
	return subnet, pools, warnings, nil
}

// CheckSubnet6 returns the warnings a change would produce, without applying it.
func (m *Manager) CheckSubnet6(ctx context.Context, id int64, in Subnet6Input) ([]string, error) {
	_, _, warnings, err := m.checkSubnet6(ctx, id, &in)
	if warnings == nil {
		warnings = []string{}
	}
	return warnings, err
}

// setLifetime sets a lifetime. Kea refuses a value outside of the bounds it reports
// with the subnet, so a bound moves just far enough to include it; a range clients
// can ask within stays a range.
func setLifetime(raw map[string]any, name string, value int64) {
	raw[name] = value
	if bound, ok := number(raw["min-"+name]); ok && bound > value {
		raw["min-"+name] = value
	}
	if bound, ok := number(raw["max-"+name]); ok && bound < value {
		raw["max-"+name] = value
	}
}

// number reads a number of a decoded Kea answer, which can be any of these types.
func number(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case float64:
		return int64(n), n == math.Trunc(n)
	case int64:
		return n, true
	case int:
		return int64(n), true
	}
	return 0, false
}

// applySubnet6 writes the edited fields into a complete subnet entry. As for
// IPv4, values that did not change are left alone, so inherited ones stay
// inherited.
func applySubnet6(raw map[string]any, subnet *kea.Subnet6, pools []kea.Pool6, in *Subnet6Input) {
	previous := map[string]any{}
	if list, ok := raw["pools"].([]any); ok {
		for _, item := range list {
			if o, ok := item.(map[string]any); ok {
				if p, err := kea.ParsePool6(fmt.Sprint(o["pool"])); err == nil {
					previous[poolKey6(p)] = o
				}
			}
		}
	}
	list := make([]any, 0, len(pools))
	for _, p := range pools {
		if existing, ok := previous[p.Text]; ok {
			list = append(list, existing)
		} else {
			list = append(list, map[string]any{"pool": p.Text})
		}
	}
	raw["pools"] = list

	if in.PDPools != nil {
		previous := map[string]any{}
		if list, ok := raw["pd-pools"].([]any); ok {
			for _, item := range list {
				if o, ok := item.(map[string]any); ok {
					prefixLen, _ := number(o["prefix-len"])
					delegatedLen, _ := number(o["delegated-len"])
					if addr, err := netip.ParseAddr(fmt.Sprint(o["prefix"])); err == nil {
						previous[pdKey(kea.PDPool{Prefix: addr.String(), PrefixLen: int(prefixLen), DelegatedLen: int(delegatedLen)})] = o
					}
				}
			}
		}
		pdList := make([]any, 0, len(in.PDPools))
		for _, p := range in.PDPools {
			if existing, ok := previous[pdKey(p)]; ok {
				pdList = append(pdList, existing)
			} else {
				pdList = append(pdList, map[string]any{"prefix": p.Prefix, "prefix-len": p.PrefixLen, "delegated-len": p.DelegatedLen})
			}
		}
		raw["pd-pools"] = pdList
	}

	if !slices.Equal(in.DNSServers, subnet.DNSServers()) {
		setSubnetOption(raw, "dns-servers", in.DNSServers)
	}
	if subnet.ValidLifetime == nil || *subnet.ValidLifetime != in.ValidLifetime {
		setLifetime(raw, "valid-lifetime", in.ValidLifetime)
	}
	if in.PreferredLifetime != 0 && (subnet.PreferredLifetime == nil || *subnet.PreferredLifetime != in.PreferredLifetime) {
		setLifetime(raw, "preferred-lifetime", in.PreferredLifetime)
	}
	prepareSubnetUpdate(raw)
}

func poolKeys6(raw map[string]any) []string {
	out := []string{}
	list, _ := raw["pools"].([]any)
	for _, item := range list {
		if o, ok := item.(map[string]any); ok {
			if p, err := kea.ParsePool6(fmt.Sprint(o["pool"])); err == nil {
				out = append(out, poolKey6(p))
			}
		}
	}
	slices.Sort(out)
	return out
}

func pdKeys(raw map[string]any) []string {
	out := []string{}
	list, _ := raw["pd-pools"].([]any)
	for _, item := range list {
		if o, ok := item.(map[string]any); ok {
			prefixLen, _ := number(o["prefix-len"])
			delegatedLen, _ := number(o["delegated-len"])
			if addr, err := netip.ParseAddr(fmt.Sprint(o["prefix"])); err == nil {
				out = append(out, pdKey(kea.PDPool{Prefix: addr.String(), PrefixLen: int(prefixLen), DelegatedLen: int(delegatedLen)}))
			}
		}
	}
	slices.Sort(out)
	return out
}

// UpdateSubnet6 changes pools, DNS servers and lifetimes of an IPv6 subnet on every
// kea-dhcp6. Kea keeps the reservations of the subnet.
func (m *Manager) UpdateSubnet6(ctx context.Context, user string, id int64, in Subnet6Input) (*Result, error) {
	ctx, cancel := detached(ctx)
	defer cancel()
	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	subnet, pools, warnings, err := m.checkSubnet6(ctx, id, &in)
	if err != nil {
		return nil, err
	}
	want := make([]string, 0, len(pools))
	for _, p := range pools {
		want = append(want, p.Text)
	}
	slices.Sort(want)
	// the reservations of the subnet before the change, per server in turn
	var kept []kea.Host
	get := func(ctx context.Context, k *kea.Client) (map[string]any, error) {
		raw, err := k.Subnet6Get(ctx, id)
		if err == nil && raw == nil {
			err = errors.New("subnet is missing in Kea")
		}
		return raw, err
	}
	return m.runOn(ctx, m.servers6(), change{
		user:         user,
		action:       "subnet6-update",
		target:       fmt.Sprintf("subnet %d, %s", id, subnet.Subnet),
		backupReason: "subnet-update",
		warnings:     warnings,
		mutate: func(ctx context.Context, k *kea.Client) error {
			raw, err := get(ctx, k)
			if err != nil {
				return err
			}
			if kept, err = k.Reservations(ctx, id); err != nil {
				return err
			}
			applySubnet6(raw, subnet, pools, &in)
			return k.Subnet6Update(ctx, raw)
		},
		verify: func(ctx context.Context, k *kea.Client) error {
			raw, err := get(ctx, k)
			if err != nil {
				return err
			}
			if got := poolKeys6(raw); !slices.Equal(got, want) {
				return fmt.Errorf("pools are %v instead of %v", got, want)
			}
			if in.PDPools != nil {
				wantPD := make([]string, 0, len(in.PDPools))
				for _, p := range in.PDPools {
					wantPD = append(wantPD, pdKey(p))
				}
				slices.Sort(wantPD)
				if got := pdKeys(raw); !slices.Equal(got, wantPD) {
					return fmt.Errorf("prefix pools are %v instead of %v", got, wantPD)
				}
			}
			options := []optionCheck{{"dns-servers", in.DNSServers, !slices.Equal(in.DNSServers, subnet.DNSServers())}}
			lifetimes := []lifetimeCheck{
				{"valid-lifetime", in.ValidLifetime, subnet.ValidLifetime == nil || *subnet.ValidLifetime != in.ValidLifetime},
				{"preferred-lifetime", in.PreferredLifetime, in.PreferredLifetime != 0 && (subnet.PreferredLifetime == nil || *subnet.PreferredLifetime != in.PreferredLifetime)},
			}
			return verifySubnet(ctx, k, raw, id, kept, options, lifetimes)
		},
	}), nil
}

// subnets6Editable reports whether kea-dhcp6 offers the subnet commands.
func (m *Manager) subnets6Editable(ctx context.Context) bool {
	commands, err := m.kea6.Commands(ctx)
	return err == nil && slices.Contains(commands, "subnet6-update")
}
