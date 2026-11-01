package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/vgropp/kea-roost/internal/kea"
)

const writeTimeout = 60 * time.Second

// Status is the outcome of a change.
type Status string

// Outcomes of a change.
const (
	StatusOK Status = "ok"
	// StatusFailed means the change was not applied.
	StatusFailed Status = "failed"
	// StatusPartial means Kea may hold a state that differs from what was asked
	// for or from what is stored on disk. An operator has to decide what happens.
	StatusPartial Status = "partial"
)

// Step is one stage of a change.
type Step struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// Result reports a change attempt. Status is only "ok" when every step passed.
type Result struct {
	Status   Status   `json:"status"`
	Message  string   `json:"message"`
	Steps    []Step   `json:"steps"`
	Warnings []string `json:"warnings"`
	Backup   string   `json:"backup,omitempty"`
}

// ValidationError lists rejected input; nothing was changed.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string { return strings.Join(e.Problems, "; ") }

// halfDoneError means a change with two steps stopped after the first one.
type halfDoneError struct {
	err     error
	message string
}

func (e *halfDoneError) Error() string { return e.err.Error() }

// ErrNotFound is returned for an unknown lease or reservation.
var ErrNotFound = errors.New("not found")

// ReservationOption is a DHCP option from the edit form.
type ReservationOption struct {
	Name string `json:"name"`
	Code int64  `json:"code"`
	Data string `json:"data"`
}

// ReservationInput is the editable part of a reservation.
type ReservationInput struct {
	SubnetID       int64               `json:"subnetId"`
	IdentifierType string              `json:"identifierType"`
	Identifier     string              `json:"identifier"`
	IPAddress      string              `json:"ipAddress"`
	Hostname       string              `json:"hostname"`
	Options        []ReservationOption `json:"options"`
	Classes        []string            `json:"classes"`
	Comment        string              `json:"comment"`
	// NewIdentifierType and NewIdentifier change who the reservation belongs to.
	NewIdentifierType string `json:"newIdentifierType"`
	NewIdentifier     string `json:"newIdentifier"`
}

var (
	macPattern        = regexp.MustCompile(`^[0-9a-fA-F]{2}([:-][0-9a-fA-F]{2}){5}$`)
	identifierPattern = regexp.MustCompile(`^([0-9a-fA-F]{2}([:-]?[0-9a-fA-F]{2})*|'[^']+')$`)
	nonBackupName     = regexp.MustCompile(`[^a-z-]`)
	hostnamePattern   = regexp.MustCompile(`^[a-zA-Z0-9_]([a-zA-Z0-9_-]*[a-zA-Z0-9_])?(\.[a-zA-Z0-9_]([a-zA-Z0-9_-]*[a-zA-Z0-9_])?)*\.?$`)
)

// NormalizeIdentifier validates an identifier and returns its canonical form.
func NormalizeIdentifier(idType, id string) (string, error) {
	id = strings.TrimSpace(id)
	if !slices.Contains(kea.IdentifierTypes, idType) {
		return "", fmt.Errorf("unknown identifier type %q", idType)
	}
	if id == "" {
		return "", errors.New("identifier is missing")
	}
	if idType == "hw-address" {
		if !macPattern.MatchString(id) {
			return "", fmt.Errorf("invalid MAC address %q", id)
		}
		return strings.ReplaceAll(strings.ToLower(id), "-", ":"), nil
	}
	if !identifierPattern.MatchString(id) {
		return "", fmt.Errorf("invalid identifier %q (expected hex or 'text')", id)
	}
	if strings.HasPrefix(id, "'") {
		return id, nil
	}
	return strings.ToLower(id), nil
}

