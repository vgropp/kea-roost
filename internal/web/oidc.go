package web

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	auth_ "github.com/vgropp/kea-roost/internal/auth"
)

// OIDC is the single sign-on through an OpenID Connect provider such as Authelia.
type OIDC struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	// RedirectURL is the callback of Kea Roost as the provider knows it.
	RedirectURL string
	// Label names the provider on the login button.
	Label string
	// AdminGroup and ViewerGroup give the role at each login. Without them only
	// accounts that exist already log in, with the role they have.
	AdminGroup  string
	ViewerGroup string
	// LinkByName links the first login to an account of the same name that has
	// no link yet. Afterwards only the link counts, so a renamed login stays.
	LinkByName bool
	// HTTPClient reaches the provider; it carries the timeout and the trusted CAs.
	HTTPClient *http.Client
}

const (
	oidcCookie = "kea_roost_oidc"
	// the login at the provider has this long, including a second factor there
	oidcLoginLifetime = 10 * time.Minute
	// bound the logins started and not finished, so that nobody can fill the memory
	// or keep the others out
	maxPendingLogins    = 1000
	maxPendingPerClient = 10
)

// oidcState holds the provider, found on first use, and the logins on their way.
type oidcState struct {
	cfg OIDC

	mu       sync.Mutex
	provider *oidc.Provider
	pending  map[string]pendingLogin
	links    map[string]linkTicket
}

type pendingLogin struct {
	nonce    string
	verifier string
	expires  time.Time
	// client is the address that started the login
	client string
	// link names the account that asked to be linked, and session the session it
	// asked from, which has to be valid still when the provider sends it back
	link, session string
}

// linkTicket lets one link start, after the password and code of the account.
type linkTicket struct {
	user, session string
	expires       time.Time
}

const linkTicketLifetime = 2 * time.Minute

