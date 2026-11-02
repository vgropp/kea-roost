package web_test

import (
	"encoding/base32"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/vgropp/kea-roost/internal/auth"
	"github.com/vgropp/kea-roost/internal/kea"
	"github.com/vgropp/kea-roost/internal/keatest"
	"github.com/vgropp/kea-roost/internal/manager"
	"github.com/vgropp/kea-roost/internal/web"
)

// browserClient keeps cookies like a browser.
func browserClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

func call(t *testing.T, c *http.Client, srv *httptest.Server, method, path, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return res, string(data)
}

func loginBody(name, password string) string {
	return fmt.Sprintf(`{"name":%q,"password":%q}`, name, password)
}

func managerFor(t *testing.T) (*keatest.Server, *manager.Manager) {
	t.Helper()
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
	return fake, manager.New(client, store, "primary")
}

func TestOnlyTheInterfaceWorksWithoutASession(t *testing.T) {
	_, srv := setup(t, login)
	c := browserClient(t)
	for _, path := range []string{"/", "/app.js", "/style.css"} {
		if res, _ := call(t, c, srv, "GET", path, ""); res.StatusCode != http.StatusOK {
			t.Errorf("%s: %d", path, res.StatusCode)
		}
	}
	for _, path := range []string{"/api/dashboard", "/api/leases", "/api/meta", "/api/account", "/api/users", "/api/backups/x"} {
		res, _ := call(t, c, srv, "GET", path, "")
		if res.StatusCode != http.StatusUnauthorized || res.Header.Get("WWW-Authenticate") != "" {
			t.Errorf("%s: %d %q", path, res.StatusCode, res.Header.Get("WWW-Authenticate"))
		}
	}
	if res, body := call(t, c, srv, "GET", "/api/session", ""); res.StatusCode != http.StatusOK || !strings.Contains(body, `"setup":false`) {
		t.Errorf("session: %d %s", res.StatusCode, body)
	}
}

func TestLoginLogout(t *testing.T) {
	_, srv := setup(t, login)
	c := browserClient(t)
	if res, body := call(t, c, srv, "POST", "/api/login", loginBody("admin", "wrong passphrase")); res.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "wrong user name or password") {
		t.Fatalf("wrong password: %d %s", res.StatusCode, body)
	}
	if res, body := call(t, c, srv, "POST", "/api/login", loginBody("nobody", adminPassword)); res.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "wrong user name or password") {
		t.Fatalf("unknown user: %d %s", res.StatusCode, body)
	}
	res, _ := call(t, c, srv, "POST", "/api/login", loginBody("ADMIN", adminPassword))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("login: %d", res.StatusCode)
	}
	var cookie *http.Cookie
	for _, k := range res.Cookies() {
		if k.Name == "kea_roost_session" {
			cookie = k
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Secure || cookie.Path != "/" {
		t.Fatalf("cookie: %+v", cookie)
	}
	if res, body := call(t, c, srv, "GET", "/api/meta", ""); res.StatusCode != http.StatusOK || !strings.Contains(body, `"user":"admin"`) || !strings.Contains(body, `"writable":true`) {
		t.Fatalf("meta: %d %s", res.StatusCode, body)
	}
	call(t, c, srv, "POST", "/api/logout", "")
	if res, _ := call(t, c, srv, "GET", "/api/meta", ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after logout: %d", res.StatusCode)
	}
	// the old cookie is dead on the server, not only in the browser
	req, _ := http.NewRequest("GET", srv.URL+"/api/meta", nil)
	req.AddCookie(cookie)
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old cookie: %v %v", res, err)
	}
}

func TestCookieIsSecureBehindHTTPS(t *testing.T) {
	_, srv := setup(t, login)
	req, _ := http.NewRequest("POST", srv.URL+"/api/login", strings.NewReader(loginBody("admin", adminPassword)))
	req.Header.Set("X-Forwarded-Proto", "https")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if len(res.Cookies()) != 1 || !res.Cookies()[0].Secure {
		t.Fatalf("cookies: %+v", res.Cookies())
	}
}

