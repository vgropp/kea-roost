package manager

import (
	"context"
	"testing"
)

func TestSubnetMap6(t *testing.T) {
	e, fake6 := setup6(t)
	fake6.Lock()
	fake6.Leases6 = append(fake6.Leases6, map[string]any{"ip-address": "fd00:db8:2:100::", "prefix-len": 56, "type": "IA_PD", "duid": "00:07", "iaid": 1,
		"subnet-id": 1, "state": 0, "cltt": fake6.Leases6[0]["cltt"], "valid-lft": 86400, "hostname": "router"})
	fake6.Hosts = append(fake6.Hosts, map[string]any{"subnet-id": 1, "duid": "00:03:00:01:00:00:0c:00:00:09", "ip-addresses": []any{"fd00:db8:1::1001"}, "hostname": "laptop-fixed"})
	fake6.Unlock()
	list, err := e.mgr.Subnets6(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := list.Subnets[0].Map
	if len(m.Pools) != 1 || len(m.Pools[0].Blocks) != 256 || m.Pools[0].Each != "240" || m.Pools[0].Used != 2 {
		t.Fatalf("pool: %+v", m.Pools)
	}
	first := m.Pools[0].Blocks[0]
	// ::1000 is leased, ::1001 leased and reserved; the reservation wins
	if first.First != "fd00:db8:1::1000" || first.Last != "fd00:db8:1::10ef" || first.State != "reserved" || len(first.Used) != 2 || first.Used[1].Name != "laptop-fixed" {
		t.Errorf("first block: %+v", first)
	}
	if last := m.Pools[0].Blocks[255]; last.Last != "fd00:db8:1::ffff" || last.State != "free" {
		t.Errorf("last block: %+v", last)
	}
	if len(m.Outside) != 1 || m.Outside[0].Address != "fd00:db8:1::10" || m.Outside[0].State != "reserved" {
		t.Errorf("outside: %+v", m.Outside)
	}
	pd := m.PDPools[0]
	if pd.Count != 256 || pd.Used != 1 || pd.Prefixes[1].Address != "fd00:db8:2:100::/56" || pd.Prefixes[1].State != "leased" || pd.Prefixes[1].Name != "router" {
		t.Errorf("prefix pool: %+v", pd.Prefixes[:2])
	}
}
