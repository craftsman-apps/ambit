package guard

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Every compressor returns soon after its context ends, with ok false, and keeps no slot.
func TestCompressorsStopAtTheDeadline(t *testing.T) {
	const deadline = time.Millisecond
	const slack = 20 * time.Millisecond
	size := 4 << 20 // maxCompressBytes: the largest text the guard hands to a compressor
	for _, tc := range []struct {
		name string
		text string
		run  func(ctx context.Context, text string) bool
	}{
		{"json", sized(queryRows, size), func(c context.Context, s string) bool { _, ok := compressJSON(c, s, true); return ok }},
		{"logs-lossless", sized(func(n int) string { return serviceLog(n, n/250) }, size), func(c context.Context, s string) bool { _, _, ok := compressLogs(c, s, false); return ok }},
		{"logs-shorten", sized(func(n int) string { return serviceLog(n, n/250) }, size), func(c context.Context, s string) bool { _, _, ok := compressLogs(c, s, true); return ok }},
		{"test-shorten", sized(func(n int) string { return goTestVerbose(n, 2) }, size), func(c context.Context, s string) bool { _, _, ok := compressTestOutput(c, s, true); return ok }},
		{"csv", sized(csvExport, size), func(c context.Context, s string) bool { _, ok := compressTable(c, s); return ok }},
		{"markdown", sized(markdownTable, size), func(c context.Context, s string) bool { _, ok := compressTable(c, s); return ok }},
		{"yaml-shorten", sized(podYAML, size), func(c context.Context, s string) bool { _, _, ok := compressYAML(c, s, true); return ok }},
		{"detect", sized(podYAML, size), func(c context.Context, s string) bool {
			_, typ, _ := detect(c, s, true, false)
			return typ != "unknown"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			begin := time.Now()
			if !tc.run(context.Background(), tc.text) { // the input is one the compressor accepts when it has time
				t.Fatal("the input is not accepted without a deadline")
			}
			// Slower builds (the race detector) get a larger bound, in proportion to a full run.
			limit := deadline + max(slack, time.Since(begin)/4)
			ctx, cancel := context.WithTimeout(t.Context(), deadline)
			defer cancel()
			start := time.Now()
			ok := tc.run(ctx, tc.text)
			if elapsed := time.Since(start); elapsed > limit {
				t.Errorf("returned after %v; want at most %v", elapsed, limit)
			}
			if ok {
				t.Error("a compressor that ran past its deadline reported a result")
			}
		})
	}
}

// A compressor that passes its deadline frees its slot, so the next calls are not skipped as busy.
func TestSlotIsFreeAfterTimeout(t *testing.T) {
	big := sized(queryRows, 4<<20)
	begin := time.Now()
	detect(t.Context(), big, true, false)
	// 20 ms, or more on a slower build (the race detector), in proportion to a full run.
	slotFreeWithin := max(20*time.Millisecond, time.Since(begin)/4)
	var wg sync.WaitGroup
	for range maxCompressing {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
			defer cancel()
			if _, _, _, outcome := compressText(ctx, big, ModeShorten, false); outcome != outTimeout {
				t.Errorf("outcome %q; want %q", outcome, outTimeout)
			}
		}()
	}
	wg.Wait()
	small := serviceLog(300, 1)
	deadline := time.Now().Add(slotFreeWithin)
	for {
		_, _, _, outcome := compressText(t.Context(), small, ModeShorten, false)
		if outcome != outBusy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("both slots still busy %v after the timeout", slotFreeWithin)
		}
		time.Sleep(time.Millisecond)
	}
}

// Compressors that are cut at different points run at the same time (go test -race).
func TestCompressTextCancelRace(t *testing.T) {
	texts := []string{sized(queryRows, 1<<20), sized(csvExport, 1<<20), sized(podYAML, 1<<20), serviceLog(20000, 40)}
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(t.Context(), time.Duration(i)*200*time.Microsecond)
			defer cancel()
			compressText(ctx, texts[i%len(texts)], ModeShorten, false)
		}()
	}
	wg.Wait()
}
