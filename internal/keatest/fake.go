// Package keatest provides an in-memory stand-in for the kea-dhcp4 HTTP control
// socket. It reproduces the documented quirks of the real API: answers wrapped
// in a list, reservation-get reporting a missing host with result 0,
// reservation-del reporting it with result 1, and reservation writes defaulting
// to the (absent) hosts database unless operation-target is "memory".
package keatest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Server is the fake. All fields are guarded by Lock; tests adjust them directly.
type Server struct {
	sync.Mutex

	Version      string
	Config       map[string]any
	HA           []any
	Stats        map[string]any
	Leases       []map[string]any
	Hosts        []map[string]any
	Persisted    []map[string]any // state of Hosts at the last config-write
	Calls        []string
	Fail         map[string]Failure
	SilentUpdate bool            // reservation-update reports success without doing anything
	Unknown      map[string]bool // commands answered with result 2
	User, Pass   string
}

// Failure makes one command misbehave.
type Failure struct {
	Result int
	Text   string
	Drop   bool // abort the connection without an answer
	After  bool // Drop only after the command took effect
}

// New returns a fake with one subnet and a hot-standby HA state.
func New() *Server {
	return &Server{
		Version: "3.0.3",
		Config: map[string]any{
			"valid-lifetime": 4000,
			"control-sockets": []any{map[string]any{
				"socket-type": "http",
				"authentication": map[string]any{
					"type":    "basic",
					"clients": []any{map[string]any{"user": "admin", "password": "s3cret"}},
				},
			}},
			"option-data": []any{map[string]any{"name": "domain-name-servers", "code": 6, "space": "dhcp4", "data": "192.0.2.53"}},
			"subnet4": []any{map[string]any{
				"id":     1,
				"subnet": "192.0.2.0/24",
				"pools":  []any{map[string]any{"pool": "192.0.2.100 - 192.0.2.199"}},
				"option-data": []any{map[string]any{
					"name": "routers", "code": 3, "space": "dhcp4", "data": "192.0.2.1",
				}},
			}},
		},
		HA: []any{map[string]any{
			"ha-mode": "hot-standby",
			"ha-servers": map[string]any{
				"local": map[string]any{
					"role": "primary", "scopes": []any{"server1"}, "server-name": "server1",
					"state": "hot-standby", "system-time": "2026-10-30 12:00:00",
				},
				"remote": map[string]any{
					"age": 3, "clock-skew": 0, "communication-interrupted": false, "in-touch": true,
					"last-scopes": []any{}, "last-state": "hot-standby", "role": "standby",
					"server-name": "server2", "system-time": "2026-10-30 12:00:00",
				},
			},
		}},
		Stats: map[string]any{
			"pkt4-received":                        [][]any{{10, "2026-10-30 12:00:00.000000"}, {9, "2026-10-30 11:00:00.000000"}},
			"subnet[1].total-addresses":            [][]any{{100, "2026-10-30 12:00:00.000000"}},
			"subnet[1].assigned-addresses":         [][]any{{42, "2026-10-30 12:00:00.000000"}},
			"subnet[1].pool[0].total-addresses":    [][]any{{100, "2026-10-30 12:00:00.000000"}},
			"subnet[1].pool[0].assigned-addresses": [][]any{{42, "2026-10-30 12:00:00.000000"}},
		},
		Fail:    map[string]Failure{},
		Unknown: map[string]bool{},
	}
}

// HasCall reports whether a command was received.
func (s *Server) HasCall(command string) bool {
	s.Lock()
	defer s.Unlock()
	return slices.Contains(s.Calls, command)
}