func TestViewerCanReadButNotChange(t *testing.T) {
	fake, srv := setup(t, login)
	c := browserClient(t)
	call(t, c, srv, "POST", "/api/login", loginBody("viewer", viewerPassword))
	for _, path := range []string{"/api/dashboard", "/api/leases", "/api/reservations"} {
		if res, _ := call(t, c, srv, "GET", path, ""); res.StatusCode != http.StatusOK {
			t.Errorf("%s: %d", path, res.StatusCode)
		}
	}
	if _, body := call(t, c, srv, "GET", "/api/meta", ""); !strings.Contains(body, `"writable":false`) || !strings.Contains(body, `"role":"viewer"`) {
		t.Errorf("meta: %s", body)
	}
	writes := [][2]string{
		{"POST", "/api/reservations"},
		{"DELETE", "/api/leases/192.0.2.1"},
		{"POST", "/api/config/write"},
		{"GET", "/api/backups"},
		{"GET", "/api/audit"},
		{"PUT", "/api/settings"},
		{"GET", "/api/users"},
		{"POST", "/api/users"},
	}
	for _, w := range writes {
		if res, _ := call(t, c, srv, w[0], w[1], payload); res.StatusCode != http.StatusForbidden {
			t.Errorf("%v: %d", w, res.StatusCode)
		}
	}
	if fake.HasCall("config-write") || fake.HasCall("reservation-add") {
		t.Error("a write reached Kea")
	}
}

func TestFailedLoginsBlock(t *testing.T) {
	_, mgr := managerFor(t)
	srv := httptest.NewServer(web.New(mgr, testAuth(t, login), ""))
	t.Cleanup(srv.Close)
	c := browserClient(t)
	for range 10 {
		call(t, c, srv, "POST", "/api/login", loginBody("admin", "wrong passphrase"))
	}
	res, _ := call(t, c, srv, "POST", "/api/login", loginBody("admin", adminPassword))
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" {
		t.Fatalf("not blocked: %d", res.StatusCode)
	}
	if err := mgr.UpdateSettings("test", map[string]int{"loginFailures": 0}); err != nil {
		t.Fatal(err)
	}
	if res, _ := call(t, c, srv, "POST", "/api/login", loginBody("admin", adminPassword)); res.StatusCode != http.StatusOK {
		t.Fatalf("a limit of zero still blocks: %d", res.StatusCode)
	}
}

