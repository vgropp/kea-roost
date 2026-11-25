package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vgropp/kea-roost/internal/kea"
)

// Kea has no command for the global part of its configuration. Kea Roost
// changes it with config-set, which replaces the whole configuration: Kea
// reconfigures completely, reloads its hooks and answers no DHCP request for a
// moment, and an HA pair synchronizes its leases again. Tested with Kea 3.0.3 as
// a hot-standby pair: leases, reservations and the HA listener stay, and an
// invalid configuration is refused with the old one kept.

const (
	// A change reloads every server of a family and waits for each to settle,
	// which takes longer than the other changes.
	globalWriteTimeout = 3 * time.Minute
	// Limits of the form; Kea checks the values themselves.
	maxGlobalOptions = 64
	maxOptionText    = 1024
)

// haSettlePoll is how often the HA state is asked for while it settles, and
// haSettleTimeout how long a server may take to return to a normal HA state.
var (
	haSettlePoll    = time.Second
	haSettleTimeout = 45 * time.Second
)

// GlobalOptions are the DHCP options set for every client of a server.
type GlobalOptions struct {
	Options []ReservationOption `json:"options"`
	// Blockers say why the options cannot be changed; empty means they can.
	Blockers []string `json:"blockers"`
	// Notes are shown before saving, such as servers that skip config-test.
	Notes []string `json:"notes"`
	// Suggestions are option names that often belong to the global level.
	Suggestions []string `json:"suggestions"`
}

// Options often set for a whole home or small office network. Any other option
// Kea knows can be entered as well.
var (
	globalSuggestions4 = []string{"domain-name-servers", "domain-name", "domain-search", "ntp-servers", "time-offset", "interface-mtu", "routers", "tftp-server-name", "boot-file-name"}
	globalSuggestions6 = []string{"dns-servers", "domain-search", "sntp-servers", "information-refresh-time"}
)

// configTestSafe reports whether a Kea release may run config-test. Before 3.0.4
// an HA peer did not open its own listener again after config-test, so its
// partner took over (ISC #4145, #4616).
func configTestSafe(version string) bool {
	var major, minor, patch int
	if n, _ := fmt.Sscanf(strings.TrimSpace(version), "%d.%d.%d", &major, &minor, &patch); n < 3 {
		return false
	}
	return major > 3 || major == 3 && (minor > 0 || patch >= 4)
}

// skippedConfigTest names the servers that get no config-test, with the reason.
func (m *Manager) skippedConfigTest(ctx context.Context, servers []server, v6 bool) (map[*kea.Client]bool, []string) {
	skip := map[*kea.Client]bool{}
	var notes []string
	for _, s := range servers {
		version, err := s.client.Version(ctx)
		if err == nil && configTestSafe(version) {
			continue
		}
		skip[s.client] = true
		if err != nil || version == "" {
			version = "of an unknown version"
		}
		notes = append(notes, fmt.Sprintf("%s runs Kea %s: config-test is skipped there, because before Kea 3.0.4 it left the HA listener closed. config-set checks the configuration itself and keeps the old one if the new one is invalid.", label(s.name, v6), version))
	}
	return skip, notes
}

