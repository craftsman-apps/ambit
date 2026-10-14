package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/craftsman-apps/ambit/internal/cli/connect"
	"github.com/craftsman-apps/ambit/internal/cli/kube"
	"github.com/craftsman-apps/ambit/internal/cli/local"
	"github.com/craftsman-apps/ambit/internal/cli/out"
)

var connectTools = []string{connect.ClaudeCode, connect.Codex, connect.OpenCode, connect.Cursor}

// ambitKeyFormat is what NewSecretKey produces.
var ambitKeyFormat = regexp.MustCompile(`^ambit_[0-9A-Za-z]{43}$`)

type connectFlags struct {
	url, model, consoleURL, keyFile, keyStore, shell, service string
	replaceKey, dryRun, force, noTest                         bool
}

type disconnectFlags struct {
	all, keepKey, dryRun bool
	consoleURL, service  string
}

// job is one config file of a tool: what it holds now and what the plan makes of it.
type job struct {
	path   string
	before []byte
	exists bool
	prev   *connect.FileRecord
	plan   *connect.Plan         // connect
	undo   *connect.Disconnected // disconnect
	rec    connect.FileRecord    // disconnect: what connect recorded
}

func (j *job) change() (connect.FileChange, bool) {
	c := connect.FileChange{Path: j.path, Before: j.before, Exists: j.exists}
	switch {
	case j.plan != nil:
		if j.plan.After == nil || j.plan.Already {
			return c, false
		}
		c.After = j.plan.After
		if j.prev != nil && len(j.prev.RemovedSecrets) > 0 {
			c.KeepBackup = true // the first backup is the only place that holds a secret connect removed
		}
	case j.undo != nil:
		if !j.exists || (!j.undo.Delete && string(j.undo.After) == string(j.before)) {
			return c, false
		}
		c.After, c.Delete, c.RemoveDirs = j.undo.After, j.undo.Delete, j.undo.RemoveDirs
	}
	return c, true
}

func readConfig(path string) ([]byte, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, &out.Error{What: "cannot read " + path, Why: err.Error(), Next: "check the file, then run the command again", Code: out.ExitFailed}
	}
	return b, true, nil
}

func prevRecord(st *connect.State, tool, path string) *connect.FileRecord {
	for i, f := range st.Tools[tool].Files {
		if f.Path == path {
			return &st.Tools[tool].Files[i]
		}
	}
	return nil
}

// planErr turns an error of a plan function into What, Why and next lines.
func planErr(err error) error {
	var e *out.Error
	if errors.As(err, &e) {
		return err
	}
	what, why, ok := strings.Cut(err.Error(), " — ")
	if !ok {
		what, why = err.Error(), "ambit cannot change this file"
	}
	return &out.Error{What: what, Why: why, Next: "fix the file, then run the command again", Code: out.ExitFailed}
}

func (a *app) failLines(lines []string, next ...string) error {
	p := a.printer()
	for _, l := range lines {
		p.Line("%s %s", p.Mark("fail"), l)
	}
	for _, n := range next {
		p.Line("  next: %s", n)
	}
	return exitCode(out.ExitFailed)
}

func storeName(goos, store string, fs connect.Store, url string) string {
	if store == connect.StoreFile {
		if f, ok := fs.(connect.FileStore); ok {
			return "the key file " + f.Path(url)
		}
		return "the key file"
	}
	switch goos {
	case "darwin":
		return "macOS Keychain"
	case "windows":
		return "Windows Credential Manager"
	}
	return "Secret Service"
}

func diffText(path string, before []byte, exists bool, after []byte, deleted bool) string {
	from, to := "a "+path, "b "+path
	if !exists {
		from, to = "/dev/null", path+" (new file)"
	}
	if deleted {
		to = path + " (file would be deleted)"
		after = nil
	}
	d, _ := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{A: difflib.SplitLines(string(before)), B: difflib.SplitLines(string(after)),
		FromFile: from, ToFile: to, Context: 3})
	return strings.TrimRight(d, "\n")
}

func validTool(t string) bool {
	for _, v := range connectTools {
		if v == t {
			return true
		}
	}
	return false
}

func (a *app) toolFiles(d *connectDeps, tool string, f connectFlags) (jobs []*job, notes []string, shell connect.Shell, err error) {
	switch tool {
	case connect.ClaudeCode:
		jobs = append(jobs, &job{path: connect.ClaudeSettingsPath(d.Getenv, d.Home)})
	case connect.Codex:
		jobs = append(jobs, &job{path: connect.CodexConfigPath(d.Getenv, d.Home)})
	case connect.OpenCode:
		path, err := connect.OpenCodePath(d.Getenv, d.Home, func(p string) bool { _, err := os.Stat(p); return err == nil })
		if err != nil {
			return nil, nil, "", err
		}
		jobs = append(jobs, &job{path: path})
		shell = connect.DetectShell(d.GOOS, d.Getenv("SHELL"))
		if f.shell != "" {
			if shell, err = connect.ParseShell(f.shell); err != nil {
				return nil, nil, "", out.Usage("--shell "+f.shell, err.Error(), "ambit connect opencode --help")
			}
		}
		if shell != connect.NoShell {
			sp, err := connect.ShellFile(shell, d.GOOS, d.Getenv, d.Home, d.Profile)
			if err != nil {
				notes = append(notes, "ambit cannot find the "+string(shell)+" profile ("+err.Error()+"); add the function below by hand")
				shell = connect.NoShell
			} else {
				jobs = append(jobs, &job{path: sp})
			}
		}
	}
	return jobs, notes, shell, nil
}

