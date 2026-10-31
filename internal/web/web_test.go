package web_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"

	"github.com/vgropp/kea-roost/internal/kea"
	"github.com/vgropp/kea-roost/internal/keatest"
	"github.com/vgropp/kea-roost/internal/manager"
	"github.com/vgropp/kea-roost/internal/web"
)

const payload = `{"subnetId":1,"identifierType":"hw-address","identifier":"aa:bb:cc:00:00:01","ipAddress":"192.0.2.10","hostname":"nas"}`

func setup(t *testing.T, auth web.Auth) (*keatest.Server, *httptest.Server) {
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
	srv := httptest.NewServer(web.New(manager.New(client, store, "primary"), auth, ""))
	t.Cleanup(srv.Close)
	return fake, srv
}

func do(t *testing.T, srv *httptest.Server, method, path, body string, login bool) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if login {
		req.SetBasicAuth("admin", "pw")
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return res, string(data)
}

var login = web.Auth{User: "admin", Password: "pw"}

func TestReadOnlyWithoutLogin(t *testing.T) {
	fake, srv := setup(t, web.Auth{})
	if res, _ := do(t, srv, "GET", "/api/dashboard", "", false); res.StatusCode != 200 {
		t.Fatalf("dashboard: %d", res.StatusCode)
	}
	writes := [][2]string{
		{"POST", "/api/reservations"},
		{"PUT", "/api/reservations/1/hw-address/aa:bb:cc:00:00:01"},
		{"DELETE", "/api/reservations/1/hw-address/aa:bb:cc:00:00:01"},
		{"DELETE", "/api/leases/192.0.2.1"},
		{"POST", "/api/config/write"},
		{"GET", "/api/backups"},
		{"GET", "/api/audit"},
	}
	for _, w := range writes {
		if res, _ := do(t, srv, w[0], w[1], payload, false); res.StatusCode != http.StatusForbidden {
			t.Errorf("%v: %d", w, res.StatusCode)
		}
	}
	if fake.HasCall("config-write") || fake.HasCall("reservation-add") {
		t.Error("write reached Kea")
	}
}

func TestLoginRequired(t *testing.T) {
	_, srv := setup(t, login)
	for _, path := range []string{"/", "/app.js", "/api/dashboard", "/api/leases"} {
		res, _ := do(t, srv, "GET", path, "", false)
		if res.StatusCode != http.StatusUnauthorized || res.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s: %d", path, res.StatusCode)
		}
	}
	if res, _ := do(t, srv, "GET", "/healthz", "", false); res.StatusCode != 200 {
		t.Errorf("healthz: %d", res.StatusCode)
	}
	if res, _ := do(t, srv, "GET", "/", "", true); res.StatusCode != 200 {
		t.Errorf("login: %d", res.StatusCode)
	}
}

func TestCrossOriginWriteRejected(t *testing.T) {
	fake, srv := setup(t, login)
	req, _ := http.NewRequest("POST", srv.URL+"/api/reservations", strings.NewReader(payload))
	req.SetBasicAuth("admin", "pw")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden || fake.HasCall("reservation-add") {
		t.Fatalf("status %d", res.StatusCode)
	}
}

func TestReverseProxyHostHeader(t *testing.T) {
	_, srv := setup(t, login)
	post := func(host string) *http.Response {
		req, _ := http.NewRequest("POST", srv.URL+"/api/reservations", strings.NewReader(payload))
		req.SetBasicAuth("admin", "pw")
		req.Host = host
		// plain HTTP on a LAN address is not a secure context, so browsers send no Sec-Fetch-Site
		req.Header.Set("Origin", "http://192.0.2.1")
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	if res := post("127.0.0.1:8323"); res.StatusCode != http.StatusForbidden {
		t.Errorf("rewritten host: %d", res.StatusCode)
	}
	if res := post("192.0.2.1"); res.StatusCode != http.StatusOK {
		t.Errorf("preserved host: %d", res.StatusCode)
	}
	req, _ := http.NewRequest("POST", srv.URL+"/api/reservations", strings.NewReader(`{}`))
	req.SetBasicAuth("admin", "pw")
	req.Host = "127.0.0.1:8323"
	req.Header.Set("Origin", "http://192.0.2.1")
	req.Header.Set("X-Forwarded-Host", "192.0.2.1")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode == http.StatusForbidden {
		t.Errorf("forwarded host: %d", res.StatusCode)
	}
}

func subpathProxy(t *testing.T, backend, stripped string) *httptest.Server {
	t.Helper()
	target, err := url.Parse(backend)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(&httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(target) // rewrites Host like a default proxy setup
		r.Out.URL.Path = strings.TrimPrefix(r.Out.URL.Path, stripped)
		r.Out.Header.Set("X-Forwarded-Host", r.In.Host)
	}})
	t.Cleanup(proxy.Close)
	return proxy
}

