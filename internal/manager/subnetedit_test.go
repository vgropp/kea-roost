package manager

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vgropp/kea-roost/internal/keatest"
)

func subnetInput() SubnetInput {
	return SubnetInput{
		Pools:         []string{"192.0.2.100 - 192.0.2.199"},
		Gateways:      []string{"192.0.2.1"},
		DNSServers:    []string{"192.0.2.53"},
		ValidLifetime: 4000,
	}
}

func (e *env) rawSubnet() map[string]any {
	return e.fake.Config["subnet4"].([]any)[0].(map[string]any)
}

func TestUpdateSubnetChangesOnlyEditedFields(t *testing.T) {
	e := setup(t)
	raw := e.rawSubnet()
	raw["min-valid-lifetime"] = 4000
	raw["max-valid-lifetime"] = 4000
	raw["4o6-subnet"] = ""
	raw["next-server"] = "192.0.2.5"
	raw["pools"] = []any{map[string]any{"pool": "192.0.2.100-192.0.2.199", "client-class": "guests"}}
	raw["option-data"] = []any{
		map[string]any{"name": "routers", "code": 3, "data": "192.0.2.1"},
		map[string]any{"name": "subnet-mask", "code": 1, "data": "255.255.255.0"},
	}
	in := subnetInput()
	in.Pools = append(in.Pools, "192.0.2.210-192.0.2.220")
	in.Gateways = []string{"192.0.2.254"}
	in.ValidLifetime = 3600

	res, err := e.mgr.UpdateSubnet(context.Background(), "u", 1, in)
	if err != nil || res.Status != StatusOK {
		t.Fatalf("got %+v %v", res, err)
	}
	got, _ := json.Marshal(e.rawSubnet())
	text := string(got)
	for _, want := range []string{`"192.0.2.210-192.0.2.220"`, `"client-class":"guests"`, `"data":"192.0.2.254"`, `"subnet-mask"`,
		`"next-server":"192.0.2.5"`, `"valid-lifetime":3600`, `"min-valid-lifetime":3600`, `"max-valid-lifetime":3600`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s in %s", want, text)
		}
	}
	if strings.Contains(text, "4o6-subnet") {
		t.Errorf("empty 4o6 setting sent: %s", text)
	}
	if !e.fake.HasCall("config-write") || res.Backup == "" {
		t.Errorf("not persisted or no backup: %+v", res)
	}
	if a := e.audit(t); a.Action != "subnet4-update" || a.Status != StatusOK {
		t.Errorf("audit %+v", a)
	}
}

func TestUpdateSubnetKeepsInheritedValues(t *testing.T) {
	e := setup(t)
	if _, err := e.mgr.UpdateSubnet(context.Background(), "u", 1, subnetInput()); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(e.rawSubnet())
	// DNS and lifetime come from the global scope and were not changed
	if text := string(got); strings.Contains(text, "domain-name-servers") || strings.Contains(text, "valid-lifetime") {
		t.Errorf("inherited values pinned to the subnet: %s", text)
	}
}

func TestSubnetValidation(t *testing.T) {
	cases := map[string]struct {
		change func(*SubnetInput)
		want   string
	}{
		"outside":   {func(i *SubnetInput) { i.Pools = []string{"10.0.0.1-10.0.0.9"} }, "not inside"},
		"reversed":  {func(i *SubnetInput) { i.Pools = []string{"192.0.2.20-192.0.2.10"} }, "ends before"},
		"overlap":   {func(i *SubnetInput) { i.Pools = []string{"192.0.2.10-192.0.2.20", "192.0.2.15-192.0.2.30"} }, "overlap"},
		"broadcast": {func(i *SubnetInput) { i.Pools = []string{"192.0.2.10-192.0.2.255"} }, "broadcast"},
		"garbage":   {func(i *SubnetInput) { i.Pools = []string{"nonsense"} }, "invalid pool"},
		"gateway":   {func(i *SubnetInput) { i.Gateways = []string{"10.0.0.1"} }, "gateway"},
		"dns":       {func(i *SubnetInput) { i.DNSServers = []string{"x"} }, "DNS"},
		"lifetime":  {func(i *SubnetInput) { i.ValidLifetime = 0 }, "lifetime"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			in := subnetInput()
			c.change(&in)
			_, err := e.mgr.UpdateSubnet(context.Background(), "u", 1, in)
			var v *ValidationError
			if !errors.As(err, &v) || !strings.Contains(v.Error(), c.want) {
				t.Fatalf("got %v", err)
			}
			if e.fake.HasCall("subnet4-update") {
				t.Error("rejected input reached Kea")
			}
		})
	}
}