// runConnect is the flow of spec section 3.1.
func (a *app) runConnect(ctx context.Context, tool string, f connectFlags) error {
	p := a.printer()
	if !validTool(tool) {
		return out.Usage("unknown tool "+tool, "choose claude-code, codex, opencode or cursor", "ambit connect --help")
	}
	if f.url == "" {
		return out.Usage("--url", "is required", "ambit connect "+tool+" --url https://llm.example.com --model <model id>")
	}
	url, err := connect.NormalizeURL(f.url)
	if err != nil {
		return out.Usage("--url "+f.url, err.Error(), "ambit connect "+tool+" --url https://llm.example.com --model <model id>")
	}
	if tool != connect.Cursor && f.model == "" {
		return out.Usage("--model", "is required", "ambit connect "+tool+" --url "+url+" --model <model id>")
	}
	if f.keyStore != "" && f.keyStore != connect.StoreKeychain && f.keyStore != connect.StoreFile {
		return out.Usage("--key-store "+f.keyStore, "must be keychain or file", "ambit connect "+tool+" --help")
	}
	d, err := a.connectEnv(f.service)
	if err != nil {
		return err
	}
	st, err := connect.LoadState(d.StatePath)
	if err != nil {
		return err
	}
	if tool == connect.Cursor {
		return a.connectCursor(d, st, url, f)
	}
	ambitPath, err := d.Bin.Path()
	if err != nil {
		return err
	}
	jobs, notes, shell, err := a.toolFiles(d, tool, f)
	if err != nil {
		return err
	}

	// 2 and 3: read the files, plan, find conflicts.
	var conflicts []string
	for i, j := range jobs {
		if j.before, j.exists, err = readConfig(j.path); err != nil {
			return err
		}
		j.prev = prevRecord(st, tool, j.path)
		in := connect.Input{Path: j.path, File: j.before, Exists: j.exists, URL: url, Model: f.model, AmbitPath: ambitPath, GOOS: d.GOOS,
			Force: f.force, Getenv: d.Getenv, Prev: j.prev}
		switch {
		case tool == connect.ClaudeCode:
			j.plan, err = connect.PlanClaudeCode(in)
		case tool == connect.Codex:
			j.plan, err = connect.PlanCodex(in)
		case i == 0:
			j.plan, err = connect.PlanOpenCode(in)
		default:
			in.Shell = string(shell)
			j.plan, err = connect.PlanShell(in)
		}
		if err != nil {
			return planErr(err)
		}
		for _, c := range j.plan.Conflicts {
			conflicts = append(conflicts, j.path+": "+c.String())
		}
		if !j.exists {
			if dir := filepath.Dir(j.path); !pathExists(dir) {
				notes = append(notes, dir+" does not exist; ambit creates it. If "+tool+" is not installed yet, install it later")
			}
		}
	}
	if len(conflicts) > 0 && !f.force {
		args := "ambit connect " + tool + " --url " + url + " --model " + f.model + " --force"
		err := a.failLines(conflicts, args)
		p.Line("  --force replaces these entries. The old value is kept so that ambit disconnect puts it back; a secret is kept in the backup file only.")
		return err
	}
	already := true
	for _, j := range jobs {
		already = already && j.plan.Already
	}

	// 4: the key.
	var key string
	reused := false
	if !f.dryRun {
		if key, reused, err = a.resolveKey(ctx, d, st, url, f); err != nil {
			return err
		}
	} else {
		_, reused = st.Keys[url]
		reused = reused && !f.replaceKey
	}

	// 5: show what would change.
	storeKind := f.keyStore
	if storeKind == "" {
		storeKind = connect.StoreKeychain
	}
	if reused {
		storeKind = st.Keys[url].Store
	}
	keyLine := fmt.Sprintf(`key: stored in %s (service "%s", account "%s")`, storeName(d.GOOS, storeKind, d.File, url), serviceName(f.service), url)
	if storeKind == connect.StoreFile {
		keyLine = "key: stored in " + storeName(d.GOOS, storeKind, d.File, url)
	}
	if reused {
		keyLine = "key: reused (stored for " + url + ")"
	}
	if !already {
		for _, j := range jobs {
			if _, changes := j.change(); !changes {
				p.Line("%s: no change", j.path)
				continue
			}
			p.Line("%s", j.path)
			p.Line("%s", diffText(j.path, j.before, j.exists, j.plan.After, false))
			if j.exists {
				p.Line("backup: %s.ambit.bak", j.path)
			}
		}
	}
	for _, j := range jobs {
		for _, w := range j.plan.Warnings {
			p.Line("%s %s", p.Mark("warn"), w)
		}
	}
	for _, n := range notes {
		p.Line("%s %s", p.Mark("warn"), n)
	}
	p.Line("%s", keyLine)
	if shell == connect.NoShell && tool == connect.OpenCode {
		a.printWrapper(d, url, ambitPath)
	}
	if f.dryRun {
		p.Line("%s dry run: no problems found", p.Mark("ok"))
		return nil
	}
	if already {
		p.Line("%s %s is already connected to %s", p.Mark("ok"), tool, url)
	} else if err := kube.Confirm(p, a.in, a.stdinTTY(), a.g.yes, nil, "Write these changes?"); err != nil {
		return err
	}

	// 7: store the key, write the files, then write the state file once. A failed write takes a key stored in this
	// run out again, so the key, the files and the state file agree after a failure.
	newStore := ""
	if !reused {
		store, warning, err := connect.Put(d.Keychain, d.File, d.GOOS, f.keyStore, url, key)
		if err != nil {
			return &out.Error{What: "store the key", Why: err.Error(), Next: "unlock your keychain, or run the command again with --key-store file", Code: out.ExitFailed}
		}
		if warning != "" {
			p.Line("%s %s", p.Mark("warn"), warning)
		}
		kr := st.Keys[url]
		kr.Store, newStore = store, store
		st.SetKey(url, kr)
	}
	if f.consoleURL != "" {
		kr := st.Keys[url]
		kr.ConsoleURL = strings.TrimRight(f.consoleURL, "/")
		st.SetKey(url, kr)
	}
	var changes []connect.FileChange
	var written []*job
	for _, j := range jobs {
		if c, ok := j.change(); ok {
			changes, written = append(changes, c), append(written, j)
		}
	}
	results, err := connect.WriteAll(changes)
	if err != nil {
		if newStore != "" {
			if s, ok := d.store(st, url); ok {
				if derr := s.Delete(url); derr != nil {
					err = fmt.Errorf("%w; the key stored in this run could not be deleted again (%v)", err, derr)
				}
			}
		}
		return err
	}
	if newStore != "" {
		p.Line("%s key stored in %s", p.Mark("ok"), storeName(d.GOOS, newStore, d.File, url))
	}
	for i, j := range written {
		r := results[i]
		j.plan.Record.Backup = r.Backup
		if r.Created {
			j.plan.Record.Created, j.plan.Record.Dirs = true, r.Dirs
		}
		p.Line("%s wrote %s", p.Mark("ok"), j.path)
	}
	rec := connect.ToolRecord{URL: url}
	for _, j := range jobs {
		rec.Files = append(rec.Files, j.plan.Record)
	}
	for _, old := range st.Tools[tool].Files { // a file of an earlier connect that this one does not use is still Ambit's
		if !containsPath(jobs, old.Path) {
			rec.Files = append(rec.Files, old)
		}
	}
	st.Tools[tool] = rec
	if err := st.Save(d.StatePath); err != nil {
		return err
	}

	// 8: the test call.
	if !f.noTest {
		if err := a.testCall(ctx, d, tool, url, f.model, key); err != nil {
			return err
		}
	}
	a.nextSteps(tool, url, shell)
	return nil
}

