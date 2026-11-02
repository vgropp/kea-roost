//go:build e2e

// Browser tests against the real interface, backed by the Kea test double. They
// need a local Chromium or Chrome: go test -tags e2e ./internal/web
package web_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/vgropp/kea-roost/internal/auth"
	"github.com/vgropp/kea-roost/internal/kea"
	"github.com/vgropp/kea-roost/internal/keatest"
	"github.com/vgropp/kea-roost/internal/manager"
	"github.com/vgropp/kea-roost/internal/web"
)

const (
	viewportWidth  = 1300
	viewportHeight = 900
	browserTimeout = 30 * time.Second
	pollInterval   = 20 * time.Millisecond
)

// Replaced by the screenshots, which show more devices than the tests need.
var monitorAnswer, dashboardAnswer string

// withoutPeer starts the next browser with a single Kea server without HA.
var withoutPeer bool

type browser struct {
	t     *testing.T
	ctx   context.Context
	url   string
	fake  *keatest.Server
	fake6 *keatest.Server
	peer6 *keatest.Server

	mu     sync.Mutex
	errors []string
}

func seed(fake *keatest.Server) {
	now := time.Now().Unix()
	fake.Hosts = []map[string]any{
		{"subnet-id": 0, "hw-address": "00:00:0c:00:00:01", "ip-address": "192.0.2.10", "hostname": "nas",
			"option-data": []any{map[string]any{"name": "ntp-servers", "code": 42, "data": "192.0.2.1"}}},
	}
	fake.Config["reservations-global"] = true
	fake.Config["client-classes"] = []any{map[string]any{"name": "direct-dns", "option-data": []any{map[string]any{"name": "domain-name-servers", "code": 6, "data": "192.0.2.1"}}}}
	fake.Hosts[0]["client-classes"] = []any{"direct-dns"}
	fake.Leases = []map[string]any{
		{"ip-address": "192.0.2.10", "hw-address": "00:00:0c:00:00:01", "hostname": "nas", "subnet-id": 1, "state": 0, "cltt": now, "valid-lft": 4000},
		{"ip-address": "192.0.2.100", "hw-address": "06:00:00:aa:01:64", "client-id": "ff:12:34:56:78:00:03:00:01:02:00:00:aa:01:66",
			"hostname": "fridge", "subnet-id": 1, "state": 0, "cltt": now - 60, "valid-lft": 4000},
		{"ip-address": "192.0.2.101", "hw-address": "00:00:0c:00:00:02", "hostname": "old", "subnet-id": 1, "state": 0, "cltt": 1000, "valid-lft": 10},
	}
}

func start(t *testing.T) *browser {
	t.Helper()
	return startWith(t, login, true)
}

// startWith opens a browser on a server with these accounts, logged in as the admin
// or not at all.
func startWith(t *testing.T, accounts web.Auth, loggedIn bool) *browser {
	t.Helper()
	fake := keatest.New()
	seed(fake)
	keaSrv := httptest.NewServer(fake)
	t.Cleanup(keaSrv.Close)
	monitor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if monitorAnswer != "" {
			fmt.Fprint(w, monitorAnswer)
			return
		}
		seen := func(age time.Duration) string { return time.Now().Add(-age).UTC().Format(time.RFC3339) }
		fmt.Fprintf(w, `[{"mac":"06:00:00:aa:01:64","ipv4":["192.0.2.100"],"ipv6":["fe80::400:ff:feaa:164","fd00:db8:1::1001"],"hostname":"phone.lan.","first_seen":%q,"last_seen":%q},
		                 {"mac":"00:00:0c:00:00:03","ipv4":["192.0.2.30"],"hostname":"camera.lan.","first_seen":%q,"last_seen":%q},
		                 {"mac":"00:00:0c:00:00:04","ipv4":["192.0.2.31"],"first_seen":%q,"last_seen":%q},
		                 {"mac":"00:00:0c:00:00:01","ipv4":["192.0.2.10"],"hostname":"printer.lan.","first_seen":%q,"last_seen":%q},
		                 {"mac":"00:00:0c:00:00:09","ipv4":["192.0.2.10"],"hostname":"intruder.lan.","first_seen":%q,"last_seen":%q}]`,
			seen(time.Hour), seen(time.Minute), seen(100*time.Hour), seen(2*time.Hour), seen(200*time.Hour), seen(72*time.Hour), seen(9*time.Hour), seen(time.Minute), seen(time.Hour), seen(time.Minute))
	}))
	t.Cleanup(monitor.Close)
	client, err := kea.NewClient(kea.Options{URL: keaSrv.URL})
	if err != nil {
		t.Fatal(err)
	}
	store, err := manager.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr := manager.New(client, store, "primary")
	mgr.SetARPMonitor(monitor.URL)
	var fake6, peer6 *keatest.Server
	if withoutPeer {
		fake.HA = nil
	} else {
		// kea-dhcp6 runs as a hot-standby pair as well; the standby misses the reservation
		fake6, peer6 = keatest.New6(), keatest.New6()
		fake6.HA, peer6.HA = pairState("primary", "standby"), pairState("standby", "primary")
		peer6.Hosts = nil
		for i, f := range []*keatest.Server{fake6, peer6} {
			srv6 := httptest.NewServer(f)
			t.Cleanup(srv6.Close)
			client6, err := kea.NewClient(kea.Options{URL: srv6.URL})
			if err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				mgr.SetDHCP6(client6)
			} else {
				mgr.AddPeer6("standby", client6)
			}
		}
	}
	peer := keatest.New()
	seed(peer)
	peer.Hosts = append(peer.Hosts, map[string]any{"subnet-id": 0, "hw-address": "00:00:0c:00:00:fe", "ip-address": "192.0.2.250"})
	// the peer is the other half of the pair
	servers := peer.HA[0].(map[string]any)["ha-servers"].(map[string]any)
	local, remote := servers["local"].(map[string]any), servers["remote"].(map[string]any)
	local["server-name"], local["role"], remote["server-name"], remote["role"] = "server2", "standby", "server1", "primary"
	peerSrv := httptest.NewServer(peer)
	t.Cleanup(peerSrv.Close)
	peerClient, err := kea.NewClient(kea.Options{URL: peerSrv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !withoutPeer {
		mgr.AddPeer("standby", peerClient)
	}
	dashboard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if dashboardAnswer != "" {
			fmt.Fprint(w, dashboardAnswer)
			return
		}
		fmt.Fprintf(w, `[{"mac":"06:00:00:AA:01:64","name":"bedroom","station_id":"ap-1","iface":"home-5ghz-network","signal":-61,
			"connected_time":3600,"inactive":200,"last_seen":%q,"device_info":{"ipaddrs":["192.0.2.100"],"name":"phone"}}]`, time.Now().UTC().Format(time.RFC3339))
	}))
	t.Cleanup(dashboard.Close)
	mgr.SetAPDashboard(dashboard.URL)
	handler := web.New(mgr, testAuth(t, accounts), "")
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	// every page starts logged in; the login form has tests of its own

	// The password store flag keeps Chromium from waiting for a locked keyring.
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(),
		chromedp.Headless, chromedp.NoSandbox, chromedp.DisableGPU, chromedp.NoFirstRun, chromedp.NoDefaultBrowserCheck,
		chromedp.Flag("password-store", "basic"),
		chromedp.WindowSize(viewportWidth, viewportHeight),
	)
	t.Cleanup(cancelAlloc)
	ctx, cancelCtx := chromedp.NewContext(allocCtx)
	t.Cleanup(cancelCtx)
	ctx, cancelTimeout := context.WithTimeout(ctx, browserTimeout)
	t.Cleanup(cancelTimeout)

	b := &browser{t: t, ctx: ctx, url: srv.URL, fake: fake, fake6: fake6, peer6: peer6}
	if err := chromedp.Do(ctx, chromedp.Navigate("about:blank")); err != nil {
		t.Fatal(err)
	}
	if loggedIn {
		session := sessionFor(t, srv.URL, "admin", adminPassword)
		if _, err := chromedp.Call(ctx, network.SetCookie, network.SetCookieParams{Name: session.Name, Value: session.Value, URL: srv.URL}); err != nil {
			t.Fatal(err)
		}
	}
	go func() {
		for e, err := range chromedp.Events(ctx, runtime.ExceptionThrown) {
			if err != nil {
				return
			}
			b.mu.Lock()
			b.errors = append(b.errors, e.ExceptionDetails.Text)
			b.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for _, e := range b.errors {
			t.Errorf("JavaScript error: %s", e)
		}
	})
	return b
}

func (b *browser) do(steps ...chromedp.Action[chromedp.Void]) {
	b.t.Helper()
	if err := chromedp.Do(b.ctx, steps...); err != nil {
		b.t.Fatal(err)
	}
}

// poll waits for a condition. Errors do not end the wait: a query that meets a
// page being replaced fails, the next one reaches the new page.
func (b *browser) poll(js string) {
	b.t.Helper()
	var last error
	for b.ctx.Err() == nil {
		done, err := chromedp.Run(b.ctx, chromedp.Evaluate[bool](js))
		if err == nil && done {
			return
		}
		last = err
		time.Sleep(pollInterval)
	}
	b.t.Fatalf("waiting for %s: %v", js, last)
}

