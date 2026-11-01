// Package manager holds the application logic between web interface and Kea.
package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vgropp/kea-roost/internal/kea"
	"github.com/vgropp/kea-roost/internal/oui"
)

// Lease states as reported by Kea.
const (
	leaseStateDefault   = 0
	leaseStateDeclined  = 1
	leaseStateReclaimed = 2
	leaseStateReleased  = 3
)

const (
	recentLeaseCount = 5
	newDeviceAge     = 24 * time.Hour
	// A slow ARP monitor must not hold up the dashboard.
	dashboardARPTimeout = 3 * time.Second
)

// Manager serves one Kea instance and keeps its peers in step with it. What it
// shows comes from the primary, or from a peer while the primary is down. Changes
// always start on the primary, so they are refused then.
type Manager struct {
	kea        *kea.Client
	peers      []server
	store      *Store
	serverName string
	readMu     sync.Mutex
	current    server
	checked    time.Time
	version    string

	hostsPath    string
	hostsDomains []string
	hostsWriteMu sync.Mutex
	hostsMu      sync.Mutex
	hostsErr     error
	now          func() time.Time
	writeMu      sync.Mutex
	arpURL       string
	settings     atomic.Pointer[Settings]
	apURL        string
}

// New creates a Manager.
func New(client *kea.Client, store *Store, serverName string) *Manager {
	m := &Manager{kea: client, store: store, serverName: serverName, now: time.Now}
	s := defaultSettings()
	m.settings.Store(&s)
	return m
}

// SetVersion names the running release.
func (m *Manager) SetVersion(version string) { m.version = version }

// Version returns the running release.
func (m *Manager) Version() string { return m.version }

// Store exposes backups and audit log.
func (m *Manager) Store() *Store { return m.store }

// ServerName returns the display name of the Kea instance.
func (m *Manager) ServerName() string { return m.serverName }

// Severity grades warnings and states.
type Severity string

// Severities: info only tells something, the others grade in ascending order.
const (
	SeverityInfo Severity = "info"
	SeverityOK   Severity = "ok"
	SeverityWarn Severity = "warn"
	SeverityCrit Severity = "crit"
)

// Warning is a condition the operator should look at.
type Warning struct {
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
}

// LeaseView is a lease prepared for display.
type LeaseView struct {
	IPAddress string `json:"ipAddress"`
	HWAddress string `json:"hwAddress"`
	Vendor    string `json:"vendor,omitempty"`
	ClientID  string `json:"clientId"`
	Hostname  string `json:"hostname"`
	SubnetID  int64  `json:"subnetId"`
	Subnet    string `json:"subnet,omitempty"`
	State     string `json:"state"`
	Active    bool   `json:"active"`
	// Started is the last contact with the client. Kea does not keep the first
	// allocation, so a renewal moves this time as well.
	Started   time.Time `json:"started"`
	Expires   time.Time `json:"expires"`
	Remaining int64     `json:"remaining"`
	Server    string    `json:"server"`
	// Reservation is the matching reservation, if any.
	Reservation *ReservationRef `json:"reservation,omitempty"`
	// New marks a client the ARP monitor saw for the first time just now.
	New bool `json:"new,omitempty"`
	// Wifi says where the client is connected, if the AP dashboard knows it.
	Wifi *WifiClient `json:"wifi,omitempty"`
}

// ReservationRef points from a lease to its reservation.
type ReservationRef struct {
	// Conflict is set when another client uses the reserved address.
	Conflict   string   `json:"conflict,omitempty"`
	Identifier string   `json:"identifier"`
	Classes    []string `json:"classes"`
	IPAddress  string   `json:"ipAddress"`
	Hostname   string   `json:"hostname"`
}

// LeaseList is the answer of LeasesWithReservations.
type LeaseList struct {
	Leases   []LeaseView `json:"leases"`
	Warnings []Warning   `json:"warnings"`
}

func (m *Manager) leaseView(l kea.Lease, now time.Time) LeaseView {
	expires := time.Unix(l.CLTT+l.ValidLft, 0)
	remaining := max(int64(expires.Sub(now).Seconds()), 0)
	v := LeaseView{
		IPAddress: l.IPAddress,
		HWAddress: l.HWAddress,
		Vendor:    oui.Lookup(l.HWAddress),
		ClientID:  l.ClientID,
		Hostname:  l.Hostname,
		SubnetID:  l.SubnetID,
		Started:   time.Unix(l.CLTT, 0),
		Expires:   expires,
		Remaining: remaining,
		Server:    m.sourceName(),
	}
	switch l.State {
	case leaseStateDefault:
		v.Active = remaining > 0
		v.State = "active"
		if !v.Active {
			v.State = "expired"
		}
	case leaseStateDeclined:
		v.State = "declined"
	case leaseStateReclaimed:
		v.State = "expired"
	case leaseStateReleased:
		v.State = "released"
	default:
		v.State = fmt.Sprintf("unknown (%d)", l.State)
	}
	return v
}

