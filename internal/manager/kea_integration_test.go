//go:build kea

// Tests against the real kea-dhcp4 binary: go test -tags kea ./internal/manager
// Each test starts a private server with a synthetic configuration. It listens on
// no DHCP interface, only on a local control socket, so it cannot affect a network.
package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vgropp/kea-roost/internal/kea"
)

const keaConfig = `{
  "Dhcp4": {
    "interfaces-config": { "interfaces": [] },
    "lease-database": { "type": "memfile", "persist": false },
    "loggers": [ { "name": "kea-dhcp4", "severity": "WARN", "output-options": [ { "output": "stdout" } ] } ],
    "valid-lifetime": 7200,
    "max-valid-lifetime": 7200,
    "reservations-global": true,
    "reservations-in-subnet": true,
    "option-def": [ %s ],
    "option-data": [ %s ],
    "client-classes": [
      { "name": "direct-dns", "option-data": [ { "name": "domain-name-servers", "data": "192.0.2.2" } ] }
    ],
    "reservations": [
      { "hw-address": "02:00:00:00:00:01", "ip-address": "192.0.2.10", "hostname": "printer", "client-classes": [ "direct-dns" ] },
      { "hw-address": "02:00:00:00:00:02", "ip-address": "192.0.2.11", "hostname": "camera" }
    ],
    "subnet4": [ {
      "id": 1,
      "subnet": "192.0.2.0/24",
      "pools": [ { "pool": "192.0.2.100 - 192.0.2.199" } ],
      "next-server": "192.0.2.5",
      "boot-file-name": "boot.efi",
      "option-data": [
        { "name": "routers", "data": "192.0.2.1" },
        { "name": "subnet-mask", "data": "255.255.255.0" }
      ]
    } ],
    "control-sockets": [ { "socket-type": "http", "socket-address": "127.0.0.1", "socket-port": %d } ],
    "hooks-libraries": [
      { "library": "libdhcp_lease_cmds.so" },
      { "library": "libdhcp_host_cmds.so" },
      { "library": "libdhcp_subnet_cmds.so" },
      { "library": "libdhcp_class_cmds.so" }
    ]
  }
}`

const dnsOption = `{ "name": "domain-name-servers", "data": "192.0.2.53" }`

type keaServer struct {
	t      *testing.T
	binary string
	dir    string
	conf   string
	port   int
	cmd    *exec.Cmd
	client *kea.Client
	mgr    *Manager
}

