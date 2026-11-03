package kea

import (
	"math/big"
	"testing"
)

// Pools come from the configuration of Kea and from the subnet dialog.
func FuzzParsePool6(f *testing.F) {
	for _, seed := range []string{"fd00:db8:1::1000 - fd00:db8:1::ffff", "fd00:db8:1::/64", "::/0", "fd00::2-fd00::1", "1.2.3.4-1.2.3.5", "-", "/"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		p, err := ParsePool6(text)
		if err != nil {
			return
		}
		if p.Last.Less(p.First) {
			t.Errorf("%q: %s before %s", text, p.Last, p.First)
		}
		if size, ok := new(big.Int).SetString(p.Size, 10); !ok || size.Sign() <= 0 {
			t.Errorf("%q: size %q", text, p.Size)
		}
	})
}

// A config-get answer of kea-dhcp6 is read without trusting its shape.
func FuzzParseSubnets6(f *testing.F) {
	f.Add([]byte(`{"Dhcp6":{"subnet6":[{"id":1,"subnet":"fd00:db8:1::/64","pools":[{"pool":"fd00:db8:1::1000-fd00:db8:1::ffff"}],"pd-pools":[{"prefix":"fd00:db8:2::","prefix-len":48,"delegated-len":56}]}]}}`))
	f.Add([]byte(`{"Dhcp6":{"shared-networks":[{"name":"x","relay":{"ip-addresses":["fd00::1"]},"subnet6":[{"id":2,"subnet":"fd00:db8:3::/64"}]}]}}`))
	f.Add([]byte(`{"Dhcp6":null}`))
	f.Fuzz(func(t *testing.T, config []byte) {
		subnets, err := ParseSubnets6(config)
		if err != nil {
			return
		}
		for _, s := range subnets {
			for _, p := range s.Pools {
				if p.Last.Less(p.First) {
					t.Errorf("subnet %d: pool %s ends before it starts", s.ID, p.Text)
				}
			}
		}
	})
}
