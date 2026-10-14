// ambit is Ambit's command-line tool: a local Ambit on kind, checks of an install, and `ambit validate` for
// Ambit objects in a configuration repository. The commands live in internal/cli.
package main

import (
	"os"

	"github.com/craftsman-apps/ambit/internal/cli"
)

func main() { os.Exit(cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }
