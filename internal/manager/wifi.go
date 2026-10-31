package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	apDashboardTimeout     = 3 * time.Second
	maxAPDashboardResponse = 16 << 20
)

// WifiClient says where a device is connected right now.
type WifiClient struct {
	AccessPoint   string `json:"accessPoint"`
	Interface     string `json:"interface"`
	Signal        int    `json:"signal"`
	ConnectedTime int64  `json:"connectedTime"`
	// Inactive is the time since the last packet in milliseconds.
	Inactive int64 `json:"inactive"`
}

// apClient is an entry of /api/clients of openwrt-ap-dashboard. Its top level name
// is the access point; the name of the client is in device_info.
type apClient struct {
	MAC           string    `json:"mac"`
	Name          string    `json:"name"`
	StationID     string    `json:"station_id"`
	Interface     string    `json:"iface"`
	Signal        int       `json:"signal"`
	ConnectedTime int64     `json:"connected_time"`
	Inactive      int64     `json:"inactive"`
	LastSeen      time.Time `json:"last_seen"`
	DeviceInfo    struct {
		IPAddresses []string `json:"ipaddrs"`
		Name        string   `json:"name"`
	} `json:"device_info"`
}

func (c apClient) view() *WifiClient {
	ap := c.Name
	if ap == "" {
		ap = c.StationID
	}
	return &WifiClient{AccessPoint: ap, Interface: c.Interface, Signal: c.Signal, ConnectedTime: c.ConnectedTime, Inactive: c.Inactive}
}

// SetAPDashboard adds the WiFi clients of an openwrt-ap-dashboard.
func (m *Manager) SetAPDashboard(url string) {
	m.apURL = strings.TrimRight(url, "/")
}

// DevicesEnabled reports whether a source for the devices view is configured.
func (m *Manager) DevicesEnabled() bool { return m.arpURL != "" || m.apURL != "" }

// wifiClients returns the connected clients by lowercase MAC. It only ever reads:
// the dashboard can also disconnect clients, which the manager never does.
func (m *Manager) wifiClients(ctx context.Context) (map[string]apClient, error) {
	if m.apURL == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, apDashboardTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.apURL+"/api/clients", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("AP dashboard unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("AP dashboard answered with HTTP %d", resp.StatusCode)
	}
	var list []apClient
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPDashboardResponse)).Decode(&list); err != nil {
		return nil, fmt.Errorf("invalid answer of the AP dashboard: %w", err)
	}
	clients := make(map[string]apClient, len(list))
	for _, c := range list {
		clients[strings.ToLower(c.MAC)] = c
	}
	return clients, nil
}