// configSection decodes the Dhcp4 or Dhcp6 part of a config-get answer and keeps
// numbers as they are, so that a configuration sent back is unchanged.
func configSection(config json.RawMessage, v6 bool) (map[string]any, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(config, &doc); err != nil {
		return nil, fmt.Errorf("cannot read configuration: %w", err)
	}
	raw := doc[sectionName(v6)]
	if raw == nil {
		return nil, fmt.Errorf("configuration has no %s section", sectionName(v6))
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var section map[string]any
	if err := dec.Decode(&section); err != nil {
		return nil, fmt.Errorf("cannot read configuration: %w", err)
	}
	return section, nil
}

func sectionName(v6 bool) string {
	if v6 {
		return "Dhcp6"
	}
	return "Dhcp4"
}

// globalOptions reads the option-data of a configuration section.
func globalOptions(section map[string]any) []ReservationOption {
	out := []ReservationOption{}
	list, _ := section["option-data"].([]any)
	for _, item := range list {
		o, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := o["name"].(string)
		data, _ := o["data"].(string)
		code, _ := number(o["code"])
		out = append(out, ReservationOption{Name: name, Code: code, Data: data})
	}
	return out
}

// globalsUnchanged tells whether every server already has exactly these global
// options: the same names, codes and values, compared strictly. When in doubt it
// says no, which costs a reload but never skips a write.
func (m *Manager) globalsUnchanged(ctx context.Context, servers []server, v6 bool, in []ReservationOption) bool {
	key := func(o ReservationOption) string {
		if o.Name != "" {
			return o.Name
		}
		return fmt.Sprint(o.Code)
	}
	want := map[string]ReservationOption{}
	for _, o := range in {
		want[key(o)] = o
	}
	for _, s := range servers {
		config, err := s.client.ConfigGet(ctx)
		if err != nil {
			return false
		}
		section, err := configSection(config, v6)
		if err != nil {
			return false
		}
		have := globalOptions(section)
		if len(have) != len(want) {
			return false
		}
		seen := map[string]bool{}
		for _, o := range have {
			w, ok := want[key(o)]
			if !ok || seen[key(o)] || strings.TrimSpace(w.Data) != o.Data || (w.Code != 0 && o.Code != 0 && w.Code != o.Code) {
				return false
			}
			seen[key(o)] = true
		}
	}
	return true
}

func (m *Manager) globalBlockers(ctx context.Context, servers []server, v6 bool) []string {
	blockers := []string{}
	for _, s := range servers {
		// each server reloads in turn, which an HA pair only rides out from a steady state
		status, err := s.client.Status(ctx)
		if err != nil {
			blockers = append(blockers, fmt.Sprintf("%s does not answer: %v", label(s.name, v6), err))
			continue
		}
		if state := unsettledHA(status); state != "" {
			blockers = append(blockers, fmt.Sprintf("HA of %s is in the state %s. Wait until it is back to normal.", label(s.name, v6), state))
		}
		config, err := s.client.ConfigGet(ctx)
		if err != nil {
			blockers = append(blockers, fmt.Sprintf("%s does not answer: %v", label(s.name, v6), err))
			continue
		}
		section, err := configSection(config, v6)
		if err != nil {
			blockers = append(blockers, fmt.Sprintf("%s: %v", label(s.name, v6), err))
			continue
		}
		// the form names options by name or code only, which would mix up spaces
		list, _ := section["option-data"].([]any)
		for _, item := range list {
			o, _ := item.(map[string]any)
			if space, _ := o["space"].(string); space != "" && space != "dhcp4" && space != "dhcp6" {
				blockers = append(blockers, fmt.Sprintf("%s has the global option %v in the space %s, which this form cannot tell apart. Change it in the configuration file.", label(s.name, v6), o["name"], space))
			}
		}
	}
	return blockers
}

// GlobalOptions returns the global options of the primary of a family and what
// keeps them from being changed.
func (m *Manager) GlobalOptions(ctx context.Context, v6 bool) (*GlobalOptions, error) {
	servers := m.serversOf(v6)
	if len(servers) == 0 {
		return nil, ErrNotFound
	}
	config, err := servers[0].client.ConfigGet(ctx)
	if err != nil {
		return nil, err
	}
	section, err := configSection(config, v6)
	if err != nil {
		return nil, err
	}
	_, notes := m.skippedConfigTest(ctx, servers, v6)
	suggestions := globalSuggestions4
	if v6 {
		suggestions = globalSuggestions6
	}
	return &GlobalOptions{Options: globalOptions(section), Blockers: m.globalBlockers(ctx, servers, v6), Notes: append([]string{}, notes...), Suggestions: suggestions}, nil
}

func checkGlobalOptions(in []ReservationOption) []string {
	var problems []string
	if len(in) > maxGlobalOptions {
		problems = append(problems, fmt.Sprintf("at most %d global options", maxGlobalOptions))
	}
	seen := map[string]bool{}
	for _, o := range in {
		key := strings.ToLower(strings.TrimSpace(o.Name))
		if key == "" && o.Code == 0 {
			problems = append(problems, "DHCP option without a name")
			continue
		}
		if key == "" {
			key = fmt.Sprint(o.Code)
		}
		if len(o.Name) > maxOptionText || len(o.Data) > maxOptionText || o.Code < 0 || o.Code > 65535 {
			problems = append(problems, fmt.Sprintf("option %.40s is too long or has an invalid code", key))
			continue
		}
		if seen[key] {
			problems = append(problems, fmt.Sprintf("option %s is listed twice", key))
		}
		seen[key] = true
	}
	return problems
}

// countHosts counts all reservations of a server, global and per subnet.
func countHosts(ctx context.Context, k *kea.Client, config json.RawMessage, v6 bool) (int, error) {
	var ids []int64
	if v6 {
		subnets, err := kea.ParseSubnets6(config)
		if err != nil {
			return 0, err
		}
		ids = subnetIDs6(subnets)
	} else {
		subnets, err := kea.ParseSubnets(config)
		if err != nil {
			return 0, err
		}
		for _, s := range subnets {
			ids = append(ids, s.ID)
		}
	}
	hosts, err := hostsIn(ctx, k, ids)
	return len(hosts), err
}

// UpdateGlobalOptions replaces the global options on every server of a family,
// primary first. Each server keeps the rest of its own configuration.
func (m *Manager) UpdateGlobalOptions(ctx context.Context, user string, v6 bool, in []ReservationOption) (*Result, error) {
	ctx, cancel := detachedFor(ctx, globalWriteTimeout)
	defer cancel()
	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	servers := m.serversOf(v6)
	if len(servers) == 0 {
		return nil, ErrNotFound
	}
	for i := range in {
		in[i].Name = strings.TrimSpace(in[i].Name)
	}
	problems := checkGlobalOptions(in)
	problems = append(problems, m.globalBlockers(ctx, servers, v6)...)
	if len(problems) > 0 {
		return nil, &ValidationError{Problems: problems}
	}
	if m.globalsUnchanged(ctx, servers, v6, in) {
		// a reload costs every server a few seconds of DHCP, so it needs a reason
		return &Result{Status: StatusOK, Message: "Nothing changed: every server already has these global options.", Steps: []Step{}, Warnings: []string{}}, nil
	}
	skip, notes := m.skippedConfigTest(ctx, servers, v6)
	names := make([]string, 0, len(in))
	for _, o := range in {
		names = append(names, o.Name)
	}
	// the reservations of the server before the change, per server in turn
	var kept int
	action := "config-set"
	if v6 {
		action = "config6-set"
	}
	return m.runOn(ctx, servers, change{
		user:         user,
		action:       action,
		target:       "global options: " + strings.Join(names, ", "),
		backupReason: "global-options",
		warnings:     append([]string{"Kea reloaded its whole configuration: each server answered no DHCP request for a moment, and an HA pair synchronized its leases again."}, notes...),
		mutate: func(ctx context.Context, k *kea.Client) error {
			config, err := k.ConfigGet(ctx)
			if err != nil {
				return err
			}
			if kept, err = countHosts(ctx, k, config, v6); err != nil {
				return fmt.Errorf("cannot count the reservations: %w", err)
			}
			var doc map[string]json.RawMessage
			if err := json.Unmarshal(config, &doc); err != nil {
				return err
			}
			section, err := configSection(config, v6)
			if err != nil {
				return err
			}
			previous, _ := section["option-data"].([]any)
			known := make([]map[string]any, 0, len(previous))
			for _, item := range previous {
				if o, ok := item.(map[string]any); ok {
					known = append(known, o)
				}
			}
			section["option-data"] = mergeOptions(known, in)
			if doc[sectionName(v6)], err = json.Marshal(section); err != nil {
				return err
			}
			// config-get answers with a hash of the configuration, which is no part of it
			delete(doc, "hash")
			next, err := json.Marshal(doc)
			if err != nil {
				return err
			}
			if !skip[k] {
				if err := k.ConfigTest(ctx, next); err != nil {
					return fmt.Errorf("config-test: %w", err)
				}
			}
			return k.ConfigSet(ctx, next)
		},
		verify: func(ctx context.Context, k *kea.Client) error {
			config, err := k.ConfigGet(ctx)
			if err != nil {
				return err
			}
			section, err := configSection(config, v6)
			if err != nil {
				return err
			}
			got := globalOptions(section)
			for _, want := range in {
				if !slices.ContainsFunc(got, func(o ReservationOption) bool {
					return (want.Name != "" && o.Name == want.Name || want.Code != 0 && o.Code == want.Code) &&
						slices.Equal(splitOption(o.Data), splitOption(want.Data))
				}) {
					return fmt.Errorf("option %s is not %q in Kea", want.Name, want.Data)
				}
			}
			if len(got) != len(in) {
				return fmt.Errorf("there are %d global options in Kea instead of %d", len(got), len(in))
			}
			after, err := countHosts(ctx, k, config, v6)
			if err != nil {
				return err
			}
			if after < kept {
				return fmt.Errorf("the server held %d reservations before and %d after the change", kept, after)
			}
			// the next server reloads only once this one serves normally again, so an
			// HA pair never has both partners reloading at once
			return waitForHA(ctx, k)
		},
	}), nil
}

// haSettled reports an HA state a server can reload from and should come back
// to. Partner-down counts as settled: with the partner away there is nothing to
// wait for.
func haSettled(state string) bool {
	return HAStateSeverity(state) == SeverityOK || state == "partner-down"
}

// unsettledHA returns the first HA state of a server that is not settled, or "".
func unsettledHA(status *kea.Status) string {
	for _, rel := range status.HA {
		if state := rel.Servers.Local.State; !haSettled(state) {
			return state
		}
	}
	return ""
}

// waitForHA waits until every HA relationship of a server is settled again after
// a reload. A server without HA returns at once, a terminated one at once with an
// error; a control socket that does not answer yet right after the reload is
// asked again.
func waitForHA(ctx context.Context, k *kea.Client) error {
	deadline := time.Now().Add(haSettleTimeout)
	for {
		status, err := k.Status(ctx)
		state := ""
		if err == nil {
			switch state = unsettledHA(status); state {
			case "":
				return nil
			case "terminated":
				return errors.New("HA terminated after the reload; the partners need a hand")
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("no answer for %s after the reload: %w", haSettleTimeout, err)
			}
			return fmt.Errorf("HA state is still %s, %s after the reload", state, haSettleTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(haSettlePoll):
		}
	}
}
