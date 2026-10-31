package oui

import "testing"

func TestLookup(t *testing.T) {
	cases := map[string]string{
		"00:00:0c:12:34:56": "Cisco Systems, Inc",
		"00-00-0C-12-34-56": "Cisco Systems, Inc",
		"02:40:cd:f8:92:2e": Private,
		"06:00:00:aa:01:64": Private,
		"ee:00:00:00:00:00": Private,
		"":                  "",
		"zz:zz":             "",
		"zz:00:00:00:00:00": "",
	}
	for mac, want := range cases {
		if got := Lookup(mac); got != want {
			t.Errorf("%q: got %q, want %q", mac, got, want)
		}
	}
}

func TestTableIsComplete(t *testing.T) {
	if n := len(vendors()); n < 30000 {
		t.Fatalf("only %d vendors", n)
	}
}
