package manager

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const devices = `[
 {"mac":"AA:BB:CC:00:00:01","ipv4":["192.0.2.10"],"hostname":"reserved-mac.lan.","last_seen":"2026-10-30T16:00:00Z"},
 {"mac":"aa:bb:cc:00:00:02","ipv4":["192.0.2.11"],"last_seen":"2026-10-30T16:00:00Z"},
 {"mac":"aa:bb:cc:00:00:03","ipv4":["192.0.2.12"],"hostname":"camera.lan.","first_seen":"2026-06-01T10:00:00Z","last_seen":"2026-10-30T16:00:00Z"},
 {"mac":"aa:bb:cc:00:00:04","ipv4":["0.0.0.0","169.254.1.1","192.0.2.150","192.0.2.13"],"hostname":"phone","last_seen":"2026-10-30T16:00:00Z"},
 {"mac":"aa:bb:cc:00:00:05","ipv4":["10.9.9.9"],"last_seen":"2026-10-30T16:00:00Z"},
 {"mac":"aa:bb:cc:00:00:06","ipv6":["fe80::1"],"last_seen":"2026-10-30T16:00:00Z"}
]`

func withMonitor(t *testing.T, e *env, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	e.mgr.SetARPMonitor(srv.URL+"/", 7)
}

func TestDevicesListsDevicesWithoutReservation(t *testing.T) {
	e := setup(t)
	e.fake.Hosts = []map[string]any{
		{"subnet-id": 0, "hw-address": "aa:bb:cc:00:00:01", "ip-address": "192.0.2.200"},
		{"subnet-id": 1, "hw-address": "aa:bb:cc:00:00:99", "ip-address": "192.0.2.11"},
	}
	e.fake.Leases = []map[string]any{{"ip-address": "192.0.2.150", "subnet-id": 1, "state": 0, "cltt": time.Now().Unix(), "valid-lft": 100}}
	var query string
	withMonitor(t, e, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.String()
		w.Write([]byte(devices))
	})

	list, err := e.mgr.Devices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if query != "/api/current?days=7" {
		t.Errorf("query %s", query)
	}
	if len(list.Devices) != 4 {
		t.Fatalf("got %+v", list.Devices)
	}
	byMAC, byIP := list.Devices[0], list.Devices[1]
	if byMAC.Reservation == nil || byMAC.Reservation.IPAddress != "192.0.2.200" {
		t.Errorf("reserved by MAC: %+v", byMAC)
	}
	if byIP.Reservation == nil || byIP.Reservation.Identifier != "aa:bb:cc:00:00:99" {
		t.Errorf("reserved by address: %+v", byIP)
	}
	camera, phone := list.Devices[2], list.Devices[3]
	if camera.Reservation != nil || phone.Reservation != nil {
		t.Errorf("unreserved devices: %+v %+v", camera, phone)
	}
	if camera.IPAddress != "192.0.2.12" || camera.Hostname != "camera.lan" || camera.InPool || camera.Lease != nil {
		t.Errorf("camera %+v", camera)
	}
	if camera.SubnetID != 1 || camera.Subnet != "192.0.2.0/24" {
		t.Errorf("camera subnet %+v", camera)
	}
	if camera.FirstSeen.Year() != 2026 || !phone.FirstSeen.IsZero() {
		t.Errorf("first seen %v %v", camera.FirstSeen, phone.FirstSeen)
	}
	if phone.IPAddress != "192.0.2.150" || !phone.InPool || phone.Lease == nil {
		t.Errorf("phone %+v", phone)
	}
}