// Leases returns all leases.
func (m *Manager) Leases(ctx context.Context) ([]LeaseView, error) {
	leases, err := m.source(ctx).client.Leases(ctx)
	if err != nil {
		return nil, err
	}
	now := m.now()
	out := make([]LeaseView, 0, len(leases))
	for _, l := range leases {
		out = append(out, m.leaseView(l, now))
	}
	return out, nil
}

// reservationRef describes the reservation matched for a client. The match can
// come from the address alone, so the MAC of the client may be a different one.
func reservationRef(h kea.Host, ip, mac string) *ReservationRef {
	idType, id := h.Identifier()
	ref := &ReservationRef{Identifier: id, Classes: h.Classes(), IPAddress: h.IP(), Hostname: h.Hostname()}
	if idType == "hw-address" && mac != "" && h.IP() == ip && !strings.EqualFold(id, mac) {
		ref.Conflict = fmt.Sprintf("%s is reserved for %s, but %s uses it", ip, id, mac)
	}
	return ref
}

// indexHosts makes reservations searchable by address and MAC.
func indexHosts(hosts []kea.Host) (byIP, byMAC map[string]kea.Host) {
	byIP = make(map[string]kea.Host, len(hosts))
	byMAC = make(map[string]kea.Host, len(hosts))
	for _, h := range hosts {
		if ip := h.IP(); ip != "" {
			byIP[ip] = h
		}
		if idType, id := h.Identifier(); idType == "hw-address" {
			byMAC[strings.ToLower(id)] = h
		}
	}
	return byIP, byMAC
}

// LeasesWithReservations returns all leases linked to their reservations. A
// reservation matches by address or, for a client that holds another address,
// by MAC. Without reservation data the leases are still served.
func (m *Manager) LeasesWithReservations(ctx context.Context) (*LeaseList, error) {
	leases, err := m.Leases(ctx)
	if err != nil {
		return nil, err
	}
	list := &LeaseList{Leases: leases, Warnings: []Warning{}}
	wifi, err := m.wifiClients(ctx)
	if err != nil {
		list.Warnings = append(list.Warnings, Warning{SeverityWarn, err.Error()})
	}
	for i := range list.Leases {
		if c, ok := wifi[strings.ToLower(list.Leases[i].HWAddress)]; ok {
			list.Leases[i].Wifi = c.view()
		}
	}
	subnets, err := m.subnets(ctx)
	var hosts []kea.Host
	if err == nil {
		for i := range list.Leases {
			for _, s := range subnets {
				if s.ID == list.Leases[i].SubnetID {
					list.Leases[i].Subnet = s.Subnet
				}
			}
		}
		hosts, err = m.allHosts(ctx, subnets)
	}
	if err != nil {
		list.Warnings = append(list.Warnings, Warning{SeverityWarn, "Reservations unavailable: " + err.Error()})
		return list, nil
	}
	byIP, byMAC := indexHosts(hosts)
	for i := range list.Leases {
		l := &list.Leases[i]
		h, ok := byIP[l.IPAddress]
		if !ok {
			h, ok = byMAC[strings.ToLower(l.HWAddress)]
		}
		if ok {
			l.Reservation = reservationRef(h, l.IPAddress, l.HWAddress)
			if !l.Active {
				l.Reservation.Conflict = ""
			}
		}
	}
	return list, nil
}

// OptionView is a DHCP option of a reservation.
type OptionView struct {
	Name string `json:"name,omitempty"`
	Code int64  `json:"code,omitempty"`
	Data string `json:"data"`
}

// ReservationView is a reservation joined with lease and pool information.
type ReservationView struct {
	SubnetID       int64        `json:"subnetId"`
	Subnet         string       `json:"subnet"`
	Global         bool         `json:"global"`
	IdentifierType string       `json:"identifierType"`
	Identifier     string       `json:"identifier"`
	Vendor         string       `json:"vendor,omitempty"`
	IPAddress      string       `json:"ipAddress"`
	Hostname       string       `json:"hostname"`
	Options        []OptionView `json:"options"`
	Classes        []string     `json:"classes"`
	Comment        string       `json:"comment"`
	InPool         bool         `json:"inPool"`
	Lease          *LeaseView   `json:"lease"`
}

