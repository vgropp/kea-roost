package web_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/vgropp/kea-roost/internal/kea"
	"github.com/vgropp/kea-roost/internal/keatest"
	"github.com/vgropp/kea-roost/internal/manager"
	"github.com/vgropp/kea-roost/internal/web"
)

// The routes are read from the source, so that a new one is covered without
// being listed here.
var routePattern = regexp.MustCompile(`mux\.HandleFunc\("([A-Z]+) (/api/[^"]*)", (s\.writer\()?`)

var apiRegistration = regexp.MustCompile(`\.Handle(?:Func)?\("[^"]*/api/`)

// Without a login, only these answer.
var publicRoutes = map[string]bool{"/api/session": true, "/api/login": true, "/api/login/totp": true, "/api/logout": true, "/api/setup": true}

// Writes that are not behind s.writer: the login itself and what every user may do
// with the own account.
var ownWrites = map[string]bool{
	"POST /api/login": true, "POST /api/login/totp": true, "POST /api/logout": true, "POST /api/setup": true,
	"POST /api/account/totp": true, "POST /api/account/totp/confirm": true, "DELETE /api/account/totp": true,
	"PUT /api/account/password": true,
}

func TestEveryRouteIsGuarded(t *testing.T) {
	source, err := os.ReadFile("web.go")
	if err != nil {
		t.Fatal(err)
	}
	routes := routePattern.FindAllStringSubmatch(string(source), -1)
	// every registration under /api/ has to be one the pattern understands
	if all := len(apiRegistration.FindAllString(string(source), -1)); len(routes) != all || all < 50 {
		t.Fatalf("%d of %d API routes match the pattern; register them as mux.HandleFunc(\"METHOD /api/...\", ...)", len(routes), all)
	}
	_, srv := setup(t, login)
	anonymous, viewer := browserClient(t), browserClient(t)
	if res, _ := call(t, viewer, srv, "POST", "/api/login", loginBody("viewer", viewerPassword)); res.StatusCode != http.StatusOK {
		t.Fatalf("viewer login: %d", res.StatusCode)
	}
	fill := strings.NewReplacer("{subnet}", "1", "{type}", "hw-address", "{id}", "aa:bb:cc:00:00:01",
		"{ip}", "192.0.2.10", "{name}", "standby", "{action}", "sync")
	for _, r := range routes {
		method, pattern, guarded := r[1], r[2], r[3] != ""
		path := fill.Replace(pattern)
		if !publicRoutes[pattern] {
			if res, _ := call(t, anonymous, srv, method, path, "{}"); res.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s without login: %d", method, pattern, res.StatusCode)
			}
		}
		if guarded {
			if res, _ := call(t, viewer, srv, method, path, "{}"); res.StatusCode != http.StatusForbidden {
				t.Errorf("%s %s as viewer: %d", method, pattern, res.StatusCode)
			}
		} else if method != "GET" && !ownWrites[method+" "+pattern] {
			t.Errorf("%s %s changes something but is not behind s.writer", method, pattern)
		}
	}
}

// The IPv6 popup hides the audit history from viewers like the IPv4 one.
func TestClient6HistoryOnlyForAdmins(t *testing.T) {
	fake := keatest.New()
	fake6 := keatest.New6()
	clients := []*kea.Client{}
	for _, f := range []*keatest.Server{fake, fake6} {
		s := httptestServer(t, f)
		c, err := kea.NewClient(kea.Options{URL: s})
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
	}
	store, err := manager.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr := manager.New(clients[0], store, "primary")
	mgr.SetDHCP6(clients[1])
	srv := newServer(t, web.New(mgr, testAuth(t, login), ""))
	admin, viewer := browserClient(t), browserClient(t)
	call(t, admin, srv, "POST", "/api/login", loginBody("admin", adminPassword))
	call(t, viewer, srv, "POST", "/api/login", loginBody("viewer", viewerPassword))
	body := `{"subnetId":1,"identifierType":"duid","identifier":"00:03:00:01:02:00:00:00:00:31","addresses":["fd00:db8:1::30"]}`
	if res, text := call(t, admin, srv, "POST", "/api/dhcp6/reservations", body); res.StatusCode != http.StatusOK {
		t.Fatalf("create: %d %s", res.StatusCode, text)
	}
	if _, text := call(t, admin, srv, "GET", "/api/dhcp6/clients/fd00:db8:1::30", ""); !strings.Contains(text, `"action":"reservation6-add"`) {
		t.Errorf("admin: %s", text)
	}
	if res, text := call(t, viewer, srv, "GET", "/api/dhcp6/clients/fd00:db8:1::30", ""); res.StatusCode != http.StatusOK || !strings.Contains(text, `"history":[]`) {
		t.Errorf("viewer: %d %s", res.StatusCode, text)
	}
}

func httptestServer(t *testing.T, h http.Handler) string {
	t.Helper()
	s := newServer(t, h)
	return s.URL
}

func newServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}