func (b *browser) open(page, heading string) {
	b.t.Helper()
	b.do(chromedp.Navigate(b.url + "/#/" + page))
	b.poll(fmt.Sprintf(`document.querySelector('h1')?.textContent === %q`, heading))
	b.poll(`document.querySelector('.tile, tbody tr td, .panel dl') !== null`)
}

func evaluate[T any](b *browser, js string) T {
	b.t.Helper()
	value, err := chromedp.Run(b.ctx, chromedp.Evaluate[T](js))
	if err != nil {
		b.t.Fatalf("%s: %v", js, err)
	}
	return value
}

func (b *browser) exec(js string) {
	b.t.Helper()
	evaluate[any](b, "(() => { "+js+" })() ?? null")
}

func (b *browser) text(js string) string {
	b.t.Helper()
	return evaluate[string](b, js)
}

func (b *browser) theme(theme, mode string) {
	b.t.Helper()
	b.do(chromedp.Navigate(b.url + "/"))
	b.exec(fmt.Sprintf(`localStorage.setItem('theme', %q); localStorage.setItem('mode', %q)`, theme, mode))
	b.do(chromedp.Reload())
}

func TestDashboard(t *testing.T) {
	b := start(t)
	b.open("dashboard", "Dashboard")
	b.poll(`document.querySelector('.table-wrap tbody tr') !== null`)
	head := b.text(`[...document.querySelectorAll('.table-wrap th')].map(t => t.textContent).join(',')`)
	for _, column := range []string{"IP", "MAC", "Client-ID", "Hostname", "Reservation", "AP", "Subnet", "Last contact", "Status", "Expires", "Remaining"} {
		if !strings.Contains(head, column) {
			t.Errorf("column %s missing in %s", column, head)
		}
	}
	rows := b.text(`[...document.querySelectorAll('h2')].find(x => x.textContent === 'Recent leases').nextElementSibling.innerText`)
	reserved := b.text(`[...document.querySelectorAll('h2')].find(x => x.textContent === 'Recent leases').nextElementSibling.querySelector('a.reservation-link')?.textContent ?? ''`)
	if !strings.Contains(rows, "active") || reserved != "nas" || !strings.Contains(rows, "new") {
		t.Errorf("recent leases: %s", rows)
	}
	if strings.Contains(b.text(`document.body.innerText`), "[object") {
		t.Error("an element was rendered as text")
	}
}

func TestTablesFitInEveryTheme(t *testing.T) {
	b := start(t)
	pages := map[string]string{"classes": "Client classes", "dashboard": "Dashboard", "leases": "Leases", "reservations": "Reservations", "devices": "Devices", "subnets": "Subnets"}
	for _, theme := range []string{"classic", "modern"} {
		for _, mode := range []string{"light", "dark"} {
			b.theme(theme, mode)
			for page, heading := range pages {
				b.open(page, heading)
				state := b.text(`document.documentElement.dataset.theme + '/' + document.documentElement.dataset.mode`)
				if state != theme+"/"+mode {
					t.Errorf("%s: state %s", page, state)
				}
				// these show the most columns and may scroll on small screens
				if page != "dashboard" && page != "devices" && evaluate[bool](b, `[...document.querySelectorAll('.table-wrap')].some(w => w.scrollWidth > w.clientWidth)`) {
					t.Errorf("%s/%s %s: table is wider than the page", theme, mode, page)
				}
			}
		}
	}
}

func TestSelectedThemeIsKept(t *testing.T) {
	b := start(t)
	b.open("dashboard", "Dashboard")
	b.exec(`const s = document.getElementById('theme-select'); s.value = 'modern'; s.dispatchEvent(new Event('change'));`)
	b.exec(`const s = document.getElementById('mode-select'); s.value = 'dark'; s.dispatchEvent(new Event('change'));`)
	b.do(chromedp.Reload())
	if got := b.text(`document.documentElement.dataset.theme + '/' + document.documentElement.dataset.mode`); got != "modern/dark" {
		t.Errorf("got %s", got)
	}
	b.exec(`localStorage.setItem('theme', 'fancy')`)
	b.do(chromedp.Reload())
	if got := b.text(`document.documentElement.dataset.theme`); got != "modern" {
		t.Errorf("former theme name: %s", got)
	}
}

func TestVendorAndRandomMAC(t *testing.T) {
	b := start(t)
	b.open("leases", "Leases")
	b.poll(`document.querySelector('td .state.neutral') !== null`)
	if got := b.text(`document.querySelector('.vendor').title`); !strings.Contains(got, "Cisco") {
		t.Errorf("vendor tooltip %q", got)
	}
	if got := b.text(`[...document.querySelectorAll('td .state.neutral')].find(e => e.textContent === 'local')?.title ?? ''`); !strings.Contains(got, "Private MAC") {
		t.Errorf("random marker %q", got)
	}
}

func TestReserveFromLeaseWithEditedAddress(t *testing.T) {
	b := start(t)
	b.open("leases", "Leases")
	b.exec(`[...document.querySelectorAll('button')].find(b => b.textContent === 'Reserve').click()`)
	b.poll(`document.querySelector('dialog form') !== null`)
	field := `[...document.querySelectorAll('dialog label')].find(l => l.firstChild.textContent === 'IP address').querySelector('input')`
	if got := b.text(field + `.value`); got != "192.0.2.100" {
		t.Errorf("prefilled address %q", got)
	}
	if got := b.text(`document.querySelector('dialog select').value`); got != "0" {
		t.Errorf("global subnet not preselected: %q", got)
	}
	b.exec(field + `.value = '192.0.2.40'; document.querySelector('dialog form').requestSubmit()`)
	b.poll(`document.querySelector('dialog h2')?.textContent === 'Change succeeded'`)
	b.exec(`[...document.querySelectorAll('dialog button')].find(b => b.textContent === 'Close').click()`)
	// the reservation shows with its name and, as it differs, its address
	b.poll(`document.querySelector('tbody').innerText.includes('fridge → 192.0.2.40')`)
	if !b.fake.HasCall("config-write") {
		t.Error("the reservation was not persisted")
	}
}

func TestEditSubnetAsksBeforeMovingLeases(t *testing.T) {
	b := start(t)
	b.open("subnets", "Subnets")
	b.exec(`document.querySelector('.subnet-edit').click()`)
	b.poll(`document.querySelector('dialog form') !== null`)
	pool := `document.querySelector('dialog .pool-row input')`
	if got := b.text(pool + `.value`); got != "192.0.2.100 - 192.0.2.199" {
		t.Errorf("pool %q", got)
	}
	b.exec(pool + `.value = '192.0.2.150-192.0.2.199'; document.querySelector('dialog form').requestSubmit()`)
	b.poll(`document.querySelector('dialog h2')?.textContent === 'Apply this change?'`)
	if got := b.text(`document.querySelector('dialog').innerText`); !strings.Contains(got, "192.0.2.100") {
		t.Errorf("warning %q", got)
	}
	if b.fake.HasCall("subnet4-update") {
		t.Fatal("applied before the confirmation")
	}
	b.exec(`document.querySelector('dialog button.solid').click()`)
	b.poll(`document.querySelector('dialog h2')?.textContent === 'Change succeeded'`)
	b.exec(`[...document.querySelectorAll('dialog button')].find(b => b.textContent === 'Close').click()`)
	b.poll(`document.body.innerText.includes('192.0.2.150-192.0.2.199')`)
}

func TestEditSubnetShowsInvalidInput(t *testing.T) {
	b := start(t)
	b.open("subnets", "Subnets")
	b.exec(`document.querySelector('.subnet-edit').click()`)
	b.poll(`document.querySelector('dialog form') !== null`)
	b.exec(`document.querySelector('dialog .pool-row input').value = '10.0.0.1-10.0.0.9'; document.querySelector('dialog form').requestSubmit()`)
	b.poll(`document.querySelector('dialog .note.crit')?.textContent.includes('is not inside') === true`)
	if b.fake.HasCall("subnet4-update") {
		t.Error("invalid input reached Kea")
	}
}

func TestClassesAreVisibleAndEditable(t *testing.T) {
	b := start(t)
	b.open("reservations", "Reservations")
	if got := b.text(`document.querySelector('tbody').innerText`); !strings.Contains(got, "direct-dns") {
		t.Errorf("reservations: %s", got)
	}
	if got := b.text(`document.querySelector('tbody a.tag').title`); got != "domain-name-servers=192.0.2.1" {
		t.Errorf("tooltip of the class: %q", got)
	}
	b.open("leases", "Leases")
	if got := b.text(`document.querySelector('tbody').innerText`); !strings.Contains(got, "direct-dns") {
		t.Errorf("leases: %s", got)
	}
	b.open("classes", "Client classes")
	if got := b.text(`document.querySelector('tbody').innerText`); !strings.Contains(got, "domain-name-servers=192.0.2.1") {
		t.Errorf("classes: %s", got)
	}
	b.exec(`[...document.querySelectorAll('button')].find(b => b.textContent === 'Edit').click()`)
	b.poll(`document.querySelector('dialog form') !== null`)
	b.exec(`document.querySelector('dialog .option-row input:nth-child(2)').value = '192.0.2.2'; document.querySelector('dialog form').requestSubmit()`)
	b.poll(`document.querySelector('dialog h2')?.textContent === 'Change succeeded'`)
	b.exec(`[...document.querySelectorAll('dialog button')].find(b => b.textContent === 'Close').click()`)
	b.poll(`document.querySelector('tbody').innerText.includes('192.0.2.2')`)
}

