package guard

import (
	"regexp"
	"strings"
	"testing"
)

func TestCompressLogsLosslessRemovesOnlyLayout(t *testing.T) {
	in := "\x1b[32m" + strings.ReplaceAll(serviceLog(300, 3), "INFO", "INFO\x1b[0m")
	out, omitted, ok := compressLogs(in, false)
	if !ok || omitted != 0 {
		t.Fatalf("ok %v omitted %d", ok, omitted)
	}
	want := strings.Split(strings.TrimRight(ansiRe.ReplaceAllString(in, ""), "\n"), "\n")
	got := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(got) != len(want) {
		t.Fatalf("line count %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != strings.TrimRight(want[i], " \t") {
			t.Fatalf("line %d: %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCompressLogsShortenKeepsErrorsAndMarksCuts(t *testing.T) {
	in := serviceLog(3000, 12)
	out, omitted, ok := compressLogs(in, true)
	if !ok {
		t.Fatal("not detected")
	}
	if r := float64(len(out)) / float64(len(in)); r > 0.1 {
		t.Errorf("ratio %.3f, want at most 0.10 (90%% repeated lines)", r)
	}
	if n := strings.Count(out, "ERROR request failed"); n != 12 {
		t.Errorf("%d ERROR lines kept, want 12", n)
	}
	if n := strings.Count(out, "main.go:88"); n != 12 {
		t.Errorf("%d stack lines kept, want 12", n)
	}
	if n := strings.Count(out, "[ambit: "); n != omitted || n == 0 {
		t.Errorf("%d markers, omitted %d", n, omitted)
	}
	for _, id := range regexp.MustCompile(`id=(\d+)`).FindAllString(in, -1) {
		if !strings.Contains(out, id) {
			t.Errorf("lost %s", id)
		}
	}
	first := strings.SplitN(in, "\n", 2)[0]
	if !strings.HasPrefix(out, first) {
		t.Error("first line lost")
	}
}

func TestCompressLogsBelowDetectionIsUnknown(t *testing.T) {
	if _, _, ok := compressLogs(serviceLog(10, 0), true); ok {
		t.Error("10 lines must not be treated as a log")
	}
	if _, _, ok := compressLogs(strings.Repeat("some prose without a time\n", 50), true); ok {
		t.Error("prose is not a log")
	}
}

func FuzzCompressLogs(f *testing.F) {
	f.Add(serviceLog(40, 1))
	f.Fuzz(func(t *testing.T, in string) {
		if out, _, ok := compressLogs(in, true); ok && len(out) > len(in)+64*len(strings.Split(in, "\n")) {
			t.Fatal("grew a lot")
		}
	})
}
