package out

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer lets the test read what the Verbose goroutine writes without a data race.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func newTest(o Options) (*Printer, *bytes.Buffer) {
	var b bytes.Buffer
	o.Out = &b
	if o.Err == nil {
		o.Err = &b
	}
	return New(o, &Redactor{}), &b
}

func TestStepLine(t *testing.T) {
	p, b := newTest(Options{})
	p.Step(true, "Cluster", 12340*time.Millisecond)
	p.Step(false, "Charts", 900*time.Millisecond)
	want := "✓ Cluster (12.3s)\n✗ Charts (900ms)\n"
	if b.String() != want {
		t.Fatalf("got %q, want %q", b.String(), want)
	}
}

func TestFailLine(t *testing.T) {
	p, b := newTest(Options{})
	p.Fail(&Error{What: "Docker is not running", Why: "cannot connect to the Docker daemon", Next: "start Docker Desktop"})
	want := "✗ Docker is not running — cannot connect to the Docker daemon\n  next: start Docker Desktop\n"
	if b.String() != want {
		t.Fatalf("got %q, want %q", b.String(), want)
	}
}

func TestNoColourWithoutTTYOrWithNoColor(t *testing.T) {
	for _, o := range []Options{{}, {TTY: true, NoColor: true}} {
		p, b := newTest(o)
		p.Step(true, "x", 0)
		if strings.Contains(b.String(), "\x1b[") {
			t.Errorf("%+v: colour codes in %q", o, b.String())
		}
	}
}

func TestColourOnTTY(t *testing.T) {
	// NO_COLOR is unset in this test (t.Setenv cannot unset; the variable must be absent in the test runner).
	if _, set := lookupNoColor(); set {
		t.Skip("NO_COLOR is set in this environment")
	}
	p, b := newTest(Options{TTY: true})
	p.Step(true, "x", 0)
	if !strings.Contains(b.String(), "\x1b[32m✓") {
		t.Fatalf("no colour on a TTY: %q", b.String())
	}
}

// A legacy Windows console that cannot process virtual-terminal sequences would print "←[32m". Colour is then off.
func TestNoColourWhenTerminalCannotProcessEscapes(t *testing.T) {
	if _, set := lookupNoColor(); set {
		t.Skip("NO_COLOR is set in this environment")
	}
	for _, json := range []bool{false, true} {
		var asked io.Writer
		old := enableVT
		enableVT = func(w io.Writer) bool { asked = w; return false }
		var out, errw bytes.Buffer
		p := New(Options{Out: &out, Err: &errw, TTY: true, JSON: json}, &Redactor{})
		enableVT = old
		p.Step(true, "x", 0)
		want := io.Writer(&out) // the writer the progress lines go to
		if json {
			want = &errw
		}
		if asked != want {
			t.Errorf("json=%v: enableVT asked about the wrong writer", json)
		}
		if strings.Contains(out.String()+errw.String(), "\x1b[") {
			t.Errorf("json=%v: colour codes although the terminal cannot process them: %q%q", json, out.String(), errw.String())
		}
	}
}

func TestRedactRegisteredSecret(t *testing.T) {
	p, b := newTest(Options{})
	p.Redactor().Add("sk-or-v1-0123456789abcdef0123")
	p.Line("key %s rejected", "sk-or-v1-0123456789abcdef0123")
	p.Fail(&Error{What: "test call", Why: "401 for key sk-or-v1-0123456789abcdef0123"})
	if strings.Contains(b.String(), "0123456789abcdef") || strings.Count(b.String(), "[redacted]") != 2 {
		t.Fatalf("not redacted: %q", b.String())
	}
}

func TestRedactEncodedForms(t *testing.T) {
	r := &Redactor{}
	secret := `p@ss"w/rd+1 x`
	r.Add(secret)
	for _, in := range []string{
		`{"password":"p@ss\"w/rd+1 x"}`,      // JSON-escaped
		`password=p%40ss%22w%2Frd%2B1+x&a=b`, // form body (query escaping)
		`/p@ss%22w%2Frd+1%20x/`,              // URL path escaping
	} {
		if got := r.Redact(in); strings.Contains(got, "p@ss") || strings.Contains(got, "p%40ss") || !strings.Contains(got, "[redacted]") {
			t.Errorf("Redact(%q) = %q", in, got)
		}
	}
}

