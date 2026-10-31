package manager

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vgropp/kea-roost/internal/keatest"
)

func (e *env) class(t *testing.T) {
	t.Helper()
	in := ClassInput{Name: "direct-dns", Options: []ReservationOption{{Name: "domain-name-servers", Data: "192.0.2.1"}}}
	if res, err := e.mgr.CreateClass(context.Background(), "u", in); err != nil || res.Status != StatusOK {
		t.Fatalf("got %+v %v", res, err)
	}
}

func TestClassLifecycle(t *testing.T) {
	e := setup(t)
	e.class(t)
	list, err := e.mgr.Classes(context.Background())
	if err != nil || len(list.Classes) != 1 || !list.Editable || list.Classes[0].Options[0].Data != "192.0.2.1" {
		t.Fatalf("got %+v %v", list, err)
	}
	if !e.fake.HasCall("config-write") {
		t.Error("not persisted")
	}

	res, err := e.mgr.UpdateClass(context.Background(), "u", "direct-dns", ClassInput{Options: []ReservationOption{{Name: "domain-name-servers", Data: "192.0.2.2"}}})
	if err != nil || res.Status != StatusOK {
		t.Fatalf("got %+v %v", res, err)
	}
	list, _ = e.mgr.Classes(context.Background())
	if list.Classes[0].Options[0].Data != "192.0.2.2" {
		t.Errorf("got %+v", list.Classes[0])
	}

	res, err = e.mgr.DeleteClass(context.Background(), "u", "direct-dns")
	if err != nil || res.Status != StatusOK {
		t.Fatalf("got %+v %v", res, err)
	}
	if list, _ = e.mgr.Classes(context.Background()); len(list.Classes) != 0 {
		t.Errorf("got %+v", list)
	}
}

