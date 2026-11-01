package manager

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/vgropp/kea-roost/internal/kea"
)

func mapOf(t *testing.T, config string, claimed map[netip.Addr]addressClaim) *SubnetMap {
	t.Helper()
	subnets, err := kea.ParseSubnets(json.RawMessage(config))
	if err != nil {
		t.Fatal(err)
	}
	out := &SubnetMap{Cells: []AddressCell{}, Blocks: []AddressBlock{}, Free: []FreeRange{}, Counts: map[string]int{}}
	walkSubnet(subnets[0], claimed, netip.Prefix{}, out)
	return out
}

const smallSubnet = `{"Dhcp4": {"subnet4": [ { "id": 1, "subnet": "192.0.2.0/28",
  "pools": [ { "pool": "192.0.2.8 - 192.0.2.12" } ] } ]}}`

func TestFreeRangesLeaveOutWhatIsClaimed(t *testing.T) {
	got := mapOf(t, smallSubnet, map[netip.Addr]addressClaim{
		netip.MustParseAddr("192.0.2.4"):  {addressDevice, "cam", macA},
		netip.MustParseAddr("192.0.2.10"): {addressLeased, "phone", macB},
	})
	want := []FreeRange{
		{"192.0.2.1", "192.0.2.3", 3, false},
		{"192.0.2.5", "192.0.2.7", 3, false},
		{"192.0.2.8", "192.0.2.9", 2, true},
		{"192.0.2.11", "192.0.2.12", 2, true},
		{"192.0.2.13", "192.0.2.14", 2, false},
	}
	if len(got.Free) != len(want) {
		t.Fatalf("free: %+v", got.Free)
	}
	for i := range want {
		if got.Free[i] != want[i] {
			t.Errorf("range %d: %+v", i, got.Free[i])
		}
	}
	if got.FreePool != 4 || got.FreeStatic != 8 || got.Counts[addressNetwork] != 2 || len(got.Cells) != 16 {
		t.Fatalf("map: %+v", got)
	}
	if c := got.Cells[4]; c.State != addressDevice || c.Name != "cam" || c.Pool {
		t.Fatalf("cell: %+v", c)
	}
	if c := got.Cells[10]; c.State != addressLeased || !c.Pool {
		t.Fatalf("cell: %+v", c)
	}
}

func TestSmallSubnetsHaveNoNetworkAddress(t *testing.T) {
	got := mapOf(t, `{"Dhcp4": {"subnet4": [ { "id": 1, "subnet": "192.0.2.0/31" } ]}}`, nil)
	if got.FreeStatic != 2 || got.Counts[addressNetwork] != 0 {
		t.Fatalf("map: %+v", got)
	}
}

func TestLargeSubnetHasRangesButNoGrid(t *testing.T) {
	got := mapOf(t, `{"Dhcp4": {"subnet4": [ { "id": 1, "subnet": "192.0.0.0/20", "pools": [ { "pool": "192.0.1.0 - 192.0.1.99" } ] } ]}}`,
		map[netip.Addr]addressClaim{netip.MustParseAddr("192.0.2.7"): {addressDevice, "", ""}})
	if len(got.Cells) != 0 || got.FreeStatic+got.FreePool != 4093 || len(got.Blocks) != 16 {
		t.Fatalf("map: cells %d blocks %d free %+v", len(got.Cells), len(got.Blocks), got.Free)
	}
	if b := got.Blocks[1]; b.First != "192.0.1.0" || b.Last != "192.0.1.255" || b.Pool != 100 || b.Free != 256 {
		t.Fatalf("block: %+v", b)
	}
	if b := got.Blocks[2]; b.Used != 1 || b.Free != 255 {
		t.Fatalf("block: %+v", b)
	}
	if got.Blocks[0].Free != 255 || got.Blocks[15].Free != 255 {
		t.Fatalf("network and broadcast are not free: %+v", got.Blocks)
	}
}

func TestSubnetMapCombinesItsSources(t *testing.T) {
	e := setup(t)
	now := time.Now()
	e.fake.Hosts = []map[string]any{{"subnet-id": 0, "hw-address": macA, "ip-address": "192.0.2.10", "hostname": "nas"}}
	e.fake.Leases = []map[string]any{{"ip-address": "192.0.2.150", "hw-address": macB, "hostname": "phone", "subnet-id": 1, "state": 0, "cltt": now.Unix(), "valid-lft": 7200}}
	monitor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`[{"mac":"02:00:00:00:00:0c","ipv4":["192.0.2.20","0.0.0.0","198.51.100.9"],"hostname":"cam.lan.","last_seen":"` + now.UTC().Format(time.RFC3339) + `"}]`))
	}))
	t.Cleanup(monitor.Close)
	e.mgr.SetARPMonitor(monitor.URL)

	got, err := e.mgr.SubnetMap(context.Background(), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]AddressCell{}
	for _, c := range got.Cells {
		state[c.IP] = c
	}
	for ip, want := range map[string]string{"192.0.2.1": addressGateway, "192.0.2.10": addressReserved, "192.0.2.150": addressLeased, "192.0.2.20": addressDevice, "192.0.2.21": addressFree, "192.0.2.0": addressNetwork} {
		if state[ip].State != want {
			t.Errorf("%s: %+v", ip, state[ip])
		}
	}
	if state["192.0.2.20"].Name != "cam.lan" || state["192.0.2.20"].MAC != "02:00:00:00:00:0c" {
		t.Errorf("device: %+v", state["192.0.2.20"])
	}
	if got.FreePool != 99 || got.Counts[addressLeased] != 1 {
		t.Fatalf("map: pool %d counts %v", got.FreePool, got.Counts)
	}
}

