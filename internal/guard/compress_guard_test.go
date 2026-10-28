package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/craftsman-apps/ambit/internal/extmcp"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type fakeCompression map[string]CompressionConfig

func (f fakeCompression) Compression(_ context.Context, s string) CompressionConfig { return f[s] }

func on(mode string) fakeCompression {
	return fakeCompression{"crm": {Mode: mode, MinBytes: 8192}}
}

func textResult(text string) []byte {
	b, _ := json.Marshal(map[string]any{"content": []map[string]any{{"type": "text", "text": text}}})
	return b
}

func call(t testing.TB, g *Guard, body []byte) *extmcp.McpResponseResult {
	t.Helper()
	res, err := g.CheckResponse(t.Context(), &extmcp.McpResponse{ServiceNames: []string{"crm"}, Method: "tools/call", McpResponse: body})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// textOf returns the text of the first content item of a Mutated result.
func textOf(t testing.TB, res *extmcp.McpResponseResult) string {
	t.Helper()
	var out struct{ Content []struct{ Text string } }
	if err := json.Unmarshal(res.GetMutated(), &out); err != nil || len(out.Content) == 0 {
		t.Fatalf("not a mutated result: %v %.200s", err, res.GetMutated())
	}
	return out.Content[0].Text
}

// Off (no source, mode off, no field, a Server that does not exist) gives the same result as a guard that
// has no compression: Pass where it passed, the same bytes where it mutated.
func TestOffIsByteIdenticalToToday(t *testing.T) {
	big := strings.Repeat("a line of text\n", 1000)
	inputs := map[string][]byte{
		"nothing to mask":    textResult(big),
		"email in text":      textResult(big + "mail me: kim@acme.example"),
		"structuredContent":  []byte(`{"content":[{"type":"text","text":"ok"}],"structuredContent":{"mail":"kim@acme.example"}}`),
		"image and big text": []byte(`{"content":[{"type":"image","mimeType":"image/png","data":"AAAA"},{"type":"text","text":` + string(mustJSON(big)) + `}]}`),
	}
	for name, in := range inputs {
		today := call(t, &Guard{Masker: Builtin{}}, in)
		for gname, g := range map[string]*Guard{
			"nil source": {Masker: Builtin{}},
			"mode off":   {Masker: Builtin{}, Compression: fakeCompression{"crm": {Mode: ModeOff}}},
			"no field":   {Masker: Builtin{}, Compression: fakeCompression{}},
		} {
			got := call(t, g, in)
			if (today.GetPass() != nil) != (got.GetPass() != nil) || !bytes.Equal(today.GetMutated(), got.GetMutated()) {
				t.Errorf("%s / %s: differs from today", name, gname)
			}
		}
	}
	if call(t, &Guard{Masker: Builtin{}}, inputs["nothing to mask"]).GetPass() == nil {
		t.Error("a result with nothing to mask must stay Pass")
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestModeOnWithNothingToChangeStaysPass(t *testing.T) {
	g := &Guard{Masker: Builtin{}, Compression: on(ModeShorten)}
	for name, in := range map[string][]byte{
		"prose":    textResult(strings.Repeat("The quick brown fox jumps over the lazy dog. ", 400)),
		"html":     textResult(strings.Repeat(htmlPage, 100)),
		"small":    textResult(serviceLog(30, 1)),
		"no table": textResult("{}"),
	} {
		if call(t, g, in).GetPass() == nil {
			t.Errorf("%s: want Pass", name)
		}
	}
}

func TestCompressedThenMasked(t *testing.T) {
	g := &Guard{Masker: Builtin{}, Compression: on(ModeLossless)}
	var rows []map[string]any
	for i := 0; i < 200; i++ {
		rows = append(rows, map[string]any{"id": i, "email": fmt.Sprintf("user%d@example.com", i), "card": "4111 1111 1111 1111", "note": "regular customer"})
	}
	pretty, _ := json.MarshalIndent(rows, "", "    ")
	res := call(t, g, textResult(string(pretty)))
	out := textOf(t, res)
	if len(out) >= len(pretty)/2 {
		t.Errorf("not smaller: %d -> %d", len(pretty), len(out))
	}
	if strings.Contains(out, "@example.com") || strings.Contains(out, "4111") {
		t.Errorf("unmasked data left: %.300s", out)
	}
	if strings.Count(out, "<EMAIL>") != 200 || strings.Count(out, "<CARD_NUMBER>") != 200 {
		t.Errorf("masks: %d emails, %d cards", strings.Count(out, "<EMAIL>"), strings.Count(out, "<CARD_NUMBER>"))
	}
}

type countingMasker struct{ bytes int }

func (c *countingMasker) Mask(ctx context.Context, s string) (string, int, error) {
	c.bytes += len(s)
	return Builtin{}.Mask(ctx, s)
}

func TestMaskerSeesTheCompressedText(t *testing.T) {
	in := textResult(issueList(100))
	without, with := &countingMasker{}, &countingMasker{}
	call(t, &Guard{Masker: without}, in)
	call(t, &Guard{Masker: with, Compression: on(ModeLossless)}, in)
	if with.bytes == 0 || with.bytes >= without.bytes {
		t.Errorf("masker input: %d with compression, %d without", with.bytes, without.bytes)
	}
}

// Property: for random mixes of personal data and filler in every compressible shape, no raw item survives
// compress then mask, and every raw item still present in the compressed text is replaced by a mask.
func TestNoPersonalDataSurvivesCompressThenMask(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	secret := func() string {
		switch r.Intn(4) {
		case 0:
			return fmt.Sprintf("%06d-%d%06d", r.Intn(1000000), 1+r.Intn(4), r.Intn(1000000))
		case 1:
			return fmt.Sprintf("%03d-%02d-%04d", r.Intn(1000), r.Intn(100), r.Intn(10000))
		case 2:
			return "4111 1111 1111 1111"
		}
		return fmt.Sprintf("u%d@example.com", r.Intn(1000))
	}
	for round := 0; round < 40; round++ {
		var items []string
		put := func() string { s := secret(); items = append(items, s); return s }
		var rows []string
		for i := 0; i < 80; i++ {
			rows = append(rows, fmt.Sprintf("  {\n    \"id\": %d,\n    \"v\": \"%s\",\n    \"w\": \"filler %d\"\n  }", i, put(), r.Intn(9)))
		}
		var logs strings.Builder
		for i := 0; i < 80; i++ {
			fmt.Fprintf(&logs, "2026-03-01T10:00:%02dZ INFO user %s did a thing  \n", i%60, put())
		}
		var csvText strings.Builder
		csvText.WriteString("id,who,note\n")
		for i := 0; i < 80; i++ {
			fmt.Fprintf(&csvText, "%d,%s,\"a, b\"\n", i, put())
		}
		for name, text := range map[string]string{"json": "[\n" + strings.Join(rows, ",\n") + "\n]", "logs": logs.String(), "csv": csvText.String()} {
			for _, mode := range []string{ModeLossless, ModeShorten} {
				g := &Guard{Masker: Builtin{}, Compression: on(mode)}
				compressed, _, _, _ := compressText(t.Context(), text, mode, true)
				res := call(t, g, textResult(text))
				final := textOf(t, res)
				for _, s := range items {
					if strings.Contains(final, s) {
						t.Fatalf("%s/%s: %q left unmasked", name, mode, s)
					}
				}
				_, n, _ := Builtin{}.Mask(t.Context(), compressed)
				_, nFinal, _ := Builtin{}.Mask(t.Context(), text)
				if n > nFinal {
					t.Fatalf("%s/%s: compression created %d masks from %d", name, mode, n, nFinal)
				}
			}
		}
	}
}

func TestPresidioSetTurnsColumnarOff(t *testing.T) {
	f := newFakeAnalyzer(t, map[string]string{})
	in := textResult(queryRows(300))
	cols := func(m Masker) bool {
		out := textOf(t, call(t, &Guard{Masker: m, Compression: on(ModeLossless)}, in))
		return strings.Contains(out, `"columns"`)
	}
	if !cols(Builtin{}) {
		t.Error("columnar form expected with the built-in masks")
	}
	if cols(BuiltinThenPresidio{Presidio: &Presidio{AnalyzerURL: f.srv.URL}}) {
		t.Error("columnar form must be off while Presidio is set")
	}
}

func TestLimitsOfCompression(t *testing.T) {
	g := &Guard{Masker: Builtin{}, Compression: fakeCompression{"crm": {Mode: ModeLossless, MinBytes: 8192}}}
	pad := func(n int) string { // pretty JSON array of exactly n bytes, mostly indentation
		s := "[" + strings.Repeat("\n      1,", (n-11)/9) + "\n      1\n]"
		return s + strings.Repeat(" ", n-len(s))
	}
	for size, want := range map[int]bool{8191: false, 8192: true} {
		in := pad(size)
		if len(in) != size {
			t.Fatalf("fixture is %d bytes", len(in))
		}
		before := testutil.ToFloat64(compressResults.WithLabelValues("crm", "json", outCompressed))
		call(t, g, textResult(in))
		got := testutil.ToFloat64(compressResults.WithLabelValues("crm", "json", outCompressed)) > before
		if got != want {
			t.Errorf("%d bytes: compressed %v, want %v", size, got, want)
		}
	}
	// isError and images are left alone.
	big := string(mustJSON(map[string]any{"a": strings.Repeat("x", 9000)}))
	pretty := "{\n  \"a\": " + big[5:len(big)-1] + "\n}"
	if r := call(t, g, []byte(`{"isError":true,"content":[{"type":"text","text":`+string(mustJSON(pretty))+`}]}`)); r.GetPass() == nil {
		t.Error("isError result must pass unchanged")
	}
	if r := call(t, g, []byte(`{"content":[{"type":"image","mimeType":"image/png","data":"`+strings.Repeat("A", 9000)+`"}]}`)); r.GetPass() == nil {
		t.Error("image must pass unchanged")
	}
	// structuredContent is masked as today, never compressed.
	sc := `{"content":[{"type":"text","text":"ok"}],"structuredContent":{"rows":` + pretty + `}}`
	if r := call(t, g, []byte(sc)); r.GetPass() == nil {
		t.Error("structuredContent must not be compressed")
	}
	// A text above the cap passes uncompressed, and is still masked.
	huge := "{\n  \"a\": \"" + strings.Repeat("x", maxCompressBytes) + " kim@acme.example\"\n}"
	out := textOf(t, call(t, g, textResult(huge)))
	if len(out) < maxCompressBytes || strings.Contains(out, "kim@acme") || !strings.Contains(out, "<EMAIL>") || !strings.Contains(out, "{\n  \"a\"") {
		t.Errorf("too-large text: len %d", len(out))
	}
}

func withDetect(t *testing.T, f func(string, bool, bool) (string, string, int)) {
	old := detectText
	detectText = f
	t.Cleanup(func() { detectText = old })
}

func TestCompressorFaultsPassTheOriginal(t *testing.T) {
	text := "{\n  \"a\": \"" + strings.Repeat("padding ", 1500) + " kim@acme.example\"\n}"
	for name, f := range map[string]func(string, bool, bool) (string, string, int){
		"panic":   func(string, bool, bool) (string, string, int) { panic("boom") },
		"garbage": func(s string, _, _ bool) (string, string, int) { return s + s, "json", 0 },
		"slow": func(s string, _, _ bool) (string, string, int) {
			time.Sleep(300 * time.Millisecond)
			return "x", "json", 0
		},
	} {
		t.Run(name, func(t *testing.T) {
			withDetect(t, f)
			g := &Guard{Masker: Builtin{}, Compression: on(ModeShorten)}
			out := textOf(t, call(t, g, textResult(text))) // masked as today: the call succeeds
			want, _, _ := Builtin{}.Mask(t.Context(), text)
			if out != want {
				t.Errorf("original text not used (len %d, want %d)", len(out), len(want))
			}
		})
	}
	time.Sleep(300 * time.Millisecond) // let the abandoned slow compressor end and free its slot
}

func TestMaskingFailureStillWithholdsAfterCompression(t *testing.T) {
	g := &Guard{Masker: brokenMasker{}, Compression: on(ModeLossless)}
	if res := call(t, g, textResult(issueList(100))); res.GetError() == nil {
		t.Fatalf("want the result withheld: %v", res)
	}
}

func TestBusyCompressorPassesUncompressed(t *testing.T) {
	for range maxCompressing {
		compressing <- struct{}{}
	}
	defer func() {
		for range maxCompressing {
			<-compressing
		}
	}()
	g := &Guard{Masker: Builtin{}, Compression: on(ModeLossless)}
	if call(t, g, textResult(issueList(100))).GetPass() == nil {
		t.Error("a busy compressor must leave the text as it is")
	}
	if testutil.ToFloat64(compressResults.WithLabelValues("crm", "unknown", outBusy)) == 0 {
		t.Error("skipped_busy not counted")
	}
}

func TestShortenHintHoldsOnlySizes(t *testing.T) {
	g := &Guard{Masker: Builtin{}, Compression: on(ModeShorten)}
	out := textOf(t, call(t, g, textResult(goTestVerbose(2000, 1))))
	if !strings.Contains(out, "[ambit: 2000 passing tests omitted here]") || !strings.Contains(out, "call the tool again with a narrower query") {
		t.Errorf("marker or hint missing:\n%s", out)
	}
	if call(t, &Guard{Masker: Builtin{}, Compression: on(ModeLossless)}, textResult(goTestVerbose(2000, 1))).GetPass() == nil {
		t.Error("lossless has nothing to remove from plain go test output: want Pass, no markers")
	}
}

// B/op of the compress step: at most 8 times the input.
func TestCompressMemoryBound(t *testing.T) {
	for name, text := range map[string]string{
		"json": queryRows(2500), "logs": serviceLog(8000, 12), "test": goTestVerbose(6000, 2),
		"csv": csvExport(8000), "markdown": markdownTable(6000), "yaml": podYAML(1200),
	} {
		for _, mode := range []string{ModeLossless, ModeShorten} {
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			detect(text, mode == ModeShorten, true)
			runtime.ReadMemStats(&after)
			if got := after.TotalAlloc - before.TotalAlloc; got > 8*uint64(len(text)) {
				t.Errorf("%s/%s: allocated %d bytes for %d bytes of input (%.1f times)", name, mode, got, len(text), float64(got)/float64(len(text)))
			}
		}
	}
}