// checkReservation validates input against the current state of Kea and returns
// the blocking problems and the warnings. When editing, the reservation with
// the same subnet and identifier is the one being changed, not a duplicate.
func checkReservation(in *ReservationInput, subnets []kea.Subnet, hosts []kea.Host, leases []kea.Lease, editing bool) (problems, warnings []string) {
	id, err := NormalizeIdentifier(in.IdentifierType, in.Identifier)
	if err != nil {
		problems = append(problems, err.Error())
	} else {
		in.Identifier = id
	}

	var subnet *kea.Subnet
	for i := range subnets {
		if subnets[i].ID == in.SubnetID {
			subnet = &subnets[i]
		}
	}
	if subnet == nil && in.SubnetID != 0 {
		problems = append(problems, fmt.Sprintf("subnet %d does not exist", in.SubnetID))
	}

	in.Hostname = strings.TrimSpace(in.Hostname)
	if in.Hostname != "" && (len(in.Hostname) > 253 || !hostnamePattern.MatchString(in.Hostname)) {
		problems = append(problems, fmt.Sprintf("invalid hostname %q", in.Hostname))
	}
	for _, o := range in.Options {
		if strings.TrimSpace(o.Name) == "" && o.Code == 0 {
			problems = append(problems, "DHCP option without a name")
		}
	}

	in.IPAddress = strings.TrimSpace(in.IPAddress)
	var ip netip.Addr
	if in.IPAddress != "" {
		ip, err = netip.ParseAddr(in.IPAddress)
		if err != nil || !ip.Is4() {
			problems = append(problems, fmt.Sprintf("invalid IPv4 address %q", in.IPAddress))
			ip = netip.Addr{}
		}
	}
	if ip.IsValid() && subnet == nil && in.SubnetID == 0 {
		subnet = kea.SubnetFor(subnets, kea.Host{"subnet-id": int64(0), "ip-address": ip.String()})
	}
	if ip.IsValid() && subnet != nil {
		if !subnet.Prefix.Contains(ip) {
			problems = append(problems, fmt.Sprintf("%s is not in subnet %s", ip, subnet.Subnet))
		} else if isNetworkOrBroadcast(ip, subnet.Prefix) {
			problems = append(problems, fmt.Sprintf("%s is the network or broadcast address of %s", ip, subnet.Subnet))
		}
		if p := subnet.PoolContaining(ip); p != nil {
			warnings = append(warnings, fmt.Sprintf("%s is inside the dynamic pool %s", ip, p.Text))
		}
	}

	for _, h := range hosts {
		idType, hostID := h.Identifier()
		same := idType == in.IdentifierType && strings.EqualFold(hostID, in.Identifier)
		if same && h.SubnetID() == in.SubnetID {
			if !editing {
				problems = append(problems, fmt.Sprintf("%s is already reserved in this subnet", in.Identifier))
			}
			continue
		}
		if same {
			warnings = append(warnings, fmt.Sprintf("%s is also reserved in subnet %d", in.Identifier, h.SubnetID()))
		}
		if ip.IsValid() && h.IP() == ip.String() {
			owner := hostID
			if name := h.Hostname(); name != "" {
				owner = name + " (" + hostID + ")"
			}
			problems = append(problems, fmt.Sprintf("%s is already reserved for %s", ip, owner))
		}
	}

	if ip.IsValid() {
		for _, l := range leases {
			if l.IPAddress == ip.String() && l.HWAddress != "" && in.IdentifierType == "hw-address" && !strings.EqualFold(l.HWAddress, in.Identifier) {
				warnings = append(warnings, fmt.Sprintf("%s is currently leased to %s", ip, l.HWAddress))
			}
		}
	}
	return problems, warnings
}

func isNetworkOrBroadcast(ip netip.Addr, prefix netip.Prefix) bool {
	if prefix.Bits() >= 31 {
		return false
	}
	p, err := kea.ParsePool(prefix.String())
	return err == nil && (ip == p.First || ip == p.Last)
}

// mergeOptions builds an option-data list from form input and keeps the settings
// of existing options the form does not show.
func mergeOptions(previous []map[string]any, input []ReservationOption) []any {
	options := make([]any, 0, len(input))
	for _, o := range input {
		entry := map[string]any{}
		for _, p := range previous {
			if p["name"] == o.Name && o.Name != "" || fmt.Sprint(p["code"]) == fmt.Sprint(o.Code) && o.Code != 0 {
				for k, v := range p {
					entry[k] = v
				}
			}
		}
		if o.Name != "" {
			entry["name"] = o.Name
		}
		if o.Code != 0 {
			entry["code"] = o.Code
		}
		entry["data"] = o.Data
		options = append(options, entry)
	}
	return options
}