func TestRedactUnregisteredTokens(t *testing.T) {
	r := &Redactor{}
	got := r.Redact("Authorization: Bearer eyJhbGciOiJSUzI1NiJ9.abc.def and sk-or-v1-aaaaaaaaaaaaaaaaaaaa")
	if got != "Authorization: Bearer [redacted] and [redacted]" {
		t.Fatalf("got %q", got)
	}
}

func TestShortValuesAreNotRegistered(t *testing.T) {
	r := &Redactor{}
	r.Add("abc")
	if r.Redact("abcdef") != "abcdef" {
		t.Fatal("a 3-character value was redacted")
	}
}

func TestJSONIsRedacted(t *testing.T) {
	p, b := newTest(Options{JSON: true})
	p.Redactor().Add("hunter2-password")
	if err := p.JSON(map[string]string{"message": "password hunter2-password"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "hunter2") {
		t.Fatalf("JSON not redacted: %s", b.String())
	}
}

func TestVerboseWriter(t *testing.T) {
	p, _ := newTest(Options{})
	if p.Verbose() != io.Discard {
		t.Fatal("library logs must be off without --verbose")
	}
	var errb lockedBuffer
	pv, _ := newTest(Options{Verbose: true, Err: &errb})
	pv.Redactor().Add("hunter2-password")
	w := pv.Verbose()
	io.WriteString(w, "debug: sent hunter2-password\n")
	w.(io.Closer).Close()
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(errb.String(), "debug:") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(errb.String(), "debug: sent [redacted]") {
		t.Fatalf("verbose output = %q", errb.String())
	}
}

func TestRevealOnTTY(t *testing.T) {
	p, b := newTest(Options{TTY: true})
	p.Redactor().Add("local-password-1")
	if !p.RevealOnTTY("Password", "local-password-1") || !strings.Contains(b.String(), "Password: local-password-1") {
		t.Fatalf("TTY: %q", b.String())
	}
	for _, o := range []Options{{}, {TTY: true, JSON: true}} {
		p, b := newTest(o)
		if p.RevealOnTTY("Password", "local-password-1") || b.Len() != 0 {
			t.Errorf("%+v printed the password: %q", o, b.String())
		}
	}
}

func TestVerboseLongLineDoesNotBlock(t *testing.T) {
	var errb lockedBuffer
	p, _ := newTest(Options{Verbose: true, Err: &errb})
	w := p.Verbose()
	done := make(chan struct{})
	go func() {
		io.WriteString(w, strings.Repeat("x", 200*1024)+"\n")
		io.WriteString(w, "after the long line\n")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Verbose writer blocked on a long line")
	}
	w.(io.Closer).Close()
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(errb.String(), "after the long line") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(errb.String(), "after the long line") {
		t.Fatal("the line after the long line was lost")
	}
}

func TestFailInJSONModeGoesToErr(t *testing.T) {
	var outb, errb bytes.Buffer
	p := New(Options{Out: &outb, Err: &errb, JSON: true}, &Redactor{})
	p.Fail(&Error{What: "Docker is not running", Why: "no daemon", Next: "start Docker"})
	if outb.Len() != 0 {
		t.Fatalf("stdout must carry only JSON, got %q", outb.String())
	}
	if !strings.Contains(errb.String(), "Docker is not running") || !strings.Contains(errb.String(), "next: start Docker") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

func TestStepAndLineInJSONModeGoToErr(t *testing.T) {
	var outb, errb bytes.Buffer
	p := New(Options{Out: &outb, Err: &errb, JSON: true}, &Redactor{})
	p.Step(true, "Cluster", 0)
	p.Line("hello")
	if outb.Len() != 0 || !strings.Contains(errb.String(), "✓ Cluster") || !strings.Contains(errb.String(), "hello") {
		t.Fatalf("stdout %q, stderr %q", outb.String(), errb.String())
	}
}

func TestRedactAmbitKeyPattern(t *testing.T) {
	r := &Redactor{}
	key := "ambit_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"
	if got := r.Redact(`ANTHROPIC_AUTH_TOKEN = "` + key + `"`); strings.Contains(got, "ambit_") || !strings.Contains(got, "[redacted]") {
		t.Fatalf("Redact = %q", got)
	}
	if got := r.Redact("the ambit_ prefix and ambit_short stay"); got != "the ambit_ prefix and ambit_short stay" {
		t.Fatalf("Redact changed ordinary text: %q", got)
	}
}

// pemKey is a fake RSA key in PEM form: 64-character base64 lines, as a GitHub App private key has.
func pemKey() string {
	var b strings.Builder
	b.WriteString("-----BEGIN RSA PRIVATE KEY-----\n")
	for i := 0; i < 5; i++ {
		b.WriteString(strings.Repeat(string(rune('A'+i)), 60) + "q1w2\n")
	}
	b.WriteString("-----END RSA PRIVATE KEY-----\n")
	return b.String()
}

func waitFor(t *testing.T, b *lockedBuffer, s string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(b.String(), s) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

// #108 R1: a library that logs a registered multi-line secret line by line must not show any line of it.
func TestVerboseRedactsEveryLineOfAMultiLineSecret(t *testing.T) {
	for name, secret := range map[string]string{
		"pem":     pemKey(),
		"3 lines": "first-line-of-the-secret\r\nsecond-line-of-secret!\nthird-line-of-the-secret\n",
	} {
		t.Run(name, func(t *testing.T) {
			var errb lockedBuffer
			p, _ := newTest(Options{Verbose: true, Err: &errb})
			p.Redactor().Add(secret)
			w := p.Verbose()
			io.WriteString(w, "debug: read key\n"+strings.ReplaceAll(secret, "\r\n", "\n")+"done\n")
			w.(io.Closer).Close()
			waitFor(t, &errb, "done")
			got := errb.String()
			for _, l := range strings.Split(strings.ReplaceAll(secret, "\r\n", "\n"), "\n") {
				if len(l) >= 16 && !strings.HasPrefix(l, "-----") && strings.Contains(got, l) {
					t.Errorf("line %q of the secret is in the output:\n%s", l, got)
				}
			}
			if !strings.Contains(got, "done") {
				t.Fatalf("output lost: %q", got)
			}
		})
	}
}

// #108 R1: a PEM private key that was never registered (inside a library error, for example) is redacted whole.
func TestRedactUnregisteredPEMBlock(t *testing.T) {
	r := &Redactor{}
	got := r.Redact("error: bad key " + pemKey() + " in file")
	if strings.Contains(got, "AAAAAAAA") || strings.Contains(got, "EEEEEEEE") || !strings.Contains(got, "error: bad key [redacted]") || !strings.HasSuffix(got, " in file") {
		t.Fatalf("Redact = %q", got)
	}
}

// #108 R2: a secret placed across the 64 KiB seam of the verbose reader is redacted, and the line stays whole.
func TestVerboseRedactsASecretAcrossThePieceSeam(t *testing.T) {
	const secret = "ghs_0123456789abcdefSECRETVALUE0123456789"
	for _, offset := range []int{1, 10, len(secret) - 1} {
		var errb lockedBuffer
		p, _ := newTest(Options{Verbose: true, Err: &errb})
		p.Redactor().Add(secret)
		w := p.Verbose()
		line := strings.Repeat("x", 64*1024-offset) + secret + strings.Repeat("y", 100) + "\n"
		go func() { io.WriteString(w, line+"end\n"); w.(io.Closer).Close() }()
		waitFor(t, &errb, "end")
		got := errb.String()
		if strings.Contains(got, "SECRETVALUE") || strings.Contains(got, "0123456789abcdef") {
			t.Fatalf("offset %d: the secret across the seam was printed", offset)
		}
		if want := strings.Repeat("x", 64*1024-offset) + "[redacted]" + strings.Repeat("y", 100) + "\nend\n"; got != want {
			t.Fatalf("offset %d: the line was changed: %d bytes, want %d", offset, len(got), len(want))
		}
	}
}

// #108 comment 1 (O3): when Err is another console than Out, both must accept colour codes, or colour is off.
func TestNoColourWhenTheErrorConsoleCannotProcessEscapes(t *testing.T) {
	if _, set := lookupNoColor(); set {
		t.Skip("NO_COLOR is set in this environment")
	}
	var out, errw bytes.Buffer
	var asked []io.Writer
	old := enableVT
	enableVT = func(w io.Writer) bool { asked = append(asked, w); return w == io.Writer(&out) }
	p := New(Options{Out: &out, Err: &errw, TTY: true}, &Redactor{})
	enableVT = old
	if len(asked) != 2 {
		t.Fatalf("enableVT asked %d writers, want Out and Err", len(asked))
	}
	p.Step(true, "x", 0)
	p.Fail(&Error{What: "y", Why: "z"})
	if strings.Contains(out.String()+errw.String(), "\x1b[") {
		t.Fatalf("colour codes although the error console cannot process them: %q", out.String())
	}
}
