// Command fake-kea runs the test double of the Kea control socket with sample
// data, for working on the interface without a real server.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/vgropp/kea-roost/internal/keatest"
)

func main() {
	addr := flag.String("listen", "127.0.0.1:8000", "listen address")
	flag.Parse()

	s := keatest.New()
	now := time.Now().Unix()
	s.Hosts = []map[string]any{
		{"subnet-id": 1, "hw-address": "aa:bb:cc:00:00:01", "ip-address": "192.0.2.10", "hostname": "nas", "user-context": map[string]any{"comment": "basement"}},
		{"subnet-id": 1, "hw-address": "aa:bb:cc:00:00:02", "ip-address": "192.0.2.150", "hostname": "printer", "option-data": []any{map[string]any{"name": "ntp-servers", "data": "192.0.2.1"}}},
	}
	for i := range 42 {
		lease := map[string]any{
			"ip-address": fmt.Sprintf("192.0.2.%d", 100+i),
			"hw-address": fmt.Sprintf("de:ad:be:ef:00:%02x", i),
			"client-id":  "",
			"hostname":   fmt.Sprintf("client-%02d", i),
			"subnet-id":  1,
			"state":      0,
			"cltt":       now - int64(i*200),
			"valid-lft":  4000,
		}
		s.Leases = append(s.Leases, lease)
	}
	s.Leases = append(s.Leases, map[string]any{
		"ip-address": "192.0.2.10", "hw-address": "aa:bb:cc:00:00:01", "hostname": "nas",
		"subnet-id": 1, "state": 0, "cltt": now, "valid-lft": 4000,
	})
	log.Printf("fake kea on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, s))
}
