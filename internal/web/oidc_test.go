package web_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/vgropp/kea-roost/internal/auth"
	"github.com/vgropp/kea-roost/internal/kea"
	"github.com/vgropp/kea-roost/internal/keatest"
	"github.com/vgropp/kea-roost/internal/manager"
	"github.com/vgropp/kea-roost/internal/web"
)

const (
	clientID     = "kea-roost"
	clientSecret = "client-secret-for-tests"
)

// provider is an OpenID Connect provider for tests. Like Authelia, it puts the
// name and the groups into UserInfo only.
type provider struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey

	mu sync.Mutex
	// who logs in next
	sub, name string
	groups    []string
	// refuse answers the authorization with this error
	refuse string
	// tampering
	nonce, infoSub string
	// codes waiting for the token request
	codes map[string]grant
}

type grant struct {
	nonce, challenge, redirect string
}

// newProvider starts a provider over TLS, or over plain HTTP for a browser that
// would not trust the test certificate.
func newProvider(t *testing.T, plain bool) *provider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &provider{t: t, key: key, sub: "0b6c6a3e-1", name: "admin", codes: map[string]grant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		base := p.srv.URL
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": base, "authorization_endpoint": base + "/authorize", "token_endpoint": base + "/token",
			"userinfo_endpoint": base + "/userinfo", "jwks_uri": base + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("GET /authorize", p.authorize)
	mux.HandleFunc("POST /token", p.token)
	mux.HandleFunc("GET /userinfo", p.userinfo)
	if plain {
		p.srv = httptest.NewServer(mux)
	} else {
		p.srv = httptest.NewTLSServer(mux)
	}
	t.Cleanup(p.srv.Close)
	return p
}

func (p *provider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != clientID || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	scopes := strings.Fields(q.Get("scope"))
	for _, want := range []string{"openid", "profile", "groups"} {
		if !strings.Contains(" "+strings.Join(scopes, " ")+" ", " "+want+" ") {
			http.Error(w, "missing scope "+want, http.StatusBadRequest)
			return
		}
	}
	back, _ := url.Parse(q.Get("redirect_uri"))
	answer := url.Values{"state": {q.Get("state")}}
	p.mu.Lock()
	if p.refuse != "" {
		answer.Set("error", p.refuse)
	} else {
		code := fmt.Sprintf("code-%d", len(p.codes)+1)
		p.codes[code] = grant{q.Get("nonce"), q.Get("code_challenge"), q.Get("redirect_uri")}
		answer.Set("code", code)
	}
	p.mu.Unlock()
	back.RawQuery = answer.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (p *provider) token(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.FormValue("client_id"), r.FormValue("client_secret")
	}
	p.mu.Lock()
	g, known := p.codes[r.FormValue("code")]
	delete(p.codes, r.FormValue("code"))
	sub, nonce := p.sub, g.nonce
	if p.nonce != "" {
		nonce = p.nonce
	}
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(r.FormValue("code_verifier")))
	switch {
	case id != clientID || secret != clientSecret:
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	case !known || r.FormValue("redirect_uri") != g.redirect:
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
		return
	case base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge:
		// PKCE: only who started the login can finish it
		http.Error(w, `{"error":"invalid_grant","error_description":"PKCE"}`, http.StatusBadRequest)
		return
	}
	now := time.Now()
	claims, _ := json.Marshal(map[string]any{
		"iss": p.srv.URL, "sub": sub, "aud": clientID, "nonce": nonce,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		p.t.Error(err)
		return
	}
	signed, err := signer.Sign(claims)
	if err != nil {
		p.t.Error(err)
		return
	}
	idToken, _ := signed.CompactSerialize()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"access_token": "access-" + sub, "token_type": "Bearer", "expires_in": 3600, "id_token": idToken})
}

func (p *provider) userinfo(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer access-"+p.sub {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sub := p.sub
	if p.infoSub != "" {
		sub = p.infoSub
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"sub": sub, "preferred_username": p.name, "groups": p.groups})
}

func (p *provider) set(f func(p *provider)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f(p)
}

type ssoEnv struct {
	idp   *provider
	srv   *httptest.Server
	users *auth.Users
}