func TestEditChangesTheMAC(t *testing.T) {
	b := start(t)
	b.open("reservations", "Reservations")
	b.exec(`[...document.querySelectorAll('button')].find(b => b.textContent === 'Edit').click()`)
	b.poll(`document.querySelector('dialog form') !== null`)
	field := `[...document.querySelectorAll('dialog label')].find(l => l.firstChild.textContent === 'Identifier').querySelector('input')`
	b.exec(field + `.value = '00:00:0c:00:00:77'; document.querySelector('dialog form').requestSubmit()`)
	// the monitor sees other devices on this address
	b.poll(`document.querySelector('dialog h2')?.textContent === 'Apply this change?'`)
	b.exec(`document.querySelector('dialog button.solid').click()`)
	b.poll(`document.querySelector('dialog h2')?.textContent === 'Change succeeded'`)
	b.exec(`[...document.querySelectorAll('dialog button')].find(b => b.textContent === 'Close').click()`)
	b.poll(`document.querySelector('tbody').innerText.includes('00:00:0c:00:00:77')`)
	if got := b.text(`document.querySelector('tbody').innerText`); !strings.Contains(got, "direct-dns") {
		t.Errorf("class lost: %s", got)
	}
}

func TestDialogShowsFormerUseOfAnAddress(t *testing.T) {
	b := start(t)
	b.open("reservations", "Reservations")
	b.poll(`[...document.querySelectorAll('button')].some(x => x.textContent === 'New reservation' && !x.disabled)`)
	b.exec(`[...document.querySelectorAll('button')].find(b => b.textContent === 'New reservation').click()`)
	b.poll(`document.querySelector('dialog form') !== null`)
	input := func(label string) string {
		return fmt.Sprintf(`[...document.querySelectorAll('dialog label')].find(l => l.firstChild.textContent === %q).querySelector('input')`, label)
	}
	b.exec(input("Identifier") + `.value = '02:00:00:00:00:55'; ` + input("IP address") + `.value = '192.0.2.30'; ` + input("IP address") + `.dispatchEvent(new Event('input'));`)
	b.poll(`document.querySelector('dialog .hint')?.textContent.includes('was last used by 00:00:0c:00:00:03 camera.lan') === true`)
	if b.fake.HasCall("reservation-add") {
		t.Fatal("the hint saved something")
	}
	b.exec(`document.querySelector('dialog form').requestSubmit()`)
	b.poll(`document.querySelector('dialog h2')?.textContent === 'Change succeeded'`)
	if got := b.text(`document.querySelector('dialog').innerText`); strings.Contains(got, "was last used") {
		t.Errorf("hint repeated in the result: %q", got)
	}
}

func TestDialogShowsAddressConflictsWhileTyping(t *testing.T) {
	b := start(t)
	b.open("reservations", "Reservations")
	b.poll(`[...document.querySelectorAll('button')].some(x => x.textContent === 'New reservation' && !x.disabled)`)
	b.exec(`[...document.querySelectorAll('button')].find(b => b.textContent === 'New reservation').click()`)
	b.poll(`document.querySelector('dialog form') !== null`)
	address := `[...document.querySelectorAll('dialog label')].find(l => l.firstChild.textContent === 'IP address').querySelector('input')`
	mac := `[...document.querySelectorAll('dialog label')].find(l => l.firstChild.textContent === 'Identifier').querySelector('input')`
	b.exec(mac + `.value = '02:00:00:00:00:55';`)
	typeAddress := func(ip string) {
		b.exec(fmt.Sprintf(`const a = %s; a.value = %q; a.dispatchEvent(new Event('input'));`, address, ip))
	}
	typeAddress("192.0.2.10")
	b.poll(`document.querySelector('dialog .hint.crit')?.textContent.includes('already reserved') === true`)
	typeAddress("192.0.2.100")
	b.poll(`document.querySelector('dialog .hint.crit')?.textContent.includes('in use by 06:00:00:aa:01:64') === true`)
	typeAddress("192.0.2.150")
	b.poll(`document.querySelector('dialog .hint.warn')?.textContent.includes('inside the dynamic pool') === true`)
	typeAddress("192.0.2.5")
	b.poll(`document.querySelector('dialog .hint') === null`)
	typeAddress("192.0.")
	b.poll(`document.querySelector('dialog .hint') === null`)
}

func TestNewReservationIsRefusedWhenAddressIsReserved(t *testing.T) {
	b := start(t)
	b.open("reservations", "Reservations")
	b.poll(`[...document.querySelectorAll('button')].some(x => x.textContent === 'New reservation' && !x.disabled)`)
	b.exec(`[...document.querySelectorAll('button')].find(b => b.textContent === 'New reservation').click()`)
	b.poll(`document.querySelector('dialog form') !== null`)
	input := func(label string) string {
		return fmt.Sprintf(`[...document.querySelectorAll('dialog label')].find(l => l.firstChild.textContent === %q).querySelector('input')`, label)
	}
	b.exec(input("Identifier") + `.value = '02:00:00:00:00:55'; ` + input("IP address") + `.value = '192.0.2.10'; document.querySelector('dialog form').requestSubmit()`)
	b.poll(`document.querySelector('dialog .note.crit')?.textContent.includes('already reserved') === true`)
	b.exec(input("IP address") + `.value = '192.0.2.100'; document.querySelector('dialog form button.primary').disabled = false; document.querySelector('dialog form').requestSubmit()`)
	b.poll(`[...document.querySelectorAll('dialog .note.crit')].some(n => n.textContent.includes('in use by 06:00:00:aa:01:64 phone.lan'))`)
	if b.fake.HasCall("reservation-add") {
		t.Error("the reservation was added")
	}
}

func TestNotesKeepTheirColorInEveryTheme(t *testing.T) {
	b := start(t)
	for _, theme := range []string{"classic", "modern"} {
		for _, mode := range []string{"light", "dark"} {
			b.theme(theme, mode)
			b.open("reservations", "Reservations")
			b.poll(`[...document.querySelectorAll('button')].some(x => x.textContent === 'New reservation' && !x.disabled)`)
			b.exec(`[...document.querySelectorAll('button')].find(b => b.textContent === 'New reservation').click()`)
			b.poll(`document.querySelector('dialog form') !== null`)
			b.exec(`const a = [...document.querySelectorAll('dialog label')].find(l => l.firstChild.textContent === 'IP address').querySelector('input');
				a.value = '192.0.2.10'; a.dispatchEvent(new Event('input'));`)
			b.poll(`document.querySelector('dialog .hint.crit') !== null`)
			want := `getComputedStyle(document.documentElement).getPropertyValue('--crit').trim()`
			colored := `(() => { const c = document.createElement('div'); c.style.color = ` + want + `; document.body.append(c);
				const expected = getComputedStyle(c).color; c.remove();
				const n = getComputedStyle(document.querySelector('dialog .hint.crit'));
				return n.borderLeftColor === expected && n.color === expected; })()`
			if !evaluate[bool](b, colored) {
				t.Errorf("%s/%s: conflict note has no color", theme, mode)
			}
			width := evaluate[int](b, `Math.round(document.querySelector('dialog .class-list input').getBoundingClientRect().width)`)
			if width > 30 {
				t.Errorf("%s/%s: checkbox is %d px wide", theme, mode, width)
			}
		}
	}
}

func TestReservationDialogOffersClasses(t *testing.T) {
	b := start(t)
	b.open("reservations", "Reservations")
	b.exec(`[...document.querySelectorAll('button')].find(b => b.textContent === 'Edit').click()`)
	b.poll(`document.querySelector('dialog .class-list') !== null`)
	if !evaluate[bool](b, `document.querySelector('dialog .class-list input').checked`) {
		t.Error("assigned class not selected")
	}
	b.exec(`[...document.querySelectorAll('dialog button')].find(b => b.textContent === 'Cancel').click()`)
	b.poll(`[...document.querySelectorAll('button')].some(x => x.textContent === 'New reservation' && !x.disabled)`)
	b.exec(`[...document.querySelectorAll('button')].find(b => b.textContent === 'New reservation').click()`)
	b.poll(`document.querySelector('dialog .class-list') !== null`)
	if evaluate[bool](b, `document.querySelector('dialog .class-list input').checked`) {
		t.Error("a new reservation must not have a class")
	}
}

func TestWifiClientsShowWhereTheyAre(t *testing.T) {
	b := start(t)
	b.open("devices", "Devices")
	if got := b.text(`document.querySelector('tbody').innerText`); !strings.Contains(got, "bedroom -61") {
		t.Errorf("devices: %s", got)
	}
	b.open("leases", "Leases")
	if got := b.text(`document.querySelector('tbody').innerText`); !strings.Contains(got, "bedroom") {
		t.Errorf("leases: %s", got)
	}
}

