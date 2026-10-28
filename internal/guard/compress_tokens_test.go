//go:build tokencheck

package guard

import (
	"strings"
	"testing"

	"github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
)

// Run with: go test -tags tokencheck -run TestTokenRatios -v ./internal/guard
// It counts tokens of every fixture before and after compression, with o200k_base (offline: the vocabulary is
// embedded in the loader module). It is not Claude's tokenizer, so only the ratios are checked, with a 10-point
// tolerance. The build tag keeps the dependency out of the normal tests and out of the ambit-guard binary.
func TestTokenRatios(t *testing.T) {
	tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
	enc, err := tiktoken.GetEncoding("o200k_base")
	if err != nil {
		t.Fatal(err)
	}
	count := func(s string) int { return len(enc.Encode(s, nil, nil)) }

	type fx struct{ typ, text string }
	fixtures := []fx{
		{"json", issueList(100)}, {"json", queryRows(2000)},
		{"table", markdownTable(800)}, {"table", csvExport(2000)},
		{"yaml", podYAML(300)},
		{"logs", serviceLog(3000, 12)},
		{"test", goTestVerbose(400, 2)}, {"test", strings.Repeat(pytestRun, 1)},
	}
	// target[mode][type] is the largest allowed tokens_after/tokens_before, from the design (section 1) with the
	// 10-point tolerance. JSON and tables keep every value, so only layout tokens go; lossless has no
	// target for YAML and test output (layout only, and kubectl output has none to remove).
	target := map[string]map[string]float64{
		ModeLossless: {"json": 0.95, "table": 0.95},
		ModeShorten:  {"json": 0.95, "table": 0.95, "yaml": 0.7, "logs": 0.4, "test": 0.4},
	}
	for _, mode := range []string{ModeLossless, ModeShorten} {
		before, after := map[string]int{}, map[string]int{}
		for _, f := range fixtures {
			if f.typ == "test" && len(f.text) < 8192 {
				continue // below minBytes, never compressed
			}
			out, typ, _, outcome := compressText(t.Context(), f.text, mode, true)
			if outcome != outCompressed {
				out, typ = f.text, f.typ
			}
			if typ != f.typ && outcome == outCompressed {
				t.Fatalf("%s detected as %s", f.typ, typ)
			}
			before[f.typ] += count(f.text)
			after[f.typ] += count(out)
		}
		for typ, b := range before {
			r := float64(after[typ]) / float64(b)
			t.Logf("%-9s %-5s tokens %7d -> %7d  ratio %.3f", mode, typ, b, after[typ], r)
			if max, ok := target[mode][typ]; ok && r > max {
				t.Errorf("%s/%s: ratio %.3f, want at most %.2f", mode, typ, r, max)
			}
		}
	}
}
