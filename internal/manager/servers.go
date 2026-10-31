package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vgropp/kea-roost/internal/kea"
)

// A partner that is down must not hold up the pages that show it.
const (
	serverCheckTimeout     = 5 * time.Second
	dashboardServerTimeout = 2 * time.Second
	sourceCheckInterval    = 5 * time.Second
	sourceCheckTimeout     = 1500 * time.Millisecond
)

type server struct {
	name   string
	client *kea.Client
}

// AddPeer adds a Kea server that gets every change after the primary.
func (m *Manager) AddPeer(name string, client *kea.Client) {
	m.peers = append(m.peers, server{name, client})
}

// HasPeers reports whether more than one Kea server is configured.
func (m *Manager) HasPeers() bool { return len(m.peers) > 0 }

// source picks the server to read from: the primary, or the first peer that
// answers while it is down. The choice is kept for a moment, because a page reads
// many times and a server that is gone only answers with a timeout.
func (m *Manager) source(ctx context.Context) server {
	primary := server{m.serverName, m.kea}
	if !m.HasPeers() {
		return primary
	}
	m.readMu.Lock()
	defer m.readMu.Unlock()
	if m.current.client != nil && m.now().Sub(m.checked) < sourceCheckInterval {
		return m.current
	}
	m.current, m.checked = primary, m.now()
	for _, s := range m.servers() {
		probe, cancel := context.WithTimeout(ctx, sourceCheckTimeout)
		_, err := s.client.Status(probe)
		cancel()
		if err == nil {
			m.current = s
			break
		}
	}
	return m.current
}

// sourceName names the server of the last read.
func (m *Manager) sourceName() string {
	m.readMu.Lock()
	defer m.readMu.Unlock()
	if m.current.client == nil {
		return m.serverName
	}
	return m.current.name
}

func (m *Manager) servers() []server {
	return append([]server{{m.serverName, m.kea}}, m.peers...)
}

// Difference is one entry a peer holds differently from the primary.
type Difference struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
	// Change is missing, extra or different, seen from the peer.
	Change string `json:"change"`
	// Manual marks what a sync cannot copy.
	Manual bool `json:"manual,omitempty"`
	apply  func(context.Context, *kea.Client) error
}

// ServerView is the state of one server compared with the primary.
type ServerView struct {
	Name        string       `json:"name"`
	Role        string       `json:"role"`
	Version     string       `json:"version,omitempty"`
	Reachable   bool         `json:"reachable"`
	Error       string       `json:"error,omitempty"`
	HA          *ServerHA    `json:"ha,omitempty"`
	Differences []Difference `json:"differences"`
}

// ServerList is the answer of Servers.
type ServerList struct {
	Servers []ServerView `json:"servers"`
}

// ServerRow is one line of the server overview on the dashboard.
type ServerRow struct {
	Name         string   `json:"name"`
	Role         string   `json:"role"`
	Reachable    bool     `json:"reachable"`
	Error        string   `json:"error,omitempty"`
	Version      string   `json:"version,omitempty"`
	Uptime       int64    `json:"uptime,omitempty"`
	HAState      string   `json:"haState,omitempty"`
	Severity     Severity `json:"severity"`
	ActiveLeases *int     `json:"activeLeases"`
	// Differences counts what differs from the primary; nil if unknown.
	Differences *int `json:"differences"`
}

func (m *Manager) serverRow(ctx context.Context, s server, role string) ServerRow {
	ctx, cancel := context.WithTimeout(ctx, dashboardServerTimeout)
	defer cancel()
	row := ServerRow{Name: s.name, Role: role, Severity: SeverityOK}
	status, err := s.client.Status(ctx)
	if err != nil {
		row.Error, row.Severity = err.Error(), SeverityCrit
		return row
	}
	row.Reachable, row.Uptime = true, status.Uptime
	if len(status.HA) > 0 {
		local := status.HA[0].Servers.Local
		row.HAState, row.Severity = local.State, HAStateSeverity(local.State)
	}
	if row.Version, err = s.client.Version(ctx); err != nil {
		row.Severity = SeverityWarn
	}
	if leases, err := s.client.Leases(ctx); err == nil {
		now := m.now()
		active := 0
		for _, l := range leases {
			if m.leaseView(l, now).Active {
				active++
			}
		}
		row.ActiveLeases = &active
	}
	if role == "peer" {
		if want, err := takeSnapshot(ctx, m.kea); err == nil {
			if have, err := takeSnapshot(ctx, s.client); err == nil {
				n := len(compare(m.kea, want, have))
				row.Differences = &n
			}
		}
	}
	return row
}