func containsPath(jobs []*job, path string) bool {
	for _, j := range jobs {
		if j.path == path {
			return true
		}
	}
	return false
}

func pathExists(p string) bool { _, err := os.Stat(p); return err == nil }

func serviceName(s string) string {
	if s == "" {
		return defaultKeychainService
	}
	return s
}

// resolveKey is step 4 of spec section 3.1: the stored key, else --key-file, else AMBIT_API_KEY, else the prompt.
func (a *app) resolveKey(ctx context.Context, d *connectDeps, st *connect.State, url string, f connectFlags) (key string, reused bool, err error) {
	p := a.printer()
	if _, ok := st.Keys[url]; ok && !f.replaceKey {
		if s, ok := d.store(st, url); ok {
			switch k, gerr := s.Get(url); {
			case gerr == nil:
				p.Redactor().Add(k)
				return k, true, nil
			case !errors.Is(gerr, connect.ErrNotFound):
				return "", false, &out.Error{What: "read the stored key of " + url, Why: gerr.Error(), Next: "unlock your keychain, then run the command again", Code: out.ExitFailed}
			}
		}
	}
	switch {
	case f.keyFile != "":
		b, rerr := os.ReadFile(f.keyFile)
		if rerr != nil {
			return "", false, out.Usage("--key-file", rerr.Error(), "check the path")
		}
		key = strings.TrimSpace(strings.TrimPrefix(string(b), "\xef\xbb\xbf"))
	case d.Getenv("AMBIT_API_KEY") != "":
		key = strings.TrimSpace(d.Getenv("AMBIT_API_KEY"))
	case d.ReadSecret != nil:
		if key, err = d.ReadSecret(ctx, "Ambit key (created in the console, starts with ambit_): "); err != nil {
			return "", false, err
		}
		key = strings.TrimSpace(key)
	default:
		return "", false, out.Usage("no key", "no key is stored for "+url+" and there is no terminal to ask in", "pass --key-file <path>, or set AMBIT_API_KEY")
	}
	p.Redactor().Add(key)
	if !ambitKeyFormat.MatchString(key) {
		return "", false, out.Usage("this is not an Ambit key", "an Ambit key starts with ambit_ and has 49 characters", "create a key in the console (Keys page)")
	}
	return key, false, nil
}

func (a *app) printWrapper(d *connectDeps, url, ambitPath string) {
	p := a.printer()
	sh := connect.Zsh
	if d.GOOS == "windows" {
		sh = connect.PowerShell
	}
	p.Line("%s opencode has no setting for a key command. Add this function to your shell profile, so that opencode gets the key:", p.Mark("warn"))
	for _, l := range connect.WrapperLines(sh, d.GOOS, ambitPath, url) {
		p.Line("  %s", l)
	}
}

var testFormats = map[string]local.Format{connect.ClaudeCode: local.FormatMessages, connect.Codex: local.FormatResponses, connect.OpenCode: local.FormatChat}

func (a *app) testCall(ctx context.Context, d *connectDeps, tool, url, model, key string) error {
	p := a.printer()
	start := time.Now()
	ans, err := local.CallWith(ctx, d.HTTP(url), url, key, model, testFormats[tool], d.CallLimit, d.CallEvery)
	if err == nil {
		p.Line("%s test call: %s (%s)", p.Mark("ok"), ans, time.Since(start).Round(100*time.Millisecond))
		return nil
	}
	e := &out.Error{What: "the test call failed", Why: err.Error(), Code: out.ExitFailed}
	var re *local.RefusedError
	var na *local.NoAnswerError
	var pe *local.ProviderError
	switch {
	case errors.As(err, &re), errors.As(err, &na) && (na.LastStatus == 401 || na.LastStatus == 403):
		e.What, e.Why = "the test call was refused", "the key was refused, or no access rule allows "+model
		e.Next = "check the key in the console (Settings > Keys) and your access rules"
	case errors.As(err, &na) && na.LastStatus == 404:
		e.What, e.Why = "the test call failed", "the gateway does not know "+model
		e.Next = "check the model ID, then run the command again"
	case errors.As(err, &na) && na.LastStatus == 0:
		e.What, e.Why = "cannot reach "+url, na.Last
		e.Next = "check the address and your network, then run the command again"
	case errors.As(err, &pe):
		e.What, e.Why = "the test call failed", "the model's provider answered "+pe.Error()
		e.Next = "try again later, or choose another model"
	}
	p.Fail(e)
	p.Line("to undo: ambit disconnect %s", tool)
	return exitCode(out.ExitFailed)
}