// startSSO runs Kea Roost with single sign-on; change adjusts the settings.
func startSSO(t *testing.T, change func(o *web.OIDC, a *web.Auth)) *ssoEnv {
	t.Helper()
	idp := newProvider(t, false)
	a := testAuth(t, web.Auth{})
	fake := keatest.New()
	keaSrv := httptest.NewServer(fake)
	t.Cleanup(keaSrv.Close)
	client, err := kea.NewClient(kea.Options{URL: keaSrv.URL})
	if err != nil {
		t.Fatal(err)
	}
	store, err := manager.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// the address is only known once the server runs, so the handler comes later
	var handler http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	o := &web.OIDC{
		Issuer: idp.srv.URL, ClientID: clientID, ClientSecret: clientSecret,
		RedirectURL: srv.URL + "/api/login/oidc/callback", Label: "Authelia", LinkByName: true,
		HTTPClient: idp.srv.Client(),
	}
	a.OIDC = o
	if change != nil {
		change(o, &a)
	}
	handler = web.New(manager.New(client, store, "primary"), a, "")
	return &ssoEnv{idp, srv, a.Users}
}

// login runs a single sign-on in a fresh browser and returns it, with the reason
// the login form got if it failed.
func (e *ssoEnv) login(t *testing.T) (*http.Client, string) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, Transport: e.idp.srv.Client().Transport}
	res, err := browser.Get(e.srv.URL + "/api/login/oidc")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.Request.URL.Host != strings.TrimPrefix(e.srv.URL, "http://") || res.Request.URL.Path != "/" {
		t.Fatalf("ended at %s (%d)", res.Request.URL, res.StatusCode)
	}
	return browser, res.Request.URL.Query().Get("sso")
}

func (e *ssoEnv) session(t *testing.T, browser *http.Client) (string, string) {
	t.Helper()
	res, err := browser.Get(e.srv.URL + "/api/session")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body struct {
		User string `json:"user"`
		Role string `json:"role"`
	}
	json.NewDecoder(res.Body).Decode(&body)
	return body.User, body.Role
}

// The first login links the account of the same name; afterwards the link counts,
// even when the name at the provider changes.
func TestSingleSignOnLinksTheExistingAccount(t *testing.T) {
	e := startSSO(t, nil)
	browser, reason := e.login(t)
	if reason != "" {
		t.Fatalf("refused: %s", reason)
	}
	if name, role := e.session(t, browser); name != "admin" || role != "admin" {
		t.Fatalf("logged in as %q (%s)", name, role)
	}
	if u, _ := e.users.Get("admin"); u.OIDC != e.idp.srv.URL+" 0b6c6a3e-1" || u.Hash == "" {
		t.Errorf("account %+v", u)
	}

	e.idp.set(func(p *provider) { p.name = "renamed" })
	browser, reason = e.login(t)
	if name, _ := e.session(t, browser); reason != "" || name != "admin" {
		t.Errorf("after the rename: %q %q", reason, name)
	}

	// another login of the name of a linked account does not get it
	e.idp.set(func(p *provider) { p.sub, p.name = "other", "admin" })
	if _, reason = e.login(t); reason != "taken" {
		t.Errorf("got %q", reason)
	}
}

func TestSingleSignOnWithoutAccount(t *testing.T) {
	e := startSSO(t, nil)
	e.idp.set(func(p *provider) { p.name = "stranger" })
	browser, reason := e.login(t)
	if reason != "unknown" {
		t.Fatalf("got %q", reason)
	}
	if name, _ := e.session(t, browser); name != "" {
		t.Errorf("logged in as %q", name)
	}
	if _, ok := e.users.Get("stranger"); ok {
		t.Error("an account was created")
	}
}

func TestSingleSignOnWithoutLinkByName(t *testing.T) {
	e := startSSO(t, func(o *web.OIDC, _ *web.Auth) { o.LinkByName = false })
	if _, reason := e.login(t); reason != "unknown" {
		t.Fatalf("got %q", reason)
	}
}

// With groups, the provider decides the role at every login and creates accounts.
func TestSingleSignOnRolesFromGroups(t *testing.T) {
	e := startSSO(t, func(o *web.OIDC, _ *web.Auth) { o.AdminGroup, o.ViewerGroup = "roost-admins", "roost-viewers" })
	e.idp.set(func(p *provider) { p.sub, p.name, p.groups = "alice-sub", "alice", []string{"staff", "roost-admins"} })
	browser, reason := e.login(t)
	if name, role := e.session(t, browser); reason != "" || name != "alice" || role != "admin" {
		t.Fatalf("got %q %q %q", reason, name, role)
	}
	if u, _ := e.users.Get("alice"); u.Hash != "" {
		t.Error("an account of single sign-on got a password")
	}
	// no password, so the login form does not let it in
	if res, _ := http.Post(e.srv.URL+"/api/login", "application/json", strings.NewReader(`{"name":"alice","password":""}`)); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("password login: %d", res.StatusCode)
	}

	e.idp.set(func(p *provider) { p.groups = []string{"roost-viewers"} })
	browser, _ = e.login(t)
	if _, role := e.session(t, browser); role != "viewer" {
		t.Errorf("role %q", role)
	}

	e.idp.set(func(p *provider) { p.groups = nil })
	if _, reason = e.login(t); reason != "nogroup" {
		t.Errorf("got %q", reason)
	}
}

// Whatever goes wrong on the way, nobody is logged in.
func TestSingleSignOnRefusesTampering(t *testing.T) {
	cases := map[string]struct {
		change func(p *provider)
		want   string
	}{
		"refused":      {func(p *provider) { p.refuse = "access_denied" }, "denied"},
		"other nonce":  {func(p *provider) { p.nonce = "replayed" }, "failed"},
		"other person": {func(p *provider) { p.infoSub = "somebody-else" }, "failed"},
		"wrong secret": {nil, "failed"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := startSSO(t, func(o *web.OIDC, _ *web.Auth) {
				if name == "wrong secret" {
					o.ClientSecret = "wrong"
				}
			})
			if c.change != nil {
				e.idp.set(c.change)
			}
			browser, reason := e.login(t)
			if reason != c.want {
				t.Fatalf("got %q, want %q", reason, c.want)
			}
			if user, _ := e.session(t, browser); c.want != "" && user != "" {
				t.Errorf("logged in as %q", user)
			}
		})
	}
}

// A callback without the cookie of the browser that started the login, as a
// forged link would bring, is refused.
func TestSingleSignOnNeedsTheStartingBrowser(t *testing.T) {
	e := startSSO(t, nil)
	jar, _ := cookiejar.New(nil)
	attacker := &http.Client{Jar: jar, Transport: e.idp.srv.Client().Transport,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if strings.HasSuffix(req.URL.Path, "/callback") {
				return http.ErrUseLastResponse
			}
			return nil
		}}
	res, err := attacker.Get(e.srv.URL + "/api/login/oidc")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	callback := res.Header.Get("Location")
	if !strings.Contains(callback, "code=") {
		t.Fatalf("no callback: %s", callback)
	}
	victim := &http.Client{}
	res, err = victim.Get(callback)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if got := res.Request.URL.Query().Get("sso"); got != "expired" {
		t.Errorf("got %q", got)
	}
	for _, c := range res.Cookies() {
		if c.Name == "kea_roost_session" && c.Value != "" {
			t.Error("the forged callback started a session")
		}
	}
}

func TestPasswordLoginCanBeTurnedOff(t *testing.T) {
	e := startSSO(t, func(_ *web.OIDC, a *web.Auth) { a.PasswordLoginOff = true })
	res, err := http.Post(e.srv.URL+"/api/login", "application/json", strings.NewReader(fmt.Sprintf(`{"name":"admin","password":%q}`, adminPassword)))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("password login: %d", res.StatusCode)
	}
	res, err = http.Get(e.srv.URL + "/api/session")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body map[string]any
	json.NewDecoder(res.Body).Decode(&body)
	if body["passwordLogin"] != false || body["sso"] == nil {
		t.Errorf("session %v", body)
	}
	if _, reason := e.login(t); reason != "" {
		t.Errorf("single sign-on: %q", reason)
	}
}

// An admin unlinks an account so that another login can take it; an account
// without a password keeps its only login.
func TestUnlinkSingleSignOn(t *testing.T) {
	e := startSSO(t, func(o *web.OIDC, _ *web.Auth) { o.ViewerGroup = "roost-viewers" })
	e.idp.set(func(p *provider) { p.sub, p.name, p.groups = "bob-sub", "bob", []string{"roost-viewers"} })
	if _, reason := e.login(t); reason != "" {
		t.Fatal(reason)
	}
	if err := e.users.UnlinkOIDC("bob"); err == nil {
		t.Error("the only login of bob was removed")
	}
	if err := e.users.LinkOIDC("viewer", "x y"); err != nil {
		t.Fatal(err)
	}
	if err := e.users.UnlinkOIDC("viewer"); err != nil {
		t.Fatal(err)
	}
	if u, _ := e.users.Get("viewer"); u.OIDC != "" {
		t.Errorf("still linked: %+v", u)
	}
	for _, u := range e.users.List() {
		if u.OIDC != "" {
			t.Errorf("the list shows the subject of %s", u.Name)
		}
	}
}

// passwordBrowser logs in with the password and returns the browser.
func (e *ssoEnv) passwordBrowser(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, Transport: e.idp.srv.Client().Transport}
	res, err := browser.Post(e.srv.URL+"/api/login", "application/json", strings.NewReader(fmt.Sprintf(`{"name":"admin","password":%q}`, adminPassword)))
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("password login: %v %v", res, err)
	}
	res.Body.Close()
	return browser
}

// linkTicket asks for a link with a password and returns the status and ticket.
func (e *ssoEnv) linkTicket(t *testing.T, browser *http.Client, password string) (int, string) {
	t.Helper()
	res, err := browser.Post(e.srv.URL+"/api/account/sso-link", "application/json", strings.NewReader(fmt.Sprintf(`{"password":%q}`, password)))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body struct {
		Ticket string `json:"ticket"`
	}
	json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body.Ticket
}

func follow(t *testing.T, browser *http.Client, address string) string {
	t.Helper()
	res, err := browser.Get(address)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.Request.URL.Query().Get("sso")
}

// The safe way to move an account: log in with the password, then link from the
// account menu with the password again. Afterwards single sign-on works without
// linking by name.
func TestLinkFromTheAccountMenu(t *testing.T) {
	e := startSSO(t, func(o *web.OIDC, _ *web.Auth) { o.LinkByName = false })
	e.idp.set(func(p *provider) { p.name = "someone-else" })
	browser := e.passwordBrowser(t)
	if status, _ := e.linkTicket(t, browser, "wrong password"); status != http.StatusUnprocessableEntity {
		t.Errorf("wrong password: %d", status)
	}
	status, ticket := e.linkTicket(t, browser, adminPassword)
	if status != http.StatusOK || ticket == "" {
		t.Fatalf("ticket: %d", status)
	}
	if got := follow(t, browser, e.srv.URL+"/api/login/oidc?link="+url.QueryEscape(ticket)); got != "linked" {
		t.Fatalf("got %q", got)
	}
	if u, _ := e.users.Get("admin"); u.OIDC == "" {
		t.Fatal("not linked")
	}
	// a ticket works once
	if got := follow(t, browser, e.srv.URL+"/api/login/oidc?link="+url.QueryEscape(ticket)); got != "expired" {
		t.Errorf("second use: %q", got)
	}
	sso, reason := e.login(t)
	if name, _ := e.session(t, sso); reason != "" || name != "admin" {
		t.Errorf("single sign-on after linking: %q %q", reason, name)
	}
	// the same login cannot link a second account
	if err := e.users.LinkOIDC("viewer", e.idp.srv.URL+" 0b6c6a3e-1"); err == nil {
		t.Error("one login linked two accounts")
	}
	// a ticket only works in the browser it was made for
	e.users.UnlinkOIDC("admin")
	_, ticket = e.linkTicket(t, browser, adminPassword)
	other := e.passwordBrowser(t)
	if got := follow(t, other, e.srv.URL+"/api/login/oidc?link="+url.QueryEscape(ticket)); got != "expired" {
		t.Errorf("ticket in another browser: %q", got)
	}
	if u, _ := e.users.Get("admin"); u.OIDC != "" {
		t.Error("linked from another browser")
	}
	// without a session there is no ticket
	if status, _ := e.linkTicket(t, &http.Client{}, adminPassword); status != http.StatusUnauthorized {
		t.Errorf("anonymous ticket: %d", status)
	}
}

// A session that ends while the browser is at the provider links nothing.
func TestLinkNeedsTheSessionToLast(t *testing.T) {
	e := startSSO(t, nil)
	browser := e.passwordBrowser(t)
	_, ticket := e.linkTicket(t, browser, adminPassword)
	browser.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if strings.HasSuffix(req.URL.Path, "/callback") {
			return http.ErrUseLastResponse
		}
		return nil
	}
	res, err := browser.Get(e.srv.URL + "/api/login/oidc?link=" + url.QueryEscape(ticket))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	callback := res.Header.Get("Location")
	logout, _ := http.NewRequest("POST", e.srv.URL+"/api/logout", nil)
	if res, err = browser.Do(logout); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	browser.CheckRedirect = nil
	if got := follow(t, browser, callback); got != "expired" {
		t.Errorf("got %q", got)
	}
	if u, _ := e.users.Get("admin"); u.OIDC != "" {
		t.Error("linked after the logout")
	}
}