// ReservationList is the answer of Reservations.
type ReservationList struct {
	Reservations []ReservationView `json:"reservations"`
	Warnings     []Warning         `json:"warnings"`
}

// allHosts fetches the reservations of every subnet plus the global ones.
func (m *Manager) allHosts(ctx context.Context, subnets []kea.Subnet) ([]kea.Host, error) {
	return hostsOn(ctx, m.source(ctx).client, subnets)
}

func hostsOn(ctx context.Context, k *kea.Client, subnets []kea.Subnet) ([]kea.Host, error) {
	hosts, err := k.Reservations(ctx, 0)
	if err != nil {
		return nil, err
	}
	for _, s := range subnets {
		list, err := k.Reservations(ctx, s.ID)
		if err != nil {
			return nil, err
		}
		hosts = append(hosts, list...)
	}
	return hosts, nil
}

func (m *Manager) subnets(ctx context.Context) ([]kea.Subnet, error) {
	config, err := m.source(ctx).client.ConfigGet(ctx)
	if err != nil {
		return nil, err
	}
	return kea.ParseSubnets(config)
}

// Reservations lists all reservations. Without the lease hook the list is still
// served, only the lease column stays empty.
func (m *Manager) Reservations(ctx context.Context) (*ReservationList, error) {
	subnets, err := m.subnets(ctx)
	if err != nil {
		return nil, err
	}
	hosts, err := m.allHosts(ctx, subnets)
	if err != nil {
		return nil, err
	}
	list := &ReservationList{Reservations: []ReservationView{}, Warnings: []Warning{}}
	leases, err := m.source(ctx).client.Leases(ctx)
	if err != nil {
		list.Warnings = append(list.Warnings, Warning{SeverityWarn, "Lease data unavailable: " + err.Error()})
	}
	byIP := make(map[string]kea.Lease, len(leases))
	for _, l := range leases {
		byIP[l.IPAddress] = l
	}
	now := m.now()
	for _, h := range hosts {
		idType, id := h.Identifier()
		v := ReservationView{
			SubnetID:       h.SubnetID(),
			Subnet:         "global",
			IdentifierType: idType,
			Identifier:     id,
			IPAddress:      h.IP(),
			Hostname:       h.Hostname(),
			Options:        optionViews(h),
			Comment:        h.Comment(),
			Classes:        h.Classes(),
			Global:         h.SubnetID() == 0,
		}
		if idType == "hw-address" {
			v.Vendor = oui.Lookup(id)
		}
		s := kea.SubnetFor(subnets, h)
		if s != nil {
			v.Subnet = s.Subnet
		}
		if ip, err := netip.ParseAddr(v.IPAddress); err == nil {
			v.InPool = s != nil && s.PoolContaining(ip) != nil
			if l, ok := byIP[v.IPAddress]; ok {
				lv := m.leaseView(l, now)
				v.Lease = &lv
			}
		}
		list.Reservations = append(list.Reservations, v)
	}
	return list, nil
}

func optionViews(h kea.Host) []OptionView {
	out := []OptionView{}
	for _, o := range h.Options() {
		v := OptionView{}
		v.Name, _ = o["name"].(string)
		v.Data, _ = o["data"].(string)
		if n, ok := o["code"].(json.Number); ok {
			v.Code, _ = n.Int64()
		}
		out = append(out, v)
	}
	return out
}

// PoolView is a pool with its utilization.
type PoolView struct {
	Pool     string   `json:"pool"`
	Total    *int64   `json:"total"`
	Assigned *int64   `json:"assigned"`
	Percent  *float64 `json:"percent"`
}

// SubnetView is a subnet prepared for display.
type SubnetView struct {
	ID            int64        `json:"id"`
	Subnet        string       `json:"subnet"`
	SharedNetwork string       `json:"sharedNetwork,omitempty"`
	Gateways      []string     `json:"gateways"`
	DNSServers    []string     `json:"dnsServers"`
	Pools         []PoolView   `json:"pools"`
	ValidLifetime *int64       `json:"validLifetime"`
	Options       []kea.Option `json:"options"`
	Total         *int64       `json:"total"`
	Assigned      *int64       `json:"assigned"`
	Declined      *int64       `json:"declined"`
	OutsidePools  *int64       `json:"outsidePools"`
	Percent       *float64     `json:"percent"`
	Reservations  *int         `json:"reservations"`
}

