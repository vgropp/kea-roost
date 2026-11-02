//go:build kea

package manager

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/vgropp/kea-roost/internal/kea"
)

const kea6Config = `{
  "Dhcp6": {
    "interfaces-config": { "interfaces": [] },
    "lease-database": { "type": "memfile", "persist": false },
    "loggers": [ { "name": "kea-dhcp6", "severity": "WARN", "output-options": [ { "output": "stdout" } ] } ],
    "valid-lifetime": 7200,
    "min-valid-lifetime": 7200,
    "max-valid-lifetime": 7200,
    "reservations-in-subnet": true,
    "subnet6": [ {
      "id": 1,
      "subnet": "fd00:db8:1::/64",
      "interface": "lo",
      "pools": [ { "pool": "fd00:db8:1::1000-fd00:db8:1::ffff" } ],
      "option-data": [ { "name": "dns-servers", "data": "fd00:db8:1::1" } ],
      "reservations": [ { "duid": "00:03:00:01:02:00:00:00:00:01", "ip-addresses": [ "fd00:db8:1::10" ], "hostname": "printer" } ]
    } ],
    "control-sockets": [ { "socket-type": "http", "socket-address": "127.0.0.1", "socket-port": %d } ],
    "hooks-libraries": [
      { "library": "libdhcp_lease_cmds.so" },
      { "library": "libdhcp_host_cmds.so" },
      { "library": "libdhcp_subnet_cmds.so" }
    ]
  }
}`

// startKea6 starts a private kea-dhcp6. Its manager reads IPv6 from it; the IPv4
// side points to the same socket and is not used.
func startKea6(t *testing.T) *keaServer {
	t.Helper()
	binary, err := exec.LookPath("kea-dhcp6")
	if err != nil {
		binary = "/usr/sbin/kea-dhcp6"
		if _, err := os.Stat(binary); err != nil {
			t.Skip("kea-dhcp6 is not installed")
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	dir := t.TempDir()
	s := &keaServer{t: t, binary: binary, dir: dir, conf: filepath.Join(dir, "kea6.conf"), port: port}
	if err := os.WriteFile(s.conf, fmt.Appendf(nil, kea6Config, port), 0o600); err != nil {
		t.Fatal(err)
	}
	if s.client, err = kea.NewClient(kea.Options{URL: fmt.Sprintf("http://127.0.0.1:%d", port)}); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(filepath.Join(dir, "manager"))
	if err != nil {
		t.Fatal(err)
	}
	s.mgr = New(s.client, store, "test")
	s.mgr.SetDHCP6(s.client)
	s.start()
	t.Cleanup(s.stop)
	return s
}

func (s *keaServer) hostnames6() []string {
	s.t.Helper()
	list, err := s.mgr.Reservations6(context.Background())
	if err != nil {
		s.t.Fatal(err)
	}
	var names []string
	for _, r := range list.Reservations {
		names = append(names, r.Hostname)
	}
	slices.Sort(names)
	return names
}

// subnet6-update keeps the reservations of the subnet, those of the file and those
// added through the API, also after a restart.
func TestRealKea6SubnetUpdateKeepsReservations(t *testing.T) {
	s := startKea6(t)
	ctx := context.Background()
	s.ok(s.mgr.CreateReservation6(ctx, "test", Reservation6Input{SubnetID: 1, IdentifierType: "duid",
		Identifier: "00:03:00:01:02:00:00:00:00:02", Addresses: []string{"fd00:db8:1::11"}, Hostname: "nas"}))
	in := Subnet6Input{Pools: []string{"fd00:db8:1::2000-fd00:db8:1::2fff"}, DNSServers: []string{"fd00:db8:1::53"}, ValidLifetime: 3600, PreferredLifetime: 1800}
	s.ok(s.mgr.UpdateSubnet6(ctx, "test", 1, in))
	s.restart()
	if got := s.hostnames6(); !slices.Equal(got, []string{"nas", "printer"}) {
		t.Fatalf("reservations after the update: %v", got)
	}
	list, err := s.mgr.Subnets6(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sub := list.Subnets[0]
	if len(sub.Pools) != 1 || sub.Pools[0].First.String() != "fd00:db8:1::2000" || sub.Pools[0].Last.String() != "fd00:db8:1::2fff" ||
		!slices.Equal(sub.DNSServers, []string{"fd00:db8:1::53"}) || *sub.ValidLifetime != 3600 || *sub.PreferredLifetime != 1800 || sub.Interface != "lo" {
		t.Fatalf("subnet after the update: %+v", sub)
	}
	if !list.Editable {
		t.Error("subnets are not editable")
	}
}