// apply fills the fields the form owns into a host and keeps all others.
func (in *ReservationInput) apply(h kea.Host) {
	h["subnet-id"] = in.SubnetID
	h[in.IdentifierType] = in.Identifier
	if in.IPAddress != "" {
		h["ip-address"] = in.IPAddress
	} else {
		delete(h, "ip-address")
	}
	if in.Hostname != "" {
		h["hostname"] = in.Hostname
	} else {
		delete(h, "hostname")
	}

	options := mergeOptions(h.Options(), in.Options)
	if len(options) > 0 {
		h["option-data"] = options
	} else {
		delete(h, "option-data")
	}

	if in.Classes != nil {
		if len(in.Classes) > 0 {
			classes := make([]any, 0, len(in.Classes))
			for _, c := range in.Classes {
				classes = append(classes, c)
			}
			h["client-classes"] = classes
		} else {
			delete(h, "client-classes")
		}
	}

	userContext, _ := h["user-context"].(map[string]any)
	if in.Comment != "" {
		if userContext == nil {
			userContext = map[string]any{}
		}
		userContext["comment"] = in.Comment
	} else {
		delete(userContext, "comment")
	}
	if len(userContext) > 0 {
		h["user-context"] = userContext
	} else {
		delete(h, "user-context")
	}
}

// change runs one modification of the Kea configuration: backup, change,
// config-write, verification, audit. It stops at the first failure and never
// tries to undo anything, because after an error the state of Kea is not known.
// With peers it runs on one server after the other, primary first.
type change struct {
	user, action, target, backupReason string
	warnings                           []string
	mutate                             func(context.Context, *kea.Client) error
	verify                             func(context.Context, *kea.Client) error
}

func (m *Manager) run(ctx context.Context, c change) *Result {
	return m.runOn(ctx, m.servers(), c)
}

func (m *Manager) runOn(ctx context.Context, servers []server, c change) *Result {
	res := &Result{Status: StatusOK, Steps: []Step{}, Warnings: slices.Clip(c.warnings)}
	if res.Warnings == nil {
		res.Warnings = []string{}
	}
	done := []string{}
	for _, s := range servers {
		status, message := m.applyOn(ctx, s, c, res)
		if status == StatusOK {
			done = append(done, s.name)
			continue
		}
		res.Status, res.Message = status, message
		// Nothing is undone: the Servers page shows the difference.
		if len(done) > 0 {
			res.Status = StatusPartial
			res.Message = fmt.Sprintf("Applied on %s, stopped at %s: %s Compare and sync on the Servers page.", strings.Join(done, ", "), s.name, message)
		}
		return m.finish(c, res)
	}
	res.Message = "Change applied, saved and verified."
	if len(done) > 1 {
		res.Message = fmt.Sprintf("Change applied, saved and verified on %s.", strings.Join(done, ", "))
	}
	return m.finish(c, res)
}

// applyOn runs a change on one server and adds its steps to res.
func (m *Manager) applyOn(ctx context.Context, s server, c change, res *Result) (Status, string) {
	step := func(name string, err error) bool {
		if len(m.peers) > 0 {
			name = s.name + ": " + name
		}
		st := Step{Name: name, OK: err == nil}
		if err != nil {
			st.Message = err.Error()
		}
		res.Steps = append(res.Steps, st)
		return err == nil
	}

	config, err := s.client.ConfigGet(ctx)
	if err == nil {
		if lossy := kea.LossyOptions(config); len(lossy) > 0 {
			step("Check configuration", errors.New(strings.Join(lossy, ", ")))
			return StatusFailed, fmt.Sprintf("Nothing was changed: config-write would erase the value of %s. Remove the option from the Kea configuration or define it differently first.", strings.Join(lossy, ", "))
		}
		var backup string
		backup, err = m.store.SaveBackup(m.backupReason(c, s), config, m.now())
		if res.Backup == "" {
			res.Backup = backup
		}
	}
	if !step("Back up configuration", err) {
		var unreachable *kea.UnreachableError
		if errors.As(err, &unreachable) {
			return StatusFailed, "Nothing was changed because " + s.name + " is unreachable."
		}
		return StatusFailed, "Nothing was changed because the backup failed."
	}

	err = c.mutate(ctx, s.client)
	if !step("Apply change in Kea", err) {
		var unreachable *kea.UnreachableError
		var halfDone *halfDoneError
		switch {
		case errors.As(err, &halfDone):
			return StatusPartial, halfDone.message
		case errors.As(err, &unreachable):
			return StatusPartial, "Kea did not answer. It is unclear whether the change was applied. Please check the current state."
		}
		return StatusFailed, "Kea rejected the change: " + err.Error()
	}

	if !step("Persist (config-write)", s.client.ConfigWrite(ctx)) {
		return StatusPartial, "The change is active but NOT saved and will be lost when Kea restarts. Trigger the save again."
	}

	if !step("Verify result", c.verify(ctx, s.client)) {
		return StatusPartial, "The change was saved, but verification failed. Please check the current state."
	}
	return StatusOK, ""
}

