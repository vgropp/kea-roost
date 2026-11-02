package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHashAndCheck(t *testing.T) {
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("hash: %s", hash)
	}
	if ok, err := checkPassword(hash, "correct horse battery"); !ok || err != nil {
		t.Fatalf("right password: %v %v", ok, err)
	}
	if ok, _ := checkPassword(hash, "correct horse batter"); ok {
		t.Fatal("wrong password accepted")
	}
	other, _ := HashPassword("correct horse battery")
	if other == hash {
		t.Fatal("the salt must differ")
	}
	for _, bad := range []string{"", "plain", "$argon2id$v=19$m=0,t=2,p=1$AAAA$AAAA", "$bcrypt$x$y$z$w"} {
		if _, err := checkPassword(bad, "x"); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPasswordRules(t *testing.T) {
	if err := CheckPasswordRules("admin", "short"); err == nil {
		t.Error("short password accepted")
	}
	if err := CheckPasswordRules("administrator1", "Administrator1"); err == nil {
		t.Error("user name as password accepted")
	}
	if err := CheckPasswordRules("admin", strings.Repeat("x", 300)); err == nil {
		t.Error("overlong password accepted")
	}
	if err := CheckPasswordRules("admin", "a long passphrase"); err != nil {
		t.Error(err)
	}
}

func TestUsersLifecycle(t *testing.T) {
	dir := t.TempDir()
	u, err := LoadUsers(dir)
	if err != nil || !u.Empty() {
		t.Fatalf("%v %v", u, err)
	}
	now := time.Now()
	if err := u.AddFirst("Admin", "a long passphrase", now); err != nil {
		t.Fatal(err)
	}
	if err := u.AddFirst("other", "a long passphrase", now); !errors.Is(err, ErrUserExists) {
		t.Fatalf("second first user: %v", err)
	}
	if err := u.Add("viewer", RoleViewer, "another passphrase", now); err != nil {
		t.Fatal(err)
	}
	if err := u.Add("ADMIN", RoleViewer, "another passphrase", now); !errors.Is(err, ErrUserExists) {
		t.Fatalf("names ignore case: %v", err)
	}
	for _, bad := range []string{"", "a b", "../x", strings.Repeat("a", 70)} {
		if err := u.Add(bad, RoleViewer, "another passphrase", now); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	if _, ok := u.Verify("admin", "a long passphrase"); !ok {
		t.Fatal("login with other case failed")
	}
	if _, ok := u.Verify("admin", "wrong passphrase"); ok {
		t.Fatal("wrong password accepted")
	}
	if _, ok := u.Verify("nobody", "a long passphrase"); ok {
		t.Fatal("unknown user accepted")
	}
	if err := u.Update("Admin", RoleViewer, "", false); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("demote last admin: %v", err)
	}
	if err := u.Delete("Admin"); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("delete last admin: %v", err)
	}
	if err := u.Update("viewer", "", "a new passphrase", false); err != nil {
		t.Fatal(err)
	}
	if err := u.Delete("viewer"); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, usersFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", info, err)
	}
	again, err := LoadUsers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := again.Verify("admin", "a long passphrase"); !ok || len(again.List()) != 1 {
		t.Fatal("not saved")
	}
	for _, user := range again.List() {
		if user.Hash != "" {
			t.Fatal("List gives away hashes")
		}
	}
}

func TestBrokenUsersFileStopsTheStart(t *testing.T) {
	dir := t.TempDir()
	for _, content := range []string{"{", `[{"name":"a b","role":"admin"}]`, `[{"name":"a","role":"root"}]`, `[{"name":"a","role":"admin"},{"name":"A","role":"viewer"}]`} {
		os.WriteFile(filepath.Join(dir, usersFile), []byte(content), 0o600)
		if _, err := LoadUsers(dir); err == nil {
			t.Errorf("%s accepted", content)
		}
	}
}

func TestSessions(t *testing.T) {
	now := time.Now()
	s := NewSessions(time.Hour, 24*time.Hour, func() time.Time { return now })
	token, err := s.Create("admin")
	if err != nil {
		t.Fatal(err)
	}
	if user, ok := s.Get(token); !ok || user != "admin" {
		t.Fatal("session lost")
	}
	if _, ok := s.Get(token + "x"); ok {
		t.Fatal("wrong token accepted")
	}
	now = now.Add(59 * time.Minute)
	if _, ok := s.Get(token); !ok {
		t.Fatal("ended before the idle time")
	}
	now = now.Add(61 * time.Minute)
	if _, ok := s.Get(token); ok {
		t.Fatal("idle session still valid")
	}

	token, _ = s.Create("admin")
	for range 30 {
		now = now.Add(50 * time.Minute)
		s.Get(token)
	}
	if _, ok := s.Get(token); ok {
		t.Fatal("session outlived the absolute limit")
	}

	a, _ := s.Create("Admin")
	b, _ := s.Create("viewer")
	s.DeleteUser("admin")
	if _, ok := s.Get(a); ok {
		t.Fatal("session of a removed user")
	}
	if _, ok := s.Get(b); !ok {
		t.Fatal("other user logged out")
	}
	s.Delete(b)
	if _, ok := s.Get(b); ok {
		t.Fatal("logout failed")
	}
	if _, ok := s.Get(""); ok {
		t.Fatal("empty token accepted")
	}
}