func TestSubnetMapSaysWhatItDoesNotKnow(t *testing.T) {
	e := setup(t)
	got, err := e.mgr.SubnetMap(context.Background(), 1, "")
	if err != nil || len(got.Warnings) != 1 || !got.Incomplete {
		t.Fatalf("%+v %v", got, err)
	}
	e.mgr.SetARPMonitor("http://127.0.0.1:1")
	got, err = e.mgr.SubnetMap(context.Background(), 1, "")
	if err != nil || len(got.Warnings) != 1 || !got.Incomplete {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := e.mgr.SubnetMap(context.Background(), 99, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestHugeSubnetIsNotWalked(t *testing.T) {
	e := setup(t)
	e.fake.Config["subnet4"].([]any)[0].(map[string]any)["subnet"] = "10.0.0.0/8"
	got, err := e.mgr.SubnetMap(context.Background(), 1, "")
	if err != nil || !got.TooLarge || len(got.Cells) != 0 {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestDeclinedLeaseIsNotFree(t *testing.T) {
	e := setup(t)
	e.fake.Leases = []map[string]any{{"ip-address": "192.0.2.150", "hw-address": macB, "subnet-id": 1, "state": 1, "cltt": time.Now().Unix(), "valid-lft": 7200}}
	got, err := e.mgr.SubnetMap(context.Background(), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got.Cells {
		if c.IP == "192.0.2.150" && c.State == addressFree {
			t.Fatal("a declined address is blocked")
		}
	}
}

func TestReservationLearnsItsNameFromTheLease(t *testing.T) {
	e := setup(t)
	e.fake.Hosts = []map[string]any{{"subnet-id": 0, "hw-address": macA, "ip-address": "192.0.2.10"}}
	e.fake.Leases = []map[string]any{{"ip-address": "192.0.2.10", "hw-address": macA, "hostname": "phone", "subnet-id": 1, "state": 0, "cltt": time.Now().Unix(), "valid-lft": 7200}}
	got, err := e.mgr.SubnetMap(context.Background(), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got.Cells {
		if c.IP == "192.0.2.10" && (c.State != addressReserved || c.Name != "phone") {
			t.Fatalf("cell: %+v", c)
		}
	}
}

func TestBlockOfALargeSubnetShowsItsAddresses(t *testing.T) {
	e := setup(t)
	e.fake.Config["subnet4"].([]any)[0].(map[string]any)["subnet"] = "192.0.0.0/20"
	e.fake.Hosts = []map[string]any{{"subnet-id": 0, "hw-address": macA, "ip-address": "192.0.4.10", "hostname": "nas"}}
	ctx := context.Background()
	got, err := e.mgr.SubnetMap(ctx, 1, "")
	if err != nil || len(got.Cells) != 0 || len(got.Blocks) != 16 {
		t.Fatalf("%d cells, %d blocks, %v", len(got.Cells), len(got.Blocks), err)
	}
	got, err = e.mgr.SubnetMap(ctx, 1, "192.0.4.0")
	if err != nil || len(got.Cells) != 256 || len(got.Blocks) != 16 {
		t.Fatalf("%d cells, %d blocks, %v", len(got.Cells), len(got.Blocks), err)
	}
	if c := got.Cells[10]; c.IP != "192.0.4.10" || c.State != addressReserved || c.Name != "nas" {
		t.Fatalf("cell: %+v", c)
	}
	var invalid *ValidationError
	for _, block := range []string{"nonsense", "198.51.100.0", "2001:db8::"} {
		if _, err := e.mgr.SubnetMap(ctx, 1, block); !errors.As(err, &invalid) {
			t.Errorf("%s: %v", block, err)
		}
	}
}

func TestOldSightingsOnlyCountOutsideOfThePools(t *testing.T) {
	e := setup(t)
	now := time.Now()
	seen := func(age time.Duration) string { return now.Add(-age).UTC().Format(time.RFC3339) }
	monitor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`[
			{"mac":"02:00:00:00:00:01","ipv4":["192.0.2.100"],"last_seen":"` + seen(24*time.Hour) + `"},
			{"mac":"02:00:00:00:00:02","ipv4":["192.0.2.101"],"last_seen":"` + seen(time.Minute) + `"},
			{"mac":"02:00:00:00:00:03","ipv4":["192.0.2.20","192.0.2.102"],"last_seen":"` + seen(time.Minute) + `"},
			{"mac":"02:00:00:00:00:05","ipv4":["0.0.0.0","192.0.2.103"],"last_seen":"` + seen(time.Minute) + `"},
			{"mac":"02:00:00:00:00:04","ipv4":["192.0.2.21"],"last_seen":"` + seen(24*time.Hour) + `"}]`))
	}))
	t.Cleanup(monitor.Close)
	e.mgr.SetARPMonitor(monitor.URL)
	got, err := e.mgr.SubnetMap(context.Background(), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]string{}
	for _, c := range got.Cells {
		state[c.IP] = c.State
	}
	for ip, want := range map[string]string{
		"192.0.2.100": addressFree,   // in the pool, gone for a day
		"192.0.2.101": addressDevice, // in the pool, online now
		"192.0.2.102": addressFree,   // in the pool, its device moved on
		"192.0.2.103": addressDevice, // in the pool, a probe before it is no address
		"192.0.2.20":  addressDevice, // outside, online now
		"192.0.2.21":  addressDevice, // outside, seen within the window
	} {
		if state[ip] != want {
			t.Errorf("%s: %s", ip, state[ip])
		}
	}
}
