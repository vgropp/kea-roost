package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	hiddenFile      = "hidden-findings.json"
	maxHiddenTarget = 300
)

func hiddenKey(rule, target string) string { return rule + "|" + target }

func (m *Manager) loadHidden() error {
	keys := map[string]bool{}
	data, err := os.ReadFile(filepath.Join(m.store.dir, hiddenFile))
	if err == nil {
		var list []string
		if err := json.Unmarshal(data, &list); err != nil {
			return fmt.Errorf("%s: %w", hiddenFile, err)
		}
		for _, key := range list {
			keys[key] = true
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	m.hidden.Store(&keys)
	return nil
}

func (m *Manager) markHidden(list []Finding) {
	keys := m.hidden.Load()
	if keys == nil {
		return
	}
	for i := range list {
		list[i].Hidden = (*keys)[hiddenKey(list[i].Rule, list[i].Target)]
	}
}

// HideFinding accepts or brings back one finding. It belongs to a rule and an entry,
// so the same rule still reports everything else.
func (m *Manager) HideFinding(user, rule, target string, hidden bool) error {
	known := slices.ContainsFunc(reviewRules, func(r ReviewRule) bool { return r.Rule == rule })
	if !known || target == "" || len(target) > maxHiddenTarget {
		return &ValidationError{Problems: []string{"unknown finding"}}
	}
	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	next := map[string]bool{}
	if keys := m.hidden.Load(); keys != nil {
		for key := range *keys {
			next[key] = true
		}
	}
	key := hiddenKey(rule, target)
	if next[key] == hidden {
		return nil
	}
	if hidden {
		next[key] = true
	} else {
		delete(next, key)
	}
	if err := m.store.saveHidden(next); err != nil {
		return err
	}
	m.hidden.Store(&next)

	action := "finding-show"
	if hidden {
		action = "finding-hide"
	}
	entry := AuditEntry{Time: m.now(), User: user, Action: action, Target: rule + " " + target, Status: StatusOK}
	if err := m.store.Audit(entry); err != nil {
		slog.Error("audit log", "error", err)
	}
	return nil
}

func (s *Store) saveHidden(keys map[string]bool) error {
	list := make([]string, 0, len(keys))
	for key := range keys {
		list = append(list, key)
	}
	slices.SortFunc(list, strings.Compare)
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, hiddenFile)
	if err := os.WriteFile(path+".tmp", append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		os.Remove(path + ".tmp")
		return err
	}
	return nil
}