func newOIDCState(cfg *OIDC) *oidcState {
	if cfg == nil {
		return nil
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &oidcState{cfg: *cfg, pending: map[string]pendingLogin{}, links: map[string]linkTicket{}}
}

// discover finds the provider. A provider that is down at the start of Kea Roost
// is tried again at the next login.
func (o *oidcState) discover() (*oidc.Provider, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.provider != nil {
		return o.provider, nil
	}
	// the context stays: go-oidc uses it later to fetch new signing keys
	ctx := oidc.ClientContext(context.Background(), o.cfg.HTTPClient)
	p, err := oidc.NewProvider(ctx, o.cfg.Issuer)
	if err != nil {
		return nil, err
	}
	o.provider = p
	return p, nil
}

func (o *oidcState) oauth(p *oidc.Provider) *oauth2.Config {
	return &oauth2.Config{
		ClientID: o.cfg.ClientID, ClientSecret: o.cfg.ClientSecret, RedirectURL: o.cfg.RedirectURL,
		Endpoint: p.Endpoint(), Scopes: []string{oidc.ScopeOpenID, "profile", "groups"},
	}
}

// remember keeps a login until the provider sends the browser back. Instead of
// refusing a new one when full, it drops the oldest, first of the same address:
// a flood then only costs logins that were left open, never all new ones.
func (o *oidcState) remember(state string, login pendingLogin) {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now()
	oldest := func(match func(pendingLogin) bool) (string, int) {
		key, n := "", 0
		for k, p := range o.pending {
			if !match(p) {
				continue
			}
			n++
			if key == "" || p.expires.Before(o.pending[key].expires) {
				key = k
			}
		}
		return key, n
	}
	for key, p := range o.pending {
		if now.After(p.expires) {
			delete(o.pending, key)
		}
	}
	if key, n := oldest(func(p pendingLogin) bool { return p.client == login.client }); n >= maxPendingPerClient {
		delete(o.pending, key)
	}
	if key, n := oldest(func(pendingLogin) bool { return true }); n >= maxPendingLogins {
		delete(o.pending, key)
	}
	o.pending[state] = login
}

func (o *oidcState) take(state string) (pendingLogin, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	p, ok := o.pending[state]
	delete(o.pending, state)
	return p, ok && time.Now().Before(p.expires)
}

func (o *oidcState) newLink(user, session string) (string, error) {
	ticket, err := auth_.RandomToken()
	if err != nil {
		return "", err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now()
	for key, t := range o.links {
		if now.After(t.expires) || strings.EqualFold(t.user, user) {
			delete(o.links, key)
		}
	}
	o.links[ticket] = linkTicket{user, session, now.Add(linkTicketLifetime)}
	return ticket, nil
}

func (o *oidcState) takeLink(ticket string) (linkTicket, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	t, ok := o.links[ticket]
	delete(o.links, ticket)
	return t, ok && time.Now().Before(t.expires)
}

// startLink checks the password and, if on, the TOTP code of the logged-in user,
// as a change of the password does, and returns a ticket for one link. So an open
// session alone cannot tie the account to a login at the provider.
func (s *server) startLink(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		http.NotFound(w, r)
		return
	}
	a, ok := r.Context().Value(contextKey{}).(account)
	cookie, err := r.Cookie(sessionCookie)
	if !ok || err != nil {
		writeJSON(w, http.StatusUnauthorized, errorBody{Error: "login required"})
		return
	}
	var in struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !s.checkOwnPassword(w, r, a.name, in.Password) {
		return
	}
	if u, _ := s.auth.Users.Get(a.name); u.TOTP != "" {
		address, key := s.clientKey(r), userKey(a.name)
		if !s.attempt(w, address, key) {
			return
		}
		if !s.totp.check(u.Name, u.TOTP, in.Code) {
			writeJSON(w, http.StatusUnprocessableEntity, errorBody{Error: "invalid input", Problems: []string{"the TOTP code is wrong"}})
			return
		}
		s.limiter.Succeeded(key, address)
	}
	ticket, err := s.oidc.newLink(a.name, cookie.Value)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ticket": ticket})
}

// The interface, seen from the callback and from the start of a login. Relative
// addresses keep it right below a base path, which the handlers do not see.
const (
	homeFromCallback = "../../../"
	homeFromStart    = "../../"
)

// startOIDC sends the browser to the provider. The state in a cookie binds the
// answer to this browser, the nonce binds the ID token to this login and PKCE the
// code to this server. With ?link=<ticket> from startLink, a logged-in user links
// the own account instead.
func (s *server) startOIDC(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		http.NotFound(w, r)
		return
	}
	login := pendingLogin{client: s.clientKey(r), expires: time.Now().Add(oidcLoginLifetime)}
	if ticket := r.URL.Query().Get("link"); ticket != "" {
		t, ok := s.oidc.takeLink(ticket)
		// the ticket only works in the browser of the session it was made for, so a
		// ticket that leaked, say from the history, is useless elsewhere
		cookie, err := r.Cookie(sessionCookie)
		if !ok || err != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(t.session)) != 1 {
			oidcFailed(w, r, homeFromStart, "expired")
			return
		}
		login.link, login.session = t.user, t.session
	}
	p, err := s.oidc.discover()
	if err != nil {
		slog.Error("single sign-on: provider not reachable", "issuer", s.oidc.cfg.Issuer, "error", err)
		oidcFailed(w, r, homeFromStart, "unreachable")
		return
	}
	state, err := auth_.RandomToken()
	if err != nil {
		writeError(w, err)
		return
	}
	if login.nonce, err = auth_.RandomToken(); err != nil {
		writeError(w, err)
		return
	}
	login.verifier = oauth2.GenerateVerifier()
	s.oidc.remember(state, login)
	// Lax, not Strict: the browser comes back from the provider, another site
	http.SetCookie(w, &http.Cookie{
		Name: oidcCookie, Value: state, Path: "/", MaxAge: int(oidcLoginLifetime.Seconds()),
		HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, s.oidc.oauth(p).AuthCodeURL(state, oidc.Nonce(login.nonce), oauth2.S256ChallengeOption(login.verifier)), http.StatusFound)
}

func clearOIDCCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: oidcCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteLaxMode})
}

// oidcFailed returns to the login form with a reason it knows. Nothing from the
// provider is shown, so that its answer cannot put text into the page.
func oidcFailed(w http.ResponseWriter, r *http.Request, home, reason string) {
	clearOIDCCookie(w, r)
	w.Header().Set("Location", home+"?sso="+reason)
	w.WriteHeader(http.StatusFound)
}

// oidcClaims are read from the UserInfo endpoint, where Authelia puts them, and
// from the ID token as well.
type oidcClaims struct {
	Subject string   `json:"sub"`
	Name    string   `json:"preferred_username"`
	Groups  []string `json:"groups"`
}

