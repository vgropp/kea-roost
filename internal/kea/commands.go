package kea

import (
	"context"
	"encoding/json"
	"strings"
)

// Reservation writes must name the target: the default is the hosts database,
// but the reservations of this setup live in the JSON configuration.
const targetMemory = "memory"

// HAServerLocal is the "local" part of the high-availability status.
type HAServerLocal struct {
	Role       string   `json:"role"`
	Scopes     []string `json:"scopes"`
	ServerName string   `json:"server-name"`
	State      string   `json:"state"`
	SystemTime string   `json:"system-time"`
}

// HAServerRemote is the partner as seen by the queried server.
type HAServerRemote struct {
	Age                      *int64   `json:"age"`
	AnalyzedPackets          int64    `json:"analyzed-packets"`
	ClockSkew                *int64   `json:"clock-skew"`
	CommunicationInterrupted bool     `json:"communication-interrupted"`
	ConnectingClients        int64    `json:"connecting-clients"`
	InTouch                  bool     `json:"in-touch"`
	LastScopes               []string `json:"last-scopes"`
	LastState                string   `json:"last-state"`
	Role                     string   `json:"role"`
	ServerName               string   `json:"server-name"`
	SystemTime               string   `json:"system-time"`
	UnackedClients           int64    `json:"unacked-clients"`
	UnackedClientsLeft       int64    `json:"unacked-clients-left"`
}

// HARelationship is one element of the high-availability status list.
type HARelationship struct {
	Mode    string `json:"ha-mode"`
	Servers struct {
		Local  HAServerLocal   `json:"local"`
		Remote *HAServerRemote `json:"remote"`
	} `json:"ha-servers"`
}

// Status is the answer of status-get.
type Status struct {
	PID     int64            `json:"pid"`
	Uptime  int64            `json:"uptime"`
	Reload  int64            `json:"reload"`
	HA      []HARelationship `json:"high-availability"`
	Sockets struct {
		Status string   `json:"status"`
		Errors []string `json:"errors"`
	} `json:"sockets"`
}

