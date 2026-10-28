package guard

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"
)

// Compression modes of Server.spec.resultCompression.
const (
	ModeOff      = "off"
	ModeLossless = "lossless"
	ModeShorten  = "shorten"
)

// DefaultMinBytes is the size below which a text is not compressed, when the Server does not say.
const DefaultMinBytes = 8192

const (
	// maxCompressBytes is the largest text the guard compresses. A larger one is masked and passed as it is.
	// Measured with the memory test (see the pull request); lower it if two results at once do not fit.
	maxCompressBytes = 4 << 20
	// maxCompressing is how many texts are compressed at the same time; another one passes uncompressed.
	maxCompressing = 2
	// minGain: the output must be at least this much (percent) smaller than the input, or the input is used.
	minGain = 10
)

// CompressionConfig is the result compression setting of one Server.
type CompressionConfig struct {
	Mode     string
	MinBytes int
}

// CompressionSource reads the setting of a server. A nil source, an unknown server or an unreadable field mean off.
type CompressionSource interface {
	Compression(ctx context.Context, server string) CompressionConfig
}

// Outcomes of one compression, the values of the "outcome" metric label.
const (
	outCompressed = "compressed"
	outSmall      = "skipped_small"
	outUnknown    = "skipped_unknown"
	outTooLarge   = "skipped_too_large"
	outBusy       = "skipped_busy"
	outNoGain     = "no_gain"
	outTimeout    = "timeout"
	outError      = "error"
)

// detectText is detect; tests replace it to inject faults.
var detectText = detect

var compressing = make(chan struct{}, maxCompressing)

// detect compresses text with the first compressor whose type matches. typ is "unknown" when none does.
// columnar is false while Presidio is set (it reads names better with their keys next to them).
func detect(text string, shorten, columnar bool) (out, typ string, omitted int) {
	if o, ok := compressJSON(text, columnar); ok {
		return o, "json", 0
	}
	if o, n, ok := compressLogs(text, shorten); ok {
		return o, "logs", n
	}
	if o, n, ok := compressTestOutput(text, shorten); ok {
		return o, "test", n
	}
	if o, ok := compressTable(text); ok {
		return o, "table", 0
	}
	if o, n, ok := compressYAML(text, shorten); ok {
		return o, "yaml", n
	}
	return "", "unknown", 0
}

// hint is the one fixed line shorten appends when it left content out. It holds sizes only.
func hint(before, after int, prefix string) string {
	return fmt.Sprintf("\n%s[ambit: this result was shortened from %s to %s. Parts were left out. If you need them, call the tool again with a narrower query (filters, a smaller range, a page number).]\n",
		prefix, kb(before), kb(after))
}

func kb(n int) string { return fmt.Sprintf("%d KB", (n+1023)/1024) }

// compressText returns the text to use and what happened. It never fails: a fault gives the original text.
// The compressors run in their own goroutine, so a slow one is abandoned after the deadline (its memory is
// held until it ends, and the semaphore counts it until then).
func compressText(ctx context.Context, text string, mode string, columnar bool) (out, typ string, omitted int, outcome string) {
	select {
	case compressing <- struct{}{}:
	default:
		return text, "unknown", 0, outBusy
	}
	type res struct {
		out, typ string
		omitted  int
		panicked bool
	}
	done := make(chan res, 1)
	go func() {
		defer func() { <-compressing }()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("guard: compressor panic; original used", "panic", fmt.Sprintf("%T", r), "stack", string(debug.Stack()))
				done <- res{panicked: true}
			}
		}()
		o, t, n := detectText(text, mode == ModeShorten, columnar)
		done <- res{out: o, typ: t, omitted: n}
	}()
	deadline := 100 * time.Millisecond
	if len(text) > 1<<20 {
		deadline = 400 * time.Millisecond
	}
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	var r res
	select {
	case r = <-done:
	case <-timer.C:
		return text, "unknown", 0, outTimeout
	case <-ctx.Done():
		return text, "unknown", 0, outTimeout
	}
	switch {
	case r.panicked:
		return text, "unknown", 0, outError
	case r.typ == "unknown":
		return text, "unknown", 0, outUnknown
	}
	if r.omitted > 0 {
		prefix := ""
		if r.typ == "yaml" {
			prefix = "# "
		}
		r.out += hint(len(text), len(r.out), prefix)
	}
	if len(r.out) > len(text)*(100-minGain)/100 {
		return text, r.typ, 0, outNoGain
	}
	return r.out, r.typ, r.omitted, outCompressed
}
