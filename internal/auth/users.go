package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Role decides what a user may do.
type Role string

// Roles of users.
const (
	RoleAdmin  Role = "admin"
	RoleViewer Role = "viewer"
)

const usersFile = "users.json"

var userName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._@-]{0,63}$`)

// Errors a caller can tell apart.
var (
	ErrUnknownUser = errors.New("unknown user")
	ErrLastAdmin   = errors.New("the last admin cannot be removed or demoted")
	ErrUserExists  = errors.New("the user already exists")
)

// User is an account of the interface.
type User struct {
	Name    string    `json:"name"`
	Role    Role      `json:"role"`
	Hash    string    `json:"hash"`
	Created time.Time `json:"created"`
	// TOTP is the secret of the second factor, if the user turned it on.
	TOTP string `json:"totp,omitempty"`
	// MFA is only filled in for lists, which hold no secrets.
	MFA bool `json:"mfa,omitempty"`
}

// Users keeps the accounts in a file in the data directory. An administrator who
// lost every password deletes the file and sets up a new account.
type Users struct {
	path  string
	mu    sync.Mutex
	users map[string]User
	// dummy is checked for an unknown name, so that a login takes as long either way.
	dummy string
	// slots limits parallel hashing: each one takes memory, see password.go
	slots chan struct{}
}

const parallelHashes = 4

func (u *Users) hashSlot() func() {
	u.slots <- struct{}{}
	return func() { <-u.slots }
}

// LoadUsers reads the users of a data directory. A missing file means no users yet.
func LoadUsers(dir string) (*Users, error) {
	dummy, err := HashPassword("not a password of anyone")
	if err != nil {
		return nil, err
	}
	u := &Users{path: filepath.Join(dir, usersFile), users: map[string]User{}, dummy: dummy, slots: make(chan struct{}, parallelHashes)}
	data, err := os.ReadFile(u.path)
	if errors.Is(err, fs.ErrNotExist) {
		return u, nil
	}
	if err != nil {
		return nil, err
	}
	var list []User
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", usersFile, err)
	}
	for _, user := range list {
		if !userName.MatchString(user.Name) || (user.Role != RoleAdmin && user.Role != RoleViewer) {
			return nil, fmt.Errorf("%s: invalid user %q", usersFile, user.Name)
		}
		key := strings.ToLower(user.Name)
		if _, twice := u.users[key]; twice {
			return nil, fmt.Errorf("%s: user %q appears twice", usersFile, user.Name)
		}
		u.users[key] = user
	}
	return u, nil
}

func (u *Users) save(next map[string]User) error {
	list := slices.SortedFunc(maps.Values(next), func(a, b User) int { return strings.Compare(a.Name, b.Name) })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := writeDurably(u.path, append(data, '\n')); err != nil {
		return err
	}
	u.users = next
	return nil
}

// writeDurably replaces a file in one step and makes sure it reached the disk, so that
// a crash leaves either the old or the new accounts, never a broken file.
func writeDurably(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Empty reports whether no account exists yet.
func (u *Users) Empty() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.users) == 0
}

// List returns the accounts without their hashes.
func (u *Users) List() []User {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]User, 0, len(u.users))
	for _, user := range u.users {
		user.MFA = user.TOTP != ""
		user.Hash, user.TOTP = "", ""
		out = append(out, user)
	}
	slices.SortFunc(out, func(a, b User) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Get returns an account by name, ignoring case.
func (u *Users) Get(name string) (User, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	user, ok := u.users[strings.ToLower(name)]
	return user, ok
}

// Verify checks a login. An unknown name costs as much time as a wrong password.
func (u *Users) Verify(name, password string) (User, bool) {
	user, ok := u.Get(name)
	hash := u.dummy
	if ok {
		hash = user.Hash
	}
	defer u.hashSlot()()
	match, err := checkPassword(hash, password)
	if !ok || err != nil || !match {
		return User{}, false
	}
	return user, true
}

// Add creates an account.
func (u *Users) Add(name string, role Role, password string, now time.Time) error {
	return u.add(name, role, password, now, false)
}

// AddFirst creates the first admin, and only while no account exists. The check and
// the creation share one lock, so that two requests cannot both become the first.
func (u *Users) AddFirst(name, password string, now time.Time) error {
	return u.add(name, RoleAdmin, password, now, true)
}

func (u *Users) add(name string, role Role, password string, now time.Time, first bool) error {
	if !userName.MatchString(name) {
		return fmt.Errorf("invalid user name %q: letters, digits and . _ @ - only", name)
	}
	if role != RoleAdmin && role != RoleViewer {
		return fmt.Errorf("unknown role %q", role)
	}
	if err := CheckPasswordRules(name, password); err != nil {
		return err
	}
	release := u.hashSlot()
	hash, err := HashPassword(password)
	release()
	if err != nil {
		return err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	key := strings.ToLower(name)
	if _, ok := u.users[key]; ok || first && len(u.users) > 0 {
		return ErrUserExists
	}
	next := maps.Clone(u.users)
	next[key] = User{Name: name, Role: role, Hash: hash, Created: now.UTC()}
	return u.save(next)
}

func (u *Users) admins(users map[string]User) int {
	n := 0
	for _, user := range users {
		if user.Role == RoleAdmin {
			n++
		}
	}
	return n
}

// Update changes the role or the password of an account and can turn its second factor
// off, all in one save. Empty values stay.
func (u *Users) Update(name string, role Role, password string, resetTOTP bool) error {
	if role != "" && role != RoleAdmin && role != RoleViewer {
		return fmt.Errorf("unknown role %q", role)
	}
	hash := ""
	if password != "" {
		if err := CheckPasswordRules(name, password); err != nil {
			return err
		}
		var err error
		release := u.hashSlot()
		hash, err = HashPassword(password)
		release()
		if err != nil {
			return err
		}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	key := strings.ToLower(name)
	user, ok := u.users[key]
	if !ok {
		return ErrUnknownUser
	}
	next := maps.Clone(u.users)
	if role != "" {
		user.Role = role
	}
	if hash != "" {
		user.Hash = hash
	}
	if resetTOTP {
		user.TOTP = ""
	}
	next[key] = user
	if u.admins(next) == 0 {
		return ErrLastAdmin
	}
	return u.save(next)
}

// SetTOTP turns the second factor on with a secret, or off with an empty one.
func (u *Users) SetTOTP(name, secret string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	key := strings.ToLower(name)
	user, ok := u.users[key]
	if !ok {
		return ErrUnknownUser
	}
	next := maps.Clone(u.users)
	user.TOTP = secret
	next[key] = user
	return u.save(next)
}

// Delete removes an account. The last admin stays.
func (u *Users) Delete(name string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	key := strings.ToLower(name)
	if _, ok := u.users[key]; !ok {
		return ErrUnknownUser
	}
	next := maps.Clone(u.users)
	delete(next, key)
	if u.admins(next) == 0 {
		return ErrLastAdmin
	}
	return u.save(next)
}
