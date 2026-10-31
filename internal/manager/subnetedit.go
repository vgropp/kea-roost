package manager

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/vgropp/kea-roost/internal/kea"
)

// Warnings list only a few addresses; the count tells the rest.
const maxListedAddresses = 8

// SubnetInput is the editable part of a subnet.
type SubnetInput struct {
	Pools         []string `json:"pools"`
	Gateways      []string `json:"gateways"`
	DNSServers    []string `json:"dnsServers"`
	ValidLifetime int64    `json:"validLifetime"`
}

// canonicalPools parses, sorts and rewrites the pools to Kea's "first-last" form.
func canonicalPools(texts []string, prefix netip.Prefix) ([]kea.Pool, []string) {
	var problems []string
	pools := make([]kea.Pool, 0, len(texts))
	bounds, err := kea.ParsePool(prefix.String())
	if err != nil {
		return nil, []string{err.Error()}
	}
	for _, text := range texts {
		p, err := kea.ParsePool(strings.TrimSpace(text))
		if err != nil || !p.First.Is4() || !p.Last.Is4() {
			problems = append(problems, fmt.Sprintf("invalid pool %q", text))
			continue
		}
		switch {
		case p.First.Compare(p.Last) > 0:
			problems = append(problems, fmt.Sprintf("pool %s ends before it starts", text))
		case !prefix.Contains(p.First) || !prefix.Contains(p.Last):
			problems = append(problems, fmt.Sprintf("pool %s is not inside %s", text, prefix))
		case prefix.Bits() < 31 && (p.First == bounds.First || p.Last == bounds.Last):
			problems = append(problems, fmt.Sprintf("pool %s contains the network or broadcast address", text))
		default:
			p.Text = p.First.String() + "-" + p.Last.String()
			pools = append(pools, p)
		}
	}
	slices.SortFunc(pools, func(a, b kea.Pool) int { return a.First.Compare(b.First) })
	for i := 1; i < len(pools); i++ {
		if pools[i].First.Compare(pools[i-1].Last) <= 0 {
			problems = append(problems, fmt.Sprintf("pools %s and %s overlap", pools[i-1].Text, pools[i].Text))
		}
	}
	return pools, problems
}

func inPools(pools []kea.Pool, ip netip.Addr) bool {
	return slices.ContainsFunc(pools, func(p kea.Pool) bool { return p.Contains(ip) })
}

func listAddresses(ips []string) string {
	if len(ips) > maxListedAddresses {
		return fmt.Sprintf("%s and %d more", strings.Join(ips[:maxListedAddresses], ", "), len(ips)-maxListedAddresses)
	}
	return strings.Join(ips, ", ")
}