// loginFrom logs in as a request that a proxy forwarded for clients.
func loginFrom(t *testing.T, srv *httptest.Server, forwardedFor, name, password string) int {
	t.Helper()
	req, err := http.NewRequest("POST", srv.URL+"/api/login", strings.NewReader(loginBody(name, password)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-For", forwardedFor)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func TestTrustedProxyNamesTheClient(t *testing.T) {
	_, mgr := managerFor(t)
	a := testAuth(t, login)
	a.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	srv := httptest.NewServer(web.New(mgr, a, ""))
	t.Cleanup(srv.Close)
	// different names, so that only the address can block
	for i := range 10 {
		loginFrom(t, srv, "192.0.2.1", fmt.Sprintf("guess%d", i), "wrong passphrase")
	}
	if code := loginFrom(t, srv, "192.0.2.1", "admin", adminPassword); code != http.StatusTooManyRequests {
		t.Fatalf("the client was not blocked: %d", code)
	}
	// an entry the client wrote itself is left of the one the proxy appended
	if code := loginFrom(t, srv, "198.51.100.7, 192.0.2.1", "admin", adminPassword); code != http.StatusTooManyRequests {
		t.Fatalf("a forged entry got past: %d", code)
	}
	if code := loginFrom(t, srv, "192.0.2.2", "admin", adminPassword); code != http.StatusOK {
		t.Fatalf("another client behind the proxy was blocked: %d", code)
	}
}

func TestUntrustedForwardedForIsIgnored(t *testing.T) {
	_, srv := setup(t, login)
	for i := range 10 {
		loginFrom(t, srv, fmt.Sprintf("192.0.2.%d", i), fmt.Sprintf("guess%d", i), "wrong passphrase")
	}
	if code := loginFrom(t, srv, "192.0.2.200", "admin", adminPassword); code != http.StatusTooManyRequests {
		t.Fatalf("a header of an untrusted peer changed the address: %d", code)
	}
}

func TestNewLoginEndsTheOldSession(t *testing.T) {
	_, srv := setup(t, login)
	c := browserClient(t)
	res, _ := call(t, c, srv, "POST", "/api/login", loginBody("admin", adminPassword))
	var old *http.Cookie
	for _, k := range res.Cookies() {
		if k.Name == "kea_roost_session" {
			old = k
		}
	}
	call(t, c, srv, "POST", "/api/login", loginBody("admin", adminPassword))
	req, _ := http.NewRequest("GET", srv.URL+"/api/meta", nil)
	req.AddCookie(old)
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old session: %v %v", res, err)
	}
	if res, _ := call(t, c, srv, "GET", "/api/meta", ""); res.StatusCode != http.StatusOK {
		t.Fatalf("new session: %d", res.StatusCode)
	}
}

func TestPasswordChecksCount(t *testing.T) {
	_, srv := setup(t, login)
	c := browserClient(t)
	call(t, c, srv, "POST", "/api/login", loginBody("viewer", viewerPassword))
	for range 9 {
		call(t, c, srv, "PUT", "/api/account/password", `{"current":"wrong passphrase","new":"another passphrase"}`)
	}
	if res, _ := call(t, c, srv, "POST", "/api/account/totp/confirm", `{"code":"000000","password":"wrong passphrase"}`); res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("tenth guess: %d", res.StatusCode)
	}
	if res, _ := call(t, c, srv, "PUT", "/api/account/password", fmt.Sprintf(`{"current":%q,"new":"another passphrase"}`, viewerPassword)); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("guessing through an open session is not limited: %d", res.StatusCode)
	}
}

func TestOnboarding(t *testing.T) {
	_, srv := setup(t, web.Auth{SetupCode: "the code from the log"})
	c := browserClient(t)
	if _, body := call(t, c, srv, "GET", "/api/session", ""); !strings.Contains(body, `"setup":true`) {
		t.Fatalf("session: %s", body)
	}
	setupBody := func(code string) string {
		return fmt.Sprintf(`{"code":%q,"name":"owner","password":"owner passphrase"}`, code)
	}
	if res, _ := call(t, c, srv, "POST", "/api/setup", setupBody("guessed")); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong code: %d", res.StatusCode)
	}
	if res, body := call(t, c, srv, "POST", "/api/setup", `{"code":"the code from the log","name":"owner","password":"short"}`); res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("short password: %d %s", res.StatusCode, body)
	}
	if res, _ := call(t, c, srv, "POST", "/api/setup", setupBody("the code from the log")); res.StatusCode != http.StatusOK {
		t.Fatalf("setup: %d", res.StatusCode)
	}
	if _, body := call(t, c, srv, "GET", "/api/meta", ""); !strings.Contains(body, `"user":"owner"`) || !strings.Contains(body, `"role":"admin"`) {
		t.Fatalf("logged in as the new admin: %s", body)
	}
	if res, _ := call(t, browserClient(t), srv, "POST", "/api/setup", setupBody("the code from the log")); res.StatusCode != http.StatusConflict {
		t.Fatalf("a second setup: %d", res.StatusCode)
	}
}

