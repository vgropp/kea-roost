package web

import (
	"image/png"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/boombuler/barcode"
	"github.com/boombuler/barcode/qr"

	auth_ "github.com/vgropp/kea-roost/internal/auth"
)

// totpState holds what lives between two steps: a login waiting for its code, a
// secret waiting for its first code, and the last step used per user against replays.
type totpState struct {
	mu       sync.Mutex
	tickets  map[string]totpTicket
	enroll   map[string]enrolment
	lastStep map[string]uint64
}

// enrolment is a secret waiting for its first code. It is dropped after a while, and
// when the user logs out, gets a new password or is removed.
type enrolment struct {
	secret  string
	expires time.Time
}

const enrolmentLifetime = 10 * time.Minute

func (t *totpState) forget(user string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.enroll, strings.ToLower(user))
}

// dropTickets ends the logins of a user that wait for a code, after the password
// they started with is no longer valid.
func (t *totpState) dropTickets(user string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for token, ticket := range t.tickets {
		if strings.EqualFold(ticket.user, user) {
			delete(t.tickets, token)
		}
	}
}

func (t *totpState) pending(user string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := strings.ToLower(user)
	e, ok := t.enroll[key]
	if !ok || time.Now().After(e.expires) {
		delete(t.enroll, key)
		return "", false
	}
	return e.secret, true
}

type totpTicket struct {
	user    string
	expires time.Time
}

func newTOTPState() *totpState {
	return &totpState{tickets: map[string]totpTicket{}, enroll: map[string]enrolment{}, lastStep: map[string]uint64{}}
}

func (t *totpState) ticket(user string) (string, error) {
	token, err := auth_.RandomToken()
	if err != nil {
		return "", err
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	for key, old := range t.tickets {
		if now.After(old.expires) {
			delete(t.tickets, key)
		}
	}
	t.tickets[token] = totpTicket{user, now.Add(totpTicketLifetime)}
	return token, nil
}

// check verifies a code and remembers its step, so that it works only once.
func (t *totpState) check(user, secret, code string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := strings.ToLower(user)
	step, ok := auth_.CheckTOTP(secret, code, time.Now(), t.lastStep[key])
	if ok {
		t.lastStep[key] = step
	}
	return ok
}

func (s *server) loginTOTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Ticket string `json:"ticket"`
		Code   string `json:"code"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	s.totp.mu.Lock()
	ticket, ok := s.totp.tickets[in.Ticket]
	s.totp.mu.Unlock()
	if !ok || time.Now().After(ticket.expires) {
		writeJSON(w, http.StatusUnauthorized, errorBody{Error: "the login expired, enter the password again"})
		return
	}
	address, key := s.clientKey(r), userKey(ticket.user)
	if !s.attempt(w, address, key) {
		return
	}
	u, known := s.auth.Users.Get(ticket.user)
	if !known || u.TOTP == "" || !s.totp.check(u.Name, u.TOTP, in.Code) {
		writeJSON(w, http.StatusUnauthorized, errorBody{Error: "wrong code"})
		return
	}
	s.totp.mu.Lock()
	delete(s.totp.tickets, in.Ticket)
	s.totp.mu.Unlock()
	s.limiter.Succeeded(key, address)
	if err := s.startSession(w, r, u.Name); err != nil {
		writeError(w, err)
		return
	}
	s.record(u.Name, "login", u.Name+" (with TOTP)")
	writeJSON(w, http.StatusOK, map[string]any{"user": u.Name, "role": u.Role})
}

func (s *server) accountInfo(w http.ResponseWriter, r *http.Request) {
	a, _ := r.Context().Value(contextKey{}).(account)
	u, _ := s.auth.Users.Get(a.name)
	writeJSON(w, http.StatusOK, map[string]any{"user": u.Name, "role": u.Role, "totp": u.TOTP != ""})
}

// startTOTP makes a new secret. It only becomes active once confirmTOTP gets a matching
// code, so that a mistyped setup cannot lock anybody out.
func (s *server) startTOTP(w http.ResponseWriter, r *http.Request) {
	name := user(r)
	if u, _ := s.auth.Users.Get(name); u.TOTP != "" {
		writeJSON(w, http.StatusConflict, errorBody{Error: "TOTP is on already; turn it off first"})
		return
	}
	secret, err := auth_.NewTOTPSecret()
	if err != nil {
		writeError(w, err)
		return
	}
	s.totp.mu.Lock()
	s.totp.enroll[strings.ToLower(name)] = enrolment{secret, time.Now().Add(enrolmentLifetime)}
	s.totp.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"secret": secret, "uri": auth_.TOTPURI(totpIssuer, name, secret)})
}

func (s *server) totpQR(w http.ResponseWriter, r *http.Request) {
	name := user(r)
	secret, ok := s.totp.pending(name)
	if !ok {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "no TOTP setup is pending"})
		return
	}
	code, err := qr.Encode(auth_.TOTPURI(totpIssuer, name, secret), qr.M, qr.Auto)
	if err == nil {
		code, err = barcode.Scale(code, 240, 240)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	if err := png.Encode(w, code); err != nil {
		slog.Warn("cannot send the QR code", "err", err)
	}
}

// confirmTOTP turns the second factor on. It needs the password, so that a session left
// open cannot be used to put another device in place.
func (s *server) confirmTOTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code     string `json:"code"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	name := user(r)
	if u, _ := s.auth.Users.Get(name); u.TOTP != "" {
		writeJSON(w, http.StatusConflict, errorBody{Error: "TOTP is on already; turn it off first"})
		return
	}
	if !s.checkOwnPassword(w, r, name, in.Password) {
		return
	}
	secret, ok := s.totp.pending(name)
	if !ok || !s.totp.check(name, secret, in.Code) {
		writeJSON(w, http.StatusUnprocessableEntity, errorBody{Error: "invalid input", Problems: []string{"the code does not match, check the time of the device"}})
		return
	}
	if err := s.auth.Users.SetTOTP(name, secret); err != nil {
		writeUserError(w, err)
		return
	}
	s.totp.forget(name)
	s.record(name, "totp-on", name)
	w.WriteHeader(http.StatusNoContent)
}

// removeTOTP needs the password and a current code: the second factor is meant to
// hold even when the password is known.
func (s *server) removeTOTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	name := user(r)
	if !s.checkOwnPassword(w, r, name, in.Password) {
		return
	}
	u, _ := s.auth.Users.Get(name)
	if u.TOTP != "" && !s.totp.check(name, u.TOTP, in.Code) {
		writeJSON(w, http.StatusUnprocessableEntity, errorBody{Error: "invalid input", Problems: []string{"the code is wrong"}})
		return
	}
	if err := s.auth.Users.SetTOTP(name, ""); err != nil {
		writeUserError(w, err)
		return
	}
	s.record(name, "totp-off", name)
	w.WriteHeader(http.StatusNoContent)
}