// Status queries status-get.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	r, err := c.call(ctx, "status-get", nil)
	if err != nil {
		return nil, err
	}
	var s Status
	if err := decodeArguments(r, "status-get", &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Version queries version-get.
func (c *Client) Version(ctx context.Context) (string, error) {
	r, err := c.call(ctx, "version-get", nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(r.Text), nil
}

// ConfigGet returns the running configuration. It holds plaintext credentials
// and must never be passed on to the web interface unfiltered.
func (c *Client) ConfigGet(ctx context.Context) (json.RawMessage, error) {
	r, err := c.call(ctx, "config-get", nil)
	if err != nil {
		return nil, err
	}
	return r.Arguments, nil
}

// ConfigWrite persists the running configuration to the startup file.
func (c *Client) ConfigWrite(ctx context.Context) error {
	_, err := c.call(ctx, "config-write", nil)
	return err
}

// Lease is an entry of lease4-get-all.
type Lease struct {
	IPAddress string `json:"ip-address"`
	HWAddress string `json:"hw-address"`
	ClientID  string `json:"client-id"`
	Hostname  string `json:"hostname"`
	SubnetID  int64  `json:"subnet-id"`
	State     int    `json:"state"`
	CLTT      int64  `json:"cltt"`
	ValidLft  int64  `json:"valid-lft"`
}

// Leases returns all IPv4 leases.
func (c *Client) Leases(ctx context.Context) ([]Lease, error) {
	r, err := c.call(ctx, "lease4-get-all", nil, ResultEmpty)
	if err != nil {
		return nil, err
	}
	var out struct {
		Leases []Lease `json:"leases"`
	}
	if err := decodeArguments(r, "lease4-get-all", &out); err != nil {
		return nil, err
	}
	return out.Leases, nil
}

// DeleteLease removes a lease and reports whether it existed.
func (c *Client) DeleteLease(ctx context.Context, ip string) (bool, error) {
	r, err := c.call(ctx, "lease4-del", map[string]any{"ip-address": ip}, ResultEmpty)
	if err != nil {
		return false, err
	}
	return r.Result == ResultSuccess, nil
}

// Reservations returns the reservations of a subnet; subnet 0 holds the global ones.
func (c *Client) Reservations(ctx context.Context, subnetID int64) ([]Host, error) {
	r, err := c.call(ctx, "reservation-get-all", map[string]any{"subnet-id": subnetID}, ResultEmpty)
	if err != nil {
		return nil, err
	}
	var out struct {
		Hosts []Host `json:"hosts"`
	}
	if err := decodeArguments(r, "reservation-get-all", &out); err != nil {
		return nil, err
	}
	for _, h := range out.Hosts {
		h["subnet-id"] = subnetID
	}
	return out.Hosts, nil
}

// Reservation fetches one reservation. Kea answers an unknown host with result 0
// and no arguments, so a missing host is recognized by its empty body.
func (c *Client) Reservation(ctx context.Context, subnetID int64, identifierType, identifier string) (Host, error) {
	r, err := c.call(ctx, "reservation-get", map[string]any{
		"subnet-id":       subnetID,
		"identifier-type": identifierType,
		"identifier":      identifier,
	}, ResultEmpty)
	if err != nil {
		return nil, err
	}
	var h Host
	if err := decodeArguments(r, "reservation-get", &h); err != nil {
		return nil, err
	}
	if len(h) == 0 {
		return nil, nil
	}
	h["subnet-id"] = subnetID
	return h, nil
}

// AddReservation creates a reservation in the running configuration.
func (c *Client) AddReservation(ctx context.Context, h Host) error {
	_, err := c.call(ctx, "reservation-add", map[string]any{"reservation": h, "operation-target": targetMemory})
	return err
}

// UpdateReservation replaces a whole reservation in the running configuration.
func (c *Client) UpdateReservation(ctx context.Context, h Host) error {
	_, err := c.call(ctx, "reservation-update", map[string]any{"reservation": h, "operation-target": targetMemory})
	return err
}

// DeleteReservation removes a reservation from the running configuration.
func (c *Client) DeleteReservation(ctx context.Context, subnetID int64, identifierType, identifier string) error {
	_, err := c.call(ctx, "reservation-del", map[string]any{
		"subnet-id":        subnetID,
		"identifier-type":  identifierType,
		"identifier":       identifier,
		"operation-target": targetMemory,
	})
	return err
}

// Commands returns the commands the server offers.
func (c *Client) Commands(ctx context.Context) ([]string, error) {
	r, err := c.call(ctx, "list-commands", nil)
	if err != nil {
		return nil, err
	}
	var out []string
	if err := decodeArguments(r, "list-commands", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SubnetGet returns the complete entry of a subnet, or nil if it does not exist.
func (c *Client) SubnetGet(ctx context.Context, id int64) (map[string]any, error) {
	r, err := c.call(ctx, "subnet4-get", map[string]any{"id": id}, ResultEmpty)
	if err != nil {
		return nil, err
	}
	var out struct {
		Subnets []map[string]any `json:"subnet4"`
	}
	if err := decodeArguments(r, "subnet4-get", &out); err != nil {
		return nil, err
	}
	if len(out.Subnets) == 0 {
		return nil, nil
	}
	return out.Subnets[0], nil
}

// SubnetUpdate overwrites a whole subnet in the running configuration.
func (c *Client) SubnetUpdate(ctx context.Context, subnet map[string]any) error {
	_, err := c.call(ctx, "subnet4-update", map[string]any{"subnet4": []any{subnet}})
	return err
}

// ClassGet returns the complete entry of a class, or nil if it does not exist.
func (c *Client) ClassGet(ctx context.Context, name string) (map[string]any, error) {
	r, err := c.call(ctx, "class-get", map[string]any{"name": name}, ResultEmpty)
	if err != nil {
		return nil, err
	}
	var out struct {
		Classes []map[string]any `json:"client-classes"`
	}
	if err := decodeArguments(r, "class-get", &out); err != nil {
		return nil, err
	}
	if len(out.Classes) == 0 {
		return nil, nil
	}
	return out.Classes[0], nil
}

// ClassAdd adds a client class to the running configuration.
func (c *Client) ClassAdd(ctx context.Context, class map[string]any) error {
	_, err := c.call(ctx, "class-add", map[string]any{"client-classes": []any{class}})
	return err
}

// ClassUpdate overwrites a whole client class in the running configuration.
func (c *Client) ClassUpdate(ctx context.Context, class map[string]any) error {
	_, err := c.call(ctx, "class-update", map[string]any{"client-classes": []any{class}})
	return err
}

// ClassDel removes a client class from the running configuration.
func (c *Client) ClassDel(ctx context.Context, name string) error {
	_, err := c.call(ctx, "class-del", map[string]any{"name": name})
	return err
}

// Statistics returns the newest sample of every integer statistic.
func (c *Client) Statistics(ctx context.Context) (map[string]int64, error) {
	r, err := c.call(ctx, "statistic-get-all", map[string]any{})
	if err != nil {
		return nil, err
	}
	var raw map[string][][]any
	if err := decodeArguments(r, "statistic-get-all", &raw); err != nil {
		return nil, err
	}
	stats := make(map[string]int64, len(raw))
	for name, samples := range raw {
		if len(samples) == 0 || len(samples[0]) == 0 {
			continue
		}
		if n, ok := samples[0][0].(json.Number); ok {
			if v, err := n.Int64(); err == nil {
				stats[name] = v
			}
		}
	}
	return stats, nil
}

// Heartbeat is the answer of ha-heartbeat.
type Heartbeat struct {
	State         string   `json:"state"`
	DateTime      string   `json:"date-time"`
	UnsentUpdates int64    `json:"unsent-update-count"`
	Scopes        []string `json:"scopes"`
}

// HAHeartbeat queries the HA state of the relationship of a local server.
func (c *Client) HAHeartbeat(ctx context.Context, serverName string) (*Heartbeat, error) {
	r, err := c.call(ctx, "ha-heartbeat", map[string]any{"server-name": serverName})
	if err != nil {
		return nil, err
	}
	var h Heartbeat
	if err := decodeArguments(r, "ha-heartbeat", &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// HACommand sends a command that operates HA and returns the answer of Kea.
func (c *Client) HACommand(ctx context.Context, command string, args map[string]any) (string, error) {
	r, err := c.call(ctx, command, args)
	if err != nil {
		return "", err
	}
	return r.Text, nil
}
