package manager

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/vgropp/kea-roost/internal/kea"
)

// HA actions, named after what happens to the server they are shown for.
const (
	haMaintenance       = "maintenance"
	haMaintenanceCancel = "maintenance-cancel"
	haSync              = "sync"
	haReset             = "reset"
	haContinue          = "continue"
)

// ServerHA is the HA state of one server and what can be done with it.
type ServerHA struct {
	Name    string   `json:"name"`
	Role    string   `json:"role"`
	State   string   `json:"state"`
	Partner string   `json:"partner,omitempty"`
	Actions []string `json:"actions"`
	// partner is the configured server behind Partner, if there is one.
	partner *server
}

func (h *ServerHA) offer() {
	h.Actions = []string{}
	switch h.State {
	case "hot-standby", "load-balancing":
		if h.partner != nil {
			h.Actions = append(h.Actions, haMaintenance)
		}
		if h.Partner != "" {
			h.Actions = append(h.Actions, haSync)
		}
	case "in-maintenance":
		if h.partner != nil {
			h.Actions = append(h.Actions, haMaintenanceCancel)
		}
	case "partner-in-maintenance":
		h.Actions = append(h.Actions, haMaintenanceCancel)
	case "terminated":
		h.Actions = append(h.Actions, haReset)
		if h.Partner != "" {
			h.Actions = append(h.Actions, haSync)
		}
	default:
		h.Actions = append(h.Actions, haContinue)
	}
}

// haStates asks every server for its HA state. Entries stay nil for servers that
// do not answer or run without HA.
func (m *Manager) haStates(ctx context.Context) []*ServerHA {
	servers := m.servers()
	states := make([]*ServerHA, len(servers))
	for i, s := range servers {
		status, err := s.client.Status(ctx)
		if err != nil || len(status.HA) == 0 {
			continue
		}
		local := status.HA[0].Servers
		states[i] = &ServerHA{Name: local.Local.ServerName, Role: local.Local.Role, State: local.Local.State}
		if local.Remote != nil {
			states[i].Partner = local.Remote.ServerName
		}
	}
	for i, h := range states {
		if h == nil {
			continue
		}
		// KEA_PEERS may name a server differently than its HA configuration
		for j, other := range states {
			if j != i && (other != nil && other.Name == h.Partner || len(servers) == 2) {
				h.partner = &servers[j]
			}
		}
		h.offer()
	}
	return states
}

// RunHA runs an action for a server. Kea sends the maintenance commands to the
// server that keeps running, so they are addressed to the partner of the server
// the operator wants to take down.
func (m *Manager) RunHA(ctx context.Context, user, name, action string) (*Result, error) {
	ctx, cancel := detached(ctx)
	defer cancel()
	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	servers := m.servers()
	i := slices.IndexFunc(servers, func(s server) bool { return s.name == name })
	if i < 0 {
		return nil, ErrNotFound
	}
	h := m.haStates(ctx)[i]
	if h == nil {
		return nil, &ValidationError{Problems: []string{name + " reports no HA state"}}
	}
	if !slices.Contains(h.Actions, action) {
		return nil, &ValidationError{Problems: []string{fmt.Sprintf("%s is not possible while %s is in state %s", action, name, h.State)}}
	}

	target, command := servers[i], "ha-"+action
	var args map[string]any
	switch action {
	case haMaintenance:
		target, command = *h.partner, "ha-maintenance-start"
	case haMaintenanceCancel:
		if h.State == "in-maintenance" {
			target = *h.partner
		}
	case haSync:
		args = map[string]any{"server-name": h.Partner}
	}

	text, err := target.client.HACommand(ctx, command, args)
	res := &Result{Status: StatusOK, Message: text, Warnings: []string{}, Steps: []Step{{Name: target.name + ": " + command, OK: err == nil}}}
	if err != nil {
		res.Steps[0].Message = err.Error()
		res.Status, res.Message = StatusFailed, "Kea rejected the action: "+err.Error()
		var unreachable *kea.UnreachableError
		if errors.As(err, &unreachable) {
			res.Status, res.Message = StatusPartial, "Kea did not answer. It is unclear whether the action ran. Please check the state."
		}
	}
	return m.finish(change{user: user, action: command, target: name}, res), nil
}
