package kea_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vgropp/kea-roost/internal/kea"
	"github.com/vgropp/kea-roost/internal/keatest"
)

func setup(t *testing.T) (*keatest.Server, *kea.Client) {
	t.Helper()
	fake := keatest.New()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	c, err := kea.NewClient(kea.Options{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return fake, c
}

func TestDecodesListWrappedAndBareResponses(t *testing.T) {
	for name, body := range map[string]string{
		"list": `[{"result":0,"text":"3.0.3"}]`,
		"bare": `{"result":0,"text":"3.0.3"}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Write([]byte(body))
			}))
			defer srv.Close()
			c, _ := kea.NewClient(kea.Options{URL: srv.URL})
			v, err := c.Version(context.Background())
			if err != nil || v != "3.0.3" {
				t.Fatalf("got %q, %v", v, err)
			}
		})
	}
}

func TestUnreachableServer(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c, _ := kea.NewClient(kea.Options{URL: url})
	_, err := c.Status(context.Background())
	var unreachable *kea.UnreachableError
	if !errors.As(err, &unreachable) {
		t.Fatalf("got %v", err)
	}
	if !c.LastContact().IsZero() {
		t.Error("last contact set without an answer")
	}
}

func TestGarbageIsNotAContact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("<html>proxy error</html>"))
	}))
	defer srv.Close()
	c, _ := kea.NewClient(kea.Options{URL: srv.URL})
	_, err := c.Status(context.Background())
	var unreachable *kea.UnreachableError
	if !errors.As(err, &unreachable) || !c.LastContact().IsZero() {
		t.Fatalf("got %v, contact %v", err, c.LastContact())
	}
}

func TestBasicAuth(t *testing.T) {
	fake := keatest.New()
	fake.User, fake.Pass = "admin", "pw"
	srv := httptest.NewServer(fake)
	defer srv.Close()

	bad, _ := kea.NewClient(kea.Options{URL: srv.URL, User: "admin", Password: "wrong"})
	_, err := bad.Status(context.Background())
	var httpErr *kea.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusUnauthorized {
		t.Fatalf("got %v", err)
	}
	good, _ := kea.NewClient(kea.Options{URL: srv.URL, User: "admin", Password: "pw"})
	if _, err := good.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestUnsupportedCommand(t *testing.T) {
	fake, c := setup(t)
	fake.Unknown["lease4-get-all"] = true
	_, err := c.Leases(context.Background())
	if !kea.IsUnsupported(err) {
		t.Fatalf("got %v", err)
	}
}

func TestEmptyLeaseListIsNotAnError(t *testing.T) {
	_, c := setup(t)
	leases, err := c.Leases(context.Background())
	if err != nil || len(leases) != 0 {
		t.Fatalf("got %v, %v", leases, err)
	}
}

func TestMissingReservationIsNil(t *testing.T) {
	_, c := setup(t)
	h, err := c.Reservation(context.Background(), 1, "hw-address", "aa:bb:cc:dd:ee:ff")
	if err != nil || h != nil {
		t.Fatalf("got %v, %v", h, err)
	}
}

func TestStatisticsUseNewestSample(t *testing.T) {
	_, c := setup(t)
	stats, err := c.Statistics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats["pkt4-received"] != 10 {
		t.Fatalf("got %d", stats["pkt4-received"])
	}
}

func TestStatusHA(t *testing.T) {
	_, c := setup(t)
	s, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.HA) != 1 || s.HA[0].Mode != "hot-standby" || s.HA[0].Servers.Local.Role != "primary" || s.HA[0].Servers.Remote.Role != "standby" {
		t.Fatalf("got %+v", s.HA)
	}
}

func TestHostAccessors(t *testing.T) {
	h := kea.Host{"subnet-id": int64(3), "client-id": "01:02", "ip-address": "0.0.0.0", "user-context": map[string]any{"comment": "x"}}
	if typ, id := h.Identifier(); typ != "client-id" || id != "01:02" {
		t.Errorf("identifier %s %s", typ, id)
	}
	if h.IP() != "" || h.SubnetID() != 3 || h.Comment() != "x" {
		t.Errorf("got %q %d %q", h.IP(), h.SubnetID(), h.Comment())
	}
}