func TestSignalIsShownAsBars(t *testing.T) {
	b := start(t)
	b.open("devices", "Devices")
	if lit := evaluate[int](b, `document.querySelectorAll('.bars i.on').length`); lit != 2 {
		t.Errorf("%d bars lit for -61 dBm", lit)
	}
	if lines := evaluate[int](b, `document.querySelector('.where').getClientRects().length`); lines != 1 {
		t.Errorf("where takes %d lines", lines)
	}
}

func TestDevicesMarkAge(t *testing.T) {
	b := start(t)
	b.open("devices", "Devices")
	rows := b.text(`document.querySelector('tbody').innerText`)
	for _, want := range []string{"current", "recent", "stale", "camera.lan"} {
		if !strings.Contains(rows, want) {
			t.Errorf("%s missing in %s", want, rows)
		}
	}
	if stale := evaluate[int](b, `document.querySelectorAll('tr.stale').length`); stale != 1 {
		t.Errorf("%d stale rows", stale)
	}
	if !strings.Contains(rows, "MAC mismatch") || strings.Contains(rows, "printer.lan") {
		t.Errorf("default view: %s", rows)
	}
	if got := b.text(`document.querySelector('.note.crit')?.textContent ?? ''`); !strings.Contains(got, "00:00:0c:00:00:09") {
		t.Errorf("no conflict warning: %q", got)
	}
	link := `[...document.querySelectorAll('tbody a')].find(a => a.textContent === 'nas')`
	if got := b.text(link + `?.getAttribute('href') ?? ''`); got != "#/reservations?q=00%3A00%3A0c%3A00%3A00%3A01" {
		t.Errorf("link to the reservation %q", got)
	}
	b.exec(`const s = document.querySelector('.toolbar select'); s.value = ''; s.dispatchEvent(new Event('change'));`)
	b.poll(`document.querySelector('tbody').innerText.includes('printer.lan')`)
	if got := b.text(`document.querySelector('tbody').innerText`); !strings.Contains(got, "nas") || !strings.Contains(got, "direct-dns") {
		t.Errorf("all devices: %s", got)
	}
}

func TestSubnetLinksToItsReservations(t *testing.T) {
	b := start(t)
	b.open("subnets", "Subnets")
	if got := b.text(`document.querySelector('dl a').getAttribute('href')`); !strings.HasPrefix(got, "#/reservations") {
		t.Errorf("link %q", got)
	}
	b.exec(`document.querySelector('dl a').click()`)
	b.poll(`document.querySelector('h1')?.textContent === 'Reservations'`)
}

func TestServersShowDifferencesAndCopyThem(t *testing.T) {
	b := start(t)
	b.do(chromedp.Navigate(b.url + "/#/servers"))
	b.poll(`[...document.querySelectorAll('.server-card')].filter(c => c.querySelector('.family-tag')?.textContent === 'DHCPv4').length === 2`)
	b.poll(`[...document.querySelectorAll('.server-card')].filter(c => c.querySelector('.family-tag')?.textContent === 'DHCPv4')[1].textContent.includes('1 difference')`)
	b.exec(`[...document.querySelectorAll('.server-card')].filter(c => c.querySelector('.family-tag')?.textContent === 'DHCPv4').flatMap(c => [...c.querySelectorAll('button')]).find(x => x.textContent === 'Copy from primary').click()`)
	b.poll(`document.querySelector('dialog')?.textContent.includes('Make standby match the primary')`)
	b.exec(`[...document.querySelectorAll('dialog button')].find(x => x.textContent === 'Copy').click()`)
	b.poll(`document.querySelector('dialog')?.textContent.includes('Change succeeded')`)
	b.exec(`[...document.querySelectorAll('dialog button')].find(x => x.textContent === 'Close').click()`)
	b.poll(`[...document.querySelectorAll('.server-card')].filter(c => c.querySelector('.family-tag')?.textContent === 'DHCPv4')[1]?.textContent.includes('in sync')`)
}

func TestDashboardListsBothServers(t *testing.T) {
	b := start(t)
	b.open("dashboard", "Dashboard")
	b.poll(`[...document.querySelectorAll('h2')].some(x => x.textContent === 'Servers')`)
	b.poll(`document.body.innerText.includes('standby') && document.body.innerText.includes('1 differ')`)
}

func TestServersPageOperatesHA(t *testing.T) {
	b := start(t)
	b.do(chromedp.Navigate(b.url + "/#/servers"))
	b.poll(`[...document.querySelectorAll('button')].some(x => x.textContent === 'Start maintenance')`)
	b.exec(`[...document.querySelectorAll('.server-card')].filter(c => c.querySelector('.family-tag')?.textContent === 'DHCPv4').find(x => x.textContent.includes('standby')).querySelector('button').click()`)
	b.poll(`document.querySelector('dialog')?.textContent.includes('takes over all DHCP traffic')`)
	b.exec(`document.querySelector('dialog button.solid').click()`)
	b.poll(`document.querySelector('dialog h2')?.textContent === 'Change succeeded'`)
}

func TestServersPageWithOneServer(t *testing.T) {
	withoutPeer = true
	b := start(t)
	withoutPeer = false
	b.do(chromedp.Navigate(b.url + "/#/servers"))
	b.poll(`document.querySelectorAll('.server-card').length === 1`)
	got := b.text(`document.querySelector('main').innerText`)
	for _, part := range []string{"No HA", "3.0.3"} {
		if !strings.Contains(got, part) {
			t.Errorf("%s missing in %s", part, got)
		}
	}
	for _, part := range []string{"Configuration", "Changes run on the primary", "Copy from primary"} {
		if strings.Contains(got, part) {
			t.Errorf("%s belongs to several servers: %s", part, got)
		}
	}
}

// pairState is the HA state of one half of a hot-standby pair.
func pairState(local, remote string) []any {
	return []any{map[string]any{
		"ha-mode": "hot-standby",
		"ha-servers": map[string]any{
			"local": map[string]any{"role": local, "scopes": []any{}, "server-name": local, "state": "hot-standby", "system-time": "2026-10-30 12:00:00"},
			"remote": map[string]any{"age": 3, "clock-skew": 0, "communication-interrupted": false, "in-touch": true,
				"last-scopes": []any{}, "last-state": "hot-standby", "role": remote, "server-name": remote},
		},
	}}
}

func TestServersPageForIPv6(t *testing.T) {
	b := start(t)
	b.do(chromedp.Navigate(b.url + "/#/servers"))
	b.poll(`[...document.querySelectorAll('.server-card')].filter(c => c.querySelector('.family-tag')?.textContent === 'DHCPv6').length === 2`)
	b.poll(`document.querySelector('.family-switch') === null && document.querySelectorAll('.family-grid > h2')[1]?.textContent === 'DHCPv6'`)
	b.poll(`[...document.querySelectorAll('.server-card')].filter(c => c.querySelector('.family-tag')?.textContent === 'DHCPv6')[1]?.textContent.includes('1 difference')`)
	got := b.text(`document.querySelector('main').innerText`)
	for _, part := range []string{"HA connection"} {
		if !strings.Contains(got, part) {
			t.Errorf("%s missing in %s", part, got)
		}
	}
	b.exec(`[...document.querySelectorAll('.server-card')].filter(c => c.querySelector('.family-tag')?.textContent === 'DHCPv6').flatMap(c => [...c.querySelectorAll('button')]).find(x => x.textContent === 'Copy from primary').click()`)
	b.exec(`[...document.querySelectorAll('dialog button')].find(x => x.textContent === 'Copy').click()`)
	b.poll(`document.querySelector('dialog')?.textContent.includes('Change succeeded')`)
	b.peer6.Lock()
	hosts := len(b.peer6.Hosts)
	b.peer6.Unlock()
	if hosts != 1 || b.fake.HasCall("reservation-add") {
		t.Errorf("the IPv6 standby holds %d reservations", hosts)
	}
	b.exec(`[...document.querySelectorAll('dialog button')].find(x => x.textContent === 'Close').click()`)
	b.poll(`[...document.querySelectorAll('.server-card')].filter(c => c.querySelector('.family-tag')?.textContent === 'DHCPv6')[1]?.textContent.includes('in sync')`)

	// maintenance goes to the IPv6 pair, not to the IPv4 one
	b.exec(`[...document.querySelectorAll('.server-card')].filter(c => c.querySelector('.family-tag')?.textContent === 'DHCPv6')[1].querySelector('.ha-actions button').click()`)
	b.poll(`document.querySelector('dialog')?.textContent.includes('takes over all DHCP traffic')`)
	b.exec(`document.querySelector('dialog button.solid').click()`)
	b.poll(`document.querySelector('dialog h2')?.textContent === 'Change succeeded'`)
	if !b.fake6.HasCall("ha-maintenance-start") || b.fake.HasCall("ha-maintenance-start") {
		t.Error("maintenance of the IPv6 standby did not go to the IPv6 primary")
	}

	b.open("dashboard", "Dashboard")
	b.poll(`[...document.querySelectorAll('td .tag')].some(x => x.textContent === 'IPv6')`)
}

