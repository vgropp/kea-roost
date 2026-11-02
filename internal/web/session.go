package web

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	auth_ "github.com/vgropp/kea-roost/internal/auth"
	"github.com/vgropp/kea-roost/internal/manager"
)

const (
	sessionCookie   = "kea_roost_session"
	sessionIdle     = 12 * time.Hour
	sessionAbsolute = 7 * 24 * time.Hour
	// a password was right, the second factor has this long to follow
	totpTicketLifetime = 5 * time.Minute
	totpIssuer         = "Kea Roost"
)

func currentRole(r *http.Request) auth_.Role {
	a, _ := r.Context().Value(contextKey{}).(account)
	return a.role
}

// account returns the user of the session of a request. The role is read from the
// accounts each time, so that a change applies at once.
func (s *server) account(r *http.Request) (account, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return account{}, false
	}
	name, ok := s.sessions.Get(cookie.Value)
	if !ok {
		return account{}, false
	}
	u, ok := s.auth.Users.Get(name)
	if !ok {
		return account{}, false
	}
	return account{u.Name, u.Role}, true
}

// secure tells whether the browser reached the manager over HTTPS, directly or through
// a proxy. Only then may the cookie be limited to HTTPS.
func secure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *server) startSession(w http.ResponseWriter, r *http.Request, name string) error {
	// a new login replaces the session the browser had before
	if old, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.Delete(old.Value)
	}
	token, err := s.sessions.Create(name)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", MaxAge: int(sessionAbsolute.Seconds()),
		HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteStrictMode,
	})
	return nil
}

func clearSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteStrictMode})
}

// clientKey is the address a login counts for. A trusted proxy names the client in
// X-Forwarded-For; the list is read from the right, where the proxies closest to the
// manager append, and a client cannot forge entries past them.
func (s *server) clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	client := host
	trusted := func(text string) bool {
		addr, err := netip.ParseAddr(strings.TrimSpace(text))
		return err == nil && slices.ContainsFunc(s.auth.TrustedProxies, func(p netip.Prefix) bool { return p.Contains(addr.Unmap()) })
	}
	if trusted(host) {
		hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		for i := len(hops) - 1; i >= 0; i-- {
			hop := strings.TrimSpace(hops[i])
			if hop == "" {
				break
			}
			client = hop
			if !trusted(hop) {
				break
			}
		}
	}
	return "address " + client
}

func userKey(name string) string { return "user " + strings.ToLower(name) }

// attempt counts a login attempt and answers 429 when the limit is reached.
func (s *server) attempt(w http.ResponseWriter, keys ...string) bool {
	if wait, ok := s.limiter.Attempt(keys...); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, errorBody{Error: "too many failed attempts, try again later"})
		return false
	}
	return true
}

// checkOwnPassword verifies the password of the logged-in user before a sensitive
// change, with the same limits as a login.
func (s *server) checkOwnPassword(w http.ResponseWriter, r *http.Request, name, password string) bool {
	address, user := s.clientKey(r), userKey(name)
	if !s.attempt(w, address, user) {
		return false
	}
	if _, ok := s.auth.Users.Verify(name, password); !ok {
		writeJSON(w, http.StatusUnprocessableEntity, errorBody{Error: "invalid input", Problems: []string{"the password is wrong"}})
		return false
	}
	s.limiter.Succeeded(user, address)
	return true
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodySize)).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid JSON"})
		return false
	}
	return true
}

func (s *server) record(user, action, target string) {
	err := s.mgr.Store().Audit(manager.AuditEntry{Time: time.Now(), User: user, Action: action, Target: target, Status: manager.StatusOK})
	if err != nil {
		slog.Error("audit log", "error", err)
	}
}

func (s *server) session(w http.ResponseWriter, r *http.Request) {
	a, ok := s.account(r)
	body := map[string]any{"setup": s.auth.Users.Empty()}
	if ok {
		body["user"], body["role"] = a.name, a.role
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	address, user := s.clientKey(r), userKey(in.Name)
	if !s.attempt(w, address, user) {
		return
	}
	u, ok := s.auth.Users.Verify(in.Name, in.Password)
	if !ok {
		// a password typed into the name field must not end up in the log
		name := "(unknown user)"
		if known, ok := s.auth.Users.Get(in.Name); ok {
			name = known.Name
		}
		slog.Warn("failed login", "user", name, "address", strings.TrimPrefix(address, "address "))
		writeJSON(w, http.StatusUnauthorized, errorBody{Error: "wrong user name or password"})
		return
	}
	if u.TOTP != "" {
		ticket, err := s.totp.ticket(u.Name)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"totp": true, "ticket": ticket})
		return
	}
	s.limiter.Succeeded(user, address)
	if err := s.startSession(w, r, u.Name); err != nil {
		writeError(w, err)
		return
	}
	s.record(u.Name, "login", u.Name)
	writeJSON(w, http.StatusOK, map[string]any{"user": u.Name, "role": u.Role})
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	if a, ok := s.account(r); ok {
		s.totp.forget(a.name)
	}
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.Delete(cookie.Value)
	}
	clearSession(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// setup creates the first admin. It needs the code from the log, so that nobody on the
// network can claim a fresh instance before its owner does.
func (s *server) setup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code     string `json:"code"`
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !s.auth.Users.Empty() || s.auth.SetupCode == "" {
		writeJSON(w, http.StatusConflict, errorBody{Error: "an account exists already"})
		return
	}
	address := s.clientKey(r)
	if !s.attempt(w, address) {
		return
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(in.Code)), []byte(s.auth.SetupCode)) != 1 {
		writeJSON(w, http.StatusUnauthorized, errorBody{Error: "wrong setup code, it is printed in the log of Kea Roost"})
		return
	}
	s.limiter.Succeeded("", address)
	if err := s.auth.Users.AddFirst(in.Name, in.Password, time.Now()); err != nil {
		writeUserError(w, err)
		return
	}
	if err := s.startSession(w, r, in.Name); err != nil {
		writeError(w, err)
		return
	}
	s.record(in.Name, "user-create", in.Name+" (admin, first account)")
	writeJSON(w, http.StatusOK, map[string]any{"user": in.Name, "role": auth_.RoleAdmin})
}

