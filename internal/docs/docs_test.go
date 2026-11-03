package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const root = "../.."

var (
	heading   = regexp.MustCompile(`(?m)^#+\s+(.+)$`)
	link      = regexp.MustCompile(`\]\(([^)\s#]*)(#[^)\s]+)?\)`)
	notAnchor = regexp.MustCompile(`[^\w\- ]`)
)

var fence = regexp.MustCompile("(?s)```.*?```")

// prose is a page without its code blocks, which hold examples, not links.
func prose(text string) string { return fence.ReplaceAllString(text, "") }

// An open code block would hide the rest of its page from the checks.
func TestCodeBlocksAreClosed(t *testing.T) {
	for _, page := range pages(t) {
		if n := strings.Count(read(t, page), "```"); n%2 != 0 {
			t.Errorf("%s has %d fences, one is not closed", page, n)
		}
	}
}

func read(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// pages are the README and every page under docs.
func pages(t *testing.T) []string {
	t.Helper()
	list, err := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	out := []string{"README.md"}
	for _, p := range list {
		out = append(out, filepath.Join("docs", filepath.Base(p)))
	}
	return out
}

// anchors are the ids GitHub gives the headings of a page.
func anchors(text string) map[string]bool {
	out := map[string]bool{}
	for _, m := range heading.FindAllStringSubmatch(text, -1) {
		out[strings.ReplaceAll(notAnchor.ReplaceAllString(strings.ToLower(strings.TrimSpace(m[1])), ""), " ", "-")] = true
	}
	return out
}

func TestLinksAndScreenshots(t *testing.T) {
	used := map[string]bool{}
	for _, page := range pages(t) {
		text := prose(read(t, page))
		for _, m := range link.FindAllStringSubmatch(text, -1) {
			target, anchor := m[1], strings.TrimPrefix(m[2], "#")
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			path := page
			if target != "" {
				path = filepath.Join(filepath.Dir(page), target)
			}
			if _, err := os.Stat(filepath.Join(root, path)); err != nil {
				t.Errorf("%s links to %s, which does not exist", page, target)
				continue
			}
			if strings.HasPrefix(filepath.Base(filepath.Dir(path)), "screenshots") {
				used[filepath.Base(path)] = true
			}
			if anchor != "" && strings.HasSuffix(path, ".md") && !anchors(prose(read(t, path)))[anchor] {
				t.Errorf("%s links to %s#%s, which has no such heading", page, path, anchor)
			}
		}
	}
	shots, _ := filepath.Glob(filepath.Join(root, "docs", "screenshots", "*.png"))
	for _, s := range shots {
		if !used[filepath.Base(s)] {
			t.Errorf("screenshot %s is not shown anywhere", filepath.Base(s))
		}
	}
}

// Every setting the program reads is in the table of docs/configuration.md.
func TestSettingsAreDocumented(t *testing.T) {
	source := read(t, "internal/config/config.go") + read(t, "cmd/kea-roost/main.go")
	table := read(t, "docs/configuration.md")
	read := regexp.MustCompile(`(?:getenv|Getenv)(?:, )?\(?"([A-Z0-9_]+)"`).FindAllStringSubmatch(source, -1)
	if len(read) < 15 {
		t.Fatalf("only %d settings found, the pattern no longer matches config.go", len(read))
	}
	for _, m := range read {
		if !strings.Contains(table, "`"+m[1]+"`") {
			t.Errorf("%s is read by the program but not in docs/configuration.md", m[1])
		}
	}
}

// Every rule of the review is listed in docs/diagnostics.md.
func TestReviewRulesAreDocumented(t *testing.T) {
	doc := strings.ToLower(read(t, "docs/diagnostics.md"))
	for _, file := range []string{"internal/manager/networkreview.go", "internal/manager/review6.go"} {
		titles := regexp.MustCompile(`Title: "(?:IPv6: )?([^"]+)"`).FindAllStringSubmatch(read(t, file), -1)
		if len(titles) < 5 {
			t.Fatalf("only %d rules found in %s", len(titles), file)
		}
		for _, m := range titles {
			if !strings.Contains(doc, "| "+strings.ToLower(m[1])+" |") {
				t.Errorf("rule %q of %s is not in docs/diagnostics.md", m[1], file)
			}
		}
	}
}

// The same names stay in the README table of the documentation pages.
func TestEveryPageIsListed(t *testing.T) {
	readme := read(t, "README.md")
	for _, page := range pages(t)[1:] {
		if !strings.Contains(readme, "]("+filepath.ToSlash(page)+")") {
			t.Errorf("%s is not linked from the README", page)
		}
	}
}
