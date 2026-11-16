package manager

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/vgropp/kea-roost/internal/kea"
)

func TestEvalTest(t *testing.T) {
	member := func(name string) truth {
		switch name {
		case "in":
			return yes
		case "out":
			return no
		}
		return unknown
	}
	cases := map[string]truth{
		"member('in')":                           yes,
		"not member('in')":                       no,
		"not member('out')":                      yes,
		"member('in') and not member('out')":     yes,
		"member('out') or (member('in'))":        yes,
		"not (member('out') or member('out'))":   yes,
		"member('maybe')":                        unknown,
		"member('maybe') and member('out')":      no,
		"member('maybe') or member('in')":        yes,
		"not member('maybe')":                    unknown,
		"member ( 'in' )":                        unknown,
		"member('in') ==":                        unknown,
		"substring(option[60].hex,0,3) == 'abc'": unknown,
		"member('in') and":                       unknown,
		"(member('in')":                          unknown,
		"notmember('in')":                        unknown,
		"member('in' )":                          yes,
		"":                                       unknown,
		strings.Repeat("(", 100) + "member('in')" + strings.Repeat(")", 100): unknown,
		strings.Repeat("not ", 100) + "member('in')":                         unknown,
	}
	for test, want := range cases {
		if got := evalTest(test, member); got != want {
			t.Errorf("%q: got %v, want %v", test, got, want)
		}
	}
}

// The two ways to give most clients one set of DNS servers and some another.
const additionalConfig = `{"Dhcp4": {
  "reservations-global": true,
  "client-classes": [
    { "name": "direct-dns", "option-data": [ { "name": "domain-name-servers", "data": "192.0.2.2" } ] },
    { "name": "lan-dns", "test": "not member('direct-dns')", "only-in-additional-list": true,
      "option-data": [ { "name": "domain-name-servers", "data": "192.0.2.53" } ] },
    { "name": "vendor", "test": "substring(option[60].hex,0,3) == 'abc'", "only-if-required": true,
      "option-data": [ { "name": "domain-name", "data": "vendor.lan" } ] }
  ],
  "subnet4": [ {
    "id": 1, "subnet": "192.0.2.0/24",
    "pools": [ { "pool": "192.0.2.100 - 192.0.2.199" } ],
    "evaluate-additional-classes": [ "lan-dns" ],
    "option-data": [ { "name": "routers", "data": "192.0.2.1" } ]
  }, {
    "id": 2, "subnet": "198.51.100.0/24",
    "require-client-classes": [ "vendor" ]
  } ]
}}`

func TestExplainAdditionalClasses(t *testing.T) {
	lease := &LeaseView{IPAddress: "192.0.2.150", SubnetID: 1, Active: true}
	e := explainFor(t, additionalConfig, nil, lease, "")
	if o := option(t, e, "domain-name-servers"); o.Data != "192.0.2.53" || o.Source != "class lan-dns" {
		t.Errorf("pool client: got %+v", o)
	}
	if !slices.ContainsFunc(e.Steps, func(s ExplainStep) bool { return s.Title == "Classes" && strings.Contains(s.Detail, "lan-dns") }) {
		t.Errorf("got %+v", e.Steps)
	}

	e = explainFor(t, additionalConfig, host("192.0.2.10", "direct-dns"), nil, "")
	if o := option(t, e, "domain-name-servers"); o.Data != "192.0.2.2" || o.Source != "class direct-dns" {
		t.Errorf("reserved client: got %+v", o)
	}
	for _, text := range e.Unknown {
		if strings.Contains(text, "lan-dns") {
			t.Errorf("lan-dns is decided, got %v", e.Unknown)
		}
	}

	// a test on the packet stays open, and an additional class only counts where listed
	lease = &LeaseView{IPAddress: "198.51.100.20", SubnetID: 2, Active: true}
	e = explainFor(t, additionalConfig, nil, lease, "")
	if !slices.ContainsFunc(e.Unknown, func(s string) bool {
		return strings.Contains(s, "vendor") && strings.Contains(s, "evaluated for this subnet")
	}) {
		t.Errorf("got %v", e.Unknown)
	}
	if slices.ContainsFunc(e.Unknown, func(s string) bool { return strings.Contains(s, "lan-dns") }) {
		t.Errorf("lan-dns is not listed in subnet 2, got %v", e.Unknown)
	}
	if slices.ContainsFunc(e.Options, func(o EffectiveOption) bool { return o.Name == "domain-name-servers" }) {
		t.Errorf("got %+v", e.Options)
	}
}

