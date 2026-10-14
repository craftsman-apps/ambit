// Package out is the CLI's one output layer. Every line the CLI prints goes through a Printer, which redacts
// known secrets first (spec section 4, "Redaction").
package out

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Exit codes, the same for every command (spec section 4).
const (
	ExitOK               = 0
	ExitFailed           = 1
	ExitUsage            = 2
	ExitProductionFailed = 3 // Release 2: install/upgrade installed and healthy, production checks failed
)

// Error is a failure that names the next step. Commands return it; Main prints it and exits with Code.
type Error struct {
	What, Why, Next string
	Code            int // ExitFailed when zero
}

func (e *Error) Error() string { return e.What + " — " + e.Why }

// Usage returns an Error with exit code 2.
func Usage(what, why, next string) *Error {
	return &Error{What: what, Why: why, Next: next, Code: ExitUsage}
}

// enableVT makes the terminal show colour codes; New asks it once. A variable so a test can simulate a console
// that refuses (the legacy Windows console).
var enableVT = enableConsoleVT

// Options for a Printer. TTY is true when Out is a terminal.
type Options struct {
	Out, Err                    io.Writer
	TTY, NoColor, JSON, Verbose bool
}

type Printer struct {
	o     Options
	r     *Redactor
	color bool
}

func New(o Options, r *Redactor) *Printer {
	_, noColorEnv := lookupNoColor()
	return &Printer{o: o, r: r, color: o.TTY && !o.NoColor && !noColorEnv && enableVT(textWriter(o))}
}

// Redactor returns the Printer's redactor, for code that reads a secret.
func (p *Printer) Redactor() *Redactor { return p.r }

// TTY reports whether output goes to a terminal.
func (p *Printer) TTY() bool { return p.o.TTY }

func (p *Printer) line(w io.Writer, s string) { fmt.Fprintln(w, p.r.Redact(s)) }

func (p *Printer) paint(code, s string) string {
	if !p.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// Mark returns the coloured result mark: ✓ (ok), ! (warning) or ✗ (failed).
func (p *Printer) Mark(kind string) string {
	switch kind {
	case "ok":
		return p.paint("32", "✓")
	case "warn":
		return p.paint("33", "!")
	default:
		return p.paint("31", "✗")
	}
}

// text is where progress lines go: Out, or Err in JSON mode, so standard output carries only the JSON document.
func (p *Printer) text() io.Writer { return textWriter(p.o) }

func textWriter(o Options) io.Writer {
	if o.JSON {
		return o.Err
	}
	return o.Out
}

// Step prints "✓ what (1.2s)" or "✗ what (1.2s)".
func (p *Printer) Step(ok bool, what string, d time.Duration) {
	kind := "fail"
	if ok {
		kind = "ok"
	}
	p.line(p.text(), fmt.Sprintf("%s %s (%s)", p.Mark(kind), what, d.Round(100*time.Millisecond)))
}

// Line prints one plain line.
func (p *Printer) Line(format string, a ...any) { p.line(p.text(), fmt.Sprintf(format, a...)) }

// Fail prints "✗ what — why" and, when next is set, "  next: next". In JSON mode it writes to Err.
func (p *Printer) Fail(e *Error) {
	w := p.text()
	p.line(w, fmt.Sprintf("%s %s — %s", p.Mark("fail"), e.What, e.Why))
	if e.Next != "" {
		p.line(w, "  next: "+e.Next)
	}
}

// JSON writes v as indented JSON, redacted.
func (p *Printer) JSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	p.line(p.o.Out, string(b))
	return nil
}

// Verbose returns a writer for library logs: io.Discard unless --verbose, else each line redacted to Err.
func (p *Printer) Verbose() io.Writer {
	if !p.o.Verbose {
		return io.Discard
	}
	pr, pw := io.Pipe()
	go func() {
		// Read in pieces of at most 64 KiB and keep draining, so a long line never blocks the writer.
		// A line longer than that is printed in several pieces; a secret that spans two pieces is not matched.
		r := bufio.NewReaderSize(pr, 64*1024)
		for {
			b, _, err := r.ReadLine()
			if err != nil {
				return
			}
			p.line(p.o.Err, string(b))
		}
	}()
	return pw
}

// RevealOnTTY prints "label: secret" without redaction, only on a terminal and never in JSON output. It reports
// whether it printed. Used only for the local bootstrap password (spec 4.1 step 8).
func (p *Printer) RevealOnTTY(label, secret string) bool {
	if !p.o.TTY || p.o.JSON {
		return false
	}
	fmt.Fprintf(p.o.Out, "%s: %s\n", label, strings.TrimSpace(secret))
	return true
}

func lookupNoColor() (string, bool) { return os.LookupEnv("NO_COLOR") }
