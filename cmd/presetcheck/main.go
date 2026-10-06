// presetcheck tests the app preset catalog: lockcheck (CI), contract (nightly), live (maintainer's laptop),
// gate (release). See docs/presets.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
)

func joinLines(s []string) string { return strings.Join(s, "\n  ") }

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: presetcheck lockcheck|contract|live|gate [flags]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	var err error
	switch os.Args[1] {
	case "lockcheck":
		base := fs.String("base", "origin/main", "git ref of the base branch")
		fs.Parse(os.Args[2:])
		err = lockcheck(*base)
	case "contract":
		jsonOut := fs.String("json", "", "write {id: [problems]} to this file")
		fs.Parse(os.Args[2:])
		err = contract(context.Background(), *jsonOut, fs.Args())
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
