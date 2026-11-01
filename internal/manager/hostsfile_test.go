package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vgropp/kea-roost/internal/kea"
	"github.com/vgropp/kea-roost/internal/keatest"
)

func TestHostsContent(t *testing.T) {
	got := string(hostsContent([]kea.Host{
		{"hw-address": "02:00:00:00:00:02", "ip-address": "192.0.2.100", "hostname": "printer"},
		{"hw-address": "02:00:00:00:00:01", "ip-address": "192.0.2.9", "hostname": "nas"},
		{"hw-address": "02:00:00:00:00:03", "ip-address": "192.0.2.11", "hostname": "cam.iot.lan."},
		{"hw-address": "02:00:00:00:00:04", "ip-address": "192.0.2.12"},
		{"hw-address": "02:00:00:00:00:05", "hostname": "no-address"},
		{"hw-address": "02:00:00:00:00:06", "ip-address": "192.0.2.13", "hostname": "living room tv"},
		{"hw-address": "02:00:00:00:00:07", "ip-address": "192.0.2.14", "hostname": "!!!"},
	}, []string{"lan"}))
	want := "192.0.2.9 nas.lan nas\n192.0.2.11 cam.iot.lan\n192.0.2.13 living-room-tv.lan living-room-tv\n192.0.2.100 printer.lan printer\n"
	if !strings.HasSuffix(got, want) {
		t.Fatalf("got:\n%s", got)
	}
}

func TestHostsContentWithoutDomain(t *testing.T) {
	got := string(hostsContent([]kea.Host{{"ip-address": "192.0.2.9", "hostname": "nas"}}, nil))
	if !strings.HasSuffix(got, "192.0.2.9 nas\n") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestHostsContentWithSeveralDomains(t *testing.T) {
	got := string(hostsContent([]kea.Host{
		{"ip-address": "192.0.2.9", "hostname": "printer"},
		{"ip-address": "192.0.2.10", "hostname": "cam.iot.lan"},
	}, []string{"lan", "home"}))
	if !strings.HasSuffix(got, "192.0.2.9 printer.lan printer.home printer\n192.0.2.10 cam.iot.lan\n") {
		t.Fatalf("got:\n%s", got)
	}
}

func hostsSetup(t *testing.T) (*env, string) {
	t.Helper()
	e := setup(t)
	dir := t.TempDir()
	e.mgr.SetHostsDir(dir, []string{"lan"})
	return e, filepath.Join(dir, "reservations")
}

func TestChangeUpdatesTheHostsFileAtOnce(t *testing.T) {
	e, path := hostsSetup(t)
	ctx := context.Background()
	res, err := e.mgr.CreateReservation(ctx, "alice", input())
	if err != nil || res.Status != StatusOK {
		t.Fatalf("%+v %v", res, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "192.0.2.10 nas.lan nas\n") {
		t.Fatalf("%q %v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("mode: %v %v", info, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("left behind: %v", entries)
	}

	if _, err := e.mgr.DeleteReservation(ctx, "alice", 1, "hw-address", "aa:bb:cc:00:00:01"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); strings.Contains(string(data), "nas") {
		t.Fatalf("still listed: %s", data)
	}
}

func TestUnchangedHostsFileIsNotRewritten(t *testing.T) {
	e, path := hostsSetup(t)
	ctx := context.Background()
	if err := e.mgr.RefreshHostsFile(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+"x"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+"x", path); err != nil {
		t.Fatal(err)
	}
	if err := e.mgr.RefreshHostsFile(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) {
		t.Fatal("the file was replaced without a change")
	}
}

func TestFailedRefreshKeepsTheFileAndShowsInHealth(t *testing.T) {
	e, path := hostsSetup(t)
	ctx := context.Background()
	if err := e.mgr.RefreshHostsFile(ctx); err != nil {
		t.Fatal(err)
	}
	if c := e.mgr.hostsCheck(); c == nil || c.Severity != SeverityOK {
		t.Fatalf("check: %+v", c)
	}
	e.fake.Fail["config-get"] = keatest.Failure{Result: 1, Text: "no"}
	if err := e.mgr.RefreshHostsFile(ctx); err == nil {
		t.Fatal("no error")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("the old file is gone")
	}
	if c := e.mgr.hostsCheck(); c == nil || c.Severity != SeverityWarn {
		t.Fatalf("check: %+v", c)
	}
}

func TestWithoutHostsFileNothingHappens(t *testing.T) {
	e := setup(t)
	if err := e.mgr.RefreshHostsFile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.mgr.hostsCheck() != nil {
		t.Fatal("a check for something unused")
	}
}

func TestHostsFileSurvivesConcurrentRefreshes(t *testing.T) {
	e, path := hostsSetup(t)
	e.fake.Hosts = []map[string]any{{"subnet-id": 1, "hw-address": "aa:bb:cc:00:00:01", "ip-address": "192.0.2.10", "hostname": "nas"}}
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			e.fake.Lock()
			e.fake.Hosts[0]["hostname"] = fmt.Sprintf("nas%d", i)
			e.fake.Unlock()
			if err := e.mgr.RefreshHostsFile(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "192.0.2.10 nas") {
		t.Fatalf("%q %v", data, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("left behind: %v", entries)
	}
}

func TestNamesBeyondTheLimitsOfDNSAreSkipped(t *testing.T) {
	long := strings.Repeat("a", 64)
	got := string(hostsContent([]kea.Host{
		{"ip-address": "192.0.2.9", "hostname": long},
		{"ip-address": "192.0.2.10", "hostname": strings.Repeat("a.", 127) + "aa"},
		{"ip-address": "192.0.2.11", "hostname": strings.Repeat("a", 63)},
	}, nil))
	if strings.Contains(got, "192.0.2.9 ") || strings.Contains(got, "192.0.2.10 ") || !strings.Contains(got, "192.0.2.11 ") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestFailedChangeLeavesTheHostsFileAlone(t *testing.T) {
	e, path := hostsSetup(t)
	e.fake.Fail["reservation-add"] = keatest.Failure{Result: 1, Text: "no"}
	if _, err := e.mgr.CreateReservation(context.Background(), "alice", input()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a failed change wrote the file")
	}
	if e.audit(t).Status != StatusFailed {
		t.Fatal("no audit entry")
	}
}