// backupReason tells the backups of the servers apart. Backup names only hold
// lowercase letters and dashes.
func (m *Manager) backupReason(c change, s server) string {
	if s.client == m.kea {
		return c.backupReason
	}
	name := nonBackupName.ReplaceAllString(strings.ToLower(s.name), "-")
	return c.backupReason + "-" + name
}

func (m *Manager) finish(c change, res *Result) *Result {
	err := m.store.Audit(AuditEntry{
		Time:   m.now(),
		User:   c.user,
		Action: c.action,
		Target: c.target,
		Status: res.Status,
		Backup: res.Backup,
		Steps:  res.Steps,
	})
	if err != nil {
		slog.Error("audit log", "error", err)
		res.Warnings = append(res.Warnings, "Could not write the audit log: "+err.Error())
	}
	slog.Info("change", "user", c.user, "action", c.action, "target", c.target, "status", res.Status)
	// comes last: the audit entry must not wait for a slow Kea
	if res.Status != StatusFailed {
		if err := m.RefreshHostsFile(context.Background()); err != nil {
			res.Warnings = append(res.Warnings, "Could not update the DNS hosts file: "+err.Error())
		}
	}
	return res
}

// A change must run to its end once Kea was touched, even if the browser is
// closed halfway through.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
}

// precheck validates a reservation. Warnings need a confirmation, hints are only
// shown: they point at something that may be outdated.
func (m *Manager) precheck(ctx context.Context, in *ReservationInput, editing bool, replaced kea.Host) (warnings, hints []string, err error) {
	subnets, err := m.subnets(ctx)
	if err != nil {
		return nil, nil, err
	}
	hosts, err := m.allHosts(ctx, subnets)
	if err != nil {
		return nil, nil, err
	}
	if replaced != nil {
		oldType, oldID := replaced.Identifier()
		hosts = slices.DeleteFunc(hosts, func(h kea.Host) bool {
			idType, id := h.Identifier()
			return h.SubnetID() == replaced.SubnetID() && idType == oldType && strings.EqualFold(id, oldID)
		})
	}
	leases, err := m.kea.Leases(ctx)
	if err != nil {
		warnings = append(warnings, "Lease check not possible: "+err.Error())
	}
	problems, found := checkReservation(in, subnets, hosts, leases, editing)
	active, stale := m.addressInUse(ctx, in)
	hints = stale
	// A reservation that keeps its address must stay editable, even if another
	// device took that address in the meantime.
	if keepsAddress(in, hosts, replaced) {
		found = append(found, active...)
	} else {
		problems = append(problems, active...)
	}
	if len(in.Classes) > 0 {
		list, err := m.Classes(ctx)
		if err != nil {
			return nil, nil, err
		}
		for _, name := range in.Classes {
			if !slices.ContainsFunc(list.Classes, func(c ClassView) bool { return c.Name == name }) {
				problems = append(problems, fmt.Sprintf("class %s does not exist", name))
			}
		}
	}
	if len(problems) > 0 {
		return nil, nil, &ValidationError{Problems: problems}
	}
	return append(warnings, found...), hints, nil
}

