package kea

import (
	"slices"
	"testing"
)

func TestMergeOptionsByNameOrCode(t *testing.T) {
	for _, c := range []struct {
		name   string
		scopes [][]rawOption
		want   []Option
	}{
		{"code only in the subnet overrides a named global option",
			[][]rawOption{{{Name: "domain-name-servers", Code: 6, Data: "192.0.2.53"}}, nil, {{Code: 6, Data: "192.0.2.99"}}},
			[]Option{{"domain-name-servers", 6, "192.0.2.99", SourceSubnet}}},
		{"a name in the subnet overrides a global option given by code",
			[][]rawOption{{{Code: 6, Data: "192.0.2.53"}}, nil, {{Name: "domain-name-servers", Data: "192.0.2.99"}}},
			[]Option{{"domain-name-servers", 6, "192.0.2.99", SourceSubnet}}},
		{"the shared network sits between global and subnet",
			[][]rawOption{{{Name: "routers", Code: 3, Data: "192.0.2.1"}}, {{Name: "routers", Code: 3, Data: "192.0.2.2"}}, nil},
			[]Option{{"routers", 3, "192.0.2.2", SourceSharedNetwork}}},
		{"different options stay apart, each with its level",
			[][]rawOption{{{Name: "domain-name", Code: 15, Data: "lan"}}, nil, {{Name: "routers", Code: 3, Data: "192.0.2.1"}}},
			[]Option{{"domain-name", 15, "lan", SourceGlobal}, {"routers", 3, "192.0.2.1", SourceSubnet}}},
		{"other spaces are left out",
			[][]rawOption{{{Name: "vendor-x", Code: 2, Space: "vendor-4491", Data: "x"}}, nil, nil},
			[]Option{}},
	} {
		if got := mergeSourced("dhcp4", subnetLevels, c.scopes...); !slices.Equal(got, c.want) {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
	// without levels, as for class options, the source stays empty
	if got := mergeOptionsIn("dhcp4", []rawOption{{Name: "routers", Code: 3, Data: "192.0.2.1"}}); len(got) != 1 || got[0].Source != "" {
		t.Errorf("%+v", got)
	}
}