func (s *server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	if q.Get("error") != "" {
		// the user cancelled or the provider refused; its text stays out of the page
		slog.Warn("single sign-on refused by the provider", "error", q.Get("error"))
		oidcFailed(w, r, homeFromCallback, "denied")
		return
	}
	state := q.Get("state")
	cookie, err := r.Cookie(oidcCookie)
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(state)) != 1 {
		oidcFailed(w, r, homeFromCallback, "expired")
		return
	}
	pending, ok := s.oidc.take(state)
	if !ok {
		oidcFailed(w, r, homeFromCallback, "expired")
		return
	}
	p, err := s.oidc.discover()
	if err != nil {
		oidcFailed(w, r, homeFromCallback, "unreachable")
		return
	}
	ctx, cancel := context.WithTimeout(oidc.ClientContext(r.Context(), s.oidc.cfg.HTTPClient), 30*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, s.oidc.cfg.HTTPClient)
	claims, err := s.oidcLogin(ctx, p, q.Get("code"), pending)
	if err != nil {
		slog.Warn("single sign-on failed", "error", err)
		oidcFailed(w, r, homeFromCallback, "failed")
		return
	}
	if pending.link != "" {
		// the session cookie does not come along from the provider, so the session
		// the link was asked from is checked by the token kept for it
		if name, ok := s.sessions.Get(pending.session); !ok || !strings.EqualFold(name, pending.link) {
			oidcFailed(w, r, homeFromCallback, "expired")
			return
		}
		if reason := s.oidcLink(pending.link, claims); reason != "" {
			oidcFailed(w, r, homeFromCallback, reason)
			return
		}
		// the browser keeps its session; it was not sent along from the provider
		clearOIDCCookie(w, r)
		w.Header().Set("Location", homeFromCallback+"?sso=linked")
		w.WriteHeader(http.StatusFound)
		return
	}
	name, reason := s.oidcAccount(claims)
	if reason != "" {
		oidcFailed(w, r, homeFromCallback, reason)
		return
	}
	clearOIDCCookie(w, r)
	if err := s.startSession(w, r, name); err != nil {
		writeError(w, err)
		return
	}
	s.record(name, "login", name+" (single sign-on)")
	w.Header().Set("Location", homeFromCallback)
	w.WriteHeader(http.StatusFound)
}