func writeUserError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth_.ErrUnknownUser):
		writeJSON(w, http.StatusNotFound, errorBody{Error: err.Error()})
	case errors.Is(err, auth_.ErrUserExists), errors.Is(err, auth_.ErrLastAdmin):
		writeJSON(w, http.StatusConflict, errorBody{Error: err.Error()})
	default:
		writeJSON(w, http.StatusUnprocessableEntity, errorBody{Error: "invalid input", Problems: []string{err.Error()}})
	}
}

// changePassword lets every user set a new password. Other sessions of the user end.
func (s *server) changePassword(w http.ResponseWriter, r *http.Request) {
	a, ok := r.Context().Value(contextKey{}).(account)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, errorBody{Error: "login required"})
		return
	}
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !s.checkOwnPassword(w, r, a.name, in.Current) {
		return
	}
	if err := s.auth.Users.Update(a.name, "", in.New, false); err != nil {
		writeUserError(w, err)
		return
	}
	s.sessions.DeleteUser(a.name)
	s.totp.forget(a.name)
	s.totp.dropTickets(a.name)
	if err := s.startSession(w, r, a.name); err != nil {
		writeError(w, err)
		return
	}
	s.record(a.name, "password-change", a.name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) listUsers(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.auth.Users.List())
}

type userInput struct {
	Name     string     `json:"name"`
	Role     auth_.Role `json:"role"`
	Password string     `json:"password"`
	// ResetTOTP turns off the second factor of a user who lost the device.
	ResetTOTP bool `json:"resetTotp"`
}

func (s *server) createUser(w http.ResponseWriter, r *http.Request) {
	var in userInput
	if !readJSON(w, r, &in) {
		return
	}
	if err := s.auth.Users.Add(in.Name, in.Role, in.Password, time.Now()); err != nil {
		writeUserError(w, err)
		return
	}
	s.record(user(r), "user-create", in.Name+" ("+string(in.Role)+")")
	writeJSON(w, http.StatusOK, s.auth.Users.List())
}

func (s *server) updateUser(w http.ResponseWriter, r *http.Request) {
	var in userInput
	if !readJSON(w, r, &in) {
		return
	}
	name := r.PathValue("name")
	// the own account goes through the account menu, which asks for the password and
	// the code, so that an open session alone cannot replace them
	if strings.EqualFold(name, user(r)) && (in.Password != "" || in.ResetTOTP) {
		writeJSON(w, http.StatusConflict, errorBody{Error: "change your own password and 2FA in the account menu"})
		return
	}
	if err := s.auth.Users.Update(name, in.Role, in.Password, in.ResetTOTP); err != nil {
		writeUserError(w, err)
		return
	}
	changes := []string{}
	if in.Password != "" || in.ResetTOTP {
		s.totp.forget(name)
		s.totp.dropTickets(name)
	}
	if in.ResetTOTP {
		changes = append(changes, "2FA reset")
	}
	if in.Role != "" {
		changes = append(changes, "role "+string(in.Role))
	}
	if in.Password != "" {
		// somebody else set the password, so the sessions with the old one end
		s.sessions.DeleteUser(name)
		changes = append(changes, "new password")
	}
	s.record(user(r), "user-update", name+" ("+strings.Join(changes, ", ")+")")
	writeJSON(w, http.StatusOK, s.auth.Users.List())
}

func (s *server) deleteUser(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if strings.EqualFold(name, user(r)) {
		writeJSON(w, http.StatusConflict, errorBody{Error: "you cannot remove your own account"})
		return
	}
	if err := s.auth.Users.Delete(name); err != nil {
		writeUserError(w, err)
		return
	}
	s.sessions.DeleteUser(name)
	s.totp.forget(name)
	s.totp.dropTickets(name)
	s.record(user(r), "user-delete", name)
	writeJSON(w, http.StatusOK, s.auth.Users.List())
}
