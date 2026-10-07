package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/console"
	"github.com/craftsman-apps/ambit/internal/controller"
	"github.com/craftsman-apps/ambit/internal/presets"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/term"
)

type Result struct {
	Preset                 string   `json:"preset"`
	Version                int      `json:"version"`
	Pass                   bool     `json:"pass"`
	Error                  string   `json:"error,omitempty"`
	Tools                  []string `json:"tools,omitempty"`
	DiffersBySecondAccount []string `json:"differsBySecondAccount,omitempty"`
	NoRefreshToken         bool     `json:"noRefreshToken,omitempty"` // users of this app must reconnect when access expires

	fingerprints map[string]string // tool name → fingerprint, to compare with the second account; not written
}

type Results struct {
	Date        time.Time `json:"date"`
	Commit      string    `json:"commit"`
	CatalogHash string    `json:"catalogHash"`
	Presets     []Result  `json:"presets"`
}

func catalogHash(ps []presets.Preset) string {
	sorted := append([]presets.Preset(nil), ps...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	h := sha256.New()
	for _, p := range sorted {
		fmt.Fprintf(h, "%s %s\n", p.ID, presets.Hash(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// redact replaces every secret in s. Values shorter than 8 characters are skipped: a real secret or token
// is longer, and replacing a short string (or an empty one) would garble every word that contains it.
func redact(s string, secrets ...string) string {
	for _, secret := range secrets {
		if len(secret) >= 8 {
			s = strings.ReplaceAll(s, secret, "[secret]")
		}
	}
	return s
}

// liveCheck renews the test account's token (saving a rotated refresh token before anything else), lists
// the tools and calls the preset's read-only tool.
func liveCheck(ctx context.Context, hc *http.Client, p presets.Preset, pc *PresetCreds, save func() error) Result {
	r := Result{Preset: p.ID, Version: p.Version}
	oldRefresh := pc.RefreshToken
	fail := func(err error) Result {
		r.Error = redact(err.Error(), oldRefresh, pc.RefreshToken, pc.AccessToken, pc.ClientSecret)
		return r
	}
	res, err := p.Resolve(pc.Values, pc.ClientID, callbackFor(p))
	if err != nil {
		return fail(err)
	}
	var access string
	if pc.RefreshToken == "" { // a vendor that issues no refresh token: use the access token until it expires
		if pc.AccessToken == "" || (!pc.Expiry.IsZero() && time.Now().After(pc.Expiry)) { // zero expiry: does not expire
			return fail(errors.New("no refresh token and the access token expired; sign in again"))
		}
		access = pc.AccessToken
		r.NoRefreshToken = true
	} else {
		conn := &v1.Connection{Spec: res.Connection}
		conn.Spec.ClientID = pc.ClientID
		tok, err := console.TokenRequest(ctx, hc, conn, pc.ClientSecret, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {pc.RefreshToken}}, time.Now())
		if err != nil {
			return fail(fmt.Errorf("renew: %w (sign in again: make preset-live APP=%s)", err, p.ID))
		}
		access = tok.AccessToken
		if tok.RefreshToken != "" {
			pc.RefreshToken = tok.RefreshToken
		}
		pc.AccessToken, pc.Expiry = tok.AccessToken, tok.Expiry
		if err := save(); err != nil { // before any MCP call: a crash must not lose a rotated token
			return fail(fmt.Errorf("save rotated token: %w", err))
		}
	}
	tools, err := controller.ListToolsAt(ctx, hc, res.Server.URL, res.Server.Transport, access)
	if err != nil && pc.RefreshToken == "" && pc.Expiry.IsZero() && strings.Contains(err.Error(), "Unauthorized") {
		// A token that does not expire (GitHub OAuth Apps) and that the vendor cancelled; nothing tells Ambit.
		return fail(fmt.Errorf("tools/list: %w: the vendor refused a token that does not expire; sign in again: remove \"accessToken\" from presets.%s in ~/.config/ambit/presetcheck.json, then run: make preset-live APP=%s (for the second test account, remove it from presets.%s.second)", err, p.ID, p.ID, p.ID))
	}
	if err != nil {
		return fail(fmt.Errorf("tools/list: %w", err))
	}
	if len(tools) == 0 {
		return fail(errors.New("tools/list returned no tools"))
	}
	r.fingerprints = map[string]string{}
	for _, t := range tools {
		r.Tools = append(r.Tools, t.Name)
		r.fingerprints[t.Name] = t.Fingerprint
	}
	if err := callTool(ctx, hc, res.Server.URL, res.Server.Transport, access, p.Live.Tool, p.Live.Arguments); err != nil {
		return fail(err)
	}
	r.Pass = true
	return r
}

func callTool(ctx context.Context, base *http.Client, endpoint, transport, bearer, name string, args map[string]any) error {
	hc := controller.BearerClient(base, endpoint, bearer) // token to the server's own host only, no redirects
	var t mcp.Transport = &mcp.StreamableClientTransport{Endpoint: endpoint, MaxRetries: -1, DisableStandaloneSSE: true, HTTPClient: hc}
	if transport == "sse" {
		t = &mcp.SSEClientTransport{Endpoint: endpoint, HTTPClient: hc}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "presetcheck", Version: "1"}, nil).Connect(ctx, t, nil)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer cs.Close()
	if args == nil {
		args = map[string]any{} // servers such as GitHub refuse `null`: arguments must be a JSON object
	}
	out, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return fmt.Errorf("tools/call %s: %w", name, err)
	}
	if out.IsError {
		text := ""
		if len(out.Content) > 0 {
			if tc, ok := out.Content[0].(*mcp.TextContent); ok {
				text = tc.Text
			}
		}
		if len(text) > 200 {
			text = text[:200]
		}
		return fmt.Errorf("tool %s answered with isError: %s", name, text)
	}
	return nil
}

func callbackFor(p presets.Preset) string {
	if p.Live.Callback == "https" {
		return "https://127.0.0.1:8766/callback"
	}
	return "http://127.0.0.1:8765/callback"
}

// runLive checks every verified preset plus the ids given, with the maintainer's test accounts, and writes
// test/presets/results.json. It never prints a secret or a token.
func runLive(ctx context.Context, ids []string) error {
	if out, err := exec.Command("git", "status", "--porcelain", "--", "internal", "cmd", "go.mod", "go.sum").Output(); err != nil {
		return fmt.Errorf("git status: %w", err)
	} else if len(bytes.TrimSpace(out)) > 0 {
		return errors.New("commit your changes first: results.json must name the commit that was tested")
	}
	headOut, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	rootOut, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return fmt.Errorf("git rev-parse --show-toplevel: %w", err)
	}
	root := strings.TrimSpace(string(rootOut))
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".config", "ambit")
	unlock, err := lockCreds(dir)
	if err != nil {
		return err
	}
	defer unlock()
	path := filepath.Join(dir, "presetcheck.json")
	creds, err := loadCreds(path)
	if err != nil {
		return err
	}
	save := func() error { return saveCreds(path, creds) }
	all, err := presets.All()
	if err != nil {
		return err
	}
	var todo []presets.Preset
	for _, p := range all {
		if p.Verified || slices.Contains(ids, p.ID) {
			todo = append(todo, p)
		}
	}
	for _, id := range ids {
		if _, ok := presets.Get(id); !ok {
			return fmt.Errorf("no preset %q in the catalog", id)
		}
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	var cert *tls.Config
	signIn := func(p presets.Preset, pc *PresetCreds) error {
		if p.Live.Callback == "https" && cert == nil {
			if cert, err = loadOrCreateCert(dir); err != nil {
				return err
			}
		}
		fmt.Printf("%s: sign in with the test account in the browser\n", p.ID)
		if err := (signInFlow{Open: openBrowser, HTTP: hc, Listen: net.Listen, TLS: cert}).run(ctx, p, pc); err != nil {
			return fmt.Errorf("sign-in: %w", err)
		}
		return save()
	}
	// check runs liveCheck for one account, signing in first when there is no usable token and once more
	// when the vendor refuses the refresh token.
	check := func(p presets.Preset, pc *PresetCreds) Result {
		if pc.RefreshToken == "" && (pc.AccessToken == "" || (!pc.Expiry.IsZero() && time.Now().After(pc.Expiry))) {
			if err := signIn(p, pc); err != nil {
				return Result{Preset: p.ID, Version: p.Version, Error: redact(err.Error(), pc.ClientSecret)}
			}
		}
		r := liveCheck(ctx, hc, p, pc, save)
		if !r.Pass && strings.Contains(r.Error, "invalid_grant") {
			if err := signIn(p, pc); err != nil {
				r.Error = redact(err.Error(), pc.ClientSecret)
				return r
			}
			r = liveCheck(ctx, hc, p, pc, save)
		}
		return r
	}
	results := Results{Date: time.Now().UTC(), Commit: strings.TrimSpace(string(headOut)), CatalogHash: catalogHash(all)}
	failed := 0
	for _, p := range todo {
		pc := creds.Presets[p.ID]
		if pc == nil {
			if pc, err = askCreds(p); err != nil {
				return err
			}
			creds.Presets[p.ID] = pc
			if err := save(); err != nil {
				return err
			}
		}
		r := check(p, pc)
		if s := pc.Second; r.Pass && s != nil {
			if s.ClientID == "" { // the second account normally signs in to the same test OAuth app
				s.ClientID, s.ClientSecret = pc.ClientID, pc.ClientSecret
			}
			if s.Values == nil {
				s.Values = pc.Values
			}
			r = withSecond(r, check(p, s))
		}
		if r.Pass {
			if err := reportToolChanges(filepath.Join(root, "test", "presets", "tools", p.ID+".txt"), r.Tools); err != nil {
				return err
			}
			fmt.Println("PASS", p.ID)
		} else {
			failed++
			fmt.Println("FAIL", p.ID+":", r.Error)
		}
		if len(r.DiffersBySecondAccount) > 0 {
			fmt.Printf("  tools that differ for the second account: %s\n", strings.Join(r.DiffersBySecondAccount, ", "))
		}
		if r.NoRefreshToken {
			fmt.Println("  the vendor issued no refresh token: users must reconnect when access expires")
		}
		results.Presets = append(results.Presets, r)
	}
	b, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "test", "presets", "results.json"), append(b, '\n'), 0o644); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d presets failed", failed, len(todo))
	}
	return nil
}

