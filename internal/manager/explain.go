package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/vgropp/kea-roost/internal/kea"
	"github.com/vgropp/kea-roost/internal/oui"
)

// The audit log is searched for a client this far back.
const clientAuditScan = 2000

// clientAuditLimit caps the history shown for a client.
const clientAuditLimit = 20

// ExplainStep is one decision Kea takes for a client.
type ExplainStep struct {
	Title    string   `json:"title"`
	Detail   string   `json:"detail"`
	Severity Severity `json:"severity"`
}

// OptionSource is one place in the configuration that sets an option.
type OptionSource struct {
	Source string `json:"source"`
	Data   string `json:"data"`
}

// EffectiveOption is an option as the client receives it.
type EffectiveOption struct {
	Name   string `json:"name"`
	Data   string `json:"data"`
	Source string `json:"source"`
	// Overrides lists the weaker values this one replaces, strongest first.
	Overrides []OptionSource `json:"overrides"`
}

// Explanation says why a client gets what it gets.
type Explanation struct {
	Steps   []ExplainStep     `json:"steps"`
	Options []EffectiveOption `json:"options"`
	// Unknown lists what cannot be decided without the packet of the client.
	Unknown []string `json:"unknown"`
	// Static means the client has its address without Kea being involved, so the
	// options only say what DHCP would give it.
	Static bool `json:"static"`
}

// ClientReservation is the reservation of a client.
type ClientReservation struct {
	SubnetID       int64    `json:"subnetId"`
	IdentifierType string   `json:"identifierType"`
	Identifier     string   `json:"identifier"`
	IPAddress      string   `json:"ipAddress"`
	Hostname       string   `json:"hostname"`
	Classes        []string `json:"classes"`
	Comment        string   `json:"comment"`
}

// ClientDevice is what the network knows about a client.
type ClientDevice struct {
	Addresses []string `json:"addresses"`
	Hostname  string   `json:"hostname"`
	FirstSeen string   `json:"firstSeen,omitempty"`
	LastSeen  string   `json:"lastSeen"`
}

// ClientView brings together everything known about one client.
type ClientView struct {
	Name        string             `json:"name"`
	HWAddress   string             `json:"hwAddress"`
	Vendor      string             `json:"vendor,omitempty"`
	IPAddress   string             `json:"ipAddress"`
	Lease       *LeaseView         `json:"lease"`
	Reservation *ClientReservation `json:"reservation"`
	Device      *ClientDevice      `json:"device"`
	Wifi        *WifiClient        `json:"wifi"`
	Explanation *Explanation       `json:"explanation"`
	History     []AuditEntry       `json:"history"`
	Warnings    []string           `json:"warnings"`
}

// reservationFlags are the settings that decide whether reservations count at all.
type reservationFlags struct {
	Global   *bool `json:"reservations-global"`
	InSubnet *bool `json:"reservations-in-subnet"`
}

func parseFlags(config json.RawMessage) reservationFlags {
	var doc struct {
		Dhcp4 reservationFlags `json:"Dhcp4"`
	}
	if json.Unmarshal(config, &doc) != nil {
		return reservationFlags{}
	}
	return doc.Dhcp4
}

func (f reservationFlags) global() bool   { return f.Global != nil && *f.Global }
func (f reservationFlags) inSubnet() bool { return f.InSubnet == nil || *f.InSubnet }

func optionKey(o kea.Option) (key, name string) {
	if o.Name != "" {
		return o.Name, o.Name
	}
	text := strconv.FormatInt(o.Code, 10)
	return text, "option " + text
}