// Both families of a server stand in one row, also when one side has a warning,
// an HA state that is not ok and a list of differences.
func TestServersPageKeepsFamiliesAligned(t *testing.T) {
	b := start(t)
	b.peer6.Lock()
	b.peer6.HA[0].(map[string]any)["ha-servers"].(map[string]any)["local"].(map[string]any)["state"] = "waiting"
	b.peer6.Unlock()
	b.fake6.Lock()
	b.fake6.HA[0].(map[string]any)["ha-servers"].(map[string]any)["remote"].(map[string]any)["last-state"] = "waiting"
	b.fake6.Unlock()
	b.do(chromedp.Navigate(b.url + "/#/servers"))
	b.poll(`document.querySelectorAll('.family-grid .server-card').length === 4 && document.body.innerText.includes('DHCPv6: HA partner')`)
	// v4, v6 pairs in the grid order: HA panels, then one pair per server
	misaligned := b.text(`(() => {
		const grid = [...document.querySelector('.family-grid').children].slice(2);
		const out = [];
		for (let i = 0; i < grid.length; i += 2) {
			const a = grid[i].getBoundingClientRect(), b = grid[i + 1].getBoundingClientRect();
			if (Math.abs(a.top - b.top) > 1 || Math.abs(a.height - b.height) > 1) out.push(i / 2 + ': ' + a.top + '/' + a.height + ' vs ' + b.top + '/' + b.height);
		}
		return out.join('; ');
	})()`)
	if misaligned != "" {
		t.Errorf("rows are not aligned: %s", misaligned)
	}
}

func TestNoIPv6SwitchWithoutDHCPv6(t *testing.T) {
	withoutPeer = true
	b := start(t)
	withoutPeer = false
	b.open("leases", "Leases")
	if b.text(`String(document.querySelector('.family-switch'))`) != "null" {
		t.Error("the IPv4/IPv6 switch shows without DHCPv6")
	}
}

func TestIPv6PagesSwitchFromIPv4(t *testing.T) {
	b := start(t)
	b.open("leases", "Leases")
	b.exec(`[...document.querySelectorAll('.family-switch a')].find(a => a.textContent === 'IPv6').click()`)
	b.poll(`document.querySelectorAll('tbody tr').length === 2 && document.querySelector('.family-switch a.active')?.textContent === 'IPv6'`)
	// the lease of a known MAC is named by the IPv4 reservation
	got := b.text(`document.querySelector('tbody').innerText`)
	for _, part := range []string{"fd00:db8:1::1000", "nas", "192.0.2.10", "printer"} {
		if !strings.Contains(got, part) {
			t.Errorf("%s missing in %s", part, got)
		}
	}
	b.do(chromedp.Navigate(b.url + "/#/reservations?v=6"))
	b.poll(`document.querySelector('tbody')?.innerText.includes('fd00:db8:1::10')`)
	b.do(chromedp.Navigate(b.url + "/#/subnets?v=6"))
	b.poll(`document.body.innerText.includes('61.440 addresses') && document.body.innerText.includes('fd00:db8:2::/48, each /56')`)
	// the IPv4 page stays the default and the navigation the same
	b.open("leases", "Leases")
	b.poll(`document.querySelector('.family-switch a.active')?.textContent === 'IPv4'`)
}

func TestIPv6ReservationFromLease(t *testing.T) {
	b := start(t)
	b.do(chromedp.Navigate(b.url + "/#/leases?v=6"))
	// the printer is reserved already, the laptop is not
	b.poll(`[...document.querySelectorAll('tbody tr')].filter(tr => tr.querySelector('button')?.textContent === 'Reserve').length === 1`)
	b.fake6.Lock()
	b.fake6.Leases6[1]["hostname"] = "laptop."
	b.fake6.Unlock()
	b.do(chromedp.Navigate(b.url + "/#/leases?v=6&reload"))
	b.poll(`document.querySelectorAll('tbody tr').length === 2 && [...document.querySelectorAll('tbody button')].some(x => x.textContent === 'Reserve')`)
	b.exec(`[...document.querySelectorAll('tbody button')].find(x => x.textContent === 'Reserve').click()`)
	b.poll(`document.querySelector('dialog form input.mono-input')?.value.startsWith('00:04:aa')`)
	// the dot of the FQDN the client sent is not part of the suggested name
	b.poll(`[...document.querySelectorAll('dialog input')].some(i => i.value === 'laptop')`)
	b.exec(`document.querySelector('dialog form').requestSubmit()`)
	b.poll(`document.querySelector('dialog h2')?.textContent === 'Change succeeded'`)
	if !b.fake6.HasCall("config-write") {
		t.Error("kea-dhcp6 did not save")
	}
	b.exec(`[...document.querySelectorAll('dialog button')].find(x => x.textContent === 'Close').click()`)
	b.do(chromedp.Navigate(b.url + "/#/reservations?v=6"))
	b.poll(`document.querySelector('tbody')?.innerText.includes('fd00:db8:1::1001') && document.querySelector('tbody')?.innerText.includes('laptop')`)
}

func TestReservationDialogsShowTheOtherFamily(t *testing.T) {
	b := start(t)
	b.open("reservations", "Reservations")
	b.exec(`[...document.querySelectorAll('tbody tr')].find(tr => tr.innerText.includes('00:00:0c:00:00:01')).querySelector('button').click()`)
	b.poll(`document.querySelector('dialog .other-family')?.textContent.includes('reserved fd00:db8:1::10')`)
	b.exec(`document.querySelector('dialog .other-family a').click()`)
	b.poll(`location.hash.startsWith('#/reservations?v=6') && document.querySelector('tbody')?.innerText.includes('fd00:db8:1::10')`)
	b.exec(`[...document.querySelectorAll('tbody button')].find(x => x.textContent === 'Edit').click()`)
	b.poll(`document.querySelector('dialog .other-family')?.textContent.includes('192.0.2.10')`)
}

func TestClippedCellOpensForCopying(t *testing.T) {
	b := start(t)
	b.do(chromedp.Navigate(b.url + "/#/leases?v=6"))
	b.poll(`document.querySelectorAll('tbody tr').length === 2`)
	b.exec(`document.querySelector('tbody td.clip').click()`)
	b.poll(`document.querySelector('tbody td.clip.open') !== null && getSelection().toString().startsWith('00:0')`)
	b.open("leases", "Leases")
	b.poll(`[...document.querySelectorAll('tbody td.clip .tag')].some(t => t.textContent === 'DUID')`)
	b.exec(`[...document.querySelectorAll('tbody td.clip .tag')].find(t => t.textContent === 'DUID').closest('td').click()`)
	// only the id is selected, not the mark in front of it
	b.poll(`getSelection().toString() === 'ff:12:34:56:78:00:03:00:01:02:00:00:aa:01:66'`)
	// one line, like the DUID of IPv6: as high as a closed cell
	b.poll(`(() => { const v = document.querySelector('tbody td.clip.open .value'); return v.getClientRects().length === 1 })()`)
}

func TestIPv6LeaseLinksToItsReservation(t *testing.T) {
	b := start(t)
	b.do(chromedp.Navigate(b.url + "/#/leases?v=6"))
	b.poll(`[...document.querySelectorAll('tbody a.reservation-link')].some(a => a.textContent === 'printer')`)
	b.exec(`[...document.querySelectorAll('tbody a.reservation-link')].find(a => a.textContent === 'printer').click()`)
	b.poll(`location.hash.startsWith('#/reservations?v=6') && document.querySelectorAll('tbody tr').length === 1 && document.querySelector('tbody').innerText.includes('fd00:db8:1::10')`)
}

func TestAboutLine(t *testing.T) {
	b := start(t)
	b.open("dashboard", "Dashboard")
	b.poll(`document.getElementById('about')?.querySelector('a')?.href === 'https://github.com/vgropp/kea-roost'`)
}