func stat(stats map[string]int64, format string, args ...any) *int64 {
	if v, ok := stats[fmt.Sprintf(format, args...)]; ok {
		return &v
	}
	return nil
}

func percent(assigned, total *int64) *float64 {
	if assigned == nil || total == nil || *total == 0 {
		return nil
	}
	p := float64(*assigned) / float64(*total) * 100
	return &p
}

// occupied reports whether a lease currently blocks its address.
func (l LeaseView) occupied() bool {
	return l.Active || (l.State == "declined" && l.Remaining > 0)
}

// countLeases sets the assigned numbers from the leases themselves. Kea's
// assigned-addresses statistic counts every lease of the subnet while its total
// only covers the pools, so leases on reservations outside a pool would inflate
// the utilization.
func countLeases(v *SubnetView, s kea.Subnet, leases []LeaseView) {
	inPool := make([]int64, len(s.Pools))
	var outside int64
	for _, l := range leases {
		if l.SubnetID != s.ID || !l.occupied() {
			continue
		}
		ip, err := netip.ParseAddr(l.IPAddress)
		if err != nil {
			continue
		}
		found := false
		for i, p := range s.Pools {
			if p.Contains(ip) {
				inPool[i]++
				found = true
				break
			}
		}
		if !found {
			outside++
		}
	}
	var assigned, total int64
	for i, p := range s.Pools {
		size := p.Size()
		assigned += inPool[i]
		total += size
		v.Pools[i].Assigned, v.Pools[i].Total = &inPool[i], &size
		v.Pools[i].Percent = percent(&inPool[i], &size)
	}
	v.Assigned, v.Total, v.OutsidePools = &assigned, &total, &outside
	v.Percent = percent(v.Assigned, v.Total)
}

// subnetViews combines configuration and live data. Nil leases mean the lease
// hook is missing; the numbers then come from Kea's statistics.
func subnetViews(subnets []kea.Subnet, stats map[string]int64, hosts []kea.Host, leases []LeaseView) []SubnetView {
	counts := map[int64]int{}
	for _, h := range hosts {
		if s := kea.SubnetFor(subnets, h); s != nil {
			counts[s.ID]++
		}
	}
	out := make([]SubnetView, 0, len(subnets))
	for _, s := range subnets {
		v := SubnetView{
			ID:            s.ID,
			Subnet:        s.Subnet,
			SharedNetwork: s.SharedNetwork,
			Gateways:      s.Gateways(),
			DNSServers:    s.DNSServers(),
			Pools:         make([]PoolView, 0, len(s.Pools)),
			ValidLifetime: s.ValidLifetime,
			Options:       s.Options,
			Total:         stat(stats, "subnet[%d].total-addresses", s.ID),
			Assigned:      stat(stats, "subnet[%d].assigned-addresses", s.ID),
			Declined:      stat(stats, "subnet[%d].declined-addresses", s.ID),
		}
		v.Percent = percent(v.Assigned, v.Total)
		if hosts != nil {
			n := counts[s.ID]
			v.Reservations = &n
		}
		for _, p := range s.Pools {
			pv := PoolView{
				Pool:     p.Text,
				Total:    stat(stats, "subnet[%d].pool[%d].total-addresses", s.ID, p.ID),
				Assigned: stat(stats, "subnet[%d].pool[%d].assigned-addresses", s.ID, p.ID),
			}
			pv.Percent = percent(pv.Assigned, pv.Total)
			v.Pools = append(v.Pools, pv)
		}
		if leases != nil {
			countLeases(&v, s, leases)
		}
		out = append(out, v)
	}
	return out
}

// SubnetList is the answer of Subnets.
type SubnetList struct {
	Subnets      []SubnetView `json:"subnets"`
	Reservations *int         `json:"reservations"`
	Editable     bool         `json:"editable"`
	Warnings     []Warning    `json:"warnings"`
}