func (a *app) nextSteps(tool, url string, shell connect.Shell) {
	p := a.printer()
	switch tool {
	case connect.ClaudeCode:
		p.Line("Restart Claude Code, then run /status: \"Anthropic base URL\" must show %s.", url)
	case connect.Codex:
		p.Line("Start codex and send one prompt.")
	case connect.OpenCode:
		if shell != connect.NoShell {
			p.Line("Open a new terminal before you start opencode. The function works only when opencode is started from that shell; an editor that starts opencode by itself does not see it.")
		} else {
			p.Line("Start opencode from a terminal that has the function above.")
		}
	}
}

func (a *app) connectCursor(d *connectDeps, st *connect.State, url string, f connectFlags) error {
	p := a.printer()
	_, stored := st.Keys[url]
	g := connect.ConnectCursor(url, f.model, stored)
	p.Line("Cursor has no settings file for this. Do these steps in Cursor:")
	for i, s := range g.Steps {
		p.Line("%d. %s", i+1, s)
	}
	for _, v := range g.Values {
		p.Line("  %s: %s", v[0], v[1])
	}
	if g.Warning != "" {
		p.Line("%s %s", p.Mark("warn"), g.Warning)
	}
	if g.KeyHint != "" {
		p.Line("%s", g.KeyHint)
	}
	if !f.dryRun && !st.CursorShown {
		st.CursorShown = true
		return st.Save(d.StatePath)
	}
	return nil
}

