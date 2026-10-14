package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/craftsman-apps/ambit/internal/cli/connect"
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
	return &connectDeps{Keychain: connect.NewKeychain(service), File: fs, StatePath: path, GOOS: runtime.GOOS}, nil
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