func TestSubpathBehindReverseProxy(t *testing.T) {
	cases := map[string]struct{ base, stripped string }{
		"proxy strips the path":   {"", "/kea-roost"},
		"manager strips the path": {"/kea-roost", ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			fake := keatest.New()
			keaSrv := httptest.NewServer(fake)
			defer keaSrv.Close()
			client, _ := kea.NewClient(kea.Options{URL: keaSrv.URL})
			store, _ := manager.NewStore(t.TempDir())
			backend := httptest.NewServer(web.New(manager.New(client, store, "primary"), login, c.base))
			defer backend.Close()
			proxy := subpathProxy(t, backend.URL, c.stripped)

			res, body := do(t, proxy, "GET", "/kea-roost/", "", true)
			if res.StatusCode != 200 {
				t.Fatalf("index: %d", res.StatusCode)
			}
			// every reference has to stay relative to work below a subpath
			for _, attr := range []string{`href="/`, `src="/`} {
				if strings.Contains(body, attr) {
					t.Errorf("index.html has an absolute reference: %s", attr)
				}
			}
			for _, path := range []string{"app.js", "style.css", "theme.js", "theme-modern.css", "api/dashboard"} {
				if res, _ := do(t, proxy, "GET", "/kea-roost/"+path, "", true); res.StatusCode != 200 {
					t.Errorf("%s: %d", path, res.StatusCode)
				}
			}
			_, js := do(t, proxy, "GET", "/kea-roost/app.js", "", true)
			if strings.Contains(js, "fetch('/") || strings.Contains(js, "href: '/") {
				t.Error("app.js has an absolute URL")
			}

			req, _ := http.NewRequest("POST", proxy.URL+"/kea-roost/api/reservations", strings.NewReader(payload))
			req.SetBasicAuth("admin", "pw")
			req.Header.Set("Origin", proxy.URL)
			res, err := proxy.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != 200 || !fake.HasCall("reservation-add") {
				t.Errorf("write through proxy: %d", res.StatusCode)
			}
		})
	}
}

func TestBasePathKeepsHealthCheckAtRoot(t *testing.T) {
	client, _ := kea.NewClient(kea.Options{URL: "http://127.0.0.1:1"})
	store, _ := manager.NewStore(t.TempDir())
	srv := httptest.NewServer(web.New(manager.New(client, store, "x"), web.Auth{}, "/kea-roost"))
	defer srv.Close()
	if res, _ := do(t, srv, "GET", "/healthz", "", false); res.StatusCode != 200 {
		t.Errorf("healthz: %d", res.StatusCode)
	}
	if res, _ := do(t, srv, "GET", "/kea-roost/", "", false); res.StatusCode != 200 {
		t.Errorf("index: %d", res.StatusCode)
	}
	if res, _ := do(t, srv, "GET", "/app.js", "", false); res.StatusCode != 404 {
		t.Errorf("outside the base path: %d", res.StatusCode)
	}
}

func TestReservationLifecycle(t *testing.T) {
	fake, srv := setup(t, login)
	res, body := do(t, srv, "POST", "/api/reservations", payload, true)
	if res.StatusCode != 200 || !strings.Contains(body, `"status":"ok"`) {
		t.Fatalf("create: %d %s", res.StatusCode, body)
	}
	path := "/api/reservations/1/hw-address/" + url.PathEscape("aa:bb:cc:00:00:01")
	res, body = do(t, srv, "PUT", path, `{"ipAddress":"192.0.2.11","hostname":"nas2"}`, true)
	if res.StatusCode != 200 {
		t.Fatalf("update: %d %s", res.StatusCode, body)
	}
	_, body = do(t, srv, "GET", "/api/reservations", "", true)
	if !strings.Contains(body, "192.0.2.11") || !strings.Contains(body, "nas2") {
		t.Fatalf("list: %s", body)
	}
	res, body = do(t, srv, "DELETE", path, "", true)
	if res.StatusCode != 200 || len(fake.Hosts) != 0 {
		t.Fatalf("delete: %d %s", res.StatusCode, body)
	}
	_, body = do(t, srv, "GET", "/api/audit", "", true)
	var entries []manager.AuditEntry
	if err := json.Unmarshal([]byte(body), &entries); err != nil || len(entries) != 3 {
		t.Fatalf("audit: %s", body)
	}
	if entries[0].User != "admin" || entries[0].Action != "reservation-del" {
		t.Errorf("audit: %+v", entries[0])
	}
}

