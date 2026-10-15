package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"golang.org/x/term"

	"github.com/craftsman-apps/ambit/internal/cli/connect"
	"github.com/craftsman-apps/ambit/internal/cli/local"
	"github.com/craftsman-apps/ambit/internal/cli/out"
)

// defaultKeychainService is the keychain service name of every real ambit.
const defaultKeychainService = "ambit"

// keyPrintPath is the command path of `ambit key print`.
const keyPrintPath = "ambit key print"

// connectDeps is where connect, disconnect and key print keep their data. Tests replace it with in-memory stores.
type connectDeps struct {
	Keychain, File connect.Store
	StatePath      string
	GOOS           string
	Home           string
	Getenv         func(string) string
	Bin            connect.BinEnv                                           // finds the ambit path that tool configs hold
	Profile        func() (string, error)                                   // the PowerShell profile path
	ScriptPolicy   func() (string, error)                                   // the execution policy of that PowerShell; nil: not asked
	HTTP           func(url string) *http.Client                            // for the test call
	CallLimit      time.Duration                                            // how long the test call waits for a 200
	CallEvery      time.Duration                                            // between two tries
	ReadSecret     func(ctx context.Context, prompt string) (string, error) // nil: there is no terminal to ask in
	SaveState      func(st *connect.State, path string) error               // nil: st.Save; tests replace it to interrupt connect
}

func (d *connectDeps) save(st *connect.State) error {
	if d.SaveState != nil {
		return d.SaveState(st, d.StatePath)
	}
	return st.Save(d.StatePath)
}

func (a *app) connectEnv(service string) (*connectDeps, error) {
	if a.testConnect != nil {
		return a.testConnect, nil
	}
	if service == "" {
		service = defaultKeychainService
	}
	fs, err := connect.DefaultFileStore()
	if err != nil {
		return nil, err
	}
	path, err := connect.DefaultStatePath()
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	d := &connectDeps{Keychain: connect.NewKeychain(service), File: fs, StatePath: path, GOOS: runtime.GOOS, Home: home,
		Getenv: os.Getenv, Bin: connect.DefaultBinEnv(), Profile: connect.PowerShellProfile, HTTP: gatewayClient,
		CallLimit: 20 * time.Second, CallEvery: time.Second}
	if runtime.GOOS == "windows" {
		d.ScriptPolicy = connect.PowerShellPolicy
	}
	if a.stdinTTY() {
		fd := int(a.in.(*os.File).Fd())
		d.ReadSecret = func(ctx context.Context, prompt string) (string, error) {
			old, err := term.GetState(fd)
			if err != nil {
				return "", err
			}
			return hiddenPrompt(ctx, prompt, func() ([]byte, error) { return term.ReadPassword(fd) }, func() { term.Restore(fd, old) }, a.errw)
		}
	}
	return d, nil
}

// gatewayClient is the HTTP client of the test call. A local install (*.localtest.me) is reached on 127.0.0.1 at the
// port of the address, without DNS; every other address uses the normal client, with the proxy settings.
func gatewayClient(addr string) *http.Client {
	if u, err := url.Parse(addr); err == nil && strings.HasSuffix(u.Hostname(), ".localtest.me") {
		port := 80
		if u.Scheme == "https" {
			port = 443
		}
		if p, err := strconv.Atoi(u.Port()); err == nil {
			port = p
		}
		return local.HTTPClient(port)
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// store returns the store that holds the key of url, as the state file records it, and false when none does.
func (d *connectDeps) store(st *connect.State, url string) (connect.Store, bool) {
	switch st.Keys[url].Store {
	case connect.StoreKeychain:
		return d.Keychain, true
	case connect.StoreFile:
		return d.File, true
	}
	return nil, false
}

func (a *app) keyCmd() *cobra.Command {
	key := &cobra.Command{Use: "key", Short: "Work with the Ambit key that ambit connect stored"}
	var urlFlag, service string
	var reveal bool
	print := &cobra.Command{
		Use:   "print --url <llm address>",
		Short: "Print the stored key (the tools run this to read it)",
		Long: `Prints the stored key for the gateway address to standard output, followed by a newline, and nothing else.
Claude Code, Codex and the opencode wrapper run this command each time they start.

It reads the keychain or the key file only: no network, no Kubernetes. On a terminal it refuses unless you pass
--reveal, so the key does not end up in the scrollback by accident. Every program that runs as your user can run
this command: the keychain keeps the key from other users and from config files, not from your own programs.`,
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			if urlFlag == "" {
				return out.Usage("--url", "is required", "ambit key print --url https://llm.example.com")
			}
			url, err := connect.NormalizeURL(urlFlag)
			if err != nil {
				return out.Usage("--url "+urlFlag, err.Error(), "ambit key print --url https://llm.example.com")
			}
			if a.tty && !reveal {
				return out.Usage("key print", "this is a terminal, and the key would stay in its scrollback", "ambit key print --url "+url+" --reveal")
			}
			d, err := a.connectEnv(service)
			if err != nil {
				return err
			}
			st, err := connect.LoadState(d.StatePath)
			if err != nil {
				return err
			}
			notFound := &out.Error{What: "no Ambit key stored for " + url, Why: "ambit connect has not stored one",
				Next: "ambit connect <tool> --url " + url + " --model <model>", Code: out.ExitFailed}
			s, ok := d.store(st, url)
			if !ok {
				return notFound
			}
			k, err := s.Get(url)
			if err == connect.ErrNotFound {
				return notFound
			}
			if err != nil {
				return &out.Error{What: "read the key of " + url, Why: err.Error(), Next: "unlock your keychain, then run the tool again", Code: out.ExitFailed}
			}
			// The one place that prints a secret as it is: the tool that runs this command needs it.
			fmt.Fprintln(a.out, k)
			return nil
		}),
	}
	print.Flags().StringVar(&urlFlag, "url", "", "LLM gateway address, for example https://llm.example.com")
	print.Flags().BoolVar(&reveal, "reveal", false, "print on a terminal too")
	addKeyringFlag(print, &service)
	key.AddCommand(print)
	return key
}