func TestClassValidation(t *testing.T) {
	cases := map[string]struct {
		in   ClassInput
		want string
	}{
		"name":       {ClassInput{Name: "a b", Options: []ReservationOption{{Name: "x", Data: "1"}}}, "invalid class name"},
		"builtin":    {ClassInput{Name: "KNOWN", Options: []ReservationOption{{Name: "x", Data: "1"}}}, "built-in"},
		"no options": {ClassInput{Name: "empty"}, "at least one option"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			_, err := e.mgr.CreateClass(context.Background(), "u", c.in)
			var v *ValidationError
			if !errors.As(err, &v) || !strings.Contains(v.Error(), c.want) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestUsedClassCannotBeDeleted(t *testing.T) {
	e := setup(t)
	e.class(t)
	e.fake.Hosts = []map[string]any{{"subnet-id": 0, "hw-address": "aa:bb:cc:00:00:01", "ip-address": "192.0.2.10", "client-classes": []any{"direct-dns"}}}
	_, err := e.mgr.DeleteClass(context.Background(), "u", "direct-dns")
	var v *ValidationError
	if !errors.As(err, &v) {
		t.Fatalf("got %v", err)
	}
}

func TestReservationClasses(t *testing.T) {
	e := setup(t)
	e.class(t)
	in := input()
	in.Classes = []string{"direct-dns"}
	if res, err := e.mgr.CreateReservation(context.Background(), "u", in); err != nil || res.Status != StatusOK {
		t.Fatalf("got %+v %v", res, err)
	}
	e.fake.Leases = []map[string]any{{"ip-address": "192.0.2.10", "hw-address": "de:ad:be:ef:00:01", "subnet-id": 1}}
	list, err := e.mgr.Reservations(context.Background())
	if err != nil || len(list.Reservations[0].Classes) != 1 {
		t.Fatalf("got %+v %v", list, err)
	}
	leases, err := e.mgr.LeasesWithReservations(context.Background())
	if err != nil || leases.Leases[0].Reservation.Classes[0] != "direct-dns" {
		t.Fatalf("got %+v %v", leases, err)
	}

	in.Classes = []string{}
	if _, err := e.mgr.UpdateReservation(context.Background(), "u", in); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.fake.Hosts[0]["client-classes"]; ok {
		t.Errorf("class not removed: %+v", e.fake.Hosts[0])
	}
}

func TestReservationMovesToAnotherMAC(t *testing.T) {
	e := setup(t)
	e.fake.Hosts = []map[string]any{{
		"subnet-id": 0, "hw-address": "aa:bb:cc:00:00:01", "ip-address": "192.0.2.10", "hostname": "nas",
		"client-classes": []any{"direct-dns"}, "boot-file-name": "pxe.efi", "user-context": map[string]any{"comment": "rack"},
	}}
	e.class(t)
	in := input()
	in.SubnetID, in.Identifier, in.Comment = 0, "aa:bb:cc:00:00:01", "rack"
	in.NewIdentifier = "AA:BB:CC:00:00:99"
	res, err := e.mgr.UpdateReservation(context.Background(), "alice", in)
	if err != nil || res.Status != StatusOK {
		t.Fatalf("got %+v %v", res, err)
	}
	if len(e.fake.Hosts) != 1 {
		t.Fatalf("hosts %+v", e.fake.Hosts)
	}
	h := e.fake.Hosts[0]
	if h["hw-address"] != "aa:bb:cc:00:00:99" || h["boot-file-name"] != "pxe.efi" || h["client-classes"] == nil || h["ip-address"] != "192.0.2.10" {
		t.Errorf("host %+v", h)
	}
	if a := e.audit(t); a.Action != "reservation-replace" || !strings.Contains(a.Target, "00:99") {
		t.Errorf("audit %+v", a)
	}
}

func TestReservationMoveRejectsTakenMAC(t *testing.T) {
	e := setup(t)
	e.fake.Hosts = []map[string]any{
		{"subnet-id": 0, "hw-address": "aa:bb:cc:00:00:01", "ip-address": "192.0.2.10"},
		{"subnet-id": 0, "hw-address": "aa:bb:cc:00:00:02", "ip-address": "192.0.2.11"},
	}
	in := input()
	in.SubnetID, in.Identifier, in.NewIdentifier, in.Hostname, in.Comment = 0, "aa:bb:cc:00:00:01", "aa:bb:cc:00:00:02", "", ""
	_, err := e.mgr.UpdateReservation(context.Background(), "u", in)
	var v *ValidationError
	if !errors.As(err, &v) || !strings.Contains(v.Error(), "already reserved") {
		t.Fatalf("got %v", err)
	}
	if e.fake.HasCall("reservation-del") {
		t.Error("reached Kea")
	}
}

func TestReservationMoveReportsHalfDoneChange(t *testing.T) {
	e := setup(t)
	e.fake.Hosts = []map[string]any{{"subnet-id": 0, "hw-address": "aa:bb:cc:00:00:01", "ip-address": "192.0.2.10"}}
	e.fake.Fail["reservation-add"] = keatest.Failure{Result: 1, Text: "refused"}
	in := input()
	in.SubnetID, in.Identifier, in.NewIdentifier, in.Hostname, in.Comment = 0, "aa:bb:cc:00:00:01", "aa:bb:cc:00:00:99", "", ""
	res, err := e.mgr.UpdateReservation(context.Background(), "u", in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusPartial || !strings.Contains(res.Message, "old reservation was removed") || e.fake.HasCall("config-write") {
		t.Fatalf("got %+v", res)
	}
}

func TestReservationWithUnknownClassIsRejected(t *testing.T) {
	e := setup(t)
	in := input()
	in.Classes = []string{"nope"}
	_, err := e.mgr.CreateReservation(context.Background(), "u", in)
	var v *ValidationError
	if !errors.As(err, &v) || !strings.Contains(v.Error(), "class") {
		t.Fatalf("got %v", err)
	}
}

func TestUpdateWithoutClassesKeepsThem(t *testing.T) {
	e := setup(t)
	e.class(t)
	e.fake.Hosts = []map[string]any{{"subnet-id": 1, "hw-address": "aa:bb:cc:00:00:01", "ip-address": "192.0.2.10", "client-classes": []any{"direct-dns"}}}
	if _, err := e.mgr.UpdateReservation(context.Background(), "u", input()); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.fake.Hosts[0]["client-classes"]; !ok {
		t.Error("classes lost")
	}
}

func TestWritesAreRefusedWhenConfigWriteWouldEraseAnOption(t *testing.T) {
	e := setup(t)
	raw := e.rawSubnet()
	raw["option-data"] = []any{map[string]any{"name": "vendor-encapsulated-options", "code": 43, "csv-format": false, "data": ""}}
	res, err := e.mgr.CreateReservation(context.Background(), "u", input())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusFailed || !strings.Contains(res.Message, "vendor-encapsulated-options") {
		t.Fatalf("got %+v", res)
	}
	if e.fake.HasCall("reservation-add") || e.fake.HasCall("config-write") {
		t.Error("the write went through")
	}
	if d := e.mgr.Dashboard(context.Background()); !strings.Contains(warningsText(d), "vendor-encapsulated-options") {
		t.Errorf("no dashboard warning: %+v", d.Warnings)
	}
}

func warningsText(d *Dashboard) string {
	var out []string
	for _, w := range d.Warnings {
		out = append(out, w.Message)
	}
	return strings.Join(out, "|")
}
