package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"

	"github.com/vgropp/kea-roost/internal/kea"
)

// Reservation6Input is the editable part of an IPv6 reservation.
type Reservation6Input struct {
	SubnetID       int64    `json:"subnetId"`
	IdentifierType string   `json:"identifierType"`
	Identifier     string   `json:"identifier"`
	Addresses      []string `json:"addresses"`
	// Prefixes are delegated prefixes such as fd00:db8:2:100::/56, for a router.
	Prefixes []string `json:"prefixes"`
	Hostname string   `json:"hostname"`
	Comment  string   `json:"comment"`
}

// A delegated prefix shorter than this covers far more than one site, and a
// reservation or a subnet holds at most maxPrefixes of them.
const (
	minPrefixLen = 16
	maxPrefixes  = 64
)

// identifierTypes6 are the identifiers offered for IPv6. Most clients send no MAC,
// so the DUID is the usual one.
var identifierTypes6 = []string{"duid", "hw-address"}

// withPrefix completes a short address such as ::20 with the prefix of the subnet,
// so that only the end has to be typed. Anything else stays as it is.
func withPrefix(text string, prefix netip.Prefix) string {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "::") || !prefix.IsValid() {
		return text
	}
	suffix, err := netip.ParseAddr(text)
	if err != nil || suffix.Zone() != "" {
		return text
	}
	net, host := prefix.Masked().Addr().As16(), suffix.As16()
	for i := range host {
		// the bits of this byte that belong to the prefix; the short form must not set them
		inPrefix := min(max(prefix.Bits()-8*i, 0), 8)
		if mask := byte(0xff << (8 - inPrefix)); host[i]&mask != 0 {
			return text
		}
		net[i] |= host[i]
	}
	return netip.AddrFrom16(net).String()
}

// check6 validates an IPv6 reservation against the subnets and the other
// reservations, and brings identifier and addresses into their canonical form.
func check6(in *Reservation6Input, subnets []kea.Subnet6, hosts []kea.Host, editing bool) []string {
	var problems []string
	if !slices.Contains(identifierTypes6, in.IdentifierType) {
		problems = append(problems, fmt.Sprintf("identifier type %q is not offered for IPv6", in.IdentifierType))
	} else if id, err := NormalizeIdentifier(in.IdentifierType, in.Identifier); err != nil {
		problems = append(problems, err.Error())
	} else {
		in.Identifier = id
	}
	var subnet *kea.Subnet6
	for i := range subnets {
		if subnets[i].ID == in.SubnetID {
			subnet = &subnets[i]
		}
	}
	if subnet == nil {
		problems = append(problems, fmt.Sprintf("subnet %d does not exist", in.SubnetID))
	}
	if len(in.Addresses) > maxPrefixes {
		problems = append(problems, fmt.Sprintf("at most %d addresses per reservation", maxPrefixes))
	}
	if len(in.Addresses) == 0 && len(in.Prefixes) == 0 {
		problems = append(problems, "an address or a prefix is missing")
	}
	if len(in.Prefixes) > maxPrefixes {
		problems = append(problems, fmt.Sprintf("at most %d prefixes per reservation", maxPrefixes))
	}
	seenPrefixes := map[netip.Prefix]bool{}
	for i, text := range in.Prefixes {
		p, err := netip.ParsePrefix(strings.TrimSpace(text))
		switch {
		case err != nil || !p.Addr().Is6() || p.Addr().Is4In6() || p.Addr().Zone() != "":
			problems = append(problems, fmt.Sprintf("%q is no IPv6 prefix", text))
			continue
		case p.Bits() < minPrefixLen:
			problems = append(problems, fmt.Sprintf("%s is too short to delegate, at least /%d", p, minPrefixLen))
			continue
		case p.Masked() != p:
			problems = append(problems, fmt.Sprintf("%s has bits set after its length, %s is meant", p, p.Masked()))
			continue
		case seenPrefixes[p]:
			problems = append(problems, fmt.Sprintf("%s is listed twice", p))
		}
		seenPrefixes[p] = true
		in.Prefixes[i] = p.String()
	}
	if subnet != nil {
		for i, text := range in.Addresses {
			in.Addresses[i] = withPrefix(text, subnet.Prefix)
		}
	}
	seen := map[netip.Addr]bool{}
	for i, text := range in.Addresses {
		ip, err := netip.ParseAddr(strings.TrimSpace(text))
		if err != nil || !ip.Is6() || ip.Is4In6() || ip.Zone() != "" {
			problems = append(problems, fmt.Sprintf("%q is no IPv6 address", text))
			continue
		}
		in.Addresses[i] = ip.String()
		if seen[ip] {
			problems = append(problems, fmt.Sprintf("%s is listed twice", ip))
		}
		seen[ip] = true
		if subnet != nil && !subnet.Prefix.Contains(ip) {
			problems = append(problems, fmt.Sprintf("%s is outside of subnet %s", ip, subnet.Subnet))
		}
		if subnet != nil && ip == subnet.Prefix.Masked().Addr() && subnet.Prefix.Bits() < 128 {
			problems = append(problems, fmt.Sprintf("%s is the subnet-router anycast address of %s", ip, subnet.Subnet))
		}
	}
	if in.Hostname = strings.TrimSpace(in.Hostname); in.Hostname != "" && !validDNSName(in.Hostname) {
		problems = append(problems, fmt.Sprintf("%q is no valid DNS name", in.Hostname))
	}
	for _, h := range hosts {
		idType, id := h.Identifier()
		same := h.SubnetID() == in.SubnetID && idType == in.IdentifierType && strings.EqualFold(id, in.Identifier)
		if same {
			if !editing {
				problems = append(problems, fmt.Sprintf("%s already has a reservation in this subnet", in.Identifier))
			}
			continue
		}
		for _, other := range h.IPs() {
			if ip, err := netip.ParseAddr(other); err == nil && seen[ip] {
				problems = append(problems, fmt.Sprintf("%s is reserved for %s already", ip, id))
			}
		}
		for _, other := range h.Prefixes() {
			if p, err := netip.ParsePrefix(other); err == nil {
				for mine := range seenPrefixes {
					if mine.Overlaps(p) {
						problems = append(problems, fmt.Sprintf("%s overlaps %s, reserved for %s", mine, p, id))
					}
				}
			}
		}
	}
	return problems
}

