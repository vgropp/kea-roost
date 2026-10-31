package kea

import (
	"encoding/json"
	"strconv"
)

// IdentifierTypes lists the host identifiers known to Kea for DHCPv4.
var IdentifierTypes = []string{"hw-address", "client-id", "duid", "circuit-id", "flex-id"}

// Host is a reservation as Kea sends it. It stays a generic map because
// reservation-update replaces the whole entry: every field the manager does not
// edit, such as client-classes or boot-file-name, has to survive the round trip.
type Host map[string]any

// Identifier returns the type and value of the identifier of the host.
func (h Host) Identifier() (string, string) {
	for _, t := range IdentifierTypes {
		if v := h.str(t); v != "" {
			return t, v
		}
	}
	return "", ""
}

// IP returns the reserved address. Kea reports 0.0.0.0 for hosts without one.
func (h Host) IP() string {
	if ip := h.str("ip-address"); ip != "0.0.0.0" {
		return ip
	}
	return ""
}

// Hostname returns the reserved hostname.
func (h Host) Hostname() string { return h.str("hostname") }

// SubnetID returns the subnet of the host.
func (h Host) SubnetID() int64 {
	switch v := h["subnet-id"].(type) {
	case int64:
		return v
	case json.Number:
		n, _ := v.Int64()
		return n
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

// Comment returns the description kept in the user context of the host.
func (h Host) Comment() string {
	if ctx, ok := h["user-context"].(map[string]any); ok {
		c, _ := ctx["comment"].(string)
		return c
	}
	return ""
}

// Classes returns the client classes of the host.
func (h Host) Classes() []string {
	list, _ := h["client-classes"].([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		if name, ok := item.(string); ok {
			out = append(out, name)
		}
	}
	return out
}

// Options returns the option-data list of the host.
func (h Host) Options() []map[string]any {
	list, _ := h["option-data"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func (h Host) str(key string) string {
	s, _ := h[key].(string)
	return s
}
