// Package config reads the settings of the manager from the environment, so
// that a compose file or systemd unit can provide them without a config file.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var hostsDomain = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)

var peerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Config holds all settings.
type Config struct {
	KeaURL         string
	Kea6URL        string
	Peers          []Peer
	Peers6         []Peer
	HostsDir       string
	TrustedProxies []netip.Prefix
	HostsDomains   []string
	KeaUser        string
	KeaPassword    string
	KeaCAFile      string
	KeaClientCert  string
	KeaClientKey   string
	ServerName     string
	Listen         string
	MetricsOff     bool
	MetricsUser    string
	MetricsPass    string
	DataDir        string
	ARPMonitorURL  string
	APDashboardURL string
	BasePath       string
}

// Peer is a further Kea server that gets every change. It uses the credentials of
// the primary.
type Peer struct {
	Name string
	URL  string
}

// Obsolete returns the settings that are set but no longer used, so that the start can
// point them out instead of ignoring them silently.
func Obsolete(getenv func(string) string) []string {
	var out []string
	for _, name := range []string{"MANAGER_USER", "MANAGER_PASSWORD", "MANAGER_PASSWORD_FILE", "ARPMONITOR_DAYS"} {
		if getenv(name) != "" {
			out = append(out, name)
		}
	}
	return out
}

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
		MetricsUser:    getenv("METRICS_USER"),
		DataDir:        or(getenv("MANAGER_DATA_DIR"), "/var/lib/kea-roost"),
		ARPMonitorURL:  getenv("ARPMONITOR_URL"),
		APDashboardURL: getenv("APDASHBOARD_URL"),
		HostsDir:       getenv("DNS_HOSTS_DIR"),
	}
	var err error
	if c.KeaPassword, err = secret(getenv, "KEA_PASSWORD"); err != nil {
		return nil, err
	}
	if c.MetricsPass, err = secret(getenv, "METRICS_PASSWORD"); err != nil {
		return nil, err
	}
	if c.KeaURL == "" {
		return nil, errors.New("KEA_URL is not set")
	}
	if !isHTTPURL(c.KeaURL) {
		return nil, fmt.Errorf("KEA_URL %q is not an http(s) URL", c.KeaURL)
	}
	if c.Kea6URL = getenv("KEA6_URL"); c.Kea6URL != "" && !isHTTPURL(c.Kea6URL) {
		return nil, fmt.Errorf("KEA6_URL %q is not an http(s) URL", c.Kea6URL)
	}
	if c.Peers, err = parsePeers("KEA_PEERS", getenv("KEA_PEERS"), c.ServerName); err != nil {
		return nil, err
	}
	if c.Peers6, err = parsePeers("KEA6_PEERS", getenv("KEA6_PEERS"), c.ServerName); err != nil {
		return nil, err
	}
	if len(c.Peers6) > 0 && c.Kea6URL == "" {
		return nil, errors.New("KEA6_PEERS needs KEA6_URL for the primary kea-dhcp6")
	}
	for _, item := range strings.Split(getenv("MANAGER_TRUSTED_PROXIES"), ",") {
		if item = strings.TrimSpace(item); item == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(item)
		if err != nil {
			addr, addrErr := netip.ParseAddr(item)
			if addrErr != nil {
				return nil, fmt.Errorf("MANAGER_TRUSTED_PROXIES holds %q, which is no address or network", item)
			}
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		c.TrustedProxies = append(c.TrustedProxies, prefix.Masked())
	}
	if c.HostsDir != "" && !strings.HasPrefix(c.HostsDir, "/") {
		return nil, fmt.Errorf("DNS_HOSTS_DIR %q must be an absolute path", c.HostsDir)
	}
	for _, domain := range strings.Split(getenv("DNS_HOSTS_DOMAIN"), ",") {
		domain = strings.Trim(strings.TrimSpace(domain), ".")
		if domain == "" {
			continue
		}
		if !hostsDomain.MatchString(domain) {
			return nil, fmt.Errorf("DNS_HOSTS_DOMAIN holds the invalid domain %q", domain)
		}
		c.HostsDomains = append(c.HostsDomains, domain)
	}
	if c.ARPMonitorURL != "" && !isHTTPURL(c.ARPMonitorURL) {
		return nil, fmt.Errorf("ARPMONITOR_URL %q is not an http(s) URL", c.ARPMonitorURL)
	}
	if c.APDashboardURL != "" && !isHTTPURL(c.APDashboardURL) {
		return nil, fmt.Errorf("APDASHBOARD_URL %q is not an http(s) URL", c.APDashboardURL)
	}
	c.BasePath = strings.TrimRight(getenv("MANAGER_BASE_PATH"), "/")
	if c.BasePath != "" && !strings.HasPrefix(c.BasePath, "/") {
		return nil, fmt.Errorf("MANAGER_BASE_PATH %q must start with /", c.BasePath)
	}
	if (c.KeaClientCert == "") != (c.KeaClientKey == "") {
		return nil, errors.New("KEA_CLIENT_CERT and KEA_CLIENT_KEY must be set together")
	}
	if text := getenv("METRICS_ENABLED"); text != "" {
		enabled, err := strconv.ParseBool(text)
		if err != nil {
			return nil, fmt.Errorf("METRICS_ENABLED %q is neither true nor false", text)
		}
		c.MetricsOff = !enabled
	}
	if (c.MetricsUser == "") != (c.MetricsPass == "") {
		return nil, errors.New("METRICS_USER and METRICS_PASSWORD must be set together")
	}
	return c, nil
}

// parsePeers reads a list like "standby=http://192.0.2.2:8051,other=http://...".
func parsePeers(setting, text, primary string) ([]Peer, error) {
	var peers []Peer
	seen := map[string]bool{primary: true}
	for _, item := range strings.Split(text, ",") {
		if item = strings.TrimSpace(item); item == "" {
			continue
		}
		name, address, ok := strings.Cut(item, "=")
		if !ok || !peerName.MatchString(name) || !isHTTPURL(address) {
			return nil, fmt.Errorf("%s entry %q is not name=http(s)-URL", setting, item)
		}
		if seen[name] {
			return nil, fmt.Errorf("%s uses the name %q twice, KEA_SERVER_NAME counts as well", setting, name)
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