func (in *Reservation6Input) apply(h kea.Host) {
	for _, t := range identifierTypes6 {
		delete(h, t)
	}
	h[in.IdentifierType] = in.Identifier
	h["subnet-id"] = in.SubnetID
	addresses := make([]any, 0, len(in.Addresses))
	for _, a := range in.Addresses {
		addresses = append(addresses, a)
	}
	if len(addresses) > 0 {
		h["ip-addresses"] = addresses
	} else {
		delete(h, "ip-addresses")
	}
	prefixes := make([]any, 0, len(in.Prefixes))
	for _, p := range in.Prefixes {
		prefixes = append(prefixes, p)
	}
	if len(prefixes) > 0 {
		h["prefixes"] = prefixes
	} else {
		delete(h, "prefixes")
	}
	if in.Hostname != "" {
		h["hostname"] = in.Hostname
	} else {
		delete(h, "hostname")
	}
	userContext, _ := h["user-context"].(map[string]any)
	if in.Comment != "" {
		if userContext == nil {
			userContext = map[string]any{}
		}
		userContext["comment"] = in.Comment
	} else {
		delete(userContext, "comment")
	}
	if len(userContext) > 0 {
		h["user-context"] = userContext
	} else {
		delete(h, "user-context")
	}
}

func (m *Manager) precheck6(ctx context.Context, in *Reservation6Input, editing bool) error {
	if m.kea6 == nil {
		return ErrNotFound
	}
	subnets, err := m.subnets6(ctx)
	if err != nil {
		return err
	}
	hosts, err := m.hosts6(ctx, subnets)
	if err != nil {
		return err
	}
	if problems := check6(in, subnets, hosts, editing); len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

func verifyHost6(ctx context.Context, k *kea.Client, in *Reservation6Input) error {
	h, err := k.Reservation(ctx, in.SubnetID, in.IdentifierType, in.Identifier)
	if err != nil {
		return err
	}
	if h == nil {
		return errors.New("reservation not found after the change")
	}
	got, want := slices.Sorted(slices.Values(h.IPs())), slices.Sorted(slices.Values(in.Addresses))
	gotPrefixes, wantPrefixes := slices.Sorted(slices.Values(h.Prefixes())), slices.Sorted(slices.Values(in.Prefixes))
	if !slices.Equal(got, want) || !slices.Equal(gotPrefixes, wantPrefixes) || !strings.EqualFold(h.Hostname(), in.Hostname) {
		return fmt.Errorf("reservation is %s / %q instead", strings.Join(append(got, gotPrefixes...), ", "), h.Hostname())
	}
	return nil
}

func target6(in *Reservation6Input) string {
	return withNote(target(in.SubnetID, in.IdentifierType, in.Identifier), hostNote(in.Hostname, strings.Join(append(slices.Clone(in.Addresses), in.Prefixes...), ", ")))
}

// CreateReservation6 adds an IPv6 reservation.
func (m *Manager) CreateReservation6(ctx context.Context, user string, in Reservation6Input) (*Result, error) {
	ctx, cancel := detached(ctx)
	defer cancel()
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	if err := m.precheck6(ctx, &in, false); err != nil {
		return nil, err
	}
	return m.runOn(ctx, m.servers6(), change{
		user:         user,
		action:       "reservation6-add",
		target:       target6(&in),
		backupReason: "reservation-add",
		mutate: func(ctx context.Context, k *kea.Client) error {
			h := kea.Host{}
			in.apply(h)
			return k.AddReservation(ctx, h)
		},
		verify: func(ctx context.Context, k *kea.Client) error { return verifyHost6(ctx, k, &in) },
	}), nil
}

// UpdateReservation6 changes the addresses, name or comment of an IPv6
// reservation and keeps every other field.
func (m *Manager) UpdateReservation6(ctx context.Context, user string, in Reservation6Input) (*Result, error) {
	ctx, cancel := detached(ctx)
	defer cancel()
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	if err := m.precheck6(ctx, &in, true); err != nil {
		return nil, err
	}
	existing, err := m.kea6.Reservation(ctx, in.SubnetID, in.IdentifierType, in.Identifier)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, ErrNotFound
	}
	return m.runOn(ctx, m.servers6(), change{
		user:         user,
		action:       "reservation6-update",
		target:       target6(&in),
		backupReason: "reservation-update",
		mutate: func(ctx context.Context, k *kea.Client) error {
			h := kea.Host{}
			for key, value := range existing {
				h[key] = value
			}
			in.apply(h)
			return k.UpdateReservation(ctx, h)
		},
		verify: func(ctx context.Context, k *kea.Client) error { return verifyHost6(ctx, k, &in) },
	}), nil
}

