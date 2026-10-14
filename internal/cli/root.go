// Package cli is the ambit command tree. cmd/ambit/main.go calls Main.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/craftsman-apps/ambit/internal/cli/helmx"
	"github.com/craftsman-apps/ambit/internal/cli/local"
	"github.com/craftsman-apps/ambit/internal/cli/out"
)

// version is the CLI version, set by GoReleaser: -ldflags "-X github.com/craftsman-apps/ambit/internal/cli.version=0.2.0".
var version = "dev"

// ChartRepo is where the CLI pulls the charts of its own version from (spec 5).
const ChartRepo = "oci://ghcr.io/craftsman-apps/charts"

type globals struct {
	kubeconfig, context, namespace, output string
	yes, noColor, verbose                  bool
}

type app struct {
	g       globals
	in      io.Reader
	out     io.Writer
	errw    io.Writer
	tty     bool
	p       *out.Printer
	newHelm func(helmx.Options) (helmx.Client, error)
	// testLocalEnv replaces the Docker- and kind-backed environment of the local commands in tests.
	testLocalEnv *local.Env
}

// exitCode ends a command with a code after it printed its own result (e.g. check with a failed check).
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit %d", int(e)) }

// Main runs the CLI and returns the process exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := &app{in: stdin, out: stdout, errw: stderr, newHelm: helmx.New}
	if f, ok := stdout.(*os.File); ok {
		a.tty = term.IsTerminal(int(f.Fd()))
	}
	root := a.root()
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	return a.exit(root.Execute())
}

func (a *app) printer() *out.Printer {
	if a.p == nil {
		a.p = out.New(out.Options{Out: a.out, Err: a.errw, TTY: a.tty, NoColor: a.g.noColor,
			JSON: a.g.output == "json", Verbose: a.g.verbose}, &out.Redactor{})
	}
	return a.p
}

func (a *app) exit(err error) int {
	if err == nil {
		return out.ExitOK
	}
	var code exitCode
	if errors.As(err, &code) {
		return int(code)
	}
	var e *out.Error
	if !errors.As(err, &e) {
		// cobra's own errors: unknown command, bad arguments.
		e = out.Usage("usage", err.Error(), "ambit --help")
	}
	a.printer().Fail(e)
	if e.Code == 0 {
		return out.ExitFailed
	}
	return e.Code
}

// run adapts a command function: errors that are not an *out.Error or exitCode become a failed step (exit 1).
func run(f func(cmd *cobra.Command, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		err := f(cmd, args)
		var e *out.Error
		var c exitCode
		if err == nil || errors.As(err, &e) || errors.As(err, &c) {
			return err
		}
		return &out.Error{What: cmd.CommandPath(), Why: err.Error(), Code: out.ExitFailed}
	}
}

const jsonAllowed = "json-output"

func (a *app) root() *cobra.Command {
	root := &cobra.Command{
		Use:           "ambit",
		Short:         "Install, check and validate Ambit",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			switch a.g.output {
			case "text":
			case "json":
				if cmd.Annotations[jsonAllowed] != "true" {
					return out.Usage("--output json", "only `ambit check` and `ambit version` print JSON", cmd.CommandPath()+" --output text")
				}
			default:
				return out.Usage("--output "+a.g.output, "must be text or json", cmd.CommandPath()+" --output text")
			}
			return nil
		},
	}
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return out.Usage("usage", err.Error(), cmd.CommandPath()+" --help")
	})
	f := root.PersistentFlags()
	f.StringVar(&a.g.kubeconfig, "kubeconfig", "", "path to the kubeconfig file (default: $KUBECONFIG or ~/.kube/config)")
	f.StringVar(&a.g.context, "context", "", "kubeconfig context (default: the current context)")
	f.StringVarP(&a.g.namespace, "namespace", "n", "ambit-system", "namespace of the Ambit releases")
	f.BoolVar(&a.g.yes, "yes", false, "do not ask for confirmation (required without a terminal)")
	f.StringVar(&a.g.output, "output", "text", "output format: text or json (check and version only)")
	f.BoolVar(&a.g.noColor, "no-color", false, "no colour in the output")
	f.BoolVar(&a.g.verbose, "verbose", false, "print library logs (secrets are redacted)")
	root.AddCommand(a.validateCmd(), a.versionCmd(), a.localCmd(), a.checkCmd())
	return root
}