// With groups, a login in none of them is not linked.
func TestLinkChecksTheGroups(t *testing.T) {
	e := startSSO(t, func(o *web.OIDC, _ *web.Auth) { o.AdminGroup = "roost-admins" })
	browser := e.passwordBrowser(t)
	_, ticket := e.linkTicket(t, browser, adminPassword)
	if got := follow(t, browser, e.srv.URL+"/api/login/oidc?link="+url.QueryEscape(ticket)); got != "nogroup" {
		t.Errorf("got %q", got)
	}
	if u, _ := e.users.Get("admin"); u.OIDC != "" {
		t.Error("linked without a group")
	}
}

// Linking by name takes the link back when the login is refused afterwards.
func TestLinkByNameTakenBackOnRefusal(t *testing.T) {
	e := startSSO(t, func(o *web.OIDC, _ *web.Auth) { o.AdminGroup, o.ViewerGroup = "roost-admins", "roost-viewers" })
	e.idp.set(func(p *provider) { p.groups = []string{"roost-viewers"} })
	if _, reason := e.login(t); reason != "lastadmin" {
		t.Fatalf("got %q", reason)
	}
	if u, _ := e.users.Get("admin"); u.OIDC != "" {
		t.Error("the refused login kept its link")
	}
}

// A provider that takes the admin role from the last admin gets no session for it.
func TestSingleSignOnKeepsTheLastAdminOut(t *testing.T) {
	e := startSSO(t, func(o *web.OIDC, _ *web.Auth) { o.AdminGroup, o.ViewerGroup = "roost-admins", "roost-viewers" })
	if err := e.users.LinkOIDC("admin", e.idp.srv.URL+" 0b6c6a3e-1"); err != nil {
		t.Fatal(err)
	}
	e.idp.set(func(p *provider) { p.groups = []string{"roost-viewers"} })
	browser, reason := e.login(t)
	if reason != "lastadmin" {
		t.Fatalf("got %q", reason)
	}
	if name, _ := e.session(t, browser); name != "" {
		t.Errorf("logged in as %q", name)
	}
	if u, _ := e.users.Get("admin"); u.Role != auth.RoleAdmin {
		t.Errorf("role %s", u.Role)
	}
}

// Below a base path, every way back ends below it as well.
func TestSingleSignOnBelowABasePath(t *testing.T) {
	a := testAuth(t, web.Auth{})
	closed := httptest.NewTLSServer(http.NotFoundHandler())
	closed.Close()
	a.OIDC = &web.OIDC{Issuer: closed.URL, ClientID: clientID, ClientSecret: clientSecret, RedirectURL: "http://roost.example/roost/api/login/oidc/callback"}
	fake := keatest.New()
	keaSrv := httptest.NewServer(fake)
	t.Cleanup(keaSrv.Close)
	client, err := kea.NewClient(kea.Options{URL: keaSrv.URL})
	if err != nil {
		t.Fatal(err)
	}
	store, err := manager.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(web.New(manager.New(client, store, "primary"), a, "/roost"))
	t.Cleanup(srv.Close)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for path, want := range map[string]string{
		"/roost/api/login/oidc":                         "/roost/?sso=unreachable",
		"/roost/api/login/oidc/callback?state=x&code=y": "/roost/?sso=expired",
	} {
		res, err := noFollow.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		location, err := res.Request.URL.Parse(res.Header.Get("Location"))
		if err != nil || location.RequestURI() != want {
			t.Errorf("%s leads to %v, want %s", path, location, want)
		}
	}
}

// One address cannot keep everybody else from logging in: its oldest open login
// makes room, and a login of another address still works.
func TestPendingLoginsPerClient(t *testing.T) {
	e := startSSO(t, nil)
	noFollow := &http.Client{Transport: e.idp.srv.Client().Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for range 30 {
		res, err := noFollow.Get(e.srv.URL + "/api/login/oidc")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if strings.Contains(res.Header.Get("Location"), "sso=") {
			t.Fatalf("refused: %s", res.Header.Get("Location"))
		}
	}
	if _, reason := e.login(t); reason != "" {
		t.Errorf("a login after the flood: %q", reason)
	}
}