// Subnets lists the configured subnets. Statistics and reservation counts are
// optional additions: their absence must not hide the configuration.
func (m *Manager) Subnets(ctx context.Context) (*SubnetList, error) {
	subnets, err := m.subnets(ctx)
	if err != nil {
		return nil, err
	}
	list := &SubnetList{Warnings: []Warning{}, Editable: m.subnetsEditable(ctx)}
	stats, err := m.source(ctx).client.Statistics(ctx)
	if err != nil {
		list.Warnings = append(list.Warnings, Warning{SeverityWarn, "Statistics unavailable: " + err.Error()})
	}
	hosts, err := m.allHosts(ctx, subnets)
	if err != nil {
		list.Warnings = append(list.Warnings, Warning{SeverityWarn, "Reservations unavailable: " + err.Error()})
		hosts = nil
	} else {
		n := len(hosts)
		list.Reservations = &n
		if hosts == nil {
			hosts = []kea.Host{}
		}
	}
	leases, err := m.Leases(ctx)
	if err != nil {
		leases = nil
	}
	list.Subnets = subnetViews(subnets, stats, hosts, leases)
	return list, nil
}

// HAView is one HA relationship with its evaluation.
type HAView struct {
	kea.HARelationship
	Severity      Severity `json:"severity"`
	UnsentUpdates *int64   `json:"unsentUpdates"`
}

// HAStateSeverity grades a Kea HA state.
func HAStateSeverity(state string) Severity {
	switch state {
	case "hot-standby", "load-balancing", "passive-backup", "backup":
		return SeverityOK
	case "terminated":
		return SeverityCrit
	}
	return SeverityWarn
}

func (m *Manager) haViews(ctx context.Context, status *kea.Status) ([]HAView, []Warning) {
	views := []HAView{}
	warnings := []Warning{}
	for _, rel := range status.HA {
		local := rel.Servers.Local
		v := HAView{HARelationship: rel, Severity: HAStateSeverity(local.State)}
		if v.Severity != SeverityOK {
			warnings = append(warnings, Warning{v.Severity, fmt.Sprintf("HA state of %s: %s", local.ServerName, local.State)})
		}
		if hb, err := m.source(ctx).client.HAHeartbeat(ctx, local.ServerName); err == nil {
			v.UnsentUpdates = &hb.UnsentUpdates
		}
		if r := rel.Servers.Remote; r != nil {
			w := remoteWarnings(r, m.cfg().ClockSkewSeconds)
			if len(w) > 0 && v.Severity == SeverityOK {
				v.Severity = SeverityWarn
			}
			warnings = append(warnings, w...)
		}
		views = append(views, v)
	}
	return views, warnings
}

func skewed(value int64, limit int) bool {
	return value > int64(limit) || value < -int64(limit)
}

func remoteWarnings(r *kea.HAServerRemote, skew int) []Warning {
	var out []Warning
	if !r.InTouch {
		out = append(out, Warning{SeverityCrit, fmt.Sprintf("HA partner %s has never answered", r.ServerName)})
	}
	if r.CommunicationInterrupted {
		out = append(out, Warning{SeverityCrit, fmt.Sprintf("Communication with HA partner %s interrupted", r.ServerName)})
	}
	if r.LastState != "" && HAStateSeverity(r.LastState) != SeverityOK {
		out = append(out, Warning{SeverityWarn, fmt.Sprintf("HA partner %s reports state %s", r.ServerName, r.LastState)})
	}
	if r.ClockSkew != nil && skewed(*r.ClockSkew, skew) {
		out = append(out, Warning{SeverityWarn, fmt.Sprintf("Clock skew to %s: %d s", r.ServerName, *r.ClockSkew)})
	}
	return out
}

// Dashboard is the overview of the Kea instance.
type Dashboard struct {
	Server struct {
		Name        string     `json:"name"`
		URL         string     `json:"url"`
		Reachable   bool       `json:"reachable"`
		Error       string     `json:"error,omitempty"`
		Version     string     `json:"version,omitempty"`
		Uptime      int64      `json:"uptime"`
		LastContact *time.Time `json:"lastContact"`
	} `json:"server"`
	HA           []HAView     `json:"ha"`
	HAWarnings   []Warning    `json:"haWarnings"`
	Servers      []ServerRow  `json:"servers"`
	RecentLeases []LeaseView  `json:"recentLeases"`
	ActiveLeases *int         `json:"activeLeases"`
	TotalLeases  *int         `json:"totalLeases"`
	Reservations *int         `json:"reservations"`
	SubnetCount  *int         `json:"subnetCount"`
	Subnets      []SubnetView `json:"subnets"`
	Warnings     []Warning    `json:"warnings"`
}

