package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(values map[string]string) func(string) string {
	return func(k string) string { return values[k] }
}

func TestDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{"KEA_URL": "http://kea:8000"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.WriteEnabled() {
		t.Fatalf("got %+v", c)
	}
}

func TestInvalid(t *testing.T) {
	cases := map[string]map[string]string{
		"no url":        {},
		"bad scheme":    {"KEA_URL": "ftp://kea"},
		"user only":     {"KEA_URL": "http://kea", "MANAGER_USER": "a"},
		"cert only":     {"KEA_URL": "http://kea", "KEA_CLIENT_CERT": "a"},
		"both secret":   {"KEA_URL": "http://kea", "KEA_PASSWORD": "a", "KEA_PASSWORD_FILE": "b"},
		"no file":       {"KEA_URL": "http://kea", "MANAGER_PASSWORD_FILE": "/does/not/exist"},
		"bad monitor":   {"KEA_URL": "http://kea", "ARPMONITOR_URL": "host:8253"},
		"bad days":      {"KEA_URL": "http://kea", "ARPMONITOR_DAYS": "0"},
		"bad dashboard": {"KEA_URL": "http://kea", "APDASHBOARD_URL": "host:8189"},
		"bad base":      {"KEA_URL": "http://kea", "MANAGER_BASE_PATH": "kea-roost"},
	}
	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(env(values)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestBasePath(t *testing.T) {
	for in, want := range map[string]string{"": "", "/": "", "/kea-roost": "/kea-roost", "/kea-roost/": "/kea-roost"} {
		c, err := Load(env(map[string]string{"KEA_URL": "http://kea", "MANAGER_BASE_PATH": in}))
		if err != nil || c.BasePath != want {
			t.Errorf("%q: got %q, %v", in, c.BasePath, err)
		}
	}
}

func TestSecretFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(file, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(env(map[string]string{"KEA_URL": "https://kea", "MANAGER_USER": "a", "MANAGER_PASSWORD_FILE": file}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.Password, "\n") || c.Password != "hunter2" || !c.WriteEnabled() {
		t.Fatalf("got %q", c.Password)
	}
}

func TestPeers(t *testing.T) {
	c, err := Load(env(map[string]string{"KEA_URL": "http://kea", "KEA_PEERS": "standby=http://a:8051, other=https://b"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Peers) != 2 || c.Peers[0] != (Peer{"standby", "http://a:8051"}) || c.Peers[1].Name != "other" {
		t.Fatalf("got %+v", c.Peers)
	}
	for _, bad := range []string{"http://a", "x=ftp://a", "=http://a", "a=http://a,a=http://b", "kea=http://a"} {
		if _, err := Load(env(map[string]string{"KEA_URL": "http://kea", "KEA_PEERS": bad})); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestHostsDir(t *testing.T) {
	c, err := Load(env(map[string]string{"KEA_URL": "http://kea", "DNS_HOSTS_DIR": "/dns", "DNS_HOSTS_DOMAIN": "lan, .home."}))
	if err != nil || c.HostsDir != "/dns" || strings.Join(c.HostsDomains, "+") != "lan+home" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := Load(env(map[string]string{"KEA_URL": "http://kea", "DNS_HOSTS_DOMAIN": "a b"})); err == nil {
		t.Fatal("an invalid domain was accepted")
	}
	if _, err := Load(env(map[string]string{"KEA_URL": "http://kea", "DNS_HOSTS_DIR": "dns"})); err == nil {
		t.Fatal("a relative path was accepted")
	}
}