func TestClientPopupExplainsAddress(t *testing.T) {
	b := start(t)
	b.open("leases", "Leases")
	b.exec(`document.querySelector('tbody .client-link').click()`)
	b.poll(`document.querySelector('dialog.wide')?.textContent.includes('Why it gets this address')`)
	got := b.text(`document.querySelector('dialog').innerText`)
	for _, part := range []string{"Lease", "Reservation", "Options it receives"} {
		if !strings.Contains(got, part) {
			t.Errorf("%s missing in %s", part, got)
		}
	}
	click := func(x, y string) string {
		return `(() => { const d = document.querySelector('dialog'), r = d.getBoundingClientRect(), o = { bubbles: true, clientX: ` + x + `, clientY: ` + y + ` };
			d.dispatchEvent(new MouseEvent('mousedown', o)); d.dispatchEvent(new MouseEvent('mouseup', o)); d.dispatchEvent(new MouseEvent('click', o)); })()`
	}
	b.exec(click("r.left + 40", "r.top + 40"))
	if !evaluate[bool](b, `document.querySelector('dialog').open`) {
		t.Fatal("a click inside closed the popup")
	}
	if got := b.text(`[...document.querySelectorAll('dialog .dialog-actions button')].map(x => x.textContent).join()`); strings.Contains(got, "Close") {
		t.Fatalf("buttons: %s", got)
	}
	b.exec(click("r.left - 20", "r.top - 20"))
	b.poll(`!document.querySelector('dialog').open && !document.querySelector('dialog').classList.contains('wide')`)
	b.exec(`document.querySelector('tbody .client-link').click()`)
	b.poll(`document.querySelector('dialog .dialog-close') !== null && document.querySelector('dialog.wide') !== null`)
	b.exec(`document.querySelector('dialog .dialog-close').click()`)
	b.poll(`!document.querySelector('dialog').open`)
	// closing while it loads must stay closed once the answer arrives
	b.exec(`document.querySelector('tbody .client-link').click(); document.querySelector('dialog .dialog-close').click()`)
	b.exec(`setTimeout(() => { window.reopened = document.querySelector('dialog').open; }, 500)`)
	b.poll(`window.reopened !== undefined`)
	if evaluate[bool](b, `window.reopened`) {
		t.Fatal("the popup came back after it was closed")
	}
	b.open("devices", "Devices")
	b.poll(`document.querySelector('tbody .client-link') !== null`)
	b.open("reservations", "Reservations")
	b.poll(`document.querySelector('tbody .client-link') !== null`)
}

func TestHealthPageShowsChecksTrafficAndReview(t *testing.T) {
	b := start(t)
	b.do(chromedp.Navigate(b.url + "/#/health"))
	b.poll(`document.querySelector('.health-summary') !== null`)
	got := b.text(`document.body.textContent`)
	for _, part := range []string{"DHCP traffic", "Configuration review", "standby", "Discover"} {
		if !strings.Contains(got, part) {
			t.Errorf("%s missing", part)
		}
	}
}

func TestReserveSitsInTheEmptyReservationCard(t *testing.T) {
	b := start(t)
	b.open("leases", "Leases")
	b.exec(`[...document.querySelectorAll('tbody .client-link')].find(x => x.textContent === '192.0.2.100').click()`)
	b.poll(`document.querySelector('dialog .client-card .card-action') !== null`)
	card := b.text(`document.querySelector('dialog .card-action').parentElement.textContent`)
	if !strings.Contains(card, "No reservation") {
		t.Fatalf("card: %s", card)
	}
	b.exec(`document.querySelector('dialog .card-action').click()`)
	b.poll(`document.querySelector('dialog form') !== null`)
}

func TestNavigationIsGrouped(t *testing.T) {
	b := start(t)
	b.open("dashboard", "Dashboard")
	got := b.text(`[...document.querySelectorAll('.nav-group')].map(g => [...g.children].map(a => a.textContent).join('+')).join(' | ')`)
	if want := "Dashboard | Leases+Reservations+Devices | Classes+Subnets | Health | Servers+Audit log | Settings"; got != want {
		t.Fatalf("got %s", got)
	}
	b.open("subnets", "Subnets")
	b.poll(`document.querySelector('nav a.active')?.textContent === 'Subnets'`)
}

func TestClientCardsKeepAddressesOnOneLine(t *testing.T) {
	b := start(t)
	b.open("leases", "Leases")
	b.exec(`document.querySelector('tbody .client-link').click()`)
	b.poll(`document.querySelector('dialog .client-card dd') !== null`)
	for _, width := range []int{1000, 760} {
		b.exec(fmt.Sprintf(`document.querySelector('dialog').style.width = '%dpx'`, width))
		overflow := evaluate[bool](b, `[...document.querySelectorAll('dialog .client-card dd')].some(d => d.scrollWidth > d.clientWidth + 1)`)
		broken := evaluate[bool](b, `[...document.querySelectorAll('dialog .client-card dd')].some(d => /^\\d+\\.\\d+\\.\\d+\\.\\d+$/.test(d.textContent) && d.getClientRects()[0].height > 30)`)
		if overflow || broken {
			t.Errorf("width %d: overflow %v, broken %v", width, overflow, broken)
		}
	}
}

func TestHealthListsItsChecksOnClick(t *testing.T) {
	b := start(t)
	b.do(chromedp.Navigate(b.url + "/#/health"))
	b.poll(`document.querySelector('.check-list summary') !== null`)
	summary := b.text(`document.querySelector('.check-list summary').textContent`)
	if !strings.Contains(summary, "checks passed") {
		t.Fatalf("summary: %s", summary)
	}
	if !strings.Contains(b.text(`document.querySelector('.check-list summary').title`), "Reachable") {
		t.Fatal("no tooltip")
	}
	b.exec(`document.querySelector('.check-list summary').click()`)
	b.poll(`document.querySelector('.check-list').open && document.querySelector('.check-list li') !== null`)
	first := b.text(`document.querySelector('.check-list li').textContent`)
	if !strings.Contains(first, "Configuration in step") {
		t.Errorf("a problem must come first: %s", first)
	}
	b.exec(`document.querySelector('.check-list').dataset.old = '1'; [...document.querySelectorAll('button')].find(x => x.textContent === 'Refresh').click()`)
	b.poll(`document.querySelector('.check-list') !== null && !document.querySelector('.check-list').dataset.old`)
	if !evaluate[bool](b, `document.querySelector('.check-list').open`) {
		t.Error("a refresh closed the list")
	}
}

func TestReviewShowsItsRulesOnClick(t *testing.T) {
	b := start(t)
	b.do(chromedp.Navigate(b.url + "/#/health"))
	b.poll(`document.querySelector('.rule-list summary') !== null`)
	if got := b.text(`document.querySelector('.rule-list summary').textContent`); !strings.Contains(got, "rules checked") {
		t.Fatalf("summary: %s", got)
	}
	b.exec(`document.querySelector('.rule-list summary').click()`)
	b.poll(`document.querySelector('.rule-list').open && document.querySelector('.rule-list').textContent.includes('No address is online on two devices')`)
}

func TestFindingsLinkToTheirEntry(t *testing.T) {
	b := start(t)
	b.do(chromedp.Navigate(b.url + "/#/health"))
	b.poll(`[...document.querySelectorAll('tbody a')].some(x => x.getAttribute('href')?.startsWith('#/reservations?q=') || x.classList.contains('client-link'))`)
}

func TestSettingsPageSaves(t *testing.T) {
	b := start(t)
	b.do(chromedp.Navigate(b.url + "/#/settings"))
	b.poll(`document.querySelector('input[name="reviewDays"]') !== null`)
	b.exec(`document.querySelector('input[name="reviewDays"]').value = '90'; document.querySelector('main form, form').requestSubmit()`)
	b.poll(`document.body.textContent.includes('Settings saved.')`)
	b.poll(`document.querySelector('input[name="reviewDays"]').value === '90' && document.body.textContent.includes('changed')`)
	// the user list below may speak of passwords, the settings themselves must not
	if got := b.text(`document.querySelector('main form').textContent`); strings.Contains(strings.ToLower(got), "password") {
		t.Errorf("no secrets belong here: %s", got)
	}
}

func TestFindingCanBeHiddenAndShownAgain(t *testing.T) {
	b := start(t)
	b.do(chromedp.Navigate(b.url + "/#/health"))
	row := `[...document.querySelectorAll('tbody tr')].find(x => [...x.querySelectorAll('button')].some(y => y.textContent === 'Hide'))`
	b.poll(row + ` !== undefined`)
	buttons := `[...document.querySelectorAll('tbody button')].filter(x => x.textContent === 'Hide').length`
	before := evaluate[int](b, buttons)
	b.exec(row + `.querySelector('button').click()`)
	b.poll(fmt.Sprintf(`%s === %d`, buttons, before-1))
	b.poll(`document.querySelector('.rule-list summary').textContent.includes('hidden')`)
	b.exec(`const s = [...document.querySelectorAll('select')].find(x => [...x.options].some(o => o.value === 'hidden')); s.value = 'hidden'; s.dispatchEvent(new Event('change'))`)
	b.poll(`[...document.querySelectorAll('tbody button')].some(x => x.textContent === 'Show')`)
	b.exec(`[...document.querySelectorAll('tbody button')].find(x => x.textContent === 'Show').click()`)
	b.poll(`![...document.querySelectorAll('tbody button')].some(x => x.textContent === 'Show')`)
}

func TestAuditLogHasPagesAndFilters(t *testing.T) {
	b := start(t)
	b.open("dashboard", "Dashboard")
	b.exec(`(async () => {
		for (let i = 2; i < 58; i++) await fetch('api/settings', { method: 'PUT', body: JSON.stringify({ reviewDays: i }) });
		window.seeded = true;
	})()`)
	b.poll(`window.seeded === true`)
	b.do(chromedp.Navigate(b.url + "/#/audit"))
	b.poll(`document.querySelector('.pager')?.textContent.includes('Page 1 of 2')`)
	rows := `document.querySelectorAll('.panel.table-wrap')[0].querySelectorAll('tbody tr').length`
	if got := evaluate[int](b, rows); got != 50 {
		t.Fatalf("first page: %d rows", got)
	}
	b.exec(`[...document.querySelectorAll('.pager button')].find(x => x.textContent === 'Next').click()`)
	b.poll(`document.querySelector('.pager').textContent.includes('Page 2 of 2')`)
	// 56 changes and the login of the test
	if got := evaluate[int](b, rows); got != 7 {
		t.Fatalf("second page: %d rows", got)
	}
	b.exec(`const s = document.querySelector('select[aria-label="All actions"]'); s.value = 'settings-update'; s.dispatchEvent(new Event('change'))`)
	b.poll(`document.querySelector('.pager').textContent.includes('Page 1 of 2')`)
	b.exec(`const r = document.querySelector('select[aria-label="All results"]'); r.value = 'failed'; r.dispatchEvent(new Event('change'))`)
	b.poll(`document.querySelector('select[aria-label="All results"]').value === ''`)
}