// addressInUse asks the ARP monitor who uses or used the address. That also finds
// static devices, which neither Kea nor a lease knows about, even while they are off.
func (m *Manager) addressInUse(ctx context.Context, in *ReservationInput) (active, stale []string) {
	if !m.ARPMonitorEnabled() || in.IPAddress == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dashboardARPTimeout)
	defer cancel()
	devices, err := m.arpDevices(ctx)
	if err != nil {
		slog.Debug("address check", "error", err)
		return nil, nil
	}
	for _, d := range devices {
		position := slices.Index(d.IPv4, in.IPAddress)
		if position < 0 || (in.IdentifierType == "hw-address" && strings.EqualFold(d.MAC, in.Identifier)) {
			continue
		}
		device := strings.TrimSpace(d.MAC + " " + strings.TrimSuffix(d.Hostname, "."))
		age := m.now().Sub(d.LastSeen)
		// the monitor lists the newest address first and keeps a time per device
		if position == 0 && age <= m.cfg().online() {
			active = append(active, fmt.Sprintf("%s is in use by %s", in.IPAddress, device))
		} else if position == 0 {
			stale = append(stale, fmt.Sprintf("%s was last used by %s, seen %s ago", in.IPAddress, device, age.Round(time.Minute)))
		} else {
			stale = append(stale, fmt.Sprintf("%s was used earlier by %s, which has another address now", in.IPAddress, device))
		}
	}
	return active, stale
}

func keepsAddress(in *ReservationInput, hosts []kea.Host, replaced kea.Host) bool {
	if replaced != nil {
		return replaced.IP() == in.IPAddress
	}
	return slices.ContainsFunc(hosts, func(h kea.Host) bool {
		idType, id := h.Identifier()
		return h.SubnetID() == in.SubnetID && idType == in.IdentifierType && strings.EqualFold(id, in.Identifier) && h.IP() == in.IPAddress
	})
}

// CheckReservation returns what a new or changed reservation would run into,
// without applying it. A set update checks the change of an existing one.
func (m *Manager) CheckReservation(ctx context.Context, in ReservationInput, update bool) (warnings, hints []string, err error) {
	switch {
	case !update:
		warnings, hints, err = m.precheck(ctx, &in, false, nil)
	default:
		var id string
		if id, err = NormalizeIdentifier(in.IdentifierType, in.Identifier); err != nil {
			return nil, nil, &ValidationError{Problems: []string{err.Error()}}
		}
		in.Identifier = id
		var existing kea.Host
		if existing, err = m.kea.Reservation(ctx, in.SubnetID, in.IdentifierType, id); err != nil {
			return nil, nil, err
		}
		if existing == nil {
			return nil, nil, ErrNotFound
		}
		newType, newID := in.NewIdentifierType, in.NewIdentifier
		if newID == "" || (newType == in.IdentifierType && strings.EqualFold(newID, id)) {
			warnings, hints, err = m.precheck(ctx, &in, true, nil)
			break
		}
		in.IdentifierType, in.Identifier = newType, newID
		warnings, hints, err = m.precheck(ctx, &in, false, existing)
	}
	if err != nil {
		return nil, nil, err
	}
	if warnings == nil {
		warnings = []string{}
	}
	if hints == nil {
		hints = []string{}
	}
	return warnings, hints, nil
}

func target(subnetID int64, identifierType, identifier string) string {
	return fmt.Sprintf("subnet %d, %s %s", subnetID, identifierType, identifier)
}

// hostNote names a reservation for the audit log, where the identifier alone says
// little once the entry is gone.
func hostNote(hostname, ip string) string {
	parts := make([]string, 0, 2)
	for _, part := range []string{hostname, ip} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, ", ")
}

func withNote(text, note string) string {
	if note == "" {
		return text
	}
	return text + " (" + note + ")"
}

// changeNote shows what became of a reservation.
func changeNote(before, after string) string {
	if before == after || before == "" {
		return after
	}
	return before + " -> " + after
}

// verifyHost compares the reservation in Kea with what it should be.
func verifyHost(ctx context.Context, k *kea.Client, in *ReservationInput) error {
	h, err := k.Reservation(ctx, in.SubnetID, in.IdentifierType, in.Identifier)
	if err != nil {
		return err
	}
	if h == nil {
		return errors.New("reservation is missing in Kea")
	}
	if h.IP() != in.IPAddress || !strings.EqualFold(h.Hostname(), in.Hostname) {
		return fmt.Errorf("reservation is %s / %q instead of %s / %q", h.IP(), h.Hostname(), in.IPAddress, in.Hostname)
	}
	return nil
}