func (a *app) connectCmd() *cobra.Command {
	var f connectFlags
	c := &cobra.Command{
		Use:   "connect <claude-code|codex|opencode|cursor> --url <llm address> --model <model id>",
		Short: "Point a coding tool at Ambit and store the key in your keychain",
		Long: `Adds Ambit's entries to the tool's user config file, stores the key in the operating system keychain, and ends
with one test call in the tool's own request format. The key is never written to a config file: the tool runs
"ambit key print" to read it.

Only user-level files are changed: ~/.claude/settings.json, ~/.codex/config.toml, ~/.config/opencode/opencode.json
and, for opencode, a function in your shell profile. Each file is copied to <file>.ambit.bak first. Cursor has no
config file for this: connect prints the steps and writes nothing.

The key comes from the keychain if one is stored for the address, else --key-file, else AMBIT_API_KEY, else a
hidden prompt. ambit disconnect removes exactly what connect added.`,
		Args: cobra.ExactArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			return a.runConnect(cmd.Context(), args[0], f)
		}),
	}
	fl := c.Flags()
	fl.StringVar(&f.url, "url", "", "LLM gateway address, for example https://llm.example.com (required)")
	fl.StringVar(&f.model, "model", "", "model ID that the tool sends (required except for cursor)")
	fl.StringVar(&f.consoleURL, "console-url", "", "console address; disconnect prints the key page of the console from it")
	fl.StringVar(&f.keyFile, "key-file", "", "read the key from this file (else AMBIT_API_KEY, else a hidden prompt)")
	fl.BoolVar(&f.replaceKey, "replace-key", false, "ask for a new key even if one is stored for this address")
	fl.StringVar(&f.keyStore, "key-store", "", "keychain or file (default: keychain; the file only where there is no Secret Service)")
	fl.StringVar(&f.shell, "shell", "", "opencode: bash, zsh, fish, powershell or none (default: detected)")
	fl.BoolVar(&f.dryRun, "dry-run", false, "print what would change; write, store and call nothing")
	fl.BoolVar(&f.force, "force", false, "replace a conflicting entry that ambit did not write")
	fl.BoolVar(&f.noTest, "no-test", false, "skip the test call")
	addKeyringFlag(c, &f.service)
	return c
}

func (a *app) disconnectCmd() *cobra.Command {
	var f disconnectFlags
	c := &cobra.Command{
		Use:   "disconnect <claude-code|codex|opencode|cursor> | --all",
		Short: "Remove what ambit connect added to a tool's config",
		Long: `Removes exactly the entries and blocks that ambit connect added, and keeps everything else in the file. An entry
you changed after connect stays, with a warning. When no other tool uses the key, the stored key is deleted. That
does not revoke it: revoke it in the console.`,
		Args: cobra.MaximumNArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			return a.runDisconnect(cmd.Context(), args, f)
		}),
	}
	fl := c.Flags()
	fl.BoolVar(&f.all, "all", false, "every tool recorded by ambit connect")
	fl.StringVar(&f.consoleURL, "console-url", "", "console address for the revoke line (overrides the one saved at connect)")
	fl.BoolVar(&f.keepKey, "keep-key", false, "do not delete the stored key")
	fl.BoolVar(&f.dryRun, "dry-run", false, "print what would change; write and delete nothing")
	addKeyringFlag(c, &f.service)
	return c
}