func TestUnknownSubnet(t *testing.T) {
	e := setup(t)
	if _, err := e.mgr.UpdateSubnet(context.Background(), "u", 9, subnetInput()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestSubnetWithOwnReservationsIsRefused(t *testing.T) {
	e := setup(t)
	e.fake.Hosts = []map[string]any{{"subnet-id": 1, "hw-address": "aa:bb:cc:00:00:01", "ip-address": "192.0.2.10"}}
	_, err := e.mgr.UpdateSubnet(context.Background(), "u", 1, subnetInput())
	var v *ValidationError
	if !errors.As(err, &v) || !strings.Contains(v.Error(), "reservations") {
		t.Fatalf("got %v", err)
	}
}

func TestSubnetCheckWarnsWithoutChanging(t *testing.T) {
	e := setup(t)
	now := time.Now().Unix()
	e.fake.Hosts = []map[string]any{
		{"subnet-id": 0, "hw-address": "aa:bb:cc:00:00:01", "ip-address": "192.0.2.50"},
		{"subnet-id": 0, "hw-address": "aa:bb:cc:00:00:02", "ip-address": "192.0.2.101"},
	}
	e.fake.Leases = []map[string]any{
		{"ip-address": "192.0.2.150", "hw-address": "de:ad:be:ef:00:01", "subnet-id": 1, "state": 0, "cltt": now, "valid-lft": 100}, // leaves the pool
		{"ip-address": "192.0.2.101", "hw-address": "aa:bb:cc:00:00:02", "subnet-id": 1, "state": 0, "cltt": now, "valid-lft": 100}, // reserved
		{"ip-address": "192.0.2.151", "hw-address": "de:ad:be:ef:00:02", "subnet-id": 1, "state": 0, "cltt": 1, "valid-lft": 100},   // expired
	}
	in := subnetInput()
	in.Pools = []string{"192.0.2.40-192.0.2.60"}
	warnings, err := e.mgr.CheckSubnet(context.Background(), 1, in)
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(warnings, "|")
	if !strings.Contains(all, "1 active leases") || !strings.Contains(all, "192.0.2.150") || strings.Contains(all, "192.0.2.151") {
		t.Errorf("lease warning: %s", all)
	}
	if !strings.Contains(all, "1 reservations now lie inside") || !strings.Contains(all, "192.0.2.50") {
		t.Errorf("reservation warning: %s", all)
	}
	if e.fake.HasCall("subnet4-update") || e.fake.HasCall("config-write") {
		t.Error("the check changed something")
	}
}

func TestUpdateSubnetFailures(t *testing.T) {
	cases := []struct {
		name    string
		command string
		failure keatest.Failure
		status  Status
	}{
		{"kea rejects", "subnet4-update", keatest.Failure{Result: 1, Text: "bad"}, StatusFailed},
		{"no answer", "subnet4-update", keatest.Failure{Drop: true}, StatusPartial},
		{"config-write fails", "config-write", keatest.Failure{Result: 1, Text: "disk"}, StatusPartial},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t)
			e.fake.Fail[c.command] = c.failure
			res, err := e.mgr.UpdateSubnet(context.Background(), "u", 1, subnetInput())
			if err != nil || res.Status != c.status {
				t.Fatalf("got %+v %v", res, err)
			}
		})
	}
}

func TestEditableNeedsSubnetCommands(t *testing.T) {
	e := setup(t)
	list, err := e.mgr.Subnets(context.Background())
	if err != nil || !list.Editable {
		t.Fatalf("got %+v %v", list, err)
	}
	e.fake.Unknown["subnet4-update"] = true
	list, err = e.mgr.Subnets(context.Background())
	if err != nil || list.Editable {
		t.Fatalf("got %+v %v", list, err)
	}
}