// CreateReservation adds a reservation.
func (m *Manager) CreateReservation(ctx context.Context, user string, in ReservationInput) (*Result, error) {
	ctx, cancel := detached(ctx)
	defer cancel()
	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	warnings, _, err := m.precheck(ctx, &in, false, nil)
	if err != nil {
		return nil, err
	}
	return m.run(ctx, change{
		user:         user,
		action:       "reservation-add",
		target:       withNote(target(in.SubnetID, in.IdentifierType, in.Identifier), hostNote(in.Hostname, in.IPAddress)),
		backupReason: "reservation-add",
		warnings:     warnings,
		mutate: func(ctx context.Context, k *kea.Client) error {
			h := kea.Host{}
			in.apply(h)
			return k.AddReservation(ctx, h)
		},
		verify: func(ctx context.Context, k *kea.Client) error { return verifyHost(ctx, k, &in) },
	}), nil
}

// UpdateReservation changes a reservation. Identifier and subnet name the
// reservation and cannot be changed; all fields the form does not know are kept.
func (m *Manager) UpdateReservation(ctx context.Context, user string, in ReservationInput) (*Result, error) {
	ctx, cancel := detached(ctx)
	defer cancel()
	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	id, err := NormalizeIdentifier(in.IdentifierType, in.Identifier)
	if err != nil {
		return nil, &ValidationError{Problems: []string{err.Error()}}
	}
	in.Identifier = id
	existing, err := m.kea.Reservation(ctx, in.SubnetID, in.IdentifierType, in.Identifier)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, ErrNotFound
	}
	if in.NewIdentifier != "" {
		newType := in.NewIdentifierType
		if newType == "" {
			newType = in.IdentifierType
		}
		newID, err := NormalizeIdentifier(newType, in.NewIdentifier)
		if err != nil {
			return nil, &ValidationError{Problems: []string{err.Error()}}
		}
		if newType != in.IdentifierType || newID != in.Identifier {
			return m.replaceReservation(ctx, user, in, existing, newType, newID)
		}
	}
	warnings, _, err := m.precheck(ctx, &in, true, nil)
	if err != nil {
		return nil, err
	}
	return m.run(ctx, change{
		user:         user,
		action:       "reservation-update",
		target:       withNote(target(in.SubnetID, in.IdentifierType, in.Identifier), changeNote(hostNote(existing.Hostname(), existing.IP()), hostNote(in.Hostname, in.IPAddress))),
		backupReason: "reservation-update",
		warnings:     warnings,
		mutate: func(ctx context.Context, k *kea.Client) error {
			in.apply(existing)
			return k.UpdateReservation(ctx, existing)
		},
		verify: func(ctx context.Context, k *kea.Client) error { return verifyHost(ctx, k, &in) },
	}), nil
}

// replaceReservation moves a reservation to another identifier. Kea cannot change
// the identifier of a host, so it is deleted and added again with all its settings.
// Adding first would fail on the address, which the old entry still holds.
func (m *Manager) replaceReservation(ctx context.Context, user string, in ReservationInput, existing kea.Host, newType, newID string) (*Result, error) {
	oldType, oldID := in.IdentifierType, in.Identifier
	in.IdentifierType, in.Identifier = newType, newID
	warnings, _, err := m.precheck(ctx, &in, false, existing)
	if err != nil {
		return nil, err
	}
	return m.run(ctx, change{
		user:         user,
		action:       "reservation-replace",
		target:       withNote(fmt.Sprintf("%s -> %s %s", target(in.SubnetID, oldType, oldID), newType, newID), changeNote(hostNote(existing.Hostname(), existing.IP()), hostNote(in.Hostname, in.IPAddress))),
		backupReason: "reservation-replace",
		warnings:     warnings,
		mutate: func(ctx context.Context, k *kea.Client) error {
			host := kea.Host{}
			for k, v := range existing {
				if k != oldType {
					host[k] = v
				}
			}
			in.apply(host)
			if err := k.DeleteReservation(ctx, in.SubnetID, oldType, oldID); err != nil {
				return err
			}
			if err := k.AddReservation(ctx, host); err != nil {
				return &halfDoneError{err, "The old reservation was removed, but the new one could not be created: " + err.Error() + ". Create it again; the settings are in the backup."}
			}
			return nil
		},
		verify: func(ctx context.Context, k *kea.Client) error {
			h, err := k.Reservation(ctx, in.SubnetID, oldType, oldID)
			if err != nil {
				return err
			}
			if h != nil {
				return errors.New("old reservation still exists")
			}
			return verifyHost(ctx, k, &in)
		},
	}), nil
}