// startServerRows asks all servers at once, because a partner that is down must
// not slow the dashboard by more than its timeout. The answer returns the rows.
func (m *Manager) startServerRows(ctx context.Context) func() []ServerRow {
	if !m.HasPeers() {
		return func() []ServerRow { return []ServerRow{} }
	}
	servers := m.servers()
	rows := make([]ServerRow, len(servers))
	var wg sync.WaitGroup
	for i, s := range servers {
		role := "peer"
		if i == 0 {
			role = "primary"
		}
		wg.Go(func() { rows[i] = m.serverRow(ctx, s, role) })
	}
	return func() []ServerRow {
		wg.Wait()
		return rows
	}
}

func rowWarnings(rows []ServerRow) []Warning {
	out := []Warning{}
	for _, r := range rows {
		switch {
		case r.Role == "primary":
		case !r.Reachable:
			out = append(out, Warning{SeverityCrit, "Server " + r.Name + " is unreachable: " + r.Error})
		case r.Differences != nil && *r.Differences > 0:
			out = append(out, Warning{SeverityWarn, fmt.Sprintf("Server %s differs from the primary in %d entries", r.Name, *r.Differences)})
		}
	}
	return out
}

// snapshot is the part of a configuration all servers must agree on.
type snapshot struct {
	hosts   map[string]kea.Host
	classes map[string]map[string]any
	subnets map[string]map[string]any
}

func takeSnapshot(ctx context.Context, k *kea.Client) (*snapshot, error) {
	config, err := k.ConfigGet(ctx)
	if err != nil {
		return nil, err
	}
	subnets, err := kea.ParseSubnets(config)
	if err != nil {
		return nil, err
	}
	hosts, err := hostsOn(ctx, k, subnets)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Dhcp4 struct {
			Classes []map[string]any `json:"client-classes"`
			Subnets []map[string]any `json:"subnet4"`
		} `json:"Dhcp4"`
	}
	if err := json.Unmarshal(config, &doc); err != nil {
		return nil, fmt.Errorf("cannot read configuration: %w", err)
	}
	s := &snapshot{map[string]kea.Host{}, map[string]map[string]any{}, map[string]map[string]any{}}
	for _, h := range hosts {
		idType, id := h.Identifier()
		s.hosts[target(h.SubnetID(), idType, strings.ToLower(id))] = h
	}
	for _, c := range doc.Dhcp4.Classes {
		s.classes[fmt.Sprint(c["name"])] = c
	}
	for _, n := range doc.Dhcp4.Subnets {
		s.subnets[fmt.Sprint(n["subnet"])] = n
	}
	return s, nil
}

func sameJSON(a, b any) bool {
	x, errX := json.Marshal(a)
	y, errY := json.Marshal(b)
	return errX == nil && errY == nil && string(x) == string(y)
}

func withoutReservations(subnet map[string]any) map[string]any {
	out := maps.Clone(subnet)
	delete(out, "reservations")
	return out
}