func TestDashboardRecentLeases(t *testing.T) {
	e := setup(t)
	now := time.Now().Unix()
	for i := range 7 {
		e.fake.Leases = append(e.fake.Leases, map[string]any{
			"ip-address": fmt.Sprintf("192.0.2.%d", 100+i), "hw-address": fmt.Sprintf("aa:bb:cc:00:01:%02x", i),
			"subnet-id": 1, "state": 0, "cltt": now - int64(i*60), "valid-lft": 4000,
		})
	}
	e.fake.Leases = append(e.fake.Leases, map[string]any{ // newest, but expired long ago
		"ip-address": "192.0.2.120", "subnet-id": 1, "state": 2, "cltt": now, "valid-lft": 4000,
	})
	withMonitor(t, e, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `[{"mac":"AA:BB:CC:00:01:00","first_seen":%q,"last_seen":%q},
		                 {"mac":"aa:bb:cc:00:01:01","first_seen":"2026-01-01T00:00:00Z","last_seen":%q}]`,
			time.Now().Add(-time.Hour).Format(time.RFC3339), time.Now().Format(time.RFC3339), time.Now().Format(time.RFC3339))
	})

	d := e.mgr.Dashboard(context.Background())
	if len(d.RecentLeases) != 5 {
		t.Fatalf("got %d leases", len(d.RecentLeases))
	}
	if d.RecentLeases[0].IPAddress != "192.0.2.100" || d.RecentLeases[4].IPAddress != "192.0.2.104" {
		t.Errorf("order %s .. %s", d.RecentLeases[0].IPAddress, d.RecentLeases[4].IPAddress)
	}
	if !d.RecentLeases[0].New || d.RecentLeases[1].New || d.RecentLeases[2].New {
		t.Errorf("new marks %v %v %v", d.RecentLeases[0].New, d.RecentLeases[1].New, d.RecentLeases[2].New)
	}
}

func TestDashboardRecentLeasesWithoutMonitor(t *testing.T) {
	e := setup(t)
	e.fake.Leases = []map[string]any{{"ip-address": "192.0.2.100", "subnet-id": 1, "state": 0, "cltt": time.Now().Unix(), "valid-lft": 100}}
	e.mgr.SetARPMonitor("http://127.0.0.1:1", 7)
	d := e.mgr.Dashboard(context.Background())
	if len(d.RecentLeases) != 1 || d.RecentLeases[0].New {
		t.Fatalf("got %+v", d.RecentLeases)
	}
}

func TestDevicesReportReservationConflicts(t *testing.T) {
	e := setup(t)
	e.fake.Hosts = []map[string]any{{"subnet-id": 0, "hw-address": "aa:bb:cc:00:00:01", "ip-address": "192.0.2.10", "hostname": "gone"}}
	now := time.Now().UTC()
	body := fmt.Sprintf(`[{"mac":"aa:bb:cc:00:00:01","ipv4":["192.0.2.10"],"last_seen":%q},
	                      {"mac":"de:ad:be:ef:00:01","ipv4":["192.0.2.10"],"last_seen":%q},
	                      {"mac":"de:ad:be:ef:00:02","ipv4":["192.0.2.10"],"last_seen":%q}]`,
		now.Format(time.RFC3339), now.Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339))
	withMonitor(t, e, func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(body)) })

	list, err := e.mgr.Devices(context.Background())
	if err != nil || len(list.Devices) != 3 {
		t.Fatalf("got %+v %v", list, err)
	}
	if c := list.Devices[0].Reservation.Conflict; c != "" {
		t.Errorf("own device: %q", c)
	}
	if c := list.Devices[1].Reservation.Conflict; !strings.Contains(c, "de:ad:be:ef:00:01") {
		t.Errorf("foreign device: %q", c)
	}
	if c := list.Devices[2].Reservation.Conflict; c != "" {
		t.Errorf("old sighting: %q", c)
	}
	if len(list.Warnings) != 1 || list.Warnings[0].Severity != SeverityCrit {
		t.Errorf("warnings %+v", list.Warnings)
	}
}

