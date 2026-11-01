package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const settingsFile = "settings.json"

// Settings are the limits an operator can tune in the interface. Addresses and
// credentials stay in the environment and never come here.
type Settings struct {
	DevicesDays        int `json:"devicesDays"`
	ReviewDays         int `json:"reviewDays"`
	OnlineMinutes      int `json:"onlineMinutes"`
	LeaseSilentMinutes int `json:"leaseSilentMinutes"`
	PoolWarnPercent    int `json:"poolWarnPercent"`
	PoolCritPercent    int `json:"poolCritPercent"`
	ClockSkewSeconds   int `json:"clockSkewSeconds"`
	DropWarnPercent    int `json:"dropWarnPercent"`
	NakWarnPercent     int `json:"nakWarnPercent"`
	MinPackets         int `json:"minPackets"`
}

func defaultSettings() Settings {
	return Settings{
		DevicesDays: 7, ReviewDays: 365, OnlineMinutes: 15, LeaseSilentMinutes: 60,
		PoolWarnPercent: 80, PoolCritPercent: 95, ClockSkewSeconds: 30,
		DropWarnPercent: 5, NakWarnPercent: 10, MinPackets: 50,
	}
}

func (s Settings) online() time.Duration { return time.Duration(s.OnlineMinutes) * time.Minute }

func (s Settings) leaseSilent() time.Duration {
	return time.Duration(s.LeaseSilentMinutes) * time.Minute
}

// SettingField describes one setting for the form.
type SettingField struct {
	Key      string `json:"key"`
	Group    string `json:"group"`
	Label    string `json:"label"`
	Help     string `json:"help"`
	Unit     string `json:"unit"`
	Min      int    `json:"min"`
	Max      int    `json:"max"`
	Value    int    `json:"value"`
	Default  int    `json:"default"`
	Changed  bool   `json:"changed"`
	fieldPtr func(*Settings) *int
}

var settingFields = []SettingField{
	{Key: "devicesDays", Group: "Network", Label: "Devices page", Help: "How far back the devices page lists devices.", Unit: "days", Min: 1, Max: 3650, fieldPtr: func(s *Settings) *int { return &s.DevicesDays }},
	{Key: "reviewDays", Group: "Network", Label: "Unseen reservations", Help: "A reservation counts as unseen when its device was not on the network for this long.", Unit: "days", Min: 1, Max: 3650, fieldPtr: func(s *Settings) *int { return &s.ReviewDays }},
	{Key: "onlineMinutes", Group: "Network", Label: "Online window", Help: "A device seen this recently counts as online. Conflicts only count between online devices.", Unit: "minutes", Min: 1, Max: 1440, fieldPtr: func(s *Settings) *int { return &s.OnlineMinutes }},
	{Key: "leaseSilentMinutes", Group: "Network", Label: "Silent lease", Help: "An active lease is noted when its device was not seen for this long.", Unit: "minutes", Min: 1, Max: 10080, fieldPtr: func(s *Settings) *int { return &s.LeaseSilentMinutes }},
	{Key: "poolWarnPercent", Group: "Warnings", Label: "Pool utilization warning", Help: "Warn when a subnet is this full.", Unit: "%", Min: 1, Max: 100, fieldPtr: func(s *Settings) *int { return &s.PoolWarnPercent }},
	{Key: "poolCritPercent", Group: "Warnings", Label: "Pool utilization problem", Help: "Report a problem when a subnet is this full.", Unit: "%", Min: 1, Max: 100, fieldPtr: func(s *Settings) *int { return &s.PoolCritPercent }},
	{Key: "clockSkewSeconds", Group: "Warnings", Label: "Clock skew", Help: "Warn when the HA partners differ by this much. Kea stops HA at 60 seconds.", Unit: "seconds", Min: 1, Max: 60, fieldPtr: func(s *Settings) *int { return &s.ClockSkewSeconds }},
	{Key: "dropWarnPercent", Group: "DHCP traffic", Label: "Dropped packets", Help: "Warn when this share of the received packets is dropped.", Unit: "%", Min: 1, Max: 100, fieldPtr: func(s *Settings) *int { return &s.DropWarnPercent }},
	{Key: "nakWarnPercent", Group: "DHCP traffic", Label: "Refused requests", Help: "Warn when this share of the answers is a NAK.", Unit: "%", Min: 1, Max: 100, fieldPtr: func(s *Settings) *int { return &s.NakWarnPercent }},
	{Key: "minPackets", Group: "DHCP traffic", Label: "Minimum packets", Help: "Shares are only rated from this many packets on.", Unit: "packets", Min: 1, Max: 1000000, fieldPtr: func(s *Settings) *int { return &s.MinPackets }},
}