func TestLimiter(t *testing.T) {
	now := time.Now()
	limit := 3
	l := NewLimiter(func() (int, time.Duration) { return limit, 15 * time.Minute }, func() time.Time { return now })
	for range 3 {
		if _, ok := l.Attempt("ip", "user"); !ok {
			t.Fatal("blocked too early")
		}
	}
	wait, ok := l.Attempt("other-ip", "user")
	if ok || wait != 15*time.Minute {
		t.Fatalf("user not blocked: %v %v", wait, ok)
	}
	if _, ok := l.Attempt("other-ip"); !ok {
		t.Fatal("a blocked attempt must not count against the other key")
	}
	now = now.Add(15 * time.Minute)
	if _, ok := l.Attempt("ip", "user"); !ok {
		t.Fatal("block outlived the window")
	}

	// two wrong guesses, then a login with an account of its own
	now = now.Add(15 * time.Minute)
	l.Attempt("ip", "victim")
	l.Attempt("ip", "victim")
	l.Attempt("ip", "own account")
	l.Succeeded("own account", "ip")
	if _, ok := l.Attempt("ip", "victim"); !ok {
		t.Fatal("the third guess is still allowed")
	}
	if _, ok := l.Attempt("ip", "other victim"); ok {
		t.Fatal("a login of its own reset the count of the address")
	}

	limit = 0
	if _, ok := l.Attempt("ip"); !ok {
		t.Fatal("a limit of zero still blocks")
	}
}

func TestLimiterKeysAreBounded(t *testing.T) {
	now := time.Now()
	l := NewLimiter(func() (int, time.Duration) { return 3, time.Minute }, func() time.Time { return now })
	l.Attempt(strings.Repeat("x", 10000))
	for key := range l.attempts {
		if len(key) > maxKeyLength {
			t.Fatal("a long key is kept")
		}
	}
	for i := range maxKeys {
		l.attempts[fmt.Sprint(i)] = []time.Time{now}
	}
	if _, ok := l.Attempt("new key"); ok {
		t.Fatal("a full map takes new keys")
	}
	now = now.Add(time.Minute)
	if _, ok := l.Attempt("new key"); !ok {
		t.Fatal("old keys are not cleared")
	}
}

func TestTOTPIsSavedButNotListed(t *testing.T) {
	dir := t.TempDir()
	u, _ := LoadUsers(dir)
	u.AddFirst("admin", "a long passphrase", time.Now())
	if err := u.SetTOTP("ADMIN", "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatal(err)
	}
	again, _ := LoadUsers(dir)
	if user, _ := again.Get("admin"); user.TOTP != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("not saved: %+v", user)
	}
	if list := again.List(); !list[0].MFA || list[0].TOTP != "" {
		t.Fatalf("list: %+v", list)
	}
	if err := again.SetTOTP("nobody", ""); !errors.Is(err, ErrUnknownUser) {
		t.Fatalf("got %v", err)
	}
}

func TestOnlyOneFirstAdmin(t *testing.T) {
	u, _ := LoadUsers(t.TempDir())
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() { u.AddFirst(fmt.Sprintf("admin%d", i), "a long passphrase", time.Now()) })
	}
	wg.Wait()
	if n := len(u.List()); n != 1 {
		t.Fatalf("%d first admins", n)
	}
}

func TestSessionsPerUserAreBounded(t *testing.T) {
	now := time.Now()
	s := NewSessions(time.Hour, 24*time.Hour, func() time.Time { now = now.Add(time.Second); return now })
	first, err := s.Create("admin")
	if err != nil {
		t.Fatal(err)
	}
	for range maxSessionsPerUser {
		if _, err := s.Create("admin"); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := s.Get(first); ok {
		t.Fatal("the oldest session survived")
	}
	if len(s.byHash) != maxSessionsPerUser {
		t.Fatalf("%d sessions", len(s.byHash))
	}
}