// checkSubnet validates a change against the current state of Kea and returns
// the subnet, the new pools and what the operator should know before applying.
func (m *Manager) checkSubnet(ctx context.Context, id int64, in *SubnetInput) (*kea.Subnet, []kea.Pool, []string, error) {
	subnets, err := m.subnets(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	i := slices.IndexFunc(subnets, func(s kea.Subnet) bool { return s.ID == id })
	if i < 0 {
		return nil, nil, nil, ErrNotFound
	}
	subnet := &subnets[i]
	hosts, err := m.allHosts(ctx, subnets)
	if err != nil {
		return nil, nil, nil, err
	}

	pools, problems := canonicalPools(in.Pools, subnet.Prefix)
	if subnet.SharedNetwork != "" {
		problems = append(problems, fmt.Sprintf("subnet belongs to the shared network %s, which cannot be edited here", subnet.SharedNetwork))
	}
	// reservations cannot be part of subnet4-update, so it could drop them
	if n := len(slices.DeleteFunc(slices.Clone(hosts), func(h kea.Host) bool { return h.SubnetID() != id })); n > 0 {
		problems = append(problems, fmt.Sprintf("subnet has %d reservations of its own, which cannot be kept by this change", n))
	}
	for _, g := range in.Gateways {
		if ip, err := netip.ParseAddr(g); err != nil || !ip.Is4() || !subnet.Prefix.Contains(ip) {
			problems = append(problems, fmt.Sprintf("gateway %q is no address inside %s", g, subnet.Subnet))
		}
	}
	for _, d := range in.DNSServers {
		if ip, err := netip.ParseAddr(d); err != nil || !ip.Is4() {
			problems = append(problems, fmt.Sprintf("invalid DNS server %q", d))
		}
	}
	if in.ValidLifetime <= 0 {
		problems = append(problems, "lease lifetime must be positive")
	}
	if len(problems) > 0 {
		return nil, nil, nil, &ValidationError{Problems: problems}
	}

	var warnings []string
	if len(pools) == 0 {
		warnings = append(warnings, "without a pool, only clients with a reservation get an address")
	}
	byIP, byMAC := indexHosts(hosts)
	leases, err := m.Leases(ctx)
	if err != nil {
		warnings = append(warnings, "Lease check not possible: "+err.Error())
	}
	var leaving []string
	for _, l := range leases {
		ip, err := netip.ParseAddr(l.IPAddress)
		_, reservedIP := byIP[l.IPAddress]
		_, reservedMAC := byMAC[strings.ToLower(l.HWAddress)]
		if err == nil && l.SubnetID == id && l.occupied() && !reservedIP && !reservedMAC && !inPools(pools, ip) {
			leaving = append(leaving, l.IPAddress)
		}
	}
	if len(leaving) > 0 {
		warnings = append(warnings, fmt.Sprintf("%d active leases without a reservation end up outside the pools and will get new addresses: %s", len(leaving), listAddresses(leaving)))
	}
	var inside []string
	for ip := range byIP {
		if addr, err := netip.ParseAddr(ip); err == nil && inPools(pools, addr) && subnet.PoolContaining(addr) == nil {
			inside = append(inside, ip)
		}
	}
	if len(inside) > 0 {
		slices.Sort(inside)
		warnings = append(warnings, fmt.Sprintf("%d reservations now lie inside a pool: %s", len(inside), listAddresses(inside)))
	}
	return subnet, pools, warnings, nil
}

// CheckSubnet returns the warnings a change would produce, without applying it.
func (m *Manager) CheckSubnet(ctx context.Context, id int64, in SubnetInput) ([]string, error) {
	_, _, warnings, err := m.checkSubnet(ctx, id, &in)
	if warnings == nil {
		warnings = []string{}
	}
	return warnings, err
}

func setSubnetOption(raw map[string]any, name string, values []string) {
	list, _ := raw["option-data"].([]any)
	data := strings.Join(values, ", ")
	out := make([]any, 0, len(list)+1)
	found := false
	for _, item := range list {
		o, ok := item.(map[string]any)
		if !ok || o["name"] != name {
			out = append(out, item)
			continue
		}
		found = true
		if data != "" {
			o["data"] = data
			out = append(out, o)
		}
	}
	if !found && data != "" {
		out = append(out, map[string]any{"name": name, "data": data})
	}
	raw["option-data"] = out
}

// applySubnet writes the edited fields into a complete subnet entry. subnet4-update
// overwrites the entry, so everything else has to stay in it. Options and the
// lifetime are only touched when they change: the values shown can be inherited,
// and saving them unchanged would pin them to the subnet.
func applySubnet(raw map[string]any, subnet *kea.Subnet, pools []kea.Pool, in *SubnetInput) {
	previous := map[string]any{}
	if list, ok := raw["pools"].([]any); ok {
		for _, item := range list {
			if o, ok := item.(map[string]any); ok {
				if p, err := kea.ParsePool(fmt.Sprint(o["pool"])); err == nil {
					previous[p.First.String()+"-"+p.Last.String()] = o
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

	if !slices.Equal(in.Gateways, subnet.Gateways()) {
		setSubnetOption(raw, "routers", in.Gateways)
	}
	if !slices.Equal(in.DNSServers, subnet.DNSServers()) {
		setSubnetOption(raw, "domain-name-servers", in.DNSServers)
	}
	if subnet.ValidLifetime == nil || *subnet.ValidLifetime != in.ValidLifetime {
		raw["valid-lifetime"] = in.ValidLifetime
		// Kea reports the bounds as well; a lifetime outside of them would not apply
		for _, bound := range []string{"min-valid-lifetime", "max-valid-lifetime"} {
			if _, ok := raw[bound]; ok {
				raw[bound] = in.ValidLifetime
			}
		}
	}

	delete(raw, "reservations")
	for _, key := range []string{"4o6-interface", "4o6-interface-id", "4o6-subnet"} {
		if raw[key] == "" {
			delete(raw, key)
		}
	}
}

func poolTexts(raw map[string]any) []string {
	out := []string{}
	list, _ := raw["pools"].([]any)
	for _, item := range list {
		if o, ok := item.(map[string]any); ok {
			if p, err := kea.ParsePool(fmt.Sprint(o["pool"])); err == nil {
				out = append(out, p.First.String()+"-"+p.Last.String())
			}
		}
	}
	slices.Sort(out)
	return out
}

// UpdateSubnet changes pools, gateway, DNS servers and lease lifetime of a subnet.
func (m *Manager) UpdateSubnet(ctx context.Context, user string, id int64, in SubnetInput) (*Result, error) {
	ctx, cancel := detached(ctx)
	defer cancel()
	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	subnet, pools, warnings, err := m.checkSubnet(ctx, id, &in)
	if err != nil {
		return nil, err
	}
	return m.run(ctx, change{
		user:         user,
		action:       "subnet4-update",
		target:       fmt.Sprintf("subnet %d, %s", id, subnet.Subnet),
		backupReason: "subnet-update",
		warnings:     warnings,
		mutate: func(ctx context.Context) error {
			raw, err := m.kea.SubnetGet(ctx, id)
			if err != nil {
				return err
			}
			if raw == nil {
				return errors.New("subnet is missing in Kea")
			}
			applySubnet(raw, subnet, pools, &in)
			return m.kea.SubnetUpdate(ctx, raw)
		},
		verify: func(ctx context.Context) error {
			raw, err := m.kea.SubnetGet(ctx, id)
			if err != nil {
				return err
			}
			if raw == nil {
				return errors.New("subnet is missing in Kea")
			}
			want := make([]string, 0, len(pools))
			for _, p := range pools {
				want = append(want, p.Text)
			}
			slices.Sort(want)
			if got := poolTexts(raw); !slices.Equal(got, want) {
				return fmt.Errorf("pools are %v instead of %v", got, want)
			}
			return nil
		},
	}), nil
}

// subnetsEditable reports whether Kea offers the subnet commands.
func (m *Manager) subnetsEditable(ctx context.Context) bool {
	commands, err := m.kea.Commands(ctx)
	return err == nil && slices.Contains(commands, "subnet4-update")
}