func (m *Manager) cfg() Settings { return *m.settings.Load() }

// LoadSettings reads the saved settings. A value missing in the file keeps its
// default, so that a new setting works with an older file.
func (m *Manager) LoadSettings() error {
	s := defaultSettings()
	data, err := os.ReadFile(m.store.settingsPath())
	if errors.Is(err, fs.ErrNotExist) {
		m.settings.Store(&s)
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("%s: %w", settingsFile, err)
	}
	if problems := checkSettings(s); len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	m.settings.Store(&s)
	return nil
}

func checkSettings(s Settings) []string {
	var problems []string
	for _, f := range settingFields {
		if v := *f.fieldPtr(&s); v < f.Min || v > f.Max {
			problems = append(problems, fmt.Sprintf("%s must be between %d and %d", f.Label, f.Min, f.Max))
		}
	}
	if s.PoolWarnPercent > s.PoolCritPercent {
		problems = append(problems, "the pool warning must not lie above the pool problem")
	}
	return problems
}

// SettingFields returns the current settings for the form.
func (m *Manager) SettingFields() []SettingField {
	current, defaults := m.cfg(), defaultSettings()
	out := make([]SettingField, 0, len(settingFields))
	for _, f := range settingFields {
		f.Value, f.Default = *f.fieldPtr(&current), *f.fieldPtr(&defaults)
		f.Changed = f.Value != f.Default
		out = append(out, f)
	}
	return out
}

// UpdateSettings validates and stores values by key. Only keys of known settings
// are taken, so nothing else can end up in the file.
func (m *Manager) UpdateSettings(user string, values map[string]int) error {
	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	next := m.cfg()
	keys := map[string]func(*Settings) *int{}
	for _, f := range settingFields {
		keys[f.Key] = f.fieldPtr
	}
	var problems []string
	for key, value := range values {
		field, ok := keys[key]
		if !ok {
			problems = append(problems, fmt.Sprintf("unknown setting %q", key))
			continue
		}
		*field(&next) = value
	}
	problems = append(problems, checkSettings(next)...)
	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	changes := settingChanges(m.cfg(), next)
	if err := m.store.saveSettings(settingOverrides(next, defaultSettings())); err != nil {
		return err
	}
	m.settings.Store(&next)
	// the change is active by now, so a failed entry must not report it as failed
	if len(changes) > 0 {
		entry := AuditEntry{Time: m.now(), User: user, Action: "settings-update", Target: strings.Join(changes, ", "), Status: StatusOK}
		if err := m.store.Audit(entry); err != nil {
			slog.Error("audit log", "error", err)
		}
	}
	return nil
}

func settingChanges(before, after Settings) []string {
	var out []string
	for _, f := range settingFields {
		if a, b := *f.fieldPtr(&before), *f.fieldPtr(&after); a != b {
			out = append(out, fmt.Sprintf("%s %d -> %d", f.Key, a, b))
		}
	}
	return out
}

// settingOverrides keeps what differs from the defaults, so that a value nobody
// changed follows the default if that moves later.
func settingOverrides(s, defaults Settings) map[string]int {
	out := map[string]int{}
	for _, f := range settingFields {
		if v := *f.fieldPtr(&s); v != *f.fieldPtr(&defaults) {
			out[f.Key] = v
		}
	}
	return out
}

func (s *Store) settingsPath() string { return filepath.Join(s.dir, settingsFile) }

func (s *Store) saveSettings(v map[string]int) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.settingsPath() + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.settingsPath()); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