// fingerprintDiff names the tools whose definition differs between two accounts, including tools that only
// one account sees.
func fingerprintDiff(a, b map[string]string) []string {
	var out []string
	for name, fp := range a {
		if b[name] != fp {
			out = append(out, name)
		}
	}
	for name := range b {
		if _, ok := a[name]; !ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// reportToolChanges prints tools added or removed since the last run (reported, not failed) and rewrites the file.
func reportToolChanges(path string, tools []string) error {
	old := map[string]bool{}
	if b, err := os.ReadFile(path); err == nil {
		for _, l := range strings.Fields(string(b)) {
			old[l] = true
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	now := map[string]bool{}
	for _, t := range tools {
		now[t] = true
		if len(old) > 0 && !old[t] {
			fmt.Println("  added tool:", t)
		}
	}
	for t := range old {
		if !now[t] {
			fmt.Println("  removed tool:", t)
		}
	}
	sorted := slices.Sorted(maps.Keys(now))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.Join(sorted, "\n")+"\n"), 0o644)
}

// askCreds asks on the terminal for a preset's test OAuth app and placeholder values. The secret is read
// without echo.
func askCreds(p presets.Preset) (*PresetCreds, error) {
	in := bufio.NewReader(os.Stdin)
	ask := func(label string) (string, error) {
		fmt.Printf("%s: %s: ", p.ID, label)
		s, err := in.ReadString('\n')
		return strings.TrimSpace(s), err
	}
	fmt.Printf("%s: no test account yet. Register the callback %s in the vendor's test OAuth app.\n", p.ID, callbackFor(p))
	pc := &PresetCreds{Values: map[string]string{}}
	var err error
	if pc.ClientID, err = ask("client ID"); err != nil {
		return nil, err
	}
	fmt.Printf("%s: client secret (not shown): ", p.ID)
	sec, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return nil, fmt.Errorf("read the client secret: %w", err)
	}
	pc.ClientSecret = strings.TrimSpace(string(sec))
	for _, ph := range p.Placeholders {
		if pc.Values[ph.Name], err = ask(fmt.Sprintf("%s (example: %s)", ph.Label, ph.Example)); err != nil {
			return nil, err
		}
	}
	return pc, nil
}

// openBrowser prints the address when no browser opens, and returns nil so the sign-in keeps waiting.
func openBrowser(u string) error {
	cmd := "xdg-open"
	if runtime.GOOS == "darwin" {
		cmd = "open"
	}
	if err := exec.Command(cmd, u).Start(); err != nil {
		fmt.Printf("could not open the browser (%v); open this address yourself:\n%s\n", err, u)
	}
	return nil
}

// withSecond folds the second account's result into the first: a failing second account fails the preset,
// and tool definitions are compared only when both accounts listed their tools.
func withSecond(r, r2 Result) Result {
	if !r2.Pass {
		r.Pass, r.Error = false, "second account: "+r2.Error
		return r
	}
	r.DiffersBySecondAccount = fingerprintDiff(r.fingerprints, r2.fingerprints)
	return r
}