func hasReservations(subnet map[string]any) bool {
	list, _ := subnet["reservations"].([]any)
	return len(list) > 0
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

// compare lists what has to happen on a peer, in an order that works: classes
// before the reservations that use them, and reservations leave before others
// take over their address.
func compare(primary *kea.Client, want, have *snapshot) []Difference {
	out := []Difference{}
	var late []Difference
	for _, name := range sortedKeys(want.classes) {
		update := func(ctx context.Context, k *kea.Client) error {
			entry, err := primary.ClassGet(ctx, name)
			if err != nil {
				return err
			}
			if entry == nil {
				return fmt.Errorf("class %s is missing on the primary", name)
			}
			return k.ClassUpdate(ctx, entry)
		}
		if _, ok := have.classes[name]; !ok {
			out = append(out, Difference{Kind: "class", Target: name, Change: "missing", apply: func(ctx context.Context, k *kea.Client) error {
				entry, err := primary.ClassGet(ctx, name)
				if err != nil {
					return err
				}
				if entry == nil {
					return fmt.Errorf("class %s is missing on the primary", name)
				}
				return k.ClassAdd(ctx, entry)
			}})
		} else if !sameJSON(want.classes[name], have.classes[name]) {
			out = append(out, Difference{Kind: "class", Target: name, Change: "different", apply: update})
		}
	}
	for _, name := range sortedKeys(have.classes) {
		if _, ok := want.classes[name]; !ok {
			late = append(late, Difference{Kind: "class", Target: name, Change: "extra", apply: func(ctx context.Context, k *kea.Client) error {
				return k.ClassDel(ctx, name)
			}})
		}
	}
	for _, name := range sortedKeys(want.subnets) {
		entry := want.subnets[name]
		other, ok := have.subnets[name]
		switch {
		case !ok:
			out = append(out, Difference{Kind: "subnet", Target: name, Change: "missing", Manual: true})
		case sameJSON(withoutReservations(entry), withoutReservations(other)):
		case hasReservations(entry) || hasReservations(other) || fmt.Sprint(entry["id"]) != fmt.Sprint(other["id"]):
			// subnet4-update could drop the reservations of a subnet
			out = append(out, Difference{Kind: "subnet", Target: name, Change: "different", Manual: true})
		default:
			id, err := strconv.ParseInt(fmt.Sprint(entry["id"]), 10, 64)
			out = append(out, Difference{Kind: "subnet", Target: name, Change: "different", apply: func(ctx context.Context, k *kea.Client) error {
				if err != nil {
					return err
				}
				raw, err := primary.SubnetGet(ctx, id)
				if err != nil {
					return err
				}
				if raw == nil {
					return fmt.Errorf("subnet %s is missing on the primary", name)
				}
				prepareSubnetUpdate(raw)
				return k.SubnetUpdate(ctx, raw)
			}})
		}
	}
	for _, name := range sortedKeys(have.subnets) {
		if _, ok := want.subnets[name]; !ok {
			out = append(out, Difference{Kind: "subnet", Target: name, Change: "extra", Manual: true})
		}
	}
	for _, key := range sortedKeys(have.hosts) {
		if _, ok := want.hosts[key]; !ok {
			h := have.hosts[key]
			idType, id := h.Identifier()
			out = append(out, Difference{Kind: "reservation", Target: key, Change: "extra", apply: func(ctx context.Context, k *kea.Client) error {
				return k.DeleteReservation(ctx, h.SubnetID(), idType, id)
			}})
		}
	}
	for _, key := range sortedKeys(want.hosts) {
		h := want.hosts[key]
		if other, ok := have.hosts[key]; !ok {
			out = append(out, Difference{Kind: "reservation", Target: key, Change: "missing", apply: func(ctx context.Context, k *kea.Client) error {
				return k.AddReservation(ctx, h)
			}})
		} else if !sameJSON(h, other) {
			out = append(out, Difference{Kind: "reservation", Target: key, Change: "different", apply: func(ctx context.Context, k *kea.Client) error {
				return k.UpdateReservation(ctx, h)
			}})
		}
	}
	return append(out, late...)
}

// Servers compares every peer with the primary.
func (m *Manager) Servers(ctx context.Context) *ServerList {
	list := &ServerList{Servers: []ServerView{}}
	var want *snapshot
	ha := m.haStates(ctx)
	for i, s := range m.servers() {
		ctx, cancel := context.WithTimeout(ctx, serverCheckTimeout)
		defer cancel()
		v := ServerView{Name: s.name, Role: "peer", Differences: []Difference{}}
		have, err := takeSnapshot(ctx, s.client)
		if err == nil {
			v.Version, err = s.client.Version(ctx)
		}
		v.Reachable = err == nil
		if err != nil {
			v.Error = err.Error()
		}
		v.HA = ha[i]
		switch {
		case i == 0:
			v.Role = "primary"
			want = have
		case err == nil && want != nil:
			v.Differences = compare(m.kea, want, have)
		}
		list.Servers = append(list.Servers, v)
	}
	return list
}

// SyncServer makes a peer match the primary. The primary is the only source: what
// a peer holds on its own is removed.
func (m *Manager) SyncServer(ctx context.Context, user, name string) (*Result, error) {
	ctx, cancel := detached(ctx)
	defer cancel()
	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	i := slices.IndexFunc(m.peers, func(s server) bool { return s.name == name })
	if i < 0 {
		return nil, ErrNotFound
	}
	peer := m.peers[i]
	want, err := takeSnapshot(ctx, m.kea)
	if err != nil {
		return nil, err
	}
	have, err := takeSnapshot(ctx, peer.client)
	if err != nil {
		return nil, err
	}
	var todo []Difference
	warnings := []string{}
	for _, d := range compare(m.kea, want, have) {
		if d.Manual {
			warnings = append(warnings, fmt.Sprintf("%s %s is %s and has to be fixed in the configuration file", d.Kind, d.Target, d.Change))
		} else {
			todo = append(todo, d)
		}
	}
	if len(todo) == 0 {
		return &Result{Status: StatusOK, Message: "Nothing to copy.", Steps: []Step{}, Warnings: warnings}, nil
	}
	return m.runOn(ctx, []server{peer}, change{
		user:         user,
		action:       "server-sync",
		target:       peer.name,
		backupReason: "server-sync",
		warnings:     warnings,
		mutate: func(ctx context.Context, k *kea.Client) error {
			for _, d := range todo {
				if err := d.apply(ctx, k); err != nil {
					err = fmt.Errorf("%s %s: %w", d.Kind, d.Target, err)
					return &halfDoneError{err, "Sync stopped halfway at " + err.Error() + ". The server now holds a mix of both states; look at the differences and sync again."}
				}
			}
			return nil
		},
		verify: func(ctx context.Context, k *kea.Client) error {
			after, err := takeSnapshot(ctx, k)
			if err != nil {
				return err
			}
			left := slices.DeleteFunc(compare(m.kea, want, after), func(d Difference) bool { return d.Manual })
			if len(left) > 0 {
				return errors.New(left[0].Kind + " " + left[0].Target + " still differs")
			}
			return nil
		},
	}), nil
}
