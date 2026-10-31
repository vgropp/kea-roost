package kea

import (
	"encoding/json"
	"net/netip"
	"testing"
)

const sample = `{"Dhcp4":{
  "valid-lifetime": 4000,
  "option-data":[
    {"name":"domain-name-servers","code":6,"space":"dhcp4","data":"10.0.0.2, 10.0.0.3"},
    {"name":"routers","code":3,"space":"dhcp4","data":"10.0.0.254"},
    {"name":"vendor-thing","code":1,"space":"vendor","data":"x"}],
  "subnet4":[{"id":1,"subnet":"10.0.1.0/24","valid-lifetime":600,
    "pools":[{"pool":"10.0.1.10 - 10.0.1.20"},{"pool":"10.0.1.128/25","pool-id":7}],
    "option-data":[{"name":"routers","code":3,"space":"dhcp4","data":"10.0.1.1"}]}],
  "shared-networks":[{"name":"lan","valid-lifetime":900,
    "option-data":[{"name":"routers","code":3,"space":"dhcp4","data":"10.0.2.1"}],
    "subnet4":[{"id":2,"subnet":"10.0.2.0/24","pools":[]}]}]
}}`

func TestParseSubnets(t *testing.T) {
	subnets, err := ParseSubnets(json.RawMessage(sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(subnets) != 2 {
		t.Fatalf("got %d subnets", len(subnets))
	}
	s := subnets[0]
	if got := s.Gateways(); len(got) != 1 || got[0] != "10.0.1.1" {
		t.Errorf("subnet option must override global: %v", got)
	}
	if got := s.DNSServers(); len(got) != 2 || got[1] != "10.0.0.3" {
		t.Errorf("global option must be inherited: %v", got)
	}
	if *s.ValidLifetime != 600 || s.Pools[1].ID != 7 || s.Pools[0].ID != 0 {
		t.Errorf("lifetime %d, pools %+v", *s.ValidLifetime, s.Pools)
	}
	for _, o := range s.Options {
		if o.Name == "vendor-thing" {
			t.Error("foreign option space leaked")
		}
	}
	n := subnets[1]
	if n.SharedNetwork != "lan" || n.Gateways()[0] != "10.0.2.1" || *n.ValidLifetime != 900 {
		t.Errorf("shared network inheritance: %+v", n)
	}
}

func TestParseSubnetsWithoutDhcp4(t *testing.T) {
	if _, err := ParseSubnets(json.RawMessage(`{"Dhcp6":{}}`)); err == nil {
		t.Fatal("expected error")
	}
}

func TestLossyOptions(t *testing.T) {
	const lost = `{"name":"vendor-encapsulated-options","csv-format":false,"data":""}`
	const sub = `{"name":"vendor-sub1","space":"vendor-encapsulated-options-space","csv-format":false,"data":"00000000"}`
	cases := map[string]struct {
		config string
		want   int
	}{
		"global":     {`{"Dhcp4":{"option-data":[` + lost + `]}}`, 1},
		"subnet":     {`{"Dhcp4":{"subnet4":[{"option-data":[` + lost + `]}]}}`, 1},
		"sub-option": {`{"Dhcp4":{"option-data":[` + lost + `,` + sub + `]}}`, 0},
		"text":       {`{"Dhcp4":{"option-data":[{"name":"x","csv-format":true,"data":""}]}}`, 0},
		"value":      {`{"Dhcp4":{"option-data":[{"name":"x","csv-format":false,"data":"00"}]}}`, 0},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := LossyOptions(json.RawMessage(c.config)); len(got) != c.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func TestParsePool(t *testing.T) {
	cases := []struct{ text, first, last string }{
		{"10.0.0.10 - 10.0.0.20", "10.0.0.10", "10.0.0.20"},
		{"10.0.0.10-10.0.0.20", "10.0.0.10", "10.0.0.20"},
		{"10.0.0.128/25", "10.0.0.128", "10.0.0.255"},
		{"10.0.0.0/32", "10.0.0.0", "10.0.0.0"},
		{"10.0.0.0/8", "10.0.0.0", "10.255.255.255"},
	}
	for _, c := range cases {
		p, err := ParsePool(c.text)
		if err != nil {
			t.Fatalf("%s: %v", c.text, err)
		}
		if p.First.String() != c.first || p.Last.String() != c.last {
			t.Errorf("%s: got %s - %s", c.text, p.First, p.Last)
		}
		if c.text == "10.0.0.128/25" && p.Size() != 128 {
			t.Errorf("%s: size %d", c.text, p.Size())
		}
		if !p.Contains(netip.MustParseAddr(c.last)) {
			t.Errorf("%s: end not contained", c.text)
		}
	}
	if _, err := ParsePool("nonsense"); err == nil {
		t.Error("expected error")
	}
}