// ServeHTTP implements the control socket.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Lock()
	defer s.Unlock()

	if s.User != "" {
		if u, p, ok := r.BasicAuth(); !ok || u != s.User || p != s.Pass {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	var req struct {
		Command   string         `json:"command"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		reply(w, 1, "bad request", nil)
		return
	}
	s.Calls = append(s.Calls, req.Command)

	if s.Unknown[req.Command] {
		reply(w, 2, fmt.Sprintf("'%s' command not supported.", req.Command), nil)
		return
	}
	failure, failing := s.Fail[req.Command]
	if failing && !failure.After {
		if failure.Drop {
			panic(http.ErrAbortHandler)
		}
		reply(w, failure.Result, failure.Text, nil)
		return
	}
	result, text, args := s.handle(req.Command, req.Arguments)
	if failing && failure.After {
		panic(http.ErrAbortHandler)
	}
	reply(w, result, text, args)
}

func reply(w http.ResponseWriter, result int, text string, args any) {
	body := map[string]any{"result": result, "text": text}
	if args != nil {
		body["arguments"] = args
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode([]any{body})
}

func (s *Server) handle(command string, args map[string]any) (int, string, any) {
	switch command {
	case "list-commands":
		commands := []any{"config-get", "config-write", "lease4-get-all", "reservation-add", "class-get", "class-update"}
		if !s.Unknown["subnet4-update"] {
			commands = append(commands, "subnet4-get", "subnet4-update")
		}
		return 0, "", commands
	case "subnet4-get":
		subnets, _ := s.Config["subnet4"].([]any)
		for _, item := range subnets {
			if o := item.(map[string]any); fmt.Sprint(o["id"]) == fmt.Sprint(args["id"]) {
				return 0, "Info about IPv4 subnet returned", map[string]any{"subnet4": []any{o}}
			}
		}
		return 3, "No subnet found", nil
	case "subnet4-update":
		update := args["subnet4"].([]any)[0].(map[string]any)
		if _, ok := update["reservations"]; ok {
			return 1, "reservations are not allowed in subnet4-update", nil
		}
		subnets, _ := s.Config["subnet4"].([]any)
		for i, item := range subnets {
			if fmt.Sprint(item.(map[string]any)["id"]) == fmt.Sprint(update["id"]) {
				subnets[i] = update
				return 0, "IPv4 subnet updated", nil
			}
		}
		return 1, "No subnet found", nil
	case "class-get":
		classes, _ := s.Config["client-classes"].([]any)
		for _, item := range classes {
			if o := item.(map[string]any); o["name"] == args["name"] {
				return 0, "Class definition returned", map[string]any{"client-classes": []any{o}}
			}
		}
		return 3, "Class not found", nil
	case "class-add":
		classes, _ := s.Config["client-classes"].([]any)
		s.Config["client-classes"] = append(classes, args["client-classes"].([]any)[0])
		return 0, "Class added.", nil
	case "class-update":
		update := args["client-classes"].([]any)[0].(map[string]any)
		classes, _ := s.Config["client-classes"].([]any)
		for i, item := range classes {
			if item.(map[string]any)["name"] == update["name"] {
				classes[i] = update
				return 0, "Class updated.", nil
			}
		}
		return 3, "Class not found", nil
	case "class-del":
		classes, _ := s.Config["client-classes"].([]any)
		for i, item := range classes {
			if item.(map[string]any)["name"] == args["name"] {
				s.Config["client-classes"] = slices.Delete(classes, i, i+1)
				return 0, "Class deleted.", nil
			}
		}
		return 3, "Class not found", nil
	case "version-get":
		return 0, s.Version, nil
	case "status-get":
		return 0, "", map[string]any{
			"pid": 4711, "uptime": 86400, "reload": 3600,
			"high-availability": s.HA,
			"sockets":           map[string]any{"status": "ready"},
		}
	case "config-get":
		cfg := map[string]any{}
		for k, v := range s.Config {
			cfg[k] = v
		}
		return 0, "", map[string]any{"Dhcp4": cfg, "hash": "ABC"}
	case "config-write":
		s.Persisted = cloneHosts(s.Hosts)
		return 0, "Configuration written.", map[string]any{"filename": "/etc/kea/kea-dhcp4.conf", "size": 1}
	case "statistic-get-all":
		return 0, "", s.Stats
	case "lease4-get-all":
		if len(s.Leases) == 0 {
			return 3, "0 IPv4 lease(s) found.", map[string]any{"leases": []any{}}
		}
		return 0, fmt.Sprintf("%d IPv4 lease(s) found.", len(s.Leases)), map[string]any{"leases": s.Leases}
	case "lease4-del":
		for i, l := range s.Leases {
			if l["ip-address"] == args["ip-address"] {
				s.Leases = slices.Delete(s.Leases, i, i+1)
				return 0, "IPv4 lease deleted.", nil
			}
		}
		return 3, "IPv4 lease not found.", nil
	case "ha-heartbeat":
		return 0, "HA peer status returned.", map[string]any{
			"state": "hot-standby", "date-time": time.Now().UTC().Format(time.RFC1123),
			"scopes": []any{"server1"}, "unsent-update-count": 7,
		}
	case "reservation-get-all":
		hosts := []map[string]any{}
		for _, h := range s.Hosts {
			if fmt.Sprint(h["subnet-id"]) == fmt.Sprint(args["subnet-id"]) {
				hosts = append(hosts, h)
			}
		}
		if len(hosts) == 0 {
			return 3, "0 IPv4 host(s) found.", map[string]any{"hosts": hosts}
		}
		return 0, fmt.Sprintf("%d IPv4 host(s) found.", len(hosts)), map[string]any{"hosts": hosts}
	case "reservation-get":
		if i := s.find(args["subnet-id"], args["identifier-type"], args["identifier"]); i >= 0 {
			return 0, "Host found.", s.Hosts[i]
		}
		return 0, "Host not found.", nil
	case "reservation-add":
		if args["operation-target"] != "memory" {
			return 1, "Unable to add a host because there is no hosts-database configured.", nil
		}
		h := args["reservation"].(map[string]any)
		for _, other := range s.Hosts {
			if fmt.Sprint(other["subnet-id"]) == fmt.Sprint(h["subnet-id"]) && h["ip-address"] != nil && other["ip-address"] == h["ip-address"] {
				return 1, "Host already exists with this address.", nil
			}
		}
		s.Hosts = append(s.Hosts, h)
		return 0, "Host added.", nil
	case "reservation-update":
		if args["operation-target"] != "memory" {
			return 1, "Unable to update a host because there is no hosts-database configured.", nil
		}
		h := args["reservation"].(map[string]any)
		idType, id := identifier(h)
		i := s.find(h["subnet-id"], idType, id)
		if i < 0 {
			return 1, "Host not updated (not found).", nil
		}
		if !s.SilentUpdate {
			s.Hosts[i] = h
		}
		return 0, "Host updated.", nil
	case "reservation-del":
		if args["operation-target"] != "memory" {
			return 1, "Unable to delete a host because there is no hosts-database configured.", nil
		}
		i := s.find(args["subnet-id"], args["identifier-type"], args["identifier"])
		if i < 0 {
			return 1, "Host not deleted (not found).", nil
		}
		s.Hosts = slices.Delete(s.Hosts, i, i+1)
		return 0, "Host deleted.", nil
	}
	return 2, fmt.Sprintf("'%s' command not supported.", command), nil
}

func (s *Server) find(subnet, idType, id any) int {
	key, _ := idType.(string)
	return slices.IndexFunc(s.Hosts, func(h map[string]any) bool {
		return fmt.Sprint(h["subnet-id"]) == fmt.Sprint(subnet) && strings.EqualFold(fmt.Sprint(h[key]), fmt.Sprint(id))
	})
}

func identifier(h map[string]any) (string, any) {
	for _, t := range []string{"hw-address", "client-id", "duid", "circuit-id", "flex-id"} {
		if v, ok := h[t]; ok {
			return t, v
		}
	}
	return "", nil
}

func cloneHosts(in []map[string]any) []map[string]any {
	out := make([]map[string]any, len(in))
	copy(out, in)
	return out
}