// DeleteReservation6 removes an IPv6 reservation.
func (m *Manager) DeleteReservation6(ctx context.Context, user string, subnetID int64, identifierType, identifier string) (*Result, error) {
	ctx, cancel := detached(ctx)
	defer cancel()
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	if m.kea6 == nil || !slices.Contains(identifierTypes6, identifierType) {
		return nil, ErrNotFound
	}
	id, err := NormalizeIdentifier(identifierType, identifier)
	if err != nil {
		return nil, &ValidationError{Problems: []string{err.Error()}}
	}
	existing, err := m.kea6.Reservation(ctx, subnetID, identifierType, id)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, ErrNotFound
	}
	return m.runOn(ctx, m.servers6(), change{
		user:         user,
		action:       "reservation6-del",
		target:       withNote(target(subnetID, identifierType, id), hostNote(existing.Hostname(), strings.Join(append(existing.IPs(), existing.Prefixes()...), ", "))),
		backupReason: "reservation-del",
		mutate: func(ctx context.Context, k *kea.Client) error {
			return k.DeleteReservation(ctx, subnetID, identifierType, id)
		},
		verify: func(ctx context.Context, k *kea.Client) error {
			h, err := k.Reservation(ctx, subnetID, identifierType, id)
			if err != nil {
				return err
			}
			if h != nil {
				return errors.New("reservation still exists")
			}
			return nil
		},
	}), nil
}

// Reservation6Check is what the dialog shows while an address is entered.
type Reservation6Check struct {
	Warnings []string `json:"warnings"`
	// Suggestion is a free address outside of the pools, if one is found near the
	// start of the subnet.
	Suggestion string `json:"suggestion,omitempty"`
}

// suggestionRange is how far from the start of a subnet a free address is looked for.
const suggestionRange = 4096

// CheckReservation6 tells whether the addresses of a reservation lie inside a pool
// and offers one outside. Kea skips a reserved address when it hands out pool
// addresses, so a reservation inside a pool only collides with a client that holds
// it already; outside of the pools it never does.
func (m *Manager) CheckReservation6(ctx context.Context, in Reservation6Input) (*Reservation6Check, error) {
	if m.kea6 == nil {
		return nil, ErrNotFound
	}
	subnets, err := m.subnets6(ctx)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(subnets, func(s kea.Subnet6) bool { return s.ID == in.SubnetID })
	if i < 0 {
		return nil, ErrNotFound
	}
	subnet := subnets[i]
	out := &Reservation6Check{Warnings: []string{}}
	inPool := false
	for _, text := range in.Addresses {
		ip, err := netip.ParseAddr(withPrefix(text, subnet.Prefix))
		if err != nil {
			continue
		}
		for _, p := range subnet.Pools {
			if p.Contains(ip) {
				inPool = true
				out.Warnings = append(out.Warnings, fmt.Sprintf("%s lies inside the dynamic pool %s. Kea keeps it for this client, but another client that holds it now keeps it until its lease ends; an address outside of the pool avoids that.", ip, p.Text))
			}
		}
	}
	if !inPool {
		return out, nil
	}
	hosts, err := m.hosts6(ctx, subnets)
	if err != nil {
		slog.Debug("no address to suggest", "error", err)
		return out, nil
	}
	taken := map[netip.Addr]bool{}
	for _, h := range hosts {
		for _, text := range h.IPs() {
			if ip, err := netip.ParseAddr(text); err == nil {
				taken[ip] = true
			}
		}
	}
	if leases, err := m.kea6.Leases6(ctx); err == nil {
		for _, l := range leases {
			if ip, err := netip.ParseAddr(l.IPAddress); err == nil {
				taken[ip] = true
			}
		}
	}
	// ::1 is commonly the router, so the search starts after it
	ip := subnet.Prefix.Masked().Addr().Next().Next()
	for range suggestionRange {
		if !subnet.Prefix.Contains(ip) {
			break
		}
		if !taken[ip] && !inPools6(subnet.Pools, ip) {
			out.Suggestion = ip.String()
			break
		}
		ip = ip.Next()
	}
	return out, nil
}
