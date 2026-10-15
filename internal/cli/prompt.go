package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// hiddenPrompt writes prompt to w and reads a secret with read. When ctx ends first it calls restore (puts the terminal's echo back, which
// the read had turned off) and returns an error at once; the read stays blocked until the process ends.
func hiddenPrompt(ctx context.Context, prompt string, read func() ([]byte, error), restore func(), w io.Writer) (string, error) {
	if ctx.Err() != nil {
		return "", errInterrupted
	}
	fmt.Fprint(w, prompt)
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() { b, err := read(); ch <- result{b, err} }()
	select {
	case r := <-ch:
		fmt.Fprintln(w)
		return string(r.b), r.err
	case <-ctx.Done():
		restore()
		fmt.Fprintln(w)
		return "", errInterrupted
	}
}

var errInterrupted = errors.New("interrupted while waiting for the key")
