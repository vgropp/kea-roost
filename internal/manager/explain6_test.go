package manager

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func steps(e *Explanation) string {
	var b strings.Builder
	for _, s := range e.Steps {
		b.WriteString(string(s.Severity) + " " + s.Title + ": " + s.Detail + "\n")
	}
	return b.String()
}

func TestClient6ExplainsALease(t *testing.T) {
	e, _ := setup6(t)
	// the printer holds a pool address, but is reserved fd00:db8:1::10
	c, err := e.mgr.Client6(context.Background(), "FD00:DB8:1::1000")
	if err != nil {
		t.Fatal(err)
	}
	got := steps(c.Explanation)
	for _, want := range []string{"chosen by the interface lan", "by duid 00:03:00:01:00:00:0c:00:00:01: fd00:db8:1::10",
		"warn Moves: Kea reserves fd00:db8:1::10", "comes from the pool"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing in\n%s", want, got)
		}
	}
	if c.Name != "printer" || c.Lease == nil || c.Reservation == nil || len(c.Explanation.Options) != 1 || c.Explanation.Options[0].Source != "subnet fd00:db8:1::/64" {
		t.Errorf("%+v %+v", c, c.Explanation.Options)
	}
}

func TestClient6ExplainsAReservationWithoutLease(t *testing.T) {
	e, _ := setup6(t)
	c, err := e.mgr.Client6(context.Background(), "fd00:db8:1::10")
	if err != nil {
		t.Fatal(err)
	}
	if got := steps(c.Explanation); !strings.Contains(got, "comes from the reservation") || strings.Contains(got, "Moves") {
		t.Errorf("%s", got)
	}
}

func TestClient6Unknown(t *testing.T) {
	e, _ := setup6(t)
	if _, err := e.mgr.Client6(context.Background(), "fd00:db8:1::9999"); !errors.Is(err, ErrNotFound) {
		t.Errorf("%v", err)
	}
	var invalid *ValidationError
	if _, err := e.mgr.Client6(context.Background(), "192.0.2.10"); !errors.As(err, &invalid) {
		t.Errorf("%v", err)
	}
}
