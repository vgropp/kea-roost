package kea

import (
	"encoding/json"
	"testing"
)

func TestParsePool6(t *testing.T) {
	for text, size := range map[string]string{
		"fd00:db8:1::1000 - fd00:db8:1::ffff": "61440",
		"fd00:db8:1::/120":                    "256",
		"fd00:db8:1::/64":                     "18446744073709551616",
	} {
		p, err := ParsePool6(text)
		if err != nil || p.Size != size {
			t.Errorf("%s: %+v %v", text, p, err)
		}
	}
	for _, text := range []string{"192.0.2.1 - 192.0.2.9", "fd00::9 - fd00::1", "nonsense"} {
		if _, err := ParsePool6(text); err == nil {
			t.Errorf("%s was accepted", text)
		}
	}
}

func TestParseSubnets6(t *testing.T) {
	config := json.RawMessage(`{"Dhcp6": {
		"option-data": [{"name": "dns-servers", "data": "fd00:db8:1::1"}],
		"subnet6": [{"id": 1, "subnet": "fd00:db8:1::/64", "interface": "lan",
			"pools": [{"pool": "fd00:db8:1::1000 - fd00:db8:1::ffff"}],
			"pd-pools": [{"prefix": "fd00:db8:2::", "prefix-len": 48, "delegated-len": 56}]}],
		"shared-networks": [{"name": "guests", "interface": "guest",
			"subnet6": [{"id": 2, "subnet": "fd00:db8:3::/64"}]}]}}`)
	subnets, err := ParseSubnets6(config)
	if err != nil || len(subnets) != 2 {
		t.Fatalf("%+v %v", subnets, err)
	}
	s := subnets[0]
	if s.Interface != "lan" || len(s.Pools) != 1 || len(s.PDPools) != 1 || s.PDPools[0].DelegatedLen != 56 {
		t.Errorf("%+v", s)
	}
	if dns := s.DNSServers(); len(dns) != 1 || dns[0] != "fd00:db8:1::1" {
		t.Errorf("global option not inherited: %v", dns)
	}
	if subnets[1].SharedNetwork != "guests" || subnets[1].Interface != "guest" {
		t.Errorf("%+v", subnets[1])
	}
	if _, err := ParseSubnets6(json.RawMessage(`{"Dhcp4": {}}`)); err == nil {
		t.Error("a DHCPv4 configuration was accepted")
	}
}

func TestHostAddresses6(t *testing.T) {
	h := Host{"ip-addresses": []any{"fd00:db8:1::10", ""}, "prefixes": []any{"fd00:db8:2::/56"}}
	if ips := h.IPs(); len(ips) != 1 || ips[0] != "fd00:db8:1::10" {
		t.Errorf("%v", ips)
	}
	if p := h.Prefixes(); len(p) != 1 {
		t.Errorf("%v", p)
	}
}