func TestSubnetShowsAddressMap(t *testing.T) {
	b := start(t)
	b.do(chromedp.Navigate(b.url + "/#/subnets"))
	b.poll(`document.querySelectorAll('.addr-grid .addr').length === 256`)
	b.poll(`document.querySelector('.addr-grid .addr.reserved') !== null && document.querySelector('.addr-grid .addr.pool') !== null`)
	if got := b.text(`document.querySelector('.address-map').textContent`); !strings.Contains(got, "Free in the pool") || !strings.Contains(got, "Free for static addresses") {
		t.Errorf("map: %s", got)
	}
	b.exec(`document.querySelector('.addr-grid a.addr').click()`)
	b.poll(`document.querySelector('dialog.wide')?.textContent.includes('Why it')`)
}

func TestFreeRangesShowTheLargestFirst(t *testing.T) {
	b := start(t)
	f := b.fake
	f.Lock()
	f.Hosts = nil
	for i := 2; i < 254; i += 3 {
		f.Hosts = append(f.Hosts, map[string]any{"subnet-id": 0, "hw-address": fmt.Sprintf("02:00:00:00:01:%02x", i), "ip-address": fmt.Sprintf("192.0.2.%d", i)})
	}
	f.Unlock()
	b.do(chromedp.Navigate(b.url + "/#/subnets"))
	b.poll(`document.querySelector('.more-ranges summary')?.textContent.startsWith('Show all')`)
	if got := evaluate[int](b, `document.querySelector('.free-ranges').children.length`); got != 8 {
		t.Fatalf("shown: %d", got)
	}
}

func TestBlockOpensItsAddresses(t *testing.T) {
	b := start(t)
	f := b.fake
	f.Lock()
	f.Config["subnet4"].([]any)[0].(map[string]any)["subnet"] = "192.0.0.0/20"
	f.Unlock()
	b.do(chromedp.Navigate(b.url + "/#/subnets"))
	b.poll(`document.querySelectorAll('.addr-block').length === 16`)
	b.exec(`document.querySelectorAll('.addr-block')[2].click()`)
	b.poll(`document.querySelectorAll('.addr-detail .addr').length === 256 && document.querySelector('.addr-block.active') !== null`)
	b.exec(`document.querySelector('.addr-detail button').click()`)
	b.poll(`document.querySelectorAll('.addr-detail .addr').length === 0 && document.querySelector('.addr-block.active') === null`)
}

func TestInheritedNamesAreNoPages(t *testing.T) {
	b := start(t)
	for _, page := range []string{"constructor", "toString", "__proto__", "nothing"} {
		b.do(chromedp.Navigate(b.url + "/#/" + page))
		b.poll(`document.querySelector('h1')?.textContent === 'Dashboard'`)
	}
}

func fill(b *browser, selector, value string) {
	b.t.Helper()
	b.exec(fmt.Sprintf(`document.querySelector(%q).value = %q`, selector, value))
}

func TestLoginPage(t *testing.T) {
	b := startWith(t, login, false)
	b.do(chromedp.Navigate(b.url + "/"))
	b.poll(`document.querySelector('h1')?.textContent === 'Log in'`)
	if evaluate[bool](b, `!document.querySelector('nav').hidden`) {
		t.Error("the navigation shows before the login")
	}
	fill(b, `input[name="username"]`, "admin")
	fill(b, `input[name="password"]`, "wrong passphrase")
	b.exec(`document.querySelector('form').requestSubmit()`)
	b.poll(`document.body.textContent.includes('wrong user name or password')`)
	fill(b, `input[name="password"]`, adminPassword)
	b.exec(`document.querySelector('form').requestSubmit()`)
	b.poll(`document.querySelector('h1')?.textContent === 'Dashboard'`)
	b.poll(`document.querySelector('.account-button')?.textContent.includes('admin')`)
	b.exec(`document.querySelector('.account-button').click()`)
	b.poll(`[...document.querySelectorAll('dialog button')].some(x => x.textContent === 'Log out')`)
	b.exec(`[...document.querySelectorAll('dialog button')].find(x => x.textContent === 'Log out').click()`)
	b.poll(`document.querySelector('h1')?.textContent === 'Log in'`)
}

func TestOnboardingPage(t *testing.T) {
	b := startWith(t, web.Auth{SetupCode: "code from the log"}, false)
	b.do(chromedp.Navigate(b.url + "/"))
	b.poll(`document.querySelector('h1')?.textContent === 'Welcome to Kea Roost'`)
	fill(b, `input[name="code"]`, "code from the log")
	fill(b, `input[name="username"]`, "owner")
	b.exec(`for (const p of document.querySelectorAll('input[type="password"]')) p.value = 'owner passphrase'`)
	b.exec(`document.querySelector('form').requestSubmit()`)
	b.poll(`document.querySelector('h1')?.textContent === 'Dashboard'`)
	b.poll(`document.querySelector('.account-button')?.textContent.includes('owner')`)
	b.poll(`[...document.querySelectorAll('nav a')].some(a => a.textContent === 'Settings')`)
}

func TestUsersPage(t *testing.T) {
	b := start(t)
	b.do(chromedp.Navigate(b.url + "/#/settings"))
	b.poll(`document.querySelectorAll('#users tbody tr').length === 2`)
	b.exec(`[...document.querySelectorAll('button')].find(x => x.textContent === 'New user').click()`)
	b.poll(`document.querySelector('dialog form') !== null`)
	b.exec(`const f = document.querySelector('dialog form'); f.querySelector('input').value = 'helper';
		f.querySelector('input[type="password"]').value = 'helper passphrase'; f.requestSubmit()`)
	b.poll(`document.querySelectorAll('#users tbody tr').length === 3`)
}

