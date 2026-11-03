package manager

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/vgropp/kea-roost/internal/kea"
)

// The short form of an address is typed by the operator.
func FuzzWithPrefix(f *testing.F) {
	for _, seed := range []string{"::20", " ::1:2 ", "fd00:db8:1::30", "::1:0:0:0:5", "::", "::zz", "::ffff:192.0.2.1", "::1%eth0"} {
		f.Add(seed, "fd00:db8:1::/64")
	}
	f.Add("::5", "fd00:db8:1:2:3:4:5:0/120")
	f.Add("::1", "::/0")
	f.Fuzz(func(t *testing.T, text, prefix string) {
		p, err := netip.ParsePrefix(prefix)
		if err != nil {
			return
		}
		got := withPrefix(text, p)
		// it either completes the address inside the prefix or leaves the text alone
		if got != strings.TrimSpace(text) {
			ip, err := netip.ParseAddr(got)
			if err != nil || !p.Masked().Contains(ip) {
				t.Errorf("%q in %s became %q", text, p, got)
			}
		}
	})
}

// The subnet map and the review take whatever kea-dhcp6 reports.
func FuzzSubnetMap6(f *testing.F) {
	f.Add([]byte(`{"Dhcp6":{"subnet6":[{"id":1,"subnet":"fd00:db8:1::/64","pools":[{"pool":"fd00:db8:1::1000-fd00:db8:1::ffff"}],"pd-pools":[{"prefix":"fd00:db8:2::","prefix-len":48,"delegated-len":56}]}]}}`),
		"fd00:db8:1::1000", 128)
	f.Add([]byte(`{"Dhcp6":{"subnet6":[{"id":1,"subnet":"::/0","pools":[{"pool":"::/0"}],"pd-pools":[{"prefix":"::","prefix-len":1,"delegated-len":128}]}]}}`),
		"::", 1)
	f.Fuzz(func(t *testing.T, config []byte, address string, prefixLen int) {
		subnets, err := kea.ParseSubnets6(config)
		if err != nil {
			return
		}
		leases := []kea.Lease6{
			{IPAddress: address, Type: "IA_NA", SubnetID: 1, ValidLft: 3600, CLTT: time.Now().Unix()},
			{IPAddress: address, Type: "IA_PD", PrefixLen: prefixLen, SubnetID: 1, ValidLft: 3600, CLTT: time.Now().Unix()},
		}
		hosts := []kea.Host{{"subnet-id": int64(1), "ip-addresses": []any{address}, "prefixes": []any{address}}}
		for _, s := range subnets {
			m := subnetMap6(s, leases, hosts, time.Now())
			for _, p := range m.Pools {
				if len(p.Blocks) > map6Blocks {
					t.Errorf("%d blocks", len(p.Blocks))
				}
			}
			for _, p := range m.PDPools {
				if len(p.Prefixes) > map6Prefixes {
					t.Errorf("%d prefixes", len(p.Prefixes))
				}
			}
		}
		reviewConfig6(config, subnets, hosts)
	})
}