func TestErrorMapping(t *testing.T) {
	fake, srv := setup(t, login)
	res, body := do(t, srv, "POST", "/api/reservations", `{"subnetId":1,"identifierType":"hw-address","identifier":"x"}`, true)
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "MAC") {
		t.Errorf("validation: %d %s", res.StatusCode, body)
	}
	if res, _ = do(t, srv, "POST", "/api/reservations", "{", true); res.StatusCode != http.StatusBadRequest {
		t.Errorf("bad json: %d", res.StatusCode)
	}
	if res, _ = do(t, srv, "DELETE", "/api/leases/192.0.2.9", "", true); res.StatusCode != http.StatusNotFound {
		t.Errorf("lease: %d", res.StatusCode)
	}
	if res, _ = do(t, srv, "DELETE", "/api/reservations/x/hw-address/aa", "", true); res.StatusCode != http.StatusBadRequest {
		t.Errorf("subnet: %d", res.StatusCode)
	}

	fake.Fail["config-write"] = keatest.Failure{Result: 1, Text: "disk"}
	res, body = do(t, srv, "POST", "/api/reservations", payload, true)
	if res.StatusCode != http.StatusBadGateway || !strings.Contains(body, `"status":"partial"`) {
		t.Errorf("partial: %d %s", res.StatusCode, body)
	}
	delete(fake.Fail, "config-write")
	if res, _ = do(t, srv, "POST", "/api/config/write", "", true); res.StatusCode != 200 {
		t.Errorf("persist: %d", res.StatusCode)
	}
}

func TestUnreachableKea(t *testing.T) {
	client, _ := kea.NewClient(kea.Options{URL: "http://127.0.0.1:1"})
	store, _ := manager.NewStore(t.TempDir())
	srv := httptest.NewServer(web.New(manager.New(client, store, "x"), web.Auth{}, ""))
	defer srv.Close()
	res, body := do(t, srv, "GET", "/api/leases", "", false)
	if res.StatusCode != http.StatusBadGateway || !strings.Contains(body, "unreachable") {
		t.Errorf("leases: %d %s", res.StatusCode, body)
	}
	res, body = do(t, srv, "GET", "/api/dashboard", "", false)
	if res.StatusCode != 200 || !strings.Contains(body, `"reachable":false`) {
		t.Errorf("dashboard: %d %s", res.StatusCode, body)
	}
}

func TestConfigSecretsStayInside(t *testing.T) {
	_, srv := setup(t, login)
	for _, path := range []string{"/api/dashboard", "/api/subnets", "/api/reservations", "/api/leases", "/api/meta"} {
		if _, body := do(t, srv, "GET", path, "", true); strings.Contains(body, "s3cret") {
			t.Errorf("%s leaks the Kea password", path)
		}
	}
}

func TestBackupDownload(t *testing.T) {
	_, srv := setup(t, login)
	_, body := do(t, srv, "POST", "/api/reservations", payload, true)
	var result manager.Result
	if err := json.Unmarshal([]byte(body), &result); err != nil || result.Backup == "" {
		t.Fatalf("result: %s", body)
	}
	res, body := do(t, srv, "GET", "/api/backups/"+result.Backup, "", true)
	if res.StatusCode != 200 || !strings.Contains(body, "subnet4") {
		t.Fatalf("download: %d", res.StatusCode)
	}
	if res, _ := do(t, srv, "GET", "/api/backups/"+url.PathEscape("../audit.log"), "", true); res.StatusCode != http.StatusBadRequest {
		t.Errorf("traversal: %d", res.StatusCode)
	}
}

func TestDevicesWithoutMonitor(t *testing.T) {
	_, srv := setup(t, login)
	if res, _ := do(t, srv, "GET", "/api/devices", "", true); res.StatusCode != http.StatusNotFound {
		t.Errorf("devices: %d", res.StatusCode)
	}
	if _, body := do(t, srv, "GET", "/api/meta", "", true); !strings.Contains(body, `"devices":false`) {
		t.Errorf("meta: %s", body)
	}
}

func TestSecurityHeadersAndAssets(t *testing.T) {
	_, srv := setup(t, web.Auth{})
	for _, path := range []string{"/", "/app.js", "/style.css", "/theme.js", "/theme-modern.css"} {
		res, _ := do(t, srv, "GET", path, "", false)
		if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Security-Policy"), "default-src 'self'") {
			t.Errorf("%s: %d %v", path, res.StatusCode, res.Header)
		}
	}
	_, js := do(t, srv, "GET", "/app.js", "", false)
	if strings.Contains(strings.ReplaceAll(js, "must never assign innerHTML", ""), "innerHTML") {
		t.Error("app.js must not use innerHTML: hostnames come from DHCP clients")
	}
}

func TestFrontendIsAlwaysRevalidated(t *testing.T) {
	_, srv := setup(t, web.Auth{})
	res, _ := do(t, srv, "GET", "/app.js", "", false)
	if got := res.Header.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control: %q", got)
	}
}

func TestMetaNamesTheVersion(t *testing.T) {
	_, srv := setup(t, web.Auth{})
	if _, body := do(t, srv, "GET", "/api/meta", "", false); !strings.Contains(body, `"version"`) {
		t.Fatalf("meta: %s", body)
	}
}
