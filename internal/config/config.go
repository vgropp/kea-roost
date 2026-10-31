// Package config reads the settings of the manager from the environment, so
// that a compose file or systemd unit can provide them without a config file.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var peerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Config holds all settings.
type Config struct {
	KeaURL         string
	Peers          []Peer
	KeaUser        string
	KeaPassword    string
	KeaCAFile      string
	KeaClientCert  string
	KeaClientKey   string
	ServerName     string
	Listen         string
	User           string
	Password       string
	DataDir        string
	ARPMonitorURL  string
	ARPDays        int
	APDashboardURL string
	BasePath       string
}

// Peer is a further Kea server that gets every change. It uses the credentials of
// the primary.
type Peer struct {
	Name string
	URL  string
}

// WriteEnabled reports whether a login is configured. Without one, the manager
// stays read-only: changes must always be attributable to an authorized user.
func (c *Config) WriteEnabled() bool { return c.User != "" && c.Password != "" }

// Load reads the configuration through getenv. For passwords, a NAME_FILE
// variable can point to a file holding the value (e.g. a Docker secret).
func Load(getenv func(string) string) (*Config, error) {
	c := &Config{
		KeaURL:         getenv("KEA_URL"),
		KeaUser:        getenv("KEA_USER"),
		KeaCAFile:      getenv("KEA_CA_FILE"),
		KeaClientCert:  getenv("KEA_CLIENT_CERT"),
		KeaClientKey:   getenv("KEA_CLIENT_KEY"),
		ServerName:     or(getenv("KEA_SERVER_NAME"), "kea"),
		Listen:         or(getenv("MANAGER_LISTEN"), ":8080"),
		User:           getenv("MANAGER_USER"),
		DataDir:        or(getenv("MANAGER_DATA_DIR"), "/var/lib/kea-roost"),
		ARPMonitorURL:  getenv("ARPMONITOR_URL"),
		ARPDays:        7,
		APDashboardURL: getenv("APDASHBOARD_URL"),
	}
	var err error
	if c.KeaPassword, err = secret(getenv, "KEA_PASSWORD"); err != nil {
		return nil, err
	}
	if c.Password, err = secret(getenv, "MANAGER_PASSWORD"); err != nil {
		return nil, err
	}
	if c.KeaURL == "" {
		return nil, errors.New("KEA_URL is not set")
	}
	if !isHTTPURL(c.KeaURL) {
		return nil, fmt.Errorf("KEA_URL %q is not an http(s) URL", c.KeaURL)
	}
	if c.Peers, err = parsePeers(getenv("KEA_PEERS"), c.ServerName); err != nil {
		return nil, err
	}
	if c.ARPMonitorURL != "" && !isHTTPURL(c.ARPMonitorURL) {
		return nil, fmt.Errorf("ARPMONITOR_URL %q is not an http(s) URL", c.ARPMonitorURL)
	}
	if c.APDashboardURL != "" && !isHTTPURL(c.APDashboardURL) {
		return nil, fmt.Errorf("APDASHBOARD_URL %q is not an http(s) URL", c.APDashboardURL)
	}
	if text := getenv("ARPMONITOR_DAYS"); text != "" {
		if c.ARPDays, err = strconv.Atoi(text); err != nil || c.ARPDays < 1 {
			return nil, fmt.Errorf("ARPMONITOR_DAYS %q is not a positive number", text)
		}
	}
	c.BasePath = strings.TrimRight(getenv("MANAGER_BASE_PATH"), "/")
	if c.BasePath != "" && !strings.HasPrefix(c.BasePath, "/") {
		return nil, fmt.Errorf("MANAGER_BASE_PATH %q must start with /", c.BasePath)
	}
	if (c.KeaClientCert == "") != (c.KeaClientKey == "") {
		return nil, errors.New("KEA_CLIENT_CERT and KEA_CLIENT_KEY must be set together")
	}
	if (c.User == "") != (c.Password == "") {
		return nil, errors.New("MANAGER_USER and MANAGER_PASSWORD must be set together")
	}
	return c, nil
}

// parsePeers reads a list like "standby=http://192.0.2.2:8051,other=http://...".
func parsePeers(text, primary string) ([]Peer, error) {
	var peers []Peer
	seen := map[string]bool{primary: true}
	for _, item := range strings.Split(text, ",") {
		if item = strings.TrimSpace(item); item == "" {
			continue
		}
		name, address, ok := strings.Cut(item, "=")
		if !ok || !peerName.MatchString(name) || !isHTTPURL(address) {
			return nil, fmt.Errorf("KEA_PEERS entry %q is not name=http(s)-URL", item)
		}
		if seen[name] {
			return nil, fmt.Errorf("KEA_PEERS uses the name %q twice, KEA_SERVER_NAME counts as well", name)
		}
		seen[name] = true
		peers = append(peers, Peer{name, address})
	}
	return peers, nil
}

func isHTTPURL(text string) bool {
	u, err := url.Parse(text)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func secret(getenv func(string) string, name string) (string, error) {
	value, file := getenv(name), getenv(name+"_FILE")
	if file == "" {
		return value, nil
	}
	if value != "" {
		return "", fmt.Errorf("%s and %s_FILE are mutually exclusive", name, name)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("%s_FILE: %w", name, err)
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

func or(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
