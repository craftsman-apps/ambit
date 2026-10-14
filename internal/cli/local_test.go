package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLocalUpDevBuildNeedsChartDir(t *testing.T) {
	code, s := runCLI(t, &fakeHelm{}, "local", "up", "--no-key")
	if code != 2 || !strings.Contains(s, "next: ambit local up --chart-dir <path to charts/ in a checkout>") {
		t.Fatalf("code %d, output %q", code, s)
	}
}

func TestLocalUpChartDirChecks(t *testing.T) {
	dir := t.TempDir()
	code, s := runCLI(t, &fakeHelm{}, "local", "up", "--no-key", "--chart-dir", dir)
	if code != 2 || !strings.Contains(s, "has no chart ambit") {
		t.Fatalf("empty dir: code %d, output %q", code, s)
	}
	os.MkdirAll(filepath.Join(dir, "ambit"), 0o755)
	os.MkdirAll(filepath.Join(dir, "ambit-crds"), 0o755)
	code, s = runCLI(t, &fakeHelm{}, "local", "up", "--no-key", "--chart-dir", dir)
	if code != 2 || !strings.Contains(s, "next: helm dependency build "+filepath.Join(dir, "ambit")) {
		t.Fatalf("no dependencies: code %d, output %q", code, s)
	}
}

func TestLocalUpKeyFlagsConflict(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	version = "0.2.0"
	code, s := runCLI(t, &fakeHelm{}, "local", "up", "--no-key", "--openrouter-key-file", "k.txt")
	if code != 2 || !strings.Contains(s, "cannot be used together") {
		t.Fatalf("code %d, output %q", code, s)
	}
}

func TestLocalRejectsJSON(t *testing.T) {
	if code, _ := runCLI(t, &fakeHelm{}, "local", "password", "--output", "json"); code != 2 {
		t.Fatalf("code %d, want 2", code)
	}
}

// fakeNotify stands in for signal.NotifyContext: cancel is the signal, stopped counts calls of the stop function.
func fakeNotify() (notify func(context.Context, ...os.Signal) (context.Context, context.CancelFunc), signal func(), stopped *atomic.Int32) {
	stopped = &atomic.Int32{}
	var cancel context.CancelFunc
	notify = func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		var ctx context.Context
		ctx, cancel = context.WithCancel(parent)
		return ctx, func() { stopped.Add(1); cancel() }
	}
	return notify, func() { cancel() }, stopped
}

// After the first Ctrl+C the handler is given up (stop is called), so the next Ctrl+C ends the process.
func TestInterruptContextStopsAfterFirstSignal(t *testing.T) {
	notify, sig, stopped := fakeNotify()
	told := make(chan struct{})
	ctx, done := interruptContext(context.Background(), notify, func() { close(told) })
	defer done()
	sig()
	select {
	case <-told:
	case <-time.After(2 * time.Second):
		t.Fatal("no message after the first signal")
	}
	if ctx.Err() == nil || stopped.Load() == 0 {
		t.Fatalf("ctx.Err %v, stop calls %d; stop must be called after the first signal", ctx.Err(), stopped.Load())
	}
}

func TestInterruptContextQuietOnNormalFinish(t *testing.T) {
	notify, _, stopped := fakeNotify()
	called := make(chan struct{}, 1)
	_, done := interruptContext(context.Background(), notify, func() { called <- struct{}{} })
	done()
	time.Sleep(50 * time.Millisecond)
	if len(called) != 0 || stopped.Load() == 0 {
		t.Fatalf("signal message on a normal finish (%d) or stop not called (%d)", len(called), stopped.Load())
	}
}

// A Ctrl+C at the hidden prompt returns at once with a clear error and puts the terminal back.
func TestHiddenPromptCancelled(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	var restored atomic.Bool
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	var w bytes.Buffer
	res := make(chan error, 1)
	go func() {
		_, err := hiddenPrompt(ctx, "", func() ([]byte, error) { close(started); <-block; return nil, nil }, func() { restored.Store(true) }, &w)
		res <- err
	}()
	<-started
	cancel()
	select {
	case err := <-res:
		if err == nil || !strings.Contains(err.Error(), "interrupted") || !restored.Load() {
			t.Fatalf("err %v, restored %v", err, restored.Load())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the prompt did not return after cancel")
	}
}

func TestHiddenPromptReadsKey(t *testing.T) {
	k, err := hiddenPrompt(context.Background(), "", func() ([]byte, error) { return []byte("sk-or-v1-x"), nil }, func() {}, io.Discard)
	if err != nil || k != "sk-or-v1-x" {
		t.Fatalf("%q, %v", k, err)
	}
}

// The prompt text and the line end go to the writer it is given (stderr in `local up`), so `ambit local up > log`
// does not put the prompt in the file.
func TestHiddenPromptWritesToItsWriter(t *testing.T) {
	var w bytes.Buffer
	hiddenPrompt(context.Background(), "Key: ", func() ([]byte, error) { return []byte("k"), nil }, func() {}, &w)
	if w.String() != "Key: \n" {
		t.Fatalf("writer got %q", w.String())
	}
}
