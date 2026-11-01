package manager

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/vgropp/kea-roost/internal/kea"
)

func TestSettingsAreSavedAndComeBack(t *testing.T) {
	e := setup(t)
	if err := e.mgr.UpdateSettings("alice", map[string]int{"reviewDays": 90, "onlineMinutes": 5}); err != nil {
		t.Fatal(err)
	}
	if c := e.mgr.cfg(); c.ReviewDays != 90 || c.OnlineMinutes != 5 || c.DevicesDays != 7 {
		t.Fatalf("settings: %+v", c)
	}
	if a := e.audit(t); a.Action != "settings-update" || a.User != "alice" || !strings.Contains(a.Target, "reviewDays 365 -> 90") {
		t.Fatalf("audit: %+v", a)
	}
	data, err := os.ReadFile(e.mgr.Store().settingsPath())
	if err != nil || strings.Contains(string(data), "poolWarnPercent") {
		t.Fatalf("only changed values belong in the file: %s %v", data, err)
	}
	info, err := os.Stat(e.mgr.Store().settingsPath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", info, err)
	}

	again := New(e.mgr.kea, e.mgr.Store(), "primary")
	if err := again.LoadSettings(); err != nil {
		t.Fatal(err)
	}
	if c := again.cfg(); c.ReviewDays != 90 || c.PoolWarnPercent != 80 {
		t.Fatalf("settings: %+v", c)
	}
}

func TestInvalidSettingsChangeNothing(t *testing.T) {
	e := setup(t)
	before := e.mgr.cfg()
	for name, values := range map[string]map[string]int{
		"too small": {"reviewDays": 0},
		"too large": {"poolWarnPercent": 101},
		"unknown":   {"password": 1},
		"crossed":   {"poolWarnPercent": 90, "poolCritPercent": 80},
	} {
		var invalid *ValidationError
		if err := e.mgr.UpdateSettings("alice", values); !errors.As(err, &invalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if e.mgr.cfg() != before {
		t.Fatal("settings changed")
	}
	if _, err := os.Stat(e.mgr.Store().settingsPath()); err == nil {
		t.Fatal("file written")
	}
}

func TestSettingsFileIsChecked(t *testing.T) {
	e := setup(t)
	for _, content := range []string{"{", `{"reviewDays": -1}`} {
		if err := os.WriteFile(e.mgr.Store().settingsPath(), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := e.mgr.LoadSettings(); err == nil {
			t.Errorf("%q was accepted", content)
		}
	}
}

func TestSettingsTakeEffect(t *testing.T) {
	e := setup(t)
	asked := make(chan string, 1)
	monitor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked <- r.URL.Query().Get("days")
		w.Write([]byte("[]"))
	}))
	t.Cleanup(monitor.Close)
	e.mgr.SetARPMonitor(monitor.URL)
	if err := e.mgr.UpdateSettings("alice", map[string]int{"reviewDays": 90}); err != nil {
		t.Fatal(err)
	}
	e.mgr.fetchNetwork(context.Background())
	if got := <-asked; got != "90" {
		t.Fatalf("asked for %s days", got)
	}
	list := reviewNetwork(reviewSubnets(t), []kea.Host{reserved(macA, "192.0.2.10")}, nil, nil, reviewNow, e.mgr.cfg())
	if len(list) != 1 || !strings.Contains(list[0].Message, "90 days") {
		t.Fatalf("list: %+v", list)
	}
}