func TestSetupNeedsACode(t *testing.T) {
	_, mgr := managerFor(t)
	users, _ := auth.LoadUsers(t.TempDir())
	srv := httptest.NewServer(web.New(mgr, web.Auth{Users: users}, ""))
	t.Cleanup(srv.Close)
	if res, _ := call(t, browserClient(t), srv, "POST", "/api/setup", `{"code":"","name":"owner","password":"owner passphrase"}`); res.StatusCode == http.StatusOK {
		t.Fatal("setup without a code")
	}
}

func TestUserManagement(t *testing.T) {
	_, srv := setup(t, login)
	admin := browserClient(t)
	call(t, admin, srv, "POST", "/api/login", loginBody("admin", adminPassword))
	if res, body := call(t, admin, srv, "POST", "/api/users", `{"name":"helper","role":"viewer","password":"helper passphrase"}`); res.StatusCode != http.StatusOK || strings.Contains(body, "argon2") {
		t.Fatalf("create: %d %s", res.StatusCode, body)
	}
	if res, _ := call(t, admin, srv, "POST", "/api/users", `{"name":"helper","role":"viewer","password":"helper passphrase"}`); res.StatusCode != http.StatusConflict {
		t.Fatalf("twice: %d", res.StatusCode)
	}
	if res, _ := call(t, admin, srv, "POST", "/api/users", `{"name":"x","role":"root","password":"helper passphrase"}`); res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad role: %d", res.StatusCode)
	}
	helper := browserClient(t)
	if res, _ := call(t, helper, srv, "POST", "/api/login", loginBody("helper", "helper passphrase")); res.StatusCode != http.StatusOK {
		t.Fatalf("helper login: %d", res.StatusCode)
	}
	if res, _ := call(t, admin, srv, "PUT", "/api/users/helper", `{"role":"admin"}`); res.StatusCode != http.StatusOK {
		t.Fatalf("promote: %d", res.StatusCode)
	}
	if _, body := call(t, helper, srv, "GET", "/api/meta", ""); !strings.Contains(body, `"writable":true`) {
		t.Fatalf("a new role applies at once: %s", body)
	}
	if res, _ := call(t, admin, srv, "PUT", "/api/users/helper", `{"password":"another passphrase"}`); res.StatusCode != http.StatusOK {
		t.Fatalf("new password: %d", res.StatusCode)
	}
	if res, _ := call(t, helper, srv, "GET", "/api/meta", ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("sessions with the old password end: %d", res.StatusCode)
	}
	if res, _ := call(t, admin, srv, "DELETE", "/api/users/admin", ""); res.StatusCode != http.StatusConflict {
		t.Fatalf("delete yourself: %d", res.StatusCode)
	}
	if res, _ := call(t, admin, srv, "DELETE", "/api/users/helper", ""); res.StatusCode != http.StatusOK {
		t.Fatalf("delete: %d", res.StatusCode)
	}
	if res, _ := call(t, admin, srv, "PUT", "/api/users/admin", `{"role":"viewer"}`); res.StatusCode != http.StatusConflict {
		t.Fatalf("demote the last admin... with viewer around: %d", res.StatusCode)
	}
	_, body := call(t, admin, srv, "GET", "/api/audit", "")
	for _, action := range []string{"user-create", "user-update", "user-delete", "login"} {
		if !strings.Contains(body, `"action":"`+action+`"`) {
			t.Errorf("audit misses %s", action)
		}
	}
}