func TestParseAdditionalClasses(t *testing.T) {
	raw := []byte(additionalConfig)
	classes, err := kea.ParseClasses(raw)
	if err != nil {
		t.Fatal(err)
	}
	if classes[0].Additional || !classes[1].Additional || !classes[2].Additional {
		t.Errorf("got %+v", classes)
	}
	subnets, err := kea.ParseSubnets(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(subnets[0].AdditionalClasses, []string{"lan-dns"}) || !slices.Equal(subnets[1].AdditionalClasses, []string{"vendor"}) {
		t.Errorf("got %+v", subnets)
	}
}

func (e *env) additionalClass(t *testing.T) {
	t.Helper()
	e.class(t)
	in := ClassInput{Name: "lan-dns", Test: new(" not member('direct-dns') "), Additional: new(true),
		Options: []ReservationOption{{Name: "domain-name-servers", Data: "192.0.2.53"}}}
	if res, err := e.mgr.CreateClass(context.Background(), "u", in); err != nil || res.Status != StatusOK {
		t.Fatalf("got %+v %v", res, err)
	}
}

func TestAdditionalClassLifecycle(t *testing.T) {
	e := setup(t)
	e.additionalClass(t)
	classes := e.fake.Config["client-classes"].([]any)
	entry := classes[1].(map[string]any)
	if entry["test"] != "not member('direct-dns')" || entry["only-in-additional-list"] != true {
		t.Fatalf("got %+v", entry)
	}

	in := subnetInput()
	in.DNSServers = nil
	list := []string{"lan-dns"}
	in.AdditionalClasses = &list
	res, err := e.mgr.UpdateSubnet(context.Background(), "u", 1, in)
	if err != nil || res.Status != StatusOK {
		t.Fatalf("got %+v %v", res, err)
	}
	raw := e.rawSubnet()
	if got := stringList(raw["evaluate-additional-classes"]); !slices.Equal(got, list) {
		t.Errorf("got %v", raw)
	}

	view, err := e.mgr.Classes(context.Background())
	if err != nil || !view.Classes[1].Additional || !slices.Equal(view.Classes[1].Subnets, []int64{1}) {
		t.Fatalf("got %+v %v", view, err)
	}
	subnets, err := e.mgr.Subnets(context.Background())
	if err != nil || len(subnets.Subnets[0].AdditionalClasses) != 1 || subnets.Subnets[0].AdditionalClasses[0].Options[0].Data != "192.0.2.53" {
		t.Fatalf("got %+v %v", subnets, err)
	}

	// listed by a subnet, so it stays
	_, err = e.mgr.DeleteClass(context.Background(), "u", "lan-dns")
	var v *ValidationError
	if !errors.As(err, &v) || !strings.Contains(v.Error(), "by 1 pools") {
		t.Fatalf("got %v", err)
	}

	// a request with only options keeps the test and the flag
	res, err = e.mgr.UpdateClass(context.Background(), "u", "lan-dns", ClassInput{Options: []ReservationOption{{Name: "domain-name-servers", Data: "192.0.2.54"}}})
	if err != nil || res.Status != StatusOK {
		t.Fatalf("got %+v %v", res, err)
	}
	entry = e.fake.Config["client-classes"].([]any)[1].(map[string]any)
	if entry["test"] != "not member('direct-dns')" || entry["only-in-additional-list"] != true {
		t.Errorf("got %+v", entry)
	}

	// clearing the flag and the test removes both keys
	res, err = e.mgr.UpdateClass(context.Background(), "u", "lan-dns", ClassInput{Test: new(""), Additional: new(false), Options: []ReservationOption{{Name: "domain-name-servers", Data: "192.0.2.53"}}})
	if err != nil || res.Status != StatusOK {
		t.Fatalf("got %+v %v", res, err)
	}
	entry = e.fake.Config["client-classes"].([]any)[1].(map[string]any)
	if _, ok := entry["test"]; ok {
		t.Errorf("got %+v", entry)
	}
	if _, ok := entry["only-in-additional-list"]; ok {
		t.Errorf("got %+v", entry)
	}

	// an empty list removes the key, and nil leaves it alone
	in.AdditionalClasses = &[]string{}
	if res, err = e.mgr.UpdateSubnet(context.Background(), "u", 1, in); err != nil || res.Status != StatusOK {
		t.Fatalf("got %+v %v", res, err)
	}
	if _, ok := e.rawSubnet()["evaluate-additional-classes"]; ok {
		t.Errorf("got %v", e.rawSubnet())
	}
}

func TestUpdateSubnetKeepsAdditionalClassesWithoutList(t *testing.T) {
	e := setup(t)
	e.additionalClass(t)
	e.rawSubnet()["require-client-classes"] = []any{"lan-dns"}
	in := subnetInput()
	in.DNSServers = nil
	if res, err := e.mgr.UpdateSubnet(context.Background(), "u", 1, in); err != nil || res.Status != StatusOK {
		t.Fatalf("got %+v %v", res, err)
	}
	if got := stringList(e.rawSubnet()["require-client-classes"]); !slices.Equal(got, []string{"lan-dns"}) {
		t.Errorf("got %v", e.rawSubnet())
	}

	// a change writes the Kea 3.0 name and drops the old one
	list := []string{"lan-dns", "direct-dns"}
	in.AdditionalClasses = &list
	if res, err := e.mgr.UpdateSubnet(context.Background(), "u", 1, in); err != nil || res.Status != StatusOK {
		t.Fatalf("got %+v %v", res, err)
	}
	raw := e.rawSubnet()
	if _, ok := raw["require-client-classes"]; ok || !slices.Equal(stringList(raw["evaluate-additional-classes"]), list) {
		t.Errorf("got %v", raw)
	}
}

func TestAdditionalClassChecks(t *testing.T) {
	e := setup(t)
	e.additionalClass(t)

	in := subnetInput()
	in.DNSServers = []string{"192.0.2.9"}
	list := []string{"lan-dns", "direct-dns"}
	in.AdditionalClasses = &list
	warnings, err := e.mgr.CheckSubnet(context.Background(), 1, in)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"wins over class lan-dns", "wins over class direct-dns", "direct-dns is not marked as additional"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %v", want, warnings)
		}
	}

	// without own DNS servers the class applies, so there is nothing to warn about
	in.DNSServers = nil
	list = []string{"lan-dns"}
	if warnings, err = e.mgr.CheckSubnet(context.Background(), 1, in); err != nil || len(warnings) != 0 {
		t.Errorf("got %v %v", warnings, err)
	}

	for _, bad := range [][]string{{"nothing"}, {"lan-dns", "lan-dns"}} {
		in.AdditionalClasses = &bad
		_, err := e.mgr.CheckSubnet(context.Background(), 1, in)
		var v *ValidationError
		if !errors.As(err, &v) {
			t.Errorf("%v: got %v", bad, err)
		}
	}
}

func TestClassTestValidation(t *testing.T) {
	for name, test := range map[string]string{
		"long":      strings.Repeat("a", maxTestLength+1),
		"multiline": "member('a')\nor member('b')",
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			_, err := e.mgr.CreateClass(context.Background(), "u", ClassInput{Name: "c", Test: &test, Options: []ReservationOption{{Name: "x", Data: "1"}}})
			var v *ValidationError
			if !errors.As(err, &v) {
				t.Fatalf("got %v", err)
			}
		})
	}
}
