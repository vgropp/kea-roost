package manager

import (
	"bytes"
	"context"
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

// hostsContent renders one line per reservation that has an address and a name.
func hostsContent(hosts []kea.Host, domains []string) []byte {
	type entry struct {
		ip   netip.Addr
		line string
	}
	var entries []entry
	for _, h := range hosts {
		name := dnsName(h.Hostname())
		ip, err := netip.ParseAddr(h.IP())
		if err != nil || !validDNSName(name) {
			continue
		}
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
	slices.SortFunc(entries, func(a, b entry) int { return a.ip.Compare(b.ip) })
	var out bytes.Buffer
	out.WriteString("# Written by Kea Roost, changes here are lost.\n")
	for _, e := range entries {
		out.WriteString(e.line + "\n")
	}
	return out.Bytes()
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
	return writeHostsFile(m.hostsPath, hostsContent(hosts, m.hostsDomains))
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

func (m *Manager) hostsCheck() *HealthCheck {
	if m.hostsPath == "" {
		return nil
	}
	m.hostsMu.Lock()
	err := m.hostsErr
	m.hostsMu.Unlock()
	if err != nil {
		return &HealthCheck{"DNS hosts file", "", SeverityWarn, fmt.Sprintf("not updated: %v", err)}
	}
	return &HealthCheck{"DNS hosts file", "", SeverityOK, m.hostsPath}
}