func TestOwnPasswordChange(t *testing.T) {
	_, srv := setup(t, login)
	first, second := browserClient(t), browserClient(t)
	call(t, first, srv, "POST", "/api/login", loginBody("viewer", viewerPassword))
	call(t, second, srv, "POST", "/api/login", loginBody("viewer", viewerPassword))
	if res, _ := call(t, first, srv, "PUT", "/api/account/password", `{"current":"wrong","new":"a new passphrase"}`); res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("wrong current: %d", res.StatusCode)
	}
	if res, _ := call(t, first, srv, "PUT", "/api/account/password", fmt.Sprintf(`{"current":%q,"new":"a new passphrase"}`, viewerPassword)); res.StatusCode != http.StatusNoContent {
		t.Fatalf("change: %d", res.StatusCode)
	}
	if res, _ := call(t, first, srv, "GET", "/api/meta", ""); res.StatusCode != http.StatusOK {
		t.Fatalf("the changing browser stays in: %d", res.StatusCode)
	}
	if res, _ := call(t, second, srv, "GET", "/api/meta", ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("other sessions end: %d", res.StatusCode)
	}
	if res, _ := call(t, browserClient(t), srv, "POST", "/api/login", loginBody("viewer", "a new passphrase")); res.StatusCode != http.StatusOK {
		t.Fatalf("new password: %d", res.StatusCode)
	}
}

// totpNow computes the current code the way an authenticator app does.
func totpNow(t *testing.T, secret string) string {
	t.Helper()
	return totpAt(t, secret, time.Now())
}

func totpAt(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := auth.TOTPAt(secret, at)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func TestTOTP(t *testing.T) {
	_, srv := setup(t, login)
	c := browserClient(t)
	call(t, c, srv, "POST", "/api/login", loginBody("viewer", viewerPassword))
	res, body := call(t, c, srv, "POST", "/api/account/totp", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("start: %d %s", res.StatusCode, body)
	}
	secret := strings.Split(strings.Split(body, `"secret":"`)[1], `"`)[0]
	if _, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret); err != nil {
		t.Fatalf("secret: %s", body)
	}
	if res, _ := call(t, c, srv, "GET", "/api/account/totp/qr", ""); res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("qr: %d", res.StatusCode)
	}
	if res, _ := call(t, c, srv, "POST", "/api/account/totp/confirm", fmt.Sprintf(`{"code":"000000","password":%q}`, viewerPassword)); res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("wrong code turned it on: %d", res.StatusCode)
	}
	used := totpNow(t, secret)
	if res, _ := call(t, c, srv, "POST", "/api/account/totp/confirm", fmt.Sprintf(`{"code":%q,"password":"wrong"}`, used)); res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("turned on without the password: %d", res.StatusCode)
	}
	if res, _ := call(t, c, srv, "POST", "/api/account/totp/confirm", fmt.Sprintf(`{"code":%q,"password":%q}`, used, viewerPassword)); res.StatusCode != http.StatusNoContent {
		t.Fatalf("confirm: %d", res.StatusCode)
	}

	next := browserClient(t)
	res, body = call(t, next, srv, "POST", "/api/login", loginBody("viewer", viewerPassword))
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `"totp":true`) || len(res.Cookies()) != 0 {
		t.Fatalf("password alone logs in: %d %s", res.StatusCode, body)
	}
	ticket := strings.Split(strings.Split(body, `"ticket":"`)[1], `"`)[0]
	if res, _ := call(t, next, srv, "GET", "/api/meta", ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("logged in without the code: %d", res.StatusCode)
	}
	if res, _ := call(t, next, srv, "POST", "/api/login/totp", fmt.Sprintf(`{"ticket":%q,"code":"000000"}`, ticket)); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong code: %d", res.StatusCode)
	}
	// the code from the setup was used already, so it fails until the next step
	if res, _ := call(t, next, srv, "POST", "/api/login/totp", fmt.Sprintf(`{"ticket":%q,"code":%q}`, ticket, used)); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a used code worked again: %d", res.StatusCode)
	}
	if res, _ := call(t, next, srv, "POST", "/api/login/totp", fmt.Sprintf(`{"ticket":"made up","code":%q}`, totpNow(t, secret))); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("made up ticket: %d", res.StatusCode)
	}

	if res, _ := call(t, c, srv, "POST", "/api/account/totp", ""); res.StatusCode != http.StatusConflict {
		t.Fatalf("a second setup replaced the first: %d", res.StatusCode)
	}
	// the code of the next step, as the current one was used already
	later := totpAt(t, secret, time.Now().Add(30*time.Second))
	if res, _ := call(t, c, srv, "DELETE", "/api/account/totp", fmt.Sprintf(`{"password":"wrong","code":%q}`, later)); res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("off without the password: %d", res.StatusCode)
	}
	if res, _ := call(t, c, srv, "DELETE", "/api/account/totp", fmt.Sprintf(`{"password":%q,"code":"000000"}`, viewerPassword)); res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("off without the code: %d", res.StatusCode)
	}
	if res, _ := call(t, c, srv, "DELETE", "/api/account/totp", fmt.Sprintf(`{"password":%q,"code":%q}`, viewerPassword, later)); res.StatusCode != http.StatusNoContent {
		t.Fatalf("off: %d", res.StatusCode)
	}
	if _, body := call(t, browserClient(t), srv, "POST", "/api/login", loginBody("viewer", viewerPassword)); strings.Contains(body, `"totp":true`) {
		t.Fatal("still asks for a code")
	}
}