func TestCheckReservationFindsStaticDevice(t *testing.T) {
	e := setup(t)
	now := time.Now().UTC().Format(time.RFC3339)
	old := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	withMonitor(t, e, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `[{"mac":"de:ad:be:ef:00:01","ipv4":["192.0.2.10"],"hostname":"static.lan.","last_seen":%q},
		                 {"mac":"de:ad:be:ef:00:02","ipv4":["192.0.2.11"],"last_seen":%q},
		                 {"mac":"de:ad:be:ef:00:03","ipv4":["192.0.2.14","192.0.2.13"],"last_seen":%q},
		                 {"mac":"aa:bb:cc:00:00:01","ipv4":["192.0.2.12"],"last_seen":%q}]`, now, old, now, now)
	})
	check := func(ip string) (string, error) {
		in := input()
		in.IPAddress = ip
		warnings, hints, err := e.mgr.CheckReservation(context.Background(), in, false)
		if len(warnings) > 0 {
			t.Errorf("%s: warnings %v", ip, warnings)
		}
		return strings.Join(hints, "|"), err
	}
	var v *ValidationError
	if _, err := check("192.0.2.10"); !errors.As(err, &v) || !strings.Contains(err.Error(), "in use by de:ad:be:ef:00:01 static.lan") {
		t.Errorf("active device: %v", err)
	}
	if got, err := check("192.0.2.11"); err != nil || !strings.Contains(got, "was last used by de:ad:be:ef:00:02, seen 1h0m0s ago") {
		t.Errorf("stale device: %q %v", got, err)
	}
	if got, err := check("192.0.2.13"); err != nil || !strings.Contains(got, "was used earlier by de:ad:be:ef:00:03") {
		t.Errorf("former address: %q %v", got, err)
	}
	if got, err := check("192.0.2.12"); err != nil || got != "" {
		t.Errorf("own device: %q %v", got, err)
	}

	// a reservation that keeps its address stays editable
	e.fake.Hosts = []map[string]any{{"subnet-id": 1, "hw-address": "aa:bb:cc:00:00:01", "ip-address": "192.0.2.10"}}
	in := input()
	in.Hostname = "renamed"
	warnings, _, err := e.mgr.CheckReservation(context.Background(), in, true)
	if err != nil || !strings.Contains(strings.Join(warnings, "|"), "in use by") {
		t.Errorf("keeping the address: %v %v", warnings, err)
	}
	in.IPAddress = "192.0.2.14"
	if _, _, err := e.mgr.CheckReservation(context.Background(), in, true); !errors.As(err, &v) {
		t.Errorf("moving to an active address: %v", err)
	}
	if e.fake.HasCall("reservation-add") {
		t.Error("the check changed something")
	}
}

const apClients = `[
 {"mac":"AA:BB:CC:00:00:03","name":"attic","station_id":"ap-1","iface":"home5","signal":-49,"connected_time":600,"inactive":400,
  "last_seen":"2099-01-01T00:00:00Z","device_info":{"ipaddrs":["192.0.2.12"],"name":"camera"}},
 {"mac":"AA:BB:CC:00:00:77","station_id":"ap-2","iface":"home2","signal":-71,"last_seen":"2099-01-01T00:00:00Z",
  "device_info":{"ipaddrs":["192.0.2.60"],"name":"only-wifi"}},
 {"mac":"AA:BB:CC:00:00:78","station_id":"ap-2","iface":"home2","signal":-71,"last_seen":"2099-01-01T00:00:00Z","device_info":{}}
]`

func withDashboard(t *testing.T, e *env, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	e.mgr.SetAPDashboard(srv.URL + "/")
}

