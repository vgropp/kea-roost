// Command oui-update refreshes the embedded vendor table from the IEEE data
// Wireshark publishes. That file changes weekly, so do not fetch it more often.
// Run it from the repository root: go run ./cmd/oui-update
package main

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"log"
	"net/http"
	"os"
	"slices"
	"strings"
)

const (
	source = "https://www.wireshark.org/download/automated/data/manuf"
	target = "internal/oui/oui.tsv.gz"
)

func main() {
	resp, err := http.Get(source)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("%s: HTTP %d", source, resp.StatusCode)
	}
	var lines []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		// the columns are block, short name and full name; only 24-bit blocks count
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) < 3 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		prefix := strings.ReplaceAll(strings.TrimSpace(fields[0]), ":", "")
		vendor := strings.Join(strings.Fields(fields[2]), " ")
		if len(prefix) == 6 && vendor != "" {
			lines = append(lines, strings.ToUpper(prefix)+"\t"+vendor+"\n")
		}
	}
	if err := scanner.Err(); err != nil {
		log.Fatal(err)
	}
	slices.Sort(lines)

	f, err := os.Create(target)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	gz, _ := gzip.NewWriterLevel(f, gzip.BestCompression)
	for _, line := range lines {
		if _, err := gz.Write([]byte(line)); err != nil {
			log.Fatal(err)
		}
	}
	if err := gz.Close(); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d vendors written to %s\n", len(lines), target)
}