func TestAdminResetsSecondFactor(t *testing.T) {
	a := testAuth(t, login)
	if err := a.Users.SetTOTP("viewer", "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatal(err)
	}
	_, srv := setup(t, a)
	admin := browserClient(t)
	call(t, admin, srv, "POST", "/api/login", loginBody("admin", adminPassword))
	if _, body := call(t, admin, srv, "GET", "/api/users", ""); !strings.Contains(body, `"mfa":true`) || strings.Contains(body, "JBSWY3") {
		t.Fatalf("users: %s", body)
	}
	if res, _ := call(t, admin, srv, "PUT", "/api/users/viewer", `{"resetTotp":true}`); res.StatusCode != http.StatusOK {
		t.Fatalf("reset: %d", res.StatusCode)
	}
	if _, body := call(t, browserClient(t), srv, "POST", "/api/login", loginBody("viewer", viewerPassword)); strings.Contains(body, `"totp":true`) {
		t.Fatal("the second factor is still on")
	}
}

func TestOwnAccountNeedsTheAccountMenu(t *testing.T) {
	_, srv := setup(t, login)
	c := browserClient(t)
	call(t, c, srv, "POST", "/api/login", loginBody("admin", adminPassword))
	if res, _ := call(t, c, srv, "PUT", "/api/users/admin", `{"password":"another passphrase"}`); res.StatusCode != http.StatusConflict {
		t.Fatalf("own password without the current one: %d", res.StatusCode)
	}
	if res, _ := call(t, c, srv, "PUT", "/api/users/ADMIN", `{"resetTotp":true}`); res.StatusCode != http.StatusConflict {
		t.Fatalf("own second factor without a code: %d", res.StatusCode)
	}
}

func TestPasswordResetEndsWaitingLogins(t *testing.T) {
	a := testAuth(t, login)
	if err := a.Users.SetTOTP("viewer", "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatal(err)
	}
	_, srv := setup(t, a)
	_, body := call(t, browserClient(t), srv, "POST", "/api/login", loginBody("viewer", viewerPassword))
	ticket := strings.Split(strings.Split(body, `"ticket":"`)[1], `"`)[0]
	admin := browserClient(t)
	call(t, admin, srv, "POST", "/api/login", loginBody("admin", adminPassword))
	if res, _ := call(t, admin, srv, "PUT", "/api/users/viewer", `{"password":"another passphrase"}`); res.StatusCode != http.StatusOK {
		t.Fatalf("reset: %d", res.StatusCode)
	}
	if res, _ := call(t, browserClient(t), srv, "POST", "/api/login/totp", fmt.Sprintf(`{"ticket":%q,"code":%q}`, ticket, totpNow(t, "JBSWY3DPEHPK3PXP"))); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the old password still logs in: %d", res.StatusCode)
	}
}
