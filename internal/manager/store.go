package manager

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

var backupName = regexp.MustCompile(`^kea-config-\d{8}T\d{9}Z-[a-z-]+\.json$`)

// ErrInvalidBackup rejects names that are not backups of this manager.
var ErrInvalidBackup = errors.New("invalid backup name")

// Store keeps configuration backups and the audit log on disk. Backups hold the
// plaintext credentials of the Kea configuration, so all files are private.
type Store struct {
	dir string
	mu  sync.Mutex
}

// NewStore prepares the data directory.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "backups"), 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	return &Store{dir: dir}, nil
}

// SaveBackup stores a configuration and returns the file name.
func (s *Store) SaveBackup(reason string, config []byte, now time.Time) (string, error) {
	now = now.UTC()
	name := fmt.Sprintf("kea-config-%s%03dZ-%s.json", now.Format("20060102T150405"), now.Nanosecond()/1e6, reason)
	f, err := os.OpenFile(filepath.Join(s.dir, "backups", name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(config); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return "", err
	}
	return name, f.Close()
}

// BackupInfo describes a stored backup.
type BackupInfo struct {
	Name string    `json:"name"`
	Size int64     `json:"size"`
	Time time.Time `json:"time"`
}

// Backups lists the stored backups, newest first.
func (s *Store) Backups() ([]BackupInfo, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "backups"))
	if err != nil {
		return nil, err
	}
	out := []BackupInfo{}
	for _, e := range entries {
		if !backupName.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, BackupInfo{e.Name(), info.Size(), info.ModTime()})
	}
	slices.SortFunc(out, func(a, b BackupInfo) int { return strings.Compare(b.Name, a.Name) })
	return out, nil
}

// BackupPath resolves a backup name to its file.
func (s *Store) BackupPath(name string) (string, error) {
	if !backupName.MatchString(name) {
		return "", ErrInvalidBackup
	}
	return filepath.Join(s.dir, "backups", name), nil
}

// AuditEntry records one change attempt.
type AuditEntry struct {
	Time   time.Time `json:"time"`
	User   string    `json:"user"`
	Action string    `json:"action"`
	Target string    `json:"target"`
	Status Status    `json:"status"`
	Backup string    `json:"backup,omitempty"`
	Steps  []Step    `json:"steps,omitempty"`
}

func (s *Store) auditPath() string { return filepath.Join(s.dir, "audit.log") }

// Audit appends an entry to the audit log.
func (s *Store) Audit(e AuditEntry) error {
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.auditPath(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// AuditTail returns the newest entries, newest first.
func (s *Store) AuditTail(limit int) ([]AuditEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []AuditEntry{}
	f, err := os.Open(s.auditPath())
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		var e AuditEntry
		if json.Unmarshal(scanner.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	slices.Reverse(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
