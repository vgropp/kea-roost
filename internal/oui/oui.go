// Package oui finds the vendor of a MAC address in an embedded copy of the IEEE data.
// Run cmd/oui-update to refresh oui.tsv.gz.
package oui

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
)

// Private is reported for locally administered addresses, which carry no vendor:
// the randomized addresses of phones, but also bridges, VMs and containers.
const Private = "Private MAC (locally assigned, e.g. a phone, bridge or VM)"

//go:embed oui.tsv.gz
var data []byte

var vendors = sync.OnceValue(func() map[string]string {
	table := map[string]string{}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		slog.Error("read OUI table", "error", err)
		return table
	}
	scanner := bufio.NewScanner(gz)
	for scanner.Scan() {
		if prefix, vendor, ok := strings.Cut(scanner.Text(), "\t"); ok {
			table[prefix] = vendor
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Error("read OUI table", "error", err)
	}
	return table
})

// Lookup returns the vendor of a MAC address, Private for a locally administered
// one, or "" if it is unknown.
func Lookup(mac string) string {
	digits := strings.ToUpper(strings.NewReplacer(":", "", "-", "", ".", "").Replace(mac))
	if len(digits) < 6 {
		return ""
	}
	first, err := hex.DecodeString(digits[:2])
	if err != nil {
		return ""
	}
	if first[0]&0x02 != 0 {
		return Private
	}
	return vendors()[digits[:6]]
}