func TestDevicesCombineMonitorAndAccessPoints(t *testing.T) {
	e := setup(t)
	withMonitor(t, e, func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(devices)) })
	var method, path string
	withDashboard(t, e, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.Write([]byte(apClients))
	})
	list, err := e.mgr.Devices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if method != "GET" || path != "/api/clients" {
		t.Errorf("request %s %s", method, path)
	}
	byMAC := map[string]DeviceView{}
	for _, d := range list.Devices {
		byMAC[d.HWAddress] = d
	}
	camera := byMAC["aa:bb:cc:00:00:03"]
	if camera.Wifi == nil || camera.Wifi.AccessPoint != "attic" || camera.Wifi.Interface != "home5" || camera.Wifi.Signal != -49 {
		t.Errorf("camera %+v", camera.Wifi)
	}
	if camera.LastSeen.Year() != 2099 || camera.Hostname != "camera.lan" {
		t.Errorf("camera %+v", camera)
	}
	only := byMAC["aa:bb:cc:00:00:77"]
	if only.IPAddress != "192.0.2.60" || only.Hostname != "only-wifi" || only.Wifi == nil || only.Wifi.AccessPoint != "ap-2" {
		t.Errorf("wifi only device %+v", only)
	}
	if _, ok := byMAC["aa:bb:cc:00:00:78"]; ok {
		t.Error("client without an address listed")
	}
	if phone := byMAC["aa:bb:cc:00:00:04"]; phone.Wifi != nil {
		t.Errorf("wired device %+v", phone.Wifi)
	}
}

func TestDevicesWorkWithAccessPointsAlone(t *testing.T) {
	e := setup(t)
	withDashboard(t, e, func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(apClients)) })
	list, err := e.mgr.Devices(context.Background())
	if err != nil || len(list.Devices) != 2 {
		t.Fatalf("got %+v %v", list, err)
	}
}

func TestAccessPointsAreOptional(t *testing.T) {
	e := setup(t)
	withMonitor(t, e, func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(devices)) })
	e.mgr.SetAPDashboard("http://127.0.0.1:1")
	list, err := e.mgr.Devices(context.Background())
	if err != nil || len(list.Devices) == 0 || len(list.Warnings) != 1 || !strings.Contains(list.Warnings[0].Message, "AP dashboard") {
		t.Fatalf("got %+v %v", list, err)
	}
	e.fake.Leases = []map[string]any{{"ip-address": "192.0.2.100", "subnet-id": 1, "state": 0, "cltt": time.Now().Unix(), "valid-lft": 100}}
	leases, err := e.mgr.LeasesWithReservations(context.Background())
	if err != nil || len(leases.Leases) != 1 {
		t.Fatalf("leases %+v %v", leases, err)
	}
}

func TestLeasesShowWhereAClientIs(t *testing.T) {
	e := setup(t)
	withDashboard(t, e, func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(apClients)) })
	e.fake.Leases = []map[string]any{{"ip-address": "192.0.2.12", "hw-address": "aa:bb:cc:00:00:03", "subnet-id": 1, "state": 0, "cltt": time.Now().Unix(), "valid-lft": 100}}
	leases, err := e.mgr.LeasesWithReservations(context.Background())
	if err != nil || leases.Leases[0].Wifi == nil || leases.Leases[0].Wifi.AccessPoint != "attic" {
		t.Fatalf("got %+v %v", leases, err)
	}
}

func TestDevicesDisabled(t *testing.T) {
	e := setup(t)
	if _, err := e.mgr.Devices(context.Background()); !errors.Is(err, ErrARPMonitorDisabled) {
		t.Fatalf("got %v", err)
	}
}

func TestDevicesMonitorFailures(t *testing.T) {
	cases := map[string]struct {
		handler http.HandlerFunc
		want    string
	}{
		"status":  {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }, "HTTP 500"},
		"garbage": {func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("<html>")) }, "invalid answer"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			withMonitor(t, e, c.handler)
			if _, err := e.mgr.Devices(context.Background()); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v", err)
			}
		})
	}
	e := setup(t)
	e.mgr.SetARPMonitor("http://127.0.0.1:1", 7)
	if _, err := e.mgr.Devices(context.Background()); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("got %v", err)
	}
}

func TestDevicesWithoutLeaseHook(t *testing.T) {
	e := setup(t)
	e.fake.Unknown["lease4-get-all"] = true
	withMonitor(t, e, func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(devices)) })
	list, err := e.mgr.Devices(context.Background())
	if err != nil || len(list.Devices) == 0 || len(list.Warnings) != 1 {
		t.Fatalf("got %+v %v", list, err)
	}
}
