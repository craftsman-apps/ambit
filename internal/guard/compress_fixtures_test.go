package guard

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
)

// Synthetic fixtures for the compressor tests. Deterministic (fixed seed), generated in code instead of
// stored, no real data: only example.com addresses and made-up ids.

func rng() *rand.Rand { return rand.New(rand.NewSource(7)) }

// issueList is a pretty-printed issue tracker list with nested labels and users.
func issueList(n int) string {
	var issues []map[string]any
	for i := 1; i <= n; i++ {
		issues = append(issues, map[string]any{
			"id": 10000 + i, "number": i, "title": fmt.Sprintf("Fix flaky test %d", i), "state": "open",
			"user":   map[string]any{"login": fmt.Sprintf("dev%d", i%7), "id": 500 + i%7, "type": "User"},
			"labels": []any{map[string]any{"name": "bug", "color": "d73a4a"}, map[string]any{"name": "area/api", "color": "0e8a16"}},
			"body":   "Steps:\n1. run the suite\n2. see the failure \"timeout\"",
			"score":  12345678901234567890.123456789, "comments": i % 4,
		})
	}
	b, _ := json.MarshalIndent(issues, "", "    ")
	return string(b)
}

// queryRows is a database result: n rows, 12 columns, many nulls.
func queryRows(n int) string {
	r := rng()
	var rows []string
	for i := 0; i < n; i++ {
		cells := []string{fmt.Sprintf(`"id": %d`, i), fmt.Sprintf(`"name": "item-%d"`, r.Intn(900))}
		for c := 0; c < 10; c++ {
			if r.Intn(3) == 0 {
				cells = append(cells, fmt.Sprintf(`"col%d": null`, c))
			} else {
				cells = append(cells, fmt.Sprintf(`"col%d": %d.%d`, c, r.Intn(1000), r.Intn(100)))
			}
		}
		rows = append(rows, "  {\n    "+strings.Join(cells, ",\n    ")+"\n  }")
	}
	return "[\n" + strings.Join(rows, ",\n") + "\n]"
}

// serviceLog is a service log: mostly repeated INFO lines, a few ERROR lines with a stack trace.
func serviceLog(n, errors int) string {
	var b strings.Builder
	every := n / (errors + 1)
	for i := 0; i < n; i++ {
		ts := fmt.Sprintf("2026-03-01T10:%02d:%02d.%03dZ", i/3600%60, i/60%60, i%1000)
		switch {
		case i > 0 && i%every == 0 && errors > 0:
			errors--
			fmt.Fprintf(&b, "%s ERROR request failed: upstream timeout id=%d\n", ts, i)
			b.WriteString("goroutine 41 [running]:\nmain.handle(0xc000123456)\n\t/app/main.go:88 +0x1d\n")
		case i%10 == 3:
			fmt.Fprintf(&b, "%s INFO served request path=/api/items/%d status=200  \n", ts, i%5)
		default:
			fmt.Fprintf(&b, "%s INFO health check ok\n", ts)
		}
	}
	return b.String()
}

// goTestVerbose is `go test -v` output with pass passing and fail failing tests.
func goTestVerbose(pass, fail int) string {
	var b strings.Builder
	for i := 0; i < pass; i++ {
		fmt.Fprintf(&b, "=== RUN   TestCase%d\n--- PASS: TestCase%d (0.01s)\n", i, i)
	}
	for i := 0; i < fail; i++ {
		fmt.Fprintf(&b, "=== RUN   TestBroken%d\n    broken_test.go:%d: got 3, want 4\n--- FAIL: TestBroken%d (0.00s)\n", i, 10+i, i)
	}
	b.WriteString("FAIL\nFAIL\texample.com/app/pkg\t0.412s\n")
	return b.String()
}

func csvExport(n int) string {
	var b strings.Builder
	b.WriteString("id,name,note,amount\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "%d,item-%d,\"hello, world\nsecond line\",%d.50\n", i, i, i*3)
	}
	return b.String()
}

func markdownTable(n int) string {
	var b strings.Builder
	b.WriteString("| ID        | Name           | Status     |\n|-----------|----------------|------------|\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "| %-9d | %-14s | %-10s |\n", i, fmt.Sprintf("service-%d", i), "healthy")
	}
	return b.String()
}

func podYAML(n int) string {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nitems:\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "- apiVersion: v1\n  kind: Pod\n  metadata:\n    name: web-%d\n    managedFields:\n    - apiVersion: v1\n      fieldsType: FieldsV1\n      fieldsV1:\n        f:metadata:\n          f:labels:\n            .: {}\n      manager: kubectl\n    namespace: default\n  spec:\n    containers:\n    - image: nginx\n      name: web\n", i)
	}
	b.WriteString("kind: List\n")
	return b.String()
}

const htmlPage = "<!doctype html>\n<html>\n  <head><title>x</title></head>\n  <body>\n    <p>Hello</p>\n  </body>\n</html>\n"
