package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/vgropp/kea-roost/internal/kea"
	"github.com/vgropp/kea-roost/internal/oui"
)

// The monitor resolves names while it answers, so it gets more time than a
// plain API call.
const arpMonitorTimeout = 30 * time.Second

const maxARPMonitorResponse = 16 << 20

// ErrARPMonitorDisabled is returned when no ARP monitor is configured.
var ErrARPMonitorDisabled = errors.New("ARP monitor is not configured")

// arpDevice is an entry of the /api/current answer of arpmonitor.
type arpDevice struct {
	MAC      string   `json:"mac"`
	IPv4     []string `json:"ipv4"`
	Hostname string   `json:"hostname"`
	// FirstSeen is missing in older monitor versions.
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// SetARPMonitor enables the devices view.
func (m *Manager) SetARPMonitor(url string) {
	m.arpURL = strings.TrimRight(url, "/")
}

// ARPMonitorEnabled reports whether an ARP monitor is configured.
func (m *Manager) ARPMonitorEnabled() bool { return m.arpURL != "" }

func (m *Manager) arpDevices(ctx context.Context) ([]arpDevice, error) {
	return m.arpDevicesWithin(ctx, m.cfg().DevicesDays)
}

func (m *Manager) arpDevicesWithin(ctx context.Context, days int) ([]arpDevice, error) {
	ctx, cancel := context.WithTimeout(ctx, arpMonitorTimeout)
	defer cancel()
	url := fmt.Sprintf("%s/api/current?days=%d", m.arpURL, days)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ARP monitor unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ARP monitor answered with HTTP %d", resp.StatusCode)
	}
	var devices []arpDevice
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxARPMonitorResponse)).Decode(&devices); err != nil {
		return nil, fmt.Errorf("invalid answer of the ARP monitor: %w", err)
	}
	return devices, nil
}

// DeviceView is a device seen on the network.
type DeviceView struct {
	IPAddress string `json:"ipAddress"`
	HWAddress string `json:"hwAddress"`
	Vendor    string `json:"vendor,omitempty"`
	Hostname  string `json:"hostname"`
	SubnetID  int64  `json:"subnetId"`
	Subnet    string `json:"subnet"`
	InPool    bool   `json:"inPool"`
	// Reservation is set when the MAC or the address is reserved.
	Reservation *ReservationRef `json:"reservation,omitempty"`
	Wifi        *WifiClient     `json:"wifi,omitempty"`
	FirstSeen   time.Time       `json:"firstSeen,omitzero"`
	LastSeen    time.Time       `json:"lastSeen"`
	Lease       *LeaseView      `json:"lease"`
}

// DeviceList is the answer of Devices.
type DeviceList struct {
	Devices  []DeviceView `json:"devices"`
	Warnings []Warning    `json:"warnings"`
}

// deviceAddress picks the newest address of a device inside a configured
// subnet; the monitor lists addresses newest first.
func deviceAddress(d arpDevice, subnets []kea.Subnet) (netip.Addr, *kea.Subnet) {
	for _, text := range d.IPv4 {
		ip, err := netip.ParseAddr(text)
		if err != nil {
			continue
		}
		if s := kea.SubnetFor(subnets, kea.Host{"subnet-id": int64(0), "ip-address": ip.String()}); s != nil {
			return ip, s
		}
	}
	return netip.Addr{}, nil
}

// mergeWifiClients adds what the access points know. A connected client counts as
// seen now, and one the ARP monitor never heard of gets its own entry.
func mergeWifiClients(devices []arpDevice, wifi map[string]apClient) []arpDevice {
	known := make(map[string]bool, len(devices))
	for i := range devices {
		mac := strings.ToLower(devices[i].MAC)
		known[mac] = true
		if c, ok := wifi[mac]; ok {
			if c.LastSeen.After(devices[i].LastSeen) {
				devices[i].LastSeen = c.LastSeen
			}
			if devices[i].Hostname == "" {
				devices[i].Hostname = c.DeviceInfo.Name
			}
		}
	}
	for _, mac := range slices.Sorted(maps.Keys(wifi)) {
		if c := wifi[mac]; !known[mac] {
			devices = append(devices, arpDevice{MAC: mac, IPv4: c.DeviceInfo.IPAddresses, Hostname: c.DeviceInfo.Name, LastSeen: c.LastSeen})
		}
	}
	return devices
}

// Devices lists the devices the ARP monitor saw or the access points serve inside
// a configured subnet, linked to their reservation if they have one.
func (m *Manager) Devices(ctx context.Context) (*DeviceList, error) {
	if !m.DevicesEnabled() {
		return nil, ErrARPMonitorDisabled
	}
	var devices []arpDevice
	var err error
	if m.ARPMonitorEnabled() {
		if devices, err = m.arpDevices(ctx); err != nil {
			return nil, err
		}
	}
	subnets, err := m.subnets(ctx)
	if err != nil {
		return nil, err
	}
	hosts, err := m.allHosts(ctx, subnets)
	if err != nil {
		return nil, err
	}
	byIP, byMAC := indexHosts(hosts)

	list := &DeviceList{Devices: []DeviceView{}, Warnings: []Warning{}}
	leases, err := m.Leases(ctx)
	if err != nil {
		list.Warnings = append(list.Warnings, Warning{SeverityWarn, "Lease data unavailable: " + err.Error()})
	}
	leaseByIP := make(map[string]LeaseView, len(leases))
	for _, l := range leases {
		leaseByIP[l.IPAddress] = l
	}

	wifi, err := m.wifiClients(ctx)
	if err != nil {
		list.Warnings = append(list.Warnings, Warning{SeverityWarn, err.Error()})
	}
	devices = mergeWifiClients(devices, wifi)

	for _, d := range devices {
		mac := strings.ToLower(d.MAC)
		ip, subnet := deviceAddress(d, subnets)
		if subnet == nil {
			continue
		}
		host, reserved := byMAC[mac]
		if !reserved {
			host, reserved = byIP[ip.String()]
		}
		v := DeviceView{
			IPAddress: ip.String(),
			HWAddress: mac,
			Vendor:    oui.Lookup(mac),
			Hostname:  strings.TrimSuffix(d.Hostname, "."),
			SubnetID:  subnet.ID,
			Subnet:    subnet.Subnet,
			InPool:    subnet.PoolContaining(ip) != nil,
			FirstSeen: d.FirstSeen,
			LastSeen:  d.LastSeen,
		}
		if l, ok := leaseByIP[v.IPAddress]; ok {
			v.Lease = &l
		}
		if c, ok := wifi[mac]; ok {
			v.Wifi = c.view()
		}
		if reserved {
			v.Reservation = reservationRef(host, v.IPAddress, mac)
			// the monitor keeps older sightings, and an address may have changed hands
			if m.now().Sub(d.LastSeen) > m.cfg().online() {
				v.Reservation.Conflict = ""
			}
			if v.Reservation.Conflict != "" {
				list.Warnings = append(list.Warnings, Warning{SeverityCrit, v.Reservation.Conflict})
			}
		}
		list.Devices = append(list.Devices, v)
	}
	return list, nil
}