func TestSecondFactorSetupInTheBrowser(t *testing.T) {
	b := start(t)
	b.open("dashboard", "Dashboard")
	b.exec(`document.querySelector('.account-button').click()`)
	b.poll(`document.querySelector('dialog input.switch') !== null`)
	b.exec(`document.querySelector('dialog input.switch').click()`)
	b.poll(`document.querySelector('.totp-qr')?.complete && document.querySelector('.totp-qr').naturalWidth > 0`)
	secret := strings.ReplaceAll(b.text(`document.querySelector('.totp-secret').textContent`), " ", "")
	code, err := auth.TOTPAt(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b.exec(fmt.Sprintf(`const f = [...document.querySelectorAll('dialog form')].at(-1); f.querySelector('input').value = %q;
		f.querySelector('input[type="password"]').value = %q; f.requestSubmit()`, code, adminPassword))
	b.poll(`document.querySelector('dialog input.switch')?.checked === true && !document.querySelector('.totp-qr')`)
}

func TestDashboardShowsIPv6LeasesAndDeletesOne(t *testing.T) {
	b := start(t)
	b.open("dashboard", "Dashboard")
	b.poll(`[...document.querySelectorAll('h2')].some(x => x.textContent === 'Recent IPv6 leases')`)
	b.poll(`[...document.querySelectorAll('.meter-row a')].some(a => a.getAttribute('href') === '#/subnets?v=6' && a.textContent === 'fd00:db8:1::/64')`)
	b.do(chromedp.Navigate(b.url + "/#/leases?v=6"))
	b.poll(`document.querySelectorAll('tbody tr').length === 2`)
	b.exec(`[...document.querySelectorAll('tbody button')].find(x => x.textContent === 'Delete').click()`)
	b.poll(`document.querySelector('dialog')?.textContent.includes('Really delete lease fd00:db8:1::1000')`)
	b.exec(`document.querySelector('dialog button.solid').click()`)
	b.poll(`document.querySelectorAll('tbody tr').length === 1`)
	b.fake6.Lock()
	left := len(b.fake6.Leases6)
	b.fake6.Unlock()
	b.peer6.Lock()
	leftPeer := len(b.peer6.Leases6)
	b.peer6.Unlock()
	if left != 1 || leftPeer != 1 {
		t.Errorf("leases left: %d on the primary, %d on the peer", left, leftPeer)
	}
}

func TestIPv6SubnetCanBeEdited(t *testing.T) {
	b := start(t)
	b.do(chromedp.Navigate(b.url + "/#/subnets?v=6"))
	b.poll(`document.querySelector('.subnet-edit') !== null`)
	b.exec(`document.querySelector('.subnet-edit').click()`)
	b.poll(`document.querySelector('dialog h2')?.textContent === 'Edit subnet fd00:db8:1::/64'`)
	got := b.text(`document.querySelector('dialog').innerText`)
	if strings.Contains(got, "Gateway") || !strings.Contains(got, "Preferred lifetime") {
		t.Errorf("dialog: %s", got)
	}
	b.exec(`(() => {
		const inputs = document.querySelectorAll('dialog .pool-row input');
		inputs[0].value = 'fd00:db8:1::1000-fd00:db8:1::1fff';
		const preferred = [...document.querySelectorAll('dialog .field')].find(f => f.textContent.includes('Preferred')).querySelector('input');
		preferred.value = '3600';
		document.querySelector('dialog form').requestSubmit();
	})()`)
	// the laptop lease at ::1001 stays inside, so there is nothing to confirm
	b.poll(`document.querySelector('dialog h2')?.textContent === 'Change succeeded'`)
	for _, f := range []*keatest.Server{b.fake6, b.peer6} {
		f.Lock()
		s := f.Config["subnet6"].([]any)[0].(map[string]any)
		pools, preferred := fmt.Sprint(s["pools"]), fmt.Sprint(s["preferred-lifetime"])
		f.Unlock()
		if !strings.Contains(pools, "fd00:db8:1::1000-fd00:db8:1::1fff") || preferred != "3600" {
			t.Errorf("subnet: %s %s", pools, preferred)
		}
	}
}

func TestIPv6AddressOpensItsExplanation(t *testing.T) {
	b := start(t)
	b.do(chromedp.Navigate(b.url + "/#/leases?v=6"))
	b.poll(`document.querySelectorAll('tbody tr').length === 2`)
	b.exec(`[...document.querySelectorAll('tbody a.client-link')].find(a => a.textContent === 'fd00:db8:1::1000').click()`)
	b.poll(`document.querySelector('dialog')?.textContent.includes('Why it has this address')`)
	got := b.text(`document.querySelector('dialog').innerText`)
	for _, part := range []string{"printer", "comes from the pool", "dns-servers", "IPv4 device"} {
		if !strings.Contains(got, part) {
			t.Errorf("%s missing in %s", part, got)
		}
	}
}

func TestIPv6PrefixDelegationCanBeEdited(t *testing.T) {
	b := start(t)
	b.do(chromedp.Navigate(b.url + "/#/subnets?v=6"))
	b.poll(`document.querySelector('.subnet-edit') !== null`)
	b.exec(`document.querySelector('.subnet-edit').click()`)
	b.poll(`document.querySelectorAll('dialog .pd-row').length === 1`)
	if got := b.text(`document.querySelector('dialog .pd-row input').value`); got != "fd00:db8:2::/48" {
		t.Fatalf("prefix pool shown as %q", got)
	}
	b.exec(`(() => {
		[...document.querySelectorAll('dialog button')].find(x => x.textContent === 'Add prefix pool').click();
		const inputs = document.querySelectorAll('dialog .pd-row')[1].querySelectorAll('input');
		inputs[0].value = 'fd00:db8:3::/48';
		inputs[1].value = '60';
		document.querySelector('dialog form').requestSubmit();
	})()`)
	b.poll(`document.querySelector('dialog h2')?.textContent === 'Change succeeded'`)
	b.fake6.Lock()
	got := fmt.Sprint(b.fake6.Config["subnet6"].([]any)[0].(map[string]any)["pd-pools"])
	b.fake6.Unlock()
	if !strings.Contains(got, "fd00:db8:3::") || !strings.Contains(got, "delegated-len:60") || !strings.Contains(got, "fd00:db8:2::") {
		t.Errorf("pd-pools: %s", got)
	}

	b.do(chromedp.Navigate(b.url + "/#/reservations?v=6"))
	b.poll(`[...document.querySelectorAll('button')].some(x => x.textContent === 'New reservation')`)
	b.poll(`[...document.querySelectorAll('button')].some(x => x.textContent === 'New reservation' && !x.disabled)`)
	b.exec(`[...document.querySelectorAll('button')].find(x => x.textContent === 'New reservation').click()`)
	b.poll(`document.querySelector('dialog form') !== null`)
	b.exec(`(() => {
		const field = label => [...document.querySelectorAll('dialog .field')].find(f => f.querySelector('label')?.textContent.startsWith(label)).querySelector('input');
		field('Identifier').value = '00:03:00:01:02:00:00:00:00:77';
		field('Delegated prefixes').value = 'fd00:db8:2:300::/56';
		field('DNS name').value = 'router';
		document.querySelector('dialog form').requestSubmit();
	})()`)
	b.poll(`document.querySelector('dialog h2')?.textContent === 'Change succeeded'`)
	b.fake6.Lock()
	last := fmt.Sprint(b.fake6.Hosts[len(b.fake6.Hosts)-1]["prefixes"])
	b.fake6.Unlock()
	if last != "[fd00:db8:2:300::/56]" {
		t.Errorf("reserved prefixes: %s", last)
	}
}

func TestIPv6SubnetMap(t *testing.T) {
	b := start(t)
	b.do(chromedp.Navigate(b.url + "/#/subnets?v=6"))
	b.poll(`document.querySelectorAll('.map6 .addr-grid')[0]?.children.length === 256`)
	// the first square holds both leases
	b.exec(`document.querySelector('.map6 .addr-grid button').click()`)
	b.poll(`document.querySelector('.addr-detail')?.textContent.includes('fd00:db8:1::1000')`)
	got := b.text(`document.querySelector('.map6-panel').innerText`)
	for _, part := range []string{"Pool fd00:db8:1::1000 - fd00:db8:1::ffff", "a square holds 240", "Prefix pool fd00:db8:2::/48, each /56", "0 of 256 prefixes used", "Outside of the pools", "fd00:db8:1::10"} {
		if !strings.Contains(got, part) {
			t.Errorf("%s missing in %s", part, got)
		}
	}
}

func TestIPv6ReservationWarnsAboutThePool(t *testing.T) {
	b := start(t)
	b.do(chromedp.Navigate(b.url + "/#/leases?v=6"))
	b.poll(`[...document.querySelectorAll('tbody button')].some(x => x.textContent === 'Reserve')`)
	b.exec(`[...document.querySelectorAll('tbody button')].find(x => x.textContent === 'Reserve').click()`)
	// the lease address lies in the pool: the dialog says so and offers a free one
	b.poll(`document.querySelector('dialog .field-note')?.textContent.includes('fd00:db8:1::1000 - fd00:db8:1::ffff')`)
	b.poll(`document.querySelector('dialog .note.warn')?.textContent.includes('inside the dynamic pool')`)
	b.exec(`[...document.querySelectorAll('dialog .note.warn button')].find(x => x.textContent.startsWith('Use ')).click()`)
	b.poll(`[...document.querySelectorAll('dialog input')].some(i => i.value === 'fd00:db8:1::2') && !document.querySelector('dialog .note.warn')`)
}

func TestIPv6ReservationStartsWithThePrefix(t *testing.T) {
	b := start(t)
	b.do(chromedp.Navigate(b.url + "/#/reservations?v=6"))
	b.poll(`[...document.querySelectorAll('button')].some(x => x.textContent === 'New reservation' && !x.disabled)`)
	b.exec(`[...document.querySelectorAll('button')].find(x => x.textContent === 'New reservation').click()`)
	field := `label => [...document.querySelectorAll('dialog .field')].find(f => f.querySelector('label')?.textContent.startsWith(label)).querySelector('input')`
	b.poll(`(` + field + `)('Addresses').value === 'fd00:db8:1::'`)
	b.exec(`(() => {
		const field = ` + field + `;
		field('Identifier').value = '00:03:00:01:02:00:00:00:00:31';
		field('Addresses').value = '::30';
		document.querySelector('dialog form').requestSubmit();
	})()`)
	b.poll(`document.querySelector('dialog h2')?.textContent === 'Change succeeded'`)
	b.fake6.Lock()
	got := fmt.Sprint(b.fake6.Hosts[len(b.fake6.Hosts)-1]["ip-addresses"])
	b.fake6.Unlock()
	if got != "[fd00:db8:1::30]" {
		t.Errorf("reserved %s", got)
	}
}

func TestDevicesShowIPv6(t *testing.T) {
	b := start(t)
	b.do(chromedp.Navigate(b.url + "/#/devices"))
	b.poll(`[...document.querySelectorAll('th')].some(th => th.textContent.startsWith('IPv6'))`)
	b.poll(`document.querySelector('tbody')?.innerText.includes('fd00:db8:1::1001')`)
	if strings.Contains(b.text(`document.querySelector('tbody').innerText`), "fe80::") {
		t.Error("a link-local address is listed")
	}
}

func TestDevicesRecentFilterIncludesCurrent(t *testing.T) {
	b := start(t)
	b.do(chromedp.Navigate(b.url + "/#/devices?reserved=&seen=recent"))
	b.poll(`document.querySelectorAll('tbody tr').length > 0`)
	got := b.text(`[...document.querySelectorAll('tbody tr')].map(tr => tr.innerText).join('\n')`)
	if !strings.Contains(got, "current") || strings.Contains(got, "stale") {
		t.Errorf("rows: %s", got)
	}
}