// runDisconnect is the flow of spec section 3.2.
func (a *app) runDisconnect(ctx context.Context, args []string, f disconnectFlags) error {
	p := a.printer()
	switch {
	case f.all && len(args) > 0:
		return out.Usage("--all and "+args[0], "cannot be used together", "ambit disconnect --all, or ambit disconnect <tool>")
	case !f.all && len(args) == 0:
		return out.Usage("disconnect", "name a tool, or use --all", "ambit disconnect claude-code, or ambit disconnect --all")
	case len(args) > 0 && !validTool(args[0]):
		return out.Usage("unknown tool "+args[0], "choose claude-code, codex, opencode or cursor", "ambit disconnect --help")
	}
	d, err := a.connectEnv(f.service)
	if err != nil {
		return err
	}
	st, err := connect.LoadState(d.StatePath)
	if err != nil {
		return err
	}
	var tools []string
	if f.all {
		for t := range st.Tools {
			tools = append(tools, t)
		}
		sort.Strings(tools)
	} else {
		tools = args[:1]
	}

	type toolJobs struct {
		tool string
		jobs []*job
	}
	var all []toolJobs
	var lines []string
	for _, tool := range tools {
		if tool == connect.Cursor {
			continue
		}
		tr, recorded := st.Tools[tool]
		var jobs []*job
		if recorded {
			for _, rec := range tr.Files {
				j := &job{path: rec.Path, rec: rec}
				if j.before, j.exists, err = readConfig(j.path); err != nil {
					return err
				}
				if len(rec.Blocks) > 0 || len(rec.Wrote) > 0 {
					j.undo, err = connect.UnplanBlocks(&rec, j.before, j.exists)
				} else {
					j.undo, err = connect.UnplanJSON(rec, j.before, j.exists)
				}
				if err != nil {
					return planErr(err)
				}
				jobs = append(jobs, j)
			}
		} else if jobs, err = a.unrecordedBlocks(d, tool); err != nil {
			return err
		}
		if !recorded && len(jobs) == 0 {
			lines = append(lines, fmt.Sprintf("%s %s is not connected (nothing recorded)", p.Mark("ok"), tool))
		}
		all = append(all, toolJobs{tool, jobs})
	}
	for _, l := range lines {
		p.Line("%s", l)
	}
	var changes []connect.FileChange
	var changed []*job
	for _, tj := range all {
		for _, j := range tj.jobs {
			if c, ok := j.change(); ok {
				changes, changed = append(changes, c), append(changed, j)
				p.Line("%s", j.path)
				p.Line("%s", diffText(j.path, j.before, true, j.undo.After, j.undo.Delete))
			}
			for _, n := range j.undo.Notes {
				p.Line("%s %s", p.Mark("warn"), n)
			}
		}
	}
	showCursor := false
	if f.all && st.CursorShown {
		showCursor = true
	}
	if len(args) > 0 && args[0] == connect.Cursor {
		showCursor = true
	}
	if showCursor {
		p.Line("Cursor has no settings file. To undo it by hand, in Cursor:")
		for i, s := range connect.DisconnectCursor() {
			p.Line("%d. %s", i+1, s)
		}
	}
	if f.dryRun {
		p.Line("%s dry run: no problems found", p.Mark("ok"))
		return nil
	}
	if len(changes) > 0 {
		if err := kube.Confirm(p, a.in, a.stdinTTY(), a.g.yes, nil, "Remove these changes?"); err != nil {
			return err
		}
		if _, err := connect.WriteAll(changes); err != nil {
			return err
		}
		for _, j := range changed {
			verb := "restored"
			if j.undo.Delete {
				verb = "deleted"
			}
			p.Line("%s %s %s", p.Mark("ok"), verb, j.path)
		}
	}
	urls := map[string]bool{}
	for _, tj := range all {
		if tr, ok := st.Tools[tj.tool]; ok {
			urls[tr.URL] = true
			delete(st.Tools, tj.tool)
		}
	}
	if showCursor {
		st.CursorShown = false
	}
	if f.all { // a key that no tool uses any more, left by a failed deletion or an earlier update
		for u := range st.Keys {
			urls[u] = true
		}
	}
	// The state file goes first: if deleting a key fails, it lists the key that is still stored and no tool.
	if err := st.Save(d.StatePath); err != nil {
		return err
	}
	var keyLines []string
	var urlList []string
	for u := range urls {
		urlList = append(urlList, u)
	}
	sort.Strings(urlList)
	for _, u := range urlList {
		kr, hasKey := st.Keys[u]
		if st.UsesURL(u, "") || f.keepKey || !hasKey {
			continue
		}
		if s, ok := d.store(st, u); ok {
			if err := s.Delete(u); err != nil {
				return &out.Error{What: "delete the stored key of " + u, Why: err.Error(), Next: "unlock your keychain, then run the command again", Code: out.ExitFailed}
			}
		}
		delete(st.Keys, u)
		if err := st.Save(d.StatePath); err != nil {
			return err
		}
		keyLines = append(keyLines, fmt.Sprintf("%s key for %s deleted from %s", p.Mark("ok"), u, storeName(d.GOOS, kr.Store, d.File, u)))
		console := strings.TrimRight(f.consoleURL, "/")
		if console == "" {
			console = kr.ConsoleURL
		}
		if console != "" {
			keyLines = append(keyLines, "  next: revoke the key in the console: "+console+"/keys (deleting it here does not revoke it)")
		} else {
			keyLines = append(keyLines, "  next: revoke the key in the console (Settings > Keys); deleting it here does not revoke it")
		}
	}
	for _, l := range keyLines {
		p.Line("%s", l)
	}
	return nil
}

// unrecordedBlocks looks for marked blocks of a tool that the state file does not know: the markers prove that
// the lines are Ambit's. Only Codex and opencode have them.
func (a *app) unrecordedBlocks(d *connectDeps, tool string) ([]*job, error) {
	var paths []string
	switch tool {
	case connect.Codex:
		paths = append(paths, connect.CodexConfigPath(d.Getenv, d.Home))
	case connect.OpenCode:
		if sh := connect.DetectShell(d.GOOS, d.Getenv("SHELL")); sh != connect.NoShell {
			if sp, err := connect.ShellFile(sh, d.GOOS, d.Getenv, d.Home, d.Profile); err == nil {
				paths = append(paths, sp)
			}
		}
	}
	var jobs []*job
	for _, path := range paths {
		b, exists, err := readConfig(path)
		if err != nil {
			return nil, err
		}
		if !exists || !strings.Contains(string(b), "# >>> ambit") {
			continue
		}
		u, err := connect.UnplanBlocks(nil, b, true)
		if err != nil {
			return nil, planErr(err)
		}
		jobs = append(jobs, &job{path: path, before: b, exists: true, undo: u})
	}
	return jobs, nil
}
