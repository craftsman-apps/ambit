package guard

import (
	"fmt"
	"strings"
	"testing"
)

// prose is ordinary English, as a wiki page or a document a tool returns. The words that wake a rule ("system", "note",
// "message", "ignore", "user") are there at a natural density: a few in each kilobyte.
func prose(n int) string {
	para := "Release planning for the platform team starts on Monday. The agenda covers the open tickets, the staffing plan and the " +
		"budget for the next quarter. Each owner brings a short status message, and the group reviews what changed since the last " +
		"meeting. Note that the notes are kept in the shared folder, and the person who takes them sends a summary the same day. " +
		"Questions about the schedule go to the project office. The user guide for the new portal is ready, and the system that " +
		"hosts it was moved to the new data centre last month. Please ignore the old drafts; the current version replaces them.\n"
	return strings.Repeat(para, max(1, n/len(para)))
}

// denseProse is the worst case for the word check: every rule that works on the words of prose finds a word to read
// around, in each 400 bytes, and no rule finds a match. A result like this is unusual.
func denseProse(n int) string {
	para := "The user asked the system for a status message. Note that the assistant service is important to the project: " +
		"we ignore the cache when the model is rebuilt, and the system: prompt of the shell stays the same. Do not tell anyone " +
		"about the party. From now on the build uses the new instructions page, and the agent reads the developer guide.\n"
	return strings.Repeat(para, max(1, n/len(para)))
}

var injectionBenchTypes = []struct {
	name string
	gen  func(n int) string
}{
	{"json", queryRows}, {"logs", func(n int) string { return serviceLog(n, n/250) }}, {"markdown", markdownTable}, {"yaml", podYAML},
	{"prose", prose}, {"dense prose", denseProse},
}

// BenchmarkInjectionOnTopOfMasking compares the whole path of a result (parse, mask, encode) with and without the
// injection guard, at 100 KiB: the built-in rules alone, and with one pattern of your own (which reads the whole text).
// The extra time is the cost of the guard (docs/concepts.md, Injection guard).
func BenchmarkInjectionOnTopOfMasking(b *testing.B) {
	for _, tc := range injectionBenchTypes {
		in := textResult(sized(tc.gen, 100<<10))
		for _, name := range []string{"masking only", "built-in rules", "built-in rules and one pattern"} {
			b.Run(fmt.Sprintf("%s/%s", tc.name, name), func(b *testing.B) {
				g := &Guard{Masker: Builtin{}}
				switch name {
				case "built-in rules":
					g.Injection = mustInjector(b, "audit", "")
				case "built-in rules and one pattern":
					g.Injection = mustInjector(b, "audit", `,"patterns":["(?i)send the (data|file) to https?://"]`)
				}
				b.SetBytes(int64(len(in)))
				for range b.N {
					call(b, g, append([]byte(nil), in...))
				}
			})
		}
	}
}

func mustInjector(tb testing.TB, mode, extra string) *Injector {
	tb.Helper()
	in, err := LoadInjector(`{"mode":"`+mode+`","builtins":true`+extra+`}`, nil)
	if err != nil {
		tb.Fatal(err)
	}
	return in
}

// BenchmarkInjectionScan is the one pass that finds the words of the built-in rules, for 100 KiB of JSON rows.
func BenchmarkInjectionScan(b *testing.B) {
	text := sized(queryRows, 100<<10)
	in := mustInjector(b, "audit", "")
	b.SetBytes(int64(len(text)))
	for range b.N {
		in.all.scanner.scan(text)
	}
}