func startKea(t *testing.T, options string, definitions ...string) *keaServer {
	t.Helper()
	binary, err := exec.LookPath("kea-dhcp4")
	if err != nil {
		binary = "/usr/sbin/kea-dhcp4"
		if _, err := os.Stat(binary); err != nil {
			t.Skip("kea-dhcp4 is not installed")
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	dir := t.TempDir()
	s := &keaServer{t: t, binary: binary, dir: dir, conf: filepath.Join(dir, "kea.conf"), port: port}
	if err := os.WriteFile(s.conf, fmt.Appendf(nil, keaConfig, strings.Join(definitions, ", "), options, port), 0o600); err != nil {
		t.Fatal(err)
	}
	s.client, err = kea.NewClient(kea.Options{URL: fmt.Sprintf("http://127.0.0.1:%d", port)})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(filepath.Join(dir, "manager"))
	if err != nil {
		t.Fatal(err)
	}
	s.mgr = New(s.client, store, "test")
	s.start()
	t.Cleanup(s.stop)
	return s
}

func (s *keaServer) start() {
	s.t.Helper()
	log, err := os.Create(filepath.Join(s.dir, "kea.log"))
	if err != nil {
		s.t.Fatal(err)
	}
	defer log.Close()
	s.cmd = exec.Command(s.binary, "-X", "-c", s.conf)
	s.cmd.Stdout, s.cmd.Stderr = log, log
	for _, name := range []string{"KEA_PIDFILE_DIR", "KEA_LOCKFILE_DIR", "KEA_LOG_FILE_DIR", "KEA_DHCP_DATA_DIR", "KEA_CONTROL_SOCKET_DIR"} {
		s.cmd.Env = append(s.cmd.Env, name+"="+s.dir)
	}
	if err := s.cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	for range 100 {
		if _, err := s.client.Status(context.Background()); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	output, _ := os.ReadFile(log.Name())
	s.t.Fatalf("kea did not start:\n%s", output)
}

func (s *keaServer) stop() {
	if s.cmd != nil {
		s.cmd.Process.Kill()
		s.cmd.Wait()
		s.cmd = nil
	}
}

func (s *keaServer) restart() {
	s.t.Helper()
	s.stop()
	s.start()
}

func (s *keaServer) ok(res *Result, err error) {
	s.t.Helper()
	if err != nil {
		s.t.Fatal(err)
	}
	if res.Status != StatusOK {
		s.t.Fatalf("%s: %+v", res.Message, res.Steps)
	}
}

// config returns the configuration file as Kea stored it.
func (s *keaServer) config() map[string]any {
	s.t.Helper()
	data, err := os.ReadFile(s.conf)
	if err != nil {
		s.t.Fatal(err)
	}
	var doc struct {
		Dhcp4 map[string]any `json:"Dhcp4"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		s.t.Fatal(err)
	}
	return doc.Dhcp4
}

func (s *keaServer) host(ip string) kea.Host {
	s.t.Helper()
	subnets, err := s.mgr.subnets(context.Background())
	if err != nil {
		s.t.Fatal(err)
	}
	hosts, err := s.mgr.allHosts(context.Background(), subnets)
	if err != nil {
		s.t.Fatal(err)
	}
	for _, h := range hosts {
		if h.IP() == ip {
			return h
		}
	}
	return nil
}

func TestRealKeaOffersEverythingTheManagerUses(t *testing.T) {
	s := startKea(t, dnsOption)
	commands, err := s.client.Commands(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"lease4-get-all", "reservation-add", "reservation-update", "reservation-del", "subnet4-update", "class-update", "config-write"} {
		if !slices.Contains(commands, c) {
			t.Errorf("command %s is missing", c)
		}
	}
	version, err := s.client.Version(context.Background())
	if err != nil || version == "" {
		t.Errorf("version %q %v", version, err)
	}
	if list, err := s.mgr.Subnets(context.Background()); err != nil || !list.Editable {
		t.Errorf("subnets %+v %v", list, err)
	}
	if list, err := s.mgr.Classes(context.Background()); err != nil || !list.Editable || list.Classes[0].Reservations != 1 {
		t.Errorf("classes %+v %v", list, err)
	}
}

func TestRealKeaReservationsSurviveRestart(t *testing.T) {
	s := startKea(t, dnsOption)
	ctx := context.Background()
	global := ReservationInput{SubnetID: 0, IdentifierType: "hw-address", Identifier: "02:00:00:00:01:01", IPAddress: "192.0.2.20", Hostname: "global-host", Classes: []string{"direct-dns"}, Comment: "rack"}
	scoped := ReservationInput{SubnetID: 1, IdentifierType: "hw-address", Identifier: "02:00:00:00:01:02", IPAddress: "192.0.2.21", Hostname: "scoped-host"}
	s.ok(s.mgr.CreateReservation(ctx, "test", global))
	s.ok(s.mgr.CreateReservation(ctx, "test", scoped))
	s.ok(s.mgr.DeleteReservation(ctx, "test", 1, "hw-address", scoped.Identifier))

	global.IPAddress, global.Hostname = "192.0.2.22", "renamed"
	s.ok(s.mgr.UpdateReservation(ctx, "test", global))
	s.restart()

	h := s.host("192.0.2.22")
	if h == nil || h.Hostname() != "renamed" || h.Comment() != "rack" || !slices.Contains(h.Classes(), "direct-dns") {
		t.Errorf("reservation after restart: %+v", h)
	}
	if s.host("192.0.2.21") != nil {
		t.Error("deleted reservation is back")
	}
	if h := s.host("192.0.2.10"); h == nil || !slices.Contains(h.Classes(), "direct-dns") {
		t.Errorf("untouched reservation lost its class: %+v", h)
	}
	if list, err := s.mgr.Store().Backups(); err != nil || len(list) != 4 {
		t.Errorf("backups %v %v", list, err)
	}
}

func TestRealKeaMovesReservationToAnotherMAC(t *testing.T) {
	s := startKea(t, dnsOption)
	in := ReservationInput{SubnetID: 0, IdentifierType: "hw-address", Identifier: "02:00:00:00:00:01", IPAddress: "192.0.2.10", Hostname: "printer",
		Classes: []string{"direct-dns"}, NewIdentifier: "02:00:00:00:00:99"}
	s.ok(s.mgr.UpdateReservation(context.Background(), "test", in))
	s.restart()

	h := s.host("192.0.2.10")
	if h == nil || !slices.Contains(h.Classes(), "direct-dns") {
		t.Fatalf("got %+v", h)
	}
	if _, id := h.Identifier(); id != "02:00:00:00:00:99" {
		t.Errorf("identifier %s", id)
	}
}

func TestRealKeaSubnetEditSurvivesRestart(t *testing.T) {
	s := startKea(t, dnsOption)
	in := SubnetInput{
		Pools:         []string{"192.0.2.100-192.0.2.199", "192.0.2.210-192.0.2.220"},
		Gateways:      []string{"192.0.2.254"},
		DNSServers:    []string{"192.0.2.53"},
		ValidLifetime: 3600,
	}
	s.ok(s.mgr.UpdateSubnet(context.Background(), "test", 1, in))
	s.restart()

	raw, err := s.client.SubnetGet(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := poolTexts(raw); !slices.Equal(got, []string{"192.0.2.100-192.0.2.199", "192.0.2.210-192.0.2.220"}) {
		t.Errorf("pools %v", got)
	}
	text, _ := json.Marshal(raw)
	for _, want := range []string{`"valid-lifetime":3600`, `"min-valid-lifetime":3600`, `"max-valid-lifetime":3600`, `"data":"192.0.2.254"`,
		`"subnet-mask"`, `"boot-file-name":"boot.efi"`, `"next-server":"192.0.2.5"`} {
		if !strings.Contains(string(text), want) {
			t.Errorf("missing %s in %s", want, text)
		}
	}
	// the inherited DNS server stays global
	if strings.Contains(string(text), "domain-name-servers") {
		t.Errorf("inherited option pinned to the subnet: %s", text)
	}
}

func TestRealKeaClassesSurviveRestart(t *testing.T) {
	s := startKea(t, dnsOption)
	ctx := context.Background()
	option := func(data string) []ReservationOption {
		return []ReservationOption{{Name: "domain-name-servers", Data: data}}
	}
	s.ok(s.mgr.CreateClass(ctx, "test", ClassInput{Name: "guests", Options: option("192.0.2.3")}))
	s.ok(s.mgr.UpdateClass(ctx, "test", "guests", ClassInput{Options: option("192.0.2.4")}))
	s.ok(s.mgr.CreateClass(ctx, "test", ClassInput{Name: "temporary", Options: option("192.0.2.5")}))
	s.ok(s.mgr.DeleteClass(ctx, "test", "temporary"))
	s.restart()

	list, err := s.mgr.Classes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range list.Classes {
		names = append(names, c.Name)
		if c.Name == "guests" && c.Options[0].Data != "192.0.2.4" {
			t.Errorf("guests %+v", c)
		}
	}
	if !slices.Equal(names, []string{"direct-dns", "guests"}) {
		t.Errorf("classes %v", names)
	}
	if _, err := s.mgr.DeleteClass(ctx, "test", "direct-dns"); err == nil {
		t.Error("a class in use was deleted")
	} else {
		var v *ValidationError
		if !errors.As(err, &v) {
			t.Errorf("got %v", err)
		}
	}
}

// Pins the reason for the write lock. If this fails because Kea keeps the value,
// the lock in Manager.run can go.
func TestRealKeaEmptiesVendorOptionOnConfigWrite(t *testing.T) {
	const vendor = `{ "name": "vendor-encapsulated-options", "code": 43, "csv-format": false, "data": "010400000000ff" }`
	s := startKea(t, dnsOption+", "+vendor)
	if err := s.client.ConfigWrite(context.Background()); err != nil {
		t.Fatal(err)
	}
	text, _ := json.Marshal(s.config()["option-data"])
	if strings.Contains(string(text), "010400000000ff") {
		t.Fatalf("Kea keeps the value now: %s", text)
	}
}

func TestRealKeaWritesAreRefusedForLossyOptions(t *testing.T) {
	const vendor = `{ "name": "vendor-encapsulated-options", "code": 43, "csv-format": false, "data": "010400000000ff" }`
	s := startKea(t, dnsOption+", "+vendor)
	before, err := os.ReadFile(s.conf)
	if err != nil {
		t.Fatal(err)
	}
	in := ReservationInput{SubnetID: 0, IdentifierType: "hw-address", Identifier: "02:00:00:00:01:01", IPAddress: "192.0.2.20"}
	res, err := s.mgr.CreateReservation(context.Background(), "test", in)
	if err != nil || res.Status != StatusFailed {
		t.Fatalf("got %+v %v", res, err)
	}
	after, err := os.ReadFile(s.conf)
	if err != nil || string(after) != string(before) {
		t.Errorf("the configuration file changed: %v", err)
	}
	if s.host("192.0.2.20") != nil {
		t.Error("the reservation was added")
	}
}

func TestRealKeaKeepsVendorSubOption(t *testing.T) {
	const vendor = `{ "name": "vendor-encapsulated-options" },
      { "name": "vendor-sub1", "space": "vendor-encapsulated-options-space", "csv-format": false, "data": "00000000" }`
	const definition = `{ "name": "vendor-sub1", "code": 1, "space": "vendor-encapsulated-options-space", "type": "binary" }`
	s := startKea(t, dnsOption+", "+vendor, definition)

	in := ReservationInput{SubnetID: 0, IdentifierType: "hw-address", Identifier: "02:00:00:00:01:01", IPAddress: "192.0.2.20"}
	s.ok(s.mgr.CreateReservation(context.Background(), "test", in))
	s.restart()
	text, _ := json.Marshal(s.config()["option-data"])
	if !strings.Contains(string(text), `"data":"00000000"`) {
		t.Errorf("sub-option lost: %s", text)
	}
}

func startPair(t *testing.T) (primary, peer *keaServer) {
	t.Helper()
	primary, peer = startKea(t, dnsOption), startKea(t, dnsOption)
	primary.mgr.AddPeer("standby", peer.client)
	return primary, peer
}

func peerDifferences(t *testing.T, s *keaServer) []Difference {
	t.Helper()
	list := s.mgr.Servers(context.Background())
	if len(list.Servers) != 2 || !list.Servers[1].Reachable {
		t.Fatalf("servers: %+v", list.Servers)
	}
	return list.Servers[1].Differences
}

// Two servers with the same file must look the same, or the Servers page would
// report differences that are none.
func TestTwoServersStayInStep(t *testing.T) {
	primary, peer := startPair(t)
	ctx := context.Background()
	if d := peerDifferences(t, primary); len(d) != 0 {
		t.Fatalf("fresh servers differ: %+v", d)
	}

	in := ReservationInput{SubnetID: 0, IdentifierType: "hw-address", Identifier: "02:00:00:00:00:21", IPAddress: "192.0.2.21", Hostname: "new", Classes: []string{"direct-dns"}, Comment: "note"}
	res, err := primary.mgr.CreateReservation(ctx, "alice", in)
	primary.ok(res, err)
	res, err = primary.mgr.CreateClass(ctx, "alice", ClassInput{Name: "ntp", Options: []ReservationOption{{Name: "ntp-servers", Data: "192.0.2.3"}}})
	primary.ok(res, err)
	res, err = primary.mgr.UpdateSubnet(ctx, "alice", 1, SubnetInput{Pools: []string{"192.0.2.100-192.0.2.150"}, Gateways: []string{"192.0.2.1"}, DNSServers: []string{"192.0.2.53"}, ValidLifetime: 7200})
	primary.ok(res, err)
	moved := in
	moved.NewIdentifierType, moved.NewIdentifier = "hw-address", "02:00:00:00:00:22"
	res, err = primary.mgr.UpdateReservation(ctx, "alice", moved)
	primary.ok(res, err)
	in.Identifier = moved.NewIdentifier
	res, err = primary.mgr.UpdateClass(ctx, "alice", "ntp", ClassInput{Options: []ReservationOption{{Name: "ntp-servers", Data: "192.0.2.4"}}})
	primary.ok(res, err)
	if d := peerDifferences(t, primary); len(d) != 0 {
		t.Fatalf("servers differ after changes: %+v", d)
	}

	// the peer file must hold the change, and its restart must keep it
	peer.restart()
	if h, err := peer.client.Reservation(ctx, 0, "hw-address", in.Identifier); err != nil || h == nil {
		t.Fatalf("reservation after restart: %v %v", h, err)
	}
	if d := peerDifferences(t, primary); len(d) != 0 {
		t.Fatalf("servers differ after a restart: %+v", d)
	}
}

func TestSyncRepairsARealServer(t *testing.T) {
	primary, peer := startPair(t)
	ctx := context.Background()

	// drift on the peer, made behind the back of the manager
	if err := peer.client.DeleteReservation(ctx, 0, "hw-address", "02:00:00:00:00:02"); err != nil {
		t.Fatal(err)
	}
	if err := peer.client.AddReservation(ctx, kea.Host{"subnet-id": int64(0), "hw-address": "02:00:00:00:00:99", "ip-address": "192.0.2.99"}); err != nil {
		t.Fatal(err)
	}
	h, err := peer.client.Reservation(ctx, 0, "hw-address", "02:00:00:00:00:01")
	if err != nil {
		t.Fatal(err)
	}
	h["hostname"] = "changed"
	if err := peer.client.UpdateReservation(ctx, h); err != nil {
		t.Fatal(err)
	}
	if err := peer.client.ClassAdd(ctx, map[string]any{"name": "stray"}); err != nil {
		t.Fatal(err)
	}
	if err := peer.client.ClassDel(ctx, "stray"); err != nil {
		t.Fatal(err)
	}
	raw, err := peer.client.SubnetGet(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	prepareSubnetUpdate(raw)
	raw["pools"] = []any{map[string]any{"pool": "192.0.2.200-192.0.2.210"}}
	if err := peer.client.SubnetUpdate(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if d := peerDifferences(t, primary); len(d) != 4 {
		t.Fatalf("differences: %+v", d)
	}

	res, err := primary.mgr.SyncServer(ctx, "alice", "standby")
	primary.ok(res, err)
	if d := peerDifferences(t, primary); len(d) != 0 {
		t.Fatalf("left: %+v", d)
	}
	peer.restart()
	if d := peerDifferences(t, primary); len(d) != 0 {
		t.Fatalf("left after restart: %+v", d)
	}
}

func TestStoppedPeerStopsAtThePrimary(t *testing.T) {
	primary, peer := startPair(t)
	ctx := context.Background()
	peer.stop()
	in := ReservationInput{SubnetID: 0, IdentifierType: "hw-address", Identifier: "02:00:00:00:00:21", IPAddress: "192.0.2.21"}
	res, err := primary.mgr.CreateReservation(ctx, "alice", in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusPartial {
		t.Fatalf("result: %+v", res)
	}
	peer.start()
	if d := peerDifferences(t, primary); len(d) != 1 || d[0].Change != "missing" {
		t.Fatalf("differences: %+v", d)
	}
	res, err = primary.mgr.SyncServer(ctx, "alice", "standby")
	primary.ok(res, err)
	if d := peerDifferences(t, primary); len(d) != 0 {
		t.Fatalf("left: %+v", d)
	}
}

const haHook = `{ "library": "libdhcp_class_cmds.so" },
      { "library": "libdhcp_ha.so", "parameters": { "high-availability": [ {
        "this-server-name": %q, "mode": "hot-standby",
        "heartbeat-delay": 500, "max-response-delay": 2000, "max-ack-delay": 500, "max-unacked-clients": 0,
        "peers": [
          { "name": "one", "url": "http://127.0.0.1:%d/", "role": "primary", "auto-failover": true },
          { "name": "two", "url": "http://127.0.0.1:%d/", "role": "standby", "auto-failover": true }
        ] } ] } }`

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func enableHA(t *testing.T, s *keaServer, name string, one, two int) {
	t.Helper()
	data, err := os.ReadFile(s.conf)
	if err != nil {
		t.Fatal(err)
	}
	hook := fmt.Sprintf(haHook, name, one, two)
	if err := os.WriteFile(s.conf, []byte(strings.Replace(string(data), `{ "library": "libdhcp_class_cmds.so" }`, hook, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	s.restart()
}

func (s *keaServer) awaitState(want string) {
	s.t.Helper()
	state := ""
	for range 300 {
		if status, err := s.client.Status(context.Background()); err == nil && len(status.HA) > 0 {
			if state = status.HA[0].Servers.Local.State; state == want {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.t.Fatalf("state is %s instead of %s", state, want)
}

func (s *keaServer) ha(name, action string) {
	s.t.Helper()
	s.ok(s.mgr.RunHA(context.Background(), "alice", name, action))
}

// Real HA between two processes: only this shows which server a command has to
// reach, and that Kea accepts it in that state.
func TestHAOperations(t *testing.T) {
	a, b := startPair(t)
	one, two := freePort(t), freePort(t)
	enableHA(t, a, "one", one, two)
	enableHA(t, b, "two", one, two)
	a.awaitState("hot-standby")
	b.awaitState("hot-standby")

	// the primary goes down for maintenance, the standby takes over
	a.ha("test", "maintenance")
	a.awaitState("in-maintenance")
	b.awaitState("partner-in-maintenance")
	a.ha("test", "maintenance-cancel")
	a.awaitState("hot-standby")
	b.awaitState("hot-standby")

	a.ha("standby", "maintenance")
	b.awaitState("in-maintenance")
	a.awaitState("partner-in-maintenance")
	a.ha("standby", "maintenance-cancel")
	a.awaitState("hot-standby")
	b.awaitState("hot-standby")

	a.ha("standby", "sync")
	b.awaitState("hot-standby")
	a.awaitState("hot-standby")

	list := a.mgr.Servers(context.Background())
	if h := list.Servers[1].HA; h == nil || h.Name != "two" || h.Partner != "one" {
		t.Fatalf("ha: %+v", h)
	}
}