// effectiveOptions applies the layers from the weakest to the strongest.
func effectiveOptions(layers []kea.OptionLayer) []EffectiveOption {
	index := map[string]int{}
	out := []EffectiveOption{}
	for _, layer := range layers {
		for _, o := range layer.Options {
			key, name := optionKey(o)
			i, ok := index[key]
			if !ok {
				index[key] = len(out)
				out = append(out, EffectiveOption{Name: name, Data: o.Data, Source: layer.Source, Overrides: []OptionSource{}})
				continue
			}
			prev := out[i]
			prev.Overrides = slices.Insert(prev.Overrides, 0, OptionSource{prev.Source, prev.Data})
			prev.Data, prev.Source = o.Data, layer.Source
			out[i] = prev
		}
	}
	slices.SortFunc(out, func(a, b EffectiveOption) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func hostOptions(h kea.Host) []kea.Option {
	out := []kea.Option{}
	for _, o := range h.Options() {
		name, _ := o["name"].(string)
		data, _ := o["data"].(string)
		code, _ := strconv.ParseInt(fmt.Sprint(o["code"]), 10, 64)
		out = append(out, kea.Option{Name: name, Code: code, Data: data})
	}
	return out
}

type explainInput struct {
	config  json.RawMessage
	subnets []kea.Subnet
	classes []kea.Class
	host    kea.Host
	lease   *LeaseView
	// other is another client holding the reserved address.
	other string
	// seen is the newest address the network saw the client with.
	seen string
}

func pickSubnet(in explainInput) *kea.Subnet {
	if in.lease != nil {
		if i := slices.IndexFunc(in.subnets, func(s kea.Subnet) bool { return s.ID == in.lease.SubnetID }); i >= 0 {
			return &in.subnets[i]
		}
	}
	if in.host != nil {
		if s := kea.SubnetFor(in.subnets, in.host); s != nil {
			return s
		}
	}
	if in.seen != "" {
		if s := kea.SubnetFor(in.subnets, kea.Host{"subnet-id": int64(0), "ip-address": in.seen}); s != nil {
			return s
		}
	}
	if len(in.subnets) == 1 {
		return &in.subnets[0]
	}
	return nil
}

// addressWithoutKea explains a client that is on the network but has neither a
// lease nor a reserved address.
func addressWithoutKea(in explainInput, subnet *kea.Subnet) (Severity, string, bool) {
	ip, err := netip.ParseAddr(in.seen)
	if err != nil {
		return "", "", false
	}
	if subnet != nil {
		if p := subnet.PoolContaining(ip); p != nil {
			return SeverityWarn, fmt.Sprintf("Seen with %s without an active lease. It is set on the device or its lease ran out. The address lies inside the pool %s, so Kea can give it to another client.", in.seen, p.Text), false
		}
	}
	return SeverityInfo, fmt.Sprintf("%s is probably set on the device itself: it lies outside of the pools and Kea holds no lease for it.", in.seen), true
}

// explain retraces the decisions of Kea from the configuration. It says what it
// cannot know instead of guessing: class tests look at the packet of the client.
func explain(in explainInput) (*Explanation, error) {
	e := &Explanation{Steps: []ExplainStep{}, Options: []EffectiveOption{}, Unknown: []string{}}
	add := func(severity Severity, title, format string, args ...any) {
		e.Steps = append(e.Steps, ExplainStep{title, fmt.Sprintf(format, args...), severity})
	}
	flags := parseFlags(in.config)

	subnet := pickSubnet(in)
	if subnet == nil {
		add(SeverityWarn, "Subnet", "No subnet can be told without a lease or a reservation: Kea takes it from where the request arrives.")
	} else {
		add(SeverityOK, "Subnet", "%s", subnet.Subnet)
	}

	reservedIP := ""
	active := in.host != nil
	var classes []string
	switch {
	case in.host == nil:
		add(SeverityInfo, "Reservation", "None.")
	case in.host.SubnetID() == 0 && !flags.global():
		active = false
		add(SeverityCrit, "Reservation", "A global reservation exists, but reservations-global is off: Kea ignores it.")
	case in.host.SubnetID() != 0 && !flags.inSubnet():
		active = false
		add(SeverityCrit, "Reservation", "A subnet reservation exists, but reservations-in-subnet is off: Kea ignores it.")
	default:
		scope := "global reservation"
		if in.host.SubnetID() != 0 {
			scope = fmt.Sprintf("reservation in subnet %d", in.host.SubnetID())
		}
		reservedIP = in.host.IP()
		add(SeverityOK, "Reservation", "Found as %s.", scope)
		classes = in.host.Classes()
	}

	leased := ""
	if in.lease != nil && in.lease.Active {
		leased = in.lease.IPAddress
	}
	switch {
	case reservedIP != "" && in.other != "":
		add(SeverityCrit, "Address", "%s is reserved here, but %s holds it. This client cannot get it until that lease ends.", reservedIP, in.other)
	case reservedIP != "" && leased != "" && leased != reservedIP:
		add(SeverityWarn, "Address", "Holds %s, reserved is %s. It moves at its next renewal.", leased, reservedIP)
	case reservedIP != "":
		add(SeverityOK, "Address", "%s from the reservation.", reservedIP)
	case leased != "":
		add(SeverityOK, "Address", "%s from the dynamic pool.", leased)
	case in.seen != "":
		severity, text, static := addressWithoutKea(in, subnet)
		if text != "" {
			e.Static = static
			add(severity, "Address", "%s", text)
		}
	case subnet != nil && len(subnet.Pools) == 0:
		add(SeverityWarn, "Address", "The subnet has no pool, so a client without a reserved address gets none.")
	default:
		add(SeverityInfo, "Address", "No active lease. The next free address of the pool would be assigned.")
	}

	if reservedIP != "" && in.seen != "" && in.seen != reservedIP && leased == "" {
		add(SeverityWarn, "Network", "Seen with %s instead of the reserved %s, so it does not seem to use DHCP.", in.seen, reservedIP)
	}

	ip, _ := netip.ParseAddr(reservedIP)
	if !ip.IsValid() {
		ip, _ = netip.ParseAddr(leased)
	}
	if subnet != nil && ip.IsValid() && reservedIP != "" {
		if p := subnet.PoolContaining(ip); p != nil {
			add(SeverityInfo, "Pool", "The reserved address lies inside the pool %s. Kea keeps it for this client, but a dynamic client may hold it first.", p.Text)
		}
	}

	if len(classes) > 0 {
		add(SeverityOK, "Classes", "Assigned by the reservation: %s.", strings.Join(classes, ", "))
	} else {
		add(SeverityInfo, "Classes", "None assigned by a reservation.")
	}
	for _, c := range in.classes {
		if c.Test != "" && !slices.Contains(classes, c.Name) {
			e.Unknown = append(e.Unknown, fmt.Sprintf("Class %s applies if the client matches %s.", c.Name, c.Test))
		}
	}

	if subnet == nil {
		return e, nil
	}
	if subnet.ValidLifetime != nil {
		add(SeverityInfo, "Lifetime", "%s.", readableSeconds(*subnet.ValidLifetime))
	}
	layers, err := kea.OptionLayers(in.config, subnet.ID, ip, classes)
	if err != nil {
		return nil, err
	}
	if active {
		layers = append(layers, kea.OptionLayer{Source: "reservation", Options: hostOptions(in.host)})
	}
	e.Options = effectiveOptions(layers)
	return e, nil
}

func trimName(name string) string { return strings.TrimSuffix(name, ".") }

// Client gathers what is known about a MAC, an address or a hostname. Sources that
// fail only leave a gap: a client view is opened when something is wrong.
func (m *Manager) Client(ctx context.Context, id string) (*ClientView, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	src := m.source(ctx).client
	config, err := src.ConfigGet(ctx)
	if err != nil {
		return nil, err
	}
	subnets, err := kea.ParseSubnets(config)
	if err != nil {
		return nil, err
	}
	classes, err := kea.ParseClasses(config)
	if err != nil {
		return nil, err
	}
	hosts, err := hostsOn(ctx, src, subnets)
	if err != nil {
		return nil, err
	}
	v := &ClientView{History: []AuditEntry{}, Warnings: []string{}}
	leases, err := m.Leases(ctx)
	if err != nil {
		v.Warnings = append(v.Warnings, "Lease data unavailable: "+err.Error())
	}
	var devices []arpDevice
	if m.ARPMonitorEnabled() {
		short, cancel := context.WithTimeout(ctx, dashboardARPTimeout)
		defer cancel()
		if devices, err = m.arpDevices(short); err != nil {
			v.Warnings = append(v.Warnings, err.Error())
		}
	}
	wifi, err := m.wifiClients(ctx)
	if err != nil {
		v.Warnings = append(v.Warnings, err.Error())
	}
	devices = mergeWifiClients(devices, wifi)

	mac, ip := "", ""
	if macPattern.MatchString(id) {
		mac = strings.ReplaceAll(id, "-", ":")
	} else if addr, err := netip.ParseAddr(id); err == nil {
		ip = addr.String()
	}
	matches := func(leaseMAC, leaseIP, name string) bool {
		return mac != "" && strings.EqualFold(leaseMAC, mac) ||
			ip != "" && leaseIP == ip ||
			mac == "" && ip == "" && strings.EqualFold(trimName(name), trimName(id))
	}

	var lease *LeaseView
	for i := range leases {
		l := &leases[i]
		if matches(l.HWAddress, l.IPAddress, l.Hostname) && (lease == nil || l.Active && !lease.Active || l.Active == lease.Active && l.Started.After(lease.Started)) {
			lease = l
		}
	}
	var device *arpDevice
	for i := range devices {
		d := &devices[i]
		if matches(d.MAC, "", d.Hostname) || ip != "" && slices.Contains(d.IPv4, ip) {
			device = d
			break
		}
	}
	if mac == "" {
		if lease != nil {
			mac = strings.ToLower(lease.HWAddress)
		} else if device != nil {
			mac = strings.ToLower(device.MAC)
		}
	}
	var host kea.Host
	for _, h := range hosts {
		idType, hostID := h.Identifier()
		if idType == "hw-address" && mac != "" && strings.EqualFold(hostID, mac) {
			host = h
			break
		}
		if host == nil && (ip != "" && h.IP() == ip || mac == "" && ip == "" && strings.EqualFold(trimName(h.Hostname()), trimName(id))) {
			host = h
		}
	}
	if host != nil && mac == "" {
		if idType, hostID := host.Identifier(); idType == "hw-address" {
			mac = strings.ToLower(hostID)
		}
	}
	if mac != "" {
		// the lookup above may have started from an address or a name
		for i := range leases {
			if l := &leases[i]; strings.EqualFold(l.HWAddress, mac) && (lease == nil || l.Active && !lease.Active) {
				lease = l
			}
		}
		for i := range devices {
			if strings.EqualFold(devices[i].MAC, mac) {
				device = &devices[i]
			}
		}
	}
	if lease == nil && host == nil && device == nil && !macPattern.MatchString(id) {
		return nil, ErrNotFound
	}

	v.HWAddress, v.Lease = mac, lease
	if mac != "" {
		v.Vendor = oui.Lookup(mac)
	}
	if lease != nil {
		v.Name, v.IPAddress = lease.Hostname, lease.IPAddress
	}
	if device != nil {
		// a client without an address yet shows up as 0.0.0.0
		addresses := slices.DeleteFunc(slices.Clone(device.IPv4), func(ip string) bool { return ip == "0.0.0.0" })
		v.Device = &ClientDevice{Addresses: addresses, Hostname: trimName(device.Hostname), LastSeen: device.LastSeen.UTC().Format("2006-01-02T15:04:05Z")}
		if !device.FirstSeen.IsZero() {
			v.Device.FirstSeen = device.FirstSeen.UTC().Format("2006-01-02T15:04:05Z")
		}
		if v.Name == "" {
			v.Name = v.Device.Hostname
		}
		if v.IPAddress == "" && len(addresses) > 0 {
			v.IPAddress = addresses[0]
		}
	}
	if c, ok := wifi[mac]; ok {
		v.Wifi = c.view()
	}
	other := ""
	if host != nil {
		idType, hostID := host.Identifier()
		v.Reservation = &ClientReservation{host.SubnetID(), idType, hostID, host.IP(), host.Hostname(), host.Classes(), host.Comment()}
		if host.Hostname() != "" {
			v.Name = host.Hostname()
		}
		if host.IP() != "" {
			v.IPAddress = host.IP()
		}
		for _, l := range leases {
			if l.Active && l.IPAddress == host.IP() && l.HWAddress != "" && !strings.EqualFold(l.HWAddress, mac) {
				other = l.HWAddress
			}
		}
	}
	seen := ""
	if v.Device != nil && len(v.Device.Addresses) > 0 {
		seen = v.Device.Addresses[0]
	}
	if v.Explanation, err = explain(explainInput{config, subnets, classes, host, lease, other, seen}); err != nil {
		return nil, err
	}

	entries, err := m.store.AuditTail(clientAuditScan)
	if err != nil {
		v.Warnings = append(v.Warnings, "Audit log unavailable: "+err.Error())
	}
	for _, a := range entries {
		text := strings.ToLower(a.Target)
		if mac != "" && strings.Contains(text, mac) || v.IPAddress != "" && (text == v.IPAddress || strings.Contains(text, v.IPAddress+" ")) {
			if v.History = append(v.History, a); len(v.History) == clientAuditLimit {
				break
			}
		}
	}
	return v, nil
}