// DeleteReservation removes a reservation.
func (m *Manager) DeleteReservation(ctx context.Context, user string, subnetID int64, identifierType, identifier string) (*Result, error) {
	ctx, cancel := detached(ctx)
	defer cancel()
	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	id, err := NormalizeIdentifier(identifierType, identifier)
	if err != nil {
		return nil, &ValidationError{Problems: []string{err.Error()}}
	}
	existing, err := m.kea.Reservation(ctx, subnetID, identifierType, id)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, ErrNotFound
	}
	return m.run(ctx, change{
		user:         user,
		action:       "reservation-del",
		target:       withNote(target(subnetID, identifierType, id), hostNote(existing.Hostname(), existing.IP())),
		backupReason: "reservation-del",
		mutate: func(ctx context.Context, k *kea.Client) error {
			return k.DeleteReservation(ctx, subnetID, identifierType, id)
		},
		verify: func(ctx context.Context, k *kea.Client) error {
			h, err := k.Reservation(ctx, subnetID, identifierType, id)
			if err != nil {
				return err
			}
			if h != nil {
				return errors.New("reservation still exists")
			}
			return nil
		},
	}), nil
}

// DeleteLease removes a lease from every server, because HA only copies what the
// DHCP service changes. Leases are not part of the configuration, so there is no
// backup and no config-write.
func (m *Manager) DeleteLease(ctx context.Context, user, ip string) error {
	if addr, err := netip.ParseAddr(ip); err != nil || !addr.Is4() {
		return &ValidationError{Problems: []string{fmt.Sprintf("invalid IPv4 address %q", ip)}}
	}
	ctx, cancel := detached(ctx)
	defer cancel()
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	found, err := m.kea.DeleteLease(ctx, ip)
	status := StatusOK
	var unreachable *kea.UnreachableError
	switch {
	case errors.As(err, &unreachable):
		status = StatusPartial
	case err != nil || !found:
		status = StatusFailed
	}
	if err == nil && found {
		// the partner may not know this lease, which is fine
		for _, p := range m.peers {
			if _, peerErr := p.client.DeleteLease(ctx, ip); peerErr != nil {
				status = StatusPartial
				err = fmt.Errorf("deleted on %s, but not on %s: %w", m.serverName, p.name, peerErr)
				break
			}
		}
	}
	auditErr := m.store.Audit(AuditEntry{Time: m.now(), User: user, Action: "lease4-del", Target: ip, Status: status})
	if auditErr != nil {
		slog.Error("audit log", "error", auditErr)
	}
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	return nil
}

// Persist repeats config-write after a failed change. It tries every server, so
// that one being down does not keep the others from saving.
func (m *Manager) Persist(ctx context.Context, user string) *Result {
	ctx, cancel := detached(ctx)
	defer cancel()
	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	res := &Result{Status: StatusOK, Message: "Configuration saved.", Warnings: []string{}, Steps: []Step{}}
	for _, srv := range m.servers() {
		err := srv.client.ConfigWrite(ctx)
		name := "Persist (config-write)"
		if len(m.peers) > 0 {
			name = srv.name + ": " + name
		}
		s := Step{Name: name, OK: err == nil}
		if err != nil {
			s.Message = err.Error()
			if res.Status == StatusOK {
				res.Status = StatusFailed
				res.Message = "Saving failed: " + err.Error()
			}
		}
		res.Steps = append(res.Steps, s)
	}
	return m.finish(change{user: user, action: "config-write", target: "configuration"}, res)
}