// oidcLogin trades the code for tokens and checks them.
func (s *server) oidcLogin(ctx context.Context, p *oidc.Provider, code string, pending pendingLogin) (oidcClaims, error) {
	var claims oidcClaims
	token, err := s.oidc.oauth(p).Exchange(ctx, code, oauth2.VerifierOption(pending.verifier))
	if err != nil {
		return claims, fmt.Errorf("code exchange: %w", err)
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		return claims, errors.New("the answer holds no ID token")
	}
	idToken, err := p.Verifier(&oidc.Config{ClientID: s.oidc.cfg.ClientID}).Verify(ctx, raw)
	if err != nil {
		return claims, fmt.Errorf("ID token: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(pending.nonce)) != 1 {
		return claims, errors.New("the ID token belongs to another login")
	}
	if err := idToken.Claims(&claims); err != nil {
		return claims, fmt.Errorf("ID token: %w", err)
	}
	info, err := p.UserInfo(ctx, oauth2.StaticTokenSource(token))
	if err != nil {
		return claims, fmt.Errorf("UserInfo: %w", err)
	}
	// UserInfo answers for the access token; it has to be the same person
	if info.Subject != idToken.Subject {
		return claims, errors.New("UserInfo describes somebody else")
	}
	var more oidcClaims
	if err := info.Claims(&more); err != nil {
		return claims, fmt.Errorf("UserInfo: %w", err)
	}
	if more.Name != "" {
		claims.Name = more.Name
	}
	if more.Groups != nil {
		claims.Groups = more.Groups
	}
	// issuer and subject together name a login for good, a name can change
	claims.Subject = idToken.Issuer + " " + idToken.Subject
	return claims, nil
}

// oidcRole is the role the groups of a login give, or none.
func (o *oidcState) role(groups []string) auth_.Role {
	switch {
	case o.cfg.AdminGroup != "" && slices.Contains(groups, o.cfg.AdminGroup):
		return auth_.RoleAdmin
	case o.cfg.ViewerGroup != "" && slices.Contains(groups, o.cfg.ViewerGroup):
		return auth_.RoleViewer
	}
	return ""
}

// oidcAccount finds or links the account of a login, or creates it when groups
// decide the roles. It returns the account name or why the login is refused.
func (s *server) oidcAccount(claims oidcClaims) (name, reason string) {
	users, cfg := s.auth.Users, s.oidc.cfg
	byGroups := cfg.AdminGroup != "" || cfg.ViewerGroup != ""
	role := s.oidc.role(claims.Groups)
	if byGroups && role == "" {
		slog.Warn("single sign-on: login is in none of the groups", "user", claims.Name)
		return "", "nogroup"
	}
	u, ok := users.FindOIDC(claims.Subject)
	justLinked := ""
	// a refused login takes back the link it made
	defer func() {
		if justLinked != "" && reason != "" {
			if err := users.UnlinkOIDC(justLinked); err != nil {
				slog.Error("single sign-on: link not taken back", "user", justLinked, "error", err)
				return
			}
			s.record(justLinked, "user-unlink", justLinked+" (single sign-on refused)")
		}
	}()
	if !ok && cfg.LinkByName && claims.Name != "" {
		if existing, found := users.Get(claims.Name); found {
			if existing.OIDC != "" {
				slog.Warn("single sign-on: the account of this name is linked to another login", "user", existing.Name)
				return "", "taken"
			}
			if err := users.LinkOIDC(existing.Name, claims.Subject); err != nil {
				slog.Error("single sign-on: linking failed", "user", existing.Name, "error", err)
				return "", "failed"
			}
			s.record(existing.Name, "user-link", existing.Name+" (single sign-on)")
			justLinked = existing.Name
			u, ok = users.FindOIDC(claims.Subject)
		}
	}
	if !ok {
		if !byGroups {
			slog.Warn("single sign-on: no account for this login", "user", claims.Name)
			return "", "unknown"
		}
		if err := users.AddOIDC(claims.Name, role, claims.Subject, time.Now()); err != nil {
			slog.Warn("single sign-on: account not created", "user", claims.Name, "error", err)
			if errors.Is(err, auth_.ErrUserExists) {
				return "", "taken"
			}
			return "", "failed"
		}
		s.record(claims.Name, "user-create", claims.Name+" ("+string(role)+", single sign-on)")
		return claims.Name, ""
	}
	if byGroups && u.Role != role {
		switch err := users.Update(u.Name, role, "", false); {
		case errors.Is(err, auth_.ErrLastAdmin):
			// the provider took the role away, but somebody has to stay admin: no
			// session with a role the provider did not give
			slog.Warn("single sign-on: refused, the login would demote the last admin", "user", u.Name)
			return "", "lastadmin"
		case err != nil:
			slog.Error("single sign-on: role not changed", "user", u.Name, "error", err)
			return "", "failed"
		default:
			s.record(u.Name, "user-update", u.Name+" (role "+string(role)+" from single sign-on)")
		}
	}
	return u.Name, ""
}

// oidcLink links the account that asked for it to the login it came back with.
func (s *server) oidcLink(name string, claims oidcClaims) string {
	users, cfg := s.auth.Users, s.oidc.cfg
	// a link that no login could use afterwards is no link
	if (cfg.AdminGroup != "" || cfg.ViewerGroup != "") && s.oidc.role(claims.Groups) == "" {
		return "nogroup"
	}
	if other, ok := users.FindOIDC(claims.Subject); ok {
		if strings.EqualFold(other.Name, name) {
			return "linked"
		}
		slog.Warn("single sign-on: the login is linked to another account", "user", name, "other", other.Name)
		return "taken"
	}
	switch err := users.LinkOIDC(name, claims.Subject); {
	case errors.Is(err, auth_.ErrLinked):
		return "taken"
	case err != nil:
		slog.Error("single sign-on: linking failed", "user", name, "error", err)
		return "failed"
	}
	s.record(name, "user-link", name+" (single sign-on)")
	return ""
}

// oidcInfo tells the login form about single sign-on.
func (s *server) oidcInfo() map[string]any {
	if s.oidc == nil {
		return nil
	}
	return map[string]any{"label": s.oidc.cfg.Label}
}