// Dashboard collects the overview. It always succeeds: an unreachable server is
// the very state it has to show.
func (m *Manager) Dashboard(ctx context.Context) *Dashboard {
	d := &Dashboard{HA: []HAView{}, HAWarnings: []Warning{}, Servers: []ServerRow{}, RecentLeases: []LeaseView{}, Subnets: []SubnetView{}, Warnings: []Warning{}}
	src := m.source(ctx)
	d.Server.Name = src.name
	d.Server.URL = src.client.URL()
	collectRows := m.startServerRows(ctx)
	if src.client != m.kea {
		d.Warnings = append(d.Warnings, Warning{SeverityCrit, fmt.Sprintf("%s is unreachable. Showing the data of %s; changes are not possible.", m.serverName, src.name)})
	}

	status, err := src.client.Status(ctx)
	if err == nil {
		d.Server.Reachable = true
		d.Server.Uptime = status.Uptime
		d.HA, d.HAWarnings = m.haViews(ctx, status)
		d.Warnings = append(d.Warnings, d.HAWarnings...)
		if status.Sockets.Status != "" && status.Sockets.Status != "ready" {
			d.Warnings = append(d.Warnings, Warning{SeverityCrit, "DHCP sockets: " + status.Sockets.Status})
		}
		d.Server.Version, err = src.client.Version(ctx)
		if err != nil {
			d.warn(err, "Version")
		}
		m.fillCounts(ctx, d)
	} else {
		d.Server.Error = err.Error()
		d.Warnings = append(d.Warnings, Warning{SeverityCrit, err.Error()})
	}
	if t := src.client.LastContact(); !t.IsZero() {
		d.Server.LastContact = &t
	}
	d.Servers = collectRows()
	d.Warnings = append(d.Warnings, rowWarnings(d.Servers)...)
	return d
}

func (d *Dashboard) warn(err error, what string) {
	d.Warnings = append(d.Warnings, Warning{SeverityWarn, what + ": " + err.Error()})
}

// recentLeases returns the active leases with the latest client contact. The
// monitor tells new clients from renewals; without it they are simply unmarked.
func (m *Manager) recentLeases(ctx context.Context, leases []LeaseView) []LeaseView {
	recent := []LeaseView{}
	for _, l := range leases {
		if l.Active {
			recent = append(recent, l)
		}
	}
	slices.SortFunc(recent, func(a, b LeaseView) int { return b.Started.Compare(a.Started) })
	recent = recent[:min(len(recent), recentLeaseCount)]
	if !m.ARPMonitorEnabled() {
		return recent
	}
	ctx, cancel := context.WithTimeout(ctx, dashboardARPTimeout)
	defer cancel()
	devices, err := m.arpDevices(ctx)
	if err != nil {
		slog.Debug("recent leases", "error", err)
		return recent
	}
	firstSeen := make(map[string]time.Time, len(devices))
	for _, d := range devices {
		firstSeen[strings.ToLower(d.MAC)] = d.FirstSeen
	}
	now := m.now()
	for i := range recent {
		t, ok := firstSeen[strings.ToLower(recent[i].HWAddress)]
		recent[i].New = ok && !t.IsZero() && now.Sub(t) < newDeviceAge
	}
	return recent
}

func (m *Manager) fillCounts(ctx context.Context, d *Dashboard) {
	if config, err := m.source(ctx).client.ConfigGet(ctx); err == nil {
		if lossy := kea.LossyOptions(config); len(lossy) > 0 {
			d.Warnings = append(d.Warnings, Warning{SeverityWarn, fmt.Sprintf("Changes are locked: config-write would erase the value of %s", strings.Join(lossy, ", "))})
		}
	}
	if leases, err := m.LeasesWithReservations(ctx); err != nil {
		d.warn(err, "Leases")
	} else {
		active := 0
		for _, l := range leases.Leases {
			if l.Active {
				active++
			}
		}
		total := len(leases.Leases)
		d.ActiveLeases, d.TotalLeases = &active, &total
		d.RecentLeases = m.recentLeases(ctx, leases.Leases)
	}
	list, err := m.Subnets(ctx)
	if err != nil {
		d.warn(err, "Configuration")
		return
	}
	d.Subnets = list.Subnets
	d.Warnings = append(d.Warnings, list.Warnings...)
	count := len(list.Subnets)
	d.SubnetCount = &count
	d.Reservations = list.Reservations
	for _, s := range list.Subnets {
		if s.Percent != nil && *s.Percent >= float64(m.cfg().PoolWarnPercent) {
			severity := SeverityWarn
			if *s.Percent >= float64(m.cfg().PoolCritPercent) {
				severity = SeverityCrit
			}
			d.Warnings = append(d.Warnings, Warning{severity, fmt.Sprintf("Subnet %s is %.0f %% utilized", s.Subnet, *s.Percent)})
		}
	}
}
