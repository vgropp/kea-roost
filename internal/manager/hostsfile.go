package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/vgropp/kea-roost/internal/kea"
)

var notInName = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

const (
	hostsFileName        = "reservations"
	hostsRefreshInterval = time.Minute
	hostsWriteTimeout    = 10 * time.Second
	maxDNSName           = 253
	maxDNSLabel          = 63
)

// SetHostsDir makes the manager keep a hosts file of all reservations in a folder, for
// a DNS server such as dnsmasq. Each domain is added to names without one; the first
// one is the name reverse lookups answer with.
func (m *Manager) SetHostsDir(dir string, domains []string) {
	m.hostsPath, m.hostsDomains = filepath.Join(dir, hostsFileName), domains
}

// dnsName makes a reservation name usable in DNS. Kea accepts anything, and in a
// hosts file each word of "living room tv" would become a name of its own.
func dnsName(name string) string {
	name = notInName.ReplaceAllString(strings.TrimSpace(name), "-")
	return strings.Trim(name, "-.")
}

// validDNSName applies the limits of DNS, which Kea does not enforce on a hostname.
func validDNSName(name string) bool {
	if name == "" || len(name) > maxDNSName || !hostnamePattern.MatchString(name) {
		return false
	}
	return !slices.ContainsFunc(strings.Split(name, "."), func(label string) bool { return len(label) > maxDNSLabel })
}

// hostsContent renders one line per reserved address with a name, IPv4 and IPv6.
func hostsContent(hosts []kea.Host, domains []string) []byte {
	type entry struct {
		ip   netip.Addr
		line string
	}
	var entries []entry
	for _, h := range hosts {
		name := dnsName(h.Hostname())
		if !validDNSName(name) {
			continue
		}
		for _, text := range append([]string{h.IP()}, h.IPs()...) {
			ip, err := netip.ParseAddr(text)
			// a zone is free text and would end up as names on the line
			if err != nil || ip.Zone() != "" {
				continue
			}
			ip = ip.Unmap()
			// a DNS server answers reverse lookups with the first name
			names := []string{name}
			if !strings.Contains(name, ".") {
				names = names[:0]
				for _, domain := range domains {
					names = append(names, name+"."+domain)
				}
				names = append(names, name)
			}
			entries = append(entries, entry{ip, ip.String() + " " + strings.Join(names, " ")})
		}
	}
	slices.SortFunc(entries, func(a, b entry) int { return a.ip.Compare(b.ip) })
	var out bytes.Buffer
	out.WriteString("# Written by Kea Roost, changes here are lost.\n")
	for _, e := range entries {
		out.WriteString(e.line + "\n")
	}
	return out.Bytes()
}

// namedHosts6 returns the IPv6 reservations. One without a hostname takes the name
// of the IPv4 reservation of its MAC, so that a device gets A and AAAA under one name.
func (m *Manager) namedHosts6(ctx context.Context, hosts4 []kea.Host) ([]kea.Host, error) {
	subnets, err := m.subnets6(ctx)
	if err != nil {
		return nil, err
	}
	hosts, err := m.hosts6(ctx, subnets)
	if err != nil {
		return nil, err
	}
	_, byMAC := indexHosts(hosts4)
	for i, h := range hosts {
		if idType, id := h.Identifier(); h.Hostname() == "" && idType == "hw-address" {
			if d, ok := byMAC[strings.ToLower(id)]; ok {
				named := kea.Host{"hostname": d.Hostname(), "ip-addresses": h["ip-addresses"]}
				hosts[i] = named
			}
		}
	}
	return hosts, nil
}

// writeHostsFile replaces the file in one step, and only if something changed: a
// DNS server watching it reloads on every write. The temporary name starts with a
// dot, because dnsmasq skips those in a watched directory.
func writeHostsFile(path string, content []byte) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, content) {
		return nil
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return err
	}
	// the mode of a file that stayed behind is kept by WriteFile
	if err := os.Chmod(tmp, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// RefreshHostsFile brings the hosts file up to date. A failed attempt keeps the old
// file, because stale names are better than none.
func (m *Manager) RefreshHostsFile(ctx context.Context) error {
	if m.hostsPath == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, hostsWriteTimeout)
	defer cancel()
	// a change and the timer must not write the same temporary file at once
	m.hostsWriteMu.Lock()
	defer m.hostsWriteMu.Unlock()
	err := m.refreshHosts(ctx)
	m.hostsMu.Lock()
	m.hostsErr = err
	m.hostsMu.Unlock()
	return err
}

func (m *Manager) refreshHosts(ctx context.Context) error {
	subnets, err := m.subnets(ctx)
	if err != nil {
		return err
	}
	hosts, err := m.allHosts(ctx, subnets)
	if err != nil {
		return err
	}
	// kea-dhcp6 being away must not hold back the IPv4 names: its last known
	// entries stay, and the error is reported after the file is written
	var err6 error
	if m.kea6 != nil {
		v6, err := m.namedHosts6(ctx, hosts)
		if err != nil {
			err6 = &staleIPv6Error{err}
		} else {
			m.hosts6Last = v6
		}
		hosts = append(hosts, m.hosts6Last...)
	}
	if err := writeHostsFile(m.hostsPath, hostsContent(hosts, m.hostsDomains)); err != nil {
		return err
	}
	return err6
}

// WatchHostsFile keeps the file current until ctx ends. Changes through the manager
// refresh it at once; the timer catches those made in Kea directly.
func (m *Manager) WatchHostsFile(ctx context.Context) {
	if m.hostsPath == "" {
		return
	}
	ticker := time.NewTicker(hostsRefreshInterval)
	defer ticker.Stop()
	for {
		if err := m.RefreshHostsFile(ctx); err != nil {
			slog.Warn("hosts file", "path", m.hostsPath, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// staleIPv6Error tells that the file was written, with the IPv6 entries of the
// last refresh because kea-dhcp6 did not answer.
type staleIPv6Error struct{ err error }

func (e *staleIPv6Error) Error() string { return "IPv6 kept from the last refresh: " + e.err.Error() }
func (e *staleIPv6Error) Unwrap() error { return e.err }

func (m *Manager) hostsCheck() *HealthCheck {
	if m.hostsPath == "" {
		return nil
	}
	m.hostsMu.Lock()
	err := m.hostsErr
	m.hostsMu.Unlock()
	var stale *staleIPv6Error
	if errors.As(err, &stale) {
		return &HealthCheck{"DNS hosts file", "", SeverityWarn, fmt.Sprintf("IPv4 is current, the IPv6 entries are from the last refresh: %v", stale.err)}
	}
	if err != nil {
		return &HealthCheck{"DNS hosts file", "", SeverityWarn, fmt.Sprintf("not updated: %v", err)}
	}
	return &HealthCheck{"DNS hosts file", "", SeverityOK, m.hostsPath}
}
