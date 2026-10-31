//go:build e2e

package web_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// showcase fills the test double with an invented home network.
func showcase(b *browser) {
	type device struct {
		ip, mac, name, class string
		reserved, leased     bool
	}
	devices := []device{
		{"192.0.2.10", "00:11:32:aa:01:10", "nas", "direct-dns", true, true},
		{"192.0.2.11", "b8:27:eb:aa:01:11", "homeassistant", "direct-dns", true, true},
		{"192.0.2.12", "00:0c:29:aa:01:12", "printer", "", true, true},
		{"192.0.2.13", "dc:a6:32:aa:01:13", "camera-door", "", true, true},
		{"192.0.2.14", "00:17:88:aa:01:14", "light-bridge", "direct-dns", true, false},
		{"192.0.2.15", "ac:de:48:aa:01:15", "tv-livingroom", "", true, true},
		{"192.0.2.100", "7e:90:27:aa:01:64", "phone", "", false, true},
		{"192.0.2.101", "3c:22:fb:aa:01:65", "laptop", "", false, true},
		{"192.0.2.102", "18:fe:34:aa:01:66", "plug-office", "", false, true},
		{"192.0.2.103", "5c:cf:7f:aa:01:67", "sensor-garden", "", false, true},
		{"192.0.2.104", "a4:cf:12:aa:01:68", "thermostat", "", false, true},
		{"192.0.2.105", "2e:b6:fd:aa:01:69", "tablet", "", false, true},
	}
	now := time.Now().Unix()
	b.fake.Lock()
	defer b.fake.Unlock()
	b.fake.Hosts, b.fake.Leases = nil, nil
	for i, d := range devices {
		if d.reserved {
			host := map[string]any{"subnet-id": 0, "hw-address": d.mac, "ip-address": d.ip, "hostname": d.name}
			if d.class != "" {
				host["client-classes"] = []any{d.class}
			}
			b.fake.Hosts = append(b.fake.Hosts, host)
		}
		if d.leased {
			b.fake.Leases = append(b.fake.Leases, map[string]any{
				"ip-address": d.ip, "hw-address": d.mac, "hostname": d.name, "subnet-id": 1, "state": 0,
				"client-id": "01:" + d.mac, "cltt": now - int64(i*190), "valid-lft": 4000,
			})
		}
	}

	seen := func(age time.Duration) string { return time.Now().Add(-age).UTC().Format(time.RFC3339) }
	monitorAnswer = "["
	dashboardAnswer = "["
	for i, d := range devices {
		age := time.Duration(i) * time.Minute
		if i > 9 {
			age = time.Duration(i) * 9 * time.Hour
		}
		monitorAnswer += fmt.Sprintf(`{"mac":%q,"ipv4":[%q],"hostname":%q,"first_seen":%q,"last_seen":%q},`,
			d.mac, d.ip, d.name+".lan.", seen(time.Duration(40+i)*24*time.Hour), seen(age))
		if i%2 == 0 && i < 9 {
			dashboardAnswer += fmt.Sprintf(`{"mac":%q,"name":%q,"station_id":"ap-1","iface":"home","signal":%d,"connected_time":3600,"inactive":300,"last_seen":%q,"device_info":{"ipaddrs":[%q],"name":%q}},`,
				d.mac, []string{"attic", "garden", "office"}[i%3], -48-i*4, seen(0), d.ip, d.name)
		}
	}
	// a device that took the address of a reservation
	monitorAnswer += fmt.Sprintf(`{"mac":"02:42:ac:aa:01:99","ipv4":["192.0.2.12"],"hostname":"guest-box.lan.","first_seen":%q,"last_seen":%q}]`, seen(time.Hour), seen(time.Minute))
	dashboardAnswer = dashboardAnswer[:len(dashboardAnswer)-1] + "]"
}

// TestScreenshots writes the pictures of the README from invented data:
// SCREENSHOT_DIR=docs/screenshots go test -tags e2e -run TestScreenshots ./internal/web
func TestScreenshots(t *testing.T) {
	dir := os.Getenv("SCREENSHOT_DIR")
	if dir == "" {
		t.Skip("SCREENSHOT_DIR is not set")
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join("..", "..", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b := start(t)
	defer func() { monitorAnswer, dashboardAnswer = "", "" }()
	showcase(b)
	b.do(chromedp.EmulateViewport(1500, 760))

	shot := func(name string) {
		t.Helper()
		b.do(chromedp.Sleep(400 * time.Millisecond))
		data, err := chromedp.Run(b.ctx, chromedp.CaptureScreenshot())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".png"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	b.theme("modern", "dark")
	b.open("dashboard", "Dashboard")
	shot("dashboard")
	b.open("devices", "Devices")
	shot("devices")
	b.open("reservations", "Reservations")
	b.exec(`[...document.querySelectorAll('button')].find(b => b.textContent === 'New reservation').click()`)
	b.poll(`document.querySelector('dialog form') !== null`)
	b.exec(`const a = [...document.querySelectorAll('dialog label')].find(l => l.firstChild.textContent === 'IP address').querySelector('input');
		[...document.querySelectorAll('dialog label')].find(l => l.firstChild.textContent === 'Identifier').querySelector('input').value = '02:00:00:aa:01:20';
		a.value = '192.0.2.10'; a.dispatchEvent(new Event('input'));`)
	b.poll(`document.querySelector('dialog .hint.crit') !== null`)
	shot("reservation-dialog")

	b.theme("classic", "light")
	b.open("leases", "Leases")
	shot("leases")
	b.open("reservations", "Reservations")
	shot("reservations")
	b.open("subnets", "Subnets")
	shot("subnets")
	b.do(chromedp.Navigate(b.url + "/#/health"))
	b.poll(`document.querySelector('.health-summary') !== null`)
	shot("health")
	b.open("leases", "Leases")
	b.exec(`document.querySelector('tbody .client-link').click()`)
	b.poll(`document.querySelector('dialog.wide')?.textContent.includes('Why it gets this address')`)
	shot("client")
}
