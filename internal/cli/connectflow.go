package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/craftsman-apps/ambit/internal/cli/connect"
	"github.com/craftsman-apps/ambit/internal/cli/kube"
	"github.com/craftsman-apps/ambit/internal/cli/local"
	"github.com/craftsman-apps/ambit/internal/cli/out"
)

var connectTools = []string{connect.ClaudeCode, connect.Codex, connect.OpenCode, connect.Cursor, connect.ClaudeDesktopMCP,
	connect.Hermes, connect.KiloCode, connect.Pi}

// toolChoice is the list of tools for usage errors.
const toolChoice = "choose claude-code, codex, opencode, cursor, claude-desktop-mcp, hermes, kilocode or pi"

// ambitKeyFormat is what NewSecretKey produces.
var ambitKeyFormat = regexp.MustCompile(`^ambit_[0-9A-Za-z]{43}$`)

type connectFlags struct {
	url, model, consoleURL, keyFile, keyStore, shell, service, mcpURL string
	replaceKey, dryRun, force, noTest                                 bool
}

type disconnectFlags struct {
	all, keepKey, dryRun, revoke bool
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
	secret bool                  // the file holds the key: never print its content
	// planner plans the file (hermes, kilocode, pi); nil: the switch in runConnect does
	planner func(connect.Input) (*connect.Plan, error)
}

func (j *job) change() (connect.FileChange, bool) {
	c := connect.FileChange{Path: j.path, Before: j.before, Exists: j.exists}
	switch {
	case j.plan != nil:
		if j.plan.After == nil || j.plan.Already {
			return c, false
		}
		c.After = j.plan.After
		c.Private = j.secret
		if j.secret && j.prev != nil && j.prev.Backup != "" {
			c.KeepBackup = true // the backup holds the header file that connect replaced, maybe with another key
		}
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

func (a *app) toolFiles(d *connectDeps, tool string, f connectFlags, oc2 bool) (jobs []*job, notes []string, shell connect.Shell, err error) {
	switch tool {
	case connect.ClaudeCode:
		jobs = append(jobs, &job{path: connect.ClaudeSettingsPath(d.Getenv, d.Home)})
	case connect.Codex:
		jobs = append(jobs, &job{path: connect.CodexConfigPath(d.Getenv, d.Home)})
	case connect.ClaudeDesktopMCP:
		jobs = append(jobs, &job{path: connect.ClaudeDesktopConfigPath(d.GOOS, d.Getenv, d.Home)}, &job{path: connect.MCPHeaderPath(d.Home), secret: true})
	case connect.Hermes:
		header := connect.HermesHeaderPath(d.Home)
		jobs = append(jobs, &job{path: connect.HermesConfigPath(d.GOOS, d.Getenv, d.Home),
			planner: func(in connect.Input) (*connect.Plan, error) { return connect.PlanHermes(in, header) }})
		if f.mcpURL != "" {
			jobs = append(jobs, &job{path: header, secret: true})
		}
	case connect.KiloCode:
		key := connect.KiloKeyPath(d.Getenv, d.Home)
		jobs = append(jobs, &job{path: connect.KiloPath(d.Getenv, d.Home, pathExists),
			planner: func(in connect.Input) (*connect.Plan, error) { in.KeyFile = key; return connect.PlanKilo(in) }},
			&job{path: key, secret: true})
	case connect.Pi:
		dir := connect.PiDir(d.Getenv, d.Home)
		jobs = append(jobs, &job{path: filepath.Join(dir, "models.json"), planner: connect.PlanPiModels},
			&job{path: filepath.Join(dir, "settings.json"), planner: connect.PlanPiSettings})
		if f.mcpURL != "" {
			jobs = append(jobs, &job{path: filepath.Join(dir, "mcp.json"), planner: connect.PlanPiMCP})
		}
	case connect.OpenCode:
		path, err := connect.OpenCodePath(d.Getenv, d.Home, func(p string) bool { _, err := os.Stat(p); return err == nil })
		if err != nil {
			return nil, nil, "", err
		}
		jobs = append(jobs, &job{path: path})
		if oc2 { // the key file instead of the shell function
			jobs = append(jobs, &job{path: connect.OpenCodeKeyPath(d.Getenv, d.Home), secret: true})
			if f.shell != "" {
				notes = append(notes, "--shell is not used with opencode 2: it reads the key from a file")
			}
			return jobs, notes, "", nil
		}
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
		return out.Usage("unknown tool "+tool, toolChoice, "ambit connect --help")
	}
	if f.url == "" {
		return out.Usage("--url", "is required", "ambit connect "+tool+" --url https://llm.example.com --model <model id>")
	}
	url, err := connect.NormalizeURL(f.url)
	if err != nil {
		return out.Usage("--url "+f.url, err.Error(), "ambit connect "+tool+" --url https://llm.example.com --model <model id>")
	}
	if tool != connect.Cursor && tool != connect.ClaudeDesktopMCP && f.model == "" {
		return out.Usage("--model", "is required", "ambit connect "+tool+" --url "+url+" --model <model id>")
	}
	if f.keyStore != "" && f.keyStore != connect.StoreKeychain && f.keyStore != connect.StoreFile {
		return out.Usage("--key-store "+f.keyStore, "must be keychain or file", "ambit connect "+tool+" --help")
	}
	if f.mcpURL != "" {
		if tool != connect.Hermes && tool != connect.KiloCode && tool != connect.Pi {
			return out.Usage("--mcp-url", "is used only by hermes, kilocode and pi", "ambit connect "+tool+" --help")
		}
		if f.mcpURL, err = connect.NormalizeURL(f.mcpURL); err != nil {
			return out.Usage("--mcp-url", err.Error(), "ambit connect "+tool+" --url "+url+" --model <model id> --mcp-url https://mcp.example.com/mcp")
		}
	}
	d, err := a.connectEnv(f.service)
	if err != nil {
		return err
	}
	unlock, err := connect.LockState(d.StatePath)
	if err != nil {
		return &out.Error{What: "cannot change " + d.StatePath, Why: err.Error(), Code: out.ExitFailed}
	}
	defer unlock()
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
	oc2, ocNote := false, ""
	if tool == connect.OpenCode {
		oc2, ocNote = openCode2(d)
	}
	jobs, notes, shell, err := a.toolFiles(d, tool, f, oc2)
	if err != nil {
		return err
	}
	if ocNote != "" {
		notes = append(notes, ocNote)
	}

	// 2 and 3: read the files, plan, find conflicts.
	var conflicts []string
	for i, j := range jobs {
		if j.before, j.exists, err = readConfig(j.path); err != nil {
			return err
		}
		j.prev = prevRecord(st, tool, j.path)
		in := connect.Input{Path: j.path, File: j.before, Exists: j.exists, URL: url, Model: f.model, AmbitPath: ambitPath, GOOS: d.GOOS,
			Force: f.force, Getenv: d.Getenv, Prev: j.prev, MCPURL: f.mcpURL}
		switch {
		case j.secret:
			j.plan = planSecretFile(j)
		case j.planner != nil:
			j.plan, err = j.planner(in)
		case tool == connect.ClaudeDesktopMCP:
			j.plan, err = connect.PlanClaudeDesktopMCP(in, jobs[1].path)
		case tool == connect.ClaudeCode:
			j.plan, err = connect.PlanClaudeCode(in)
		case tool == connect.Codex:
			j.plan, err = connect.PlanCodex(in)
		case i == 0:
			if oc2 {
				in.KeyFile = jobs[1].path
			}
			j.plan, err = connect.PlanOpenCode(in)
		default:
			in.Shell = string(shell)
			j.plan, err = connect.PlanShell(in)
		}
		if err != nil {
			return planErr(err)
		}
		if j.secret && j.exists && !j.plan.Already {
			j.plan.Conflicts = append(j.plan.Conflicts, connect.Conflict{What: "the file exists and ambit did not write it", Why: "it may hold another key; --force replaces it (the old file is kept in " + j.path + ".ambit.bak)"})
		}
		for _, c := range j.plan.Conflicts {
			conflicts = append(conflicts, j.path+": "+c.String())
		}
		if !j.exists && !j.secret { // the folder of a key file says nothing about the tool
			if dir := filepath.Dir(j.path); !pathExists(dir) {
				notes = append(notes, dir+" does not exist; ambit creates it. If "+tool+" is not installed yet, install it later")
			}
		}
	}
	if tool == connect.ClaudeDesktopMCP && f.model != "" {
		notes = append(notes, "--model is not used by claude-desktop-mcp")
	}
	var drops []*job // opencode 2: the opencode function of an earlier connect comes out of the shell profiles
	if oc2 {
		if drops, err = a.openCodeFunctions(d, st); err != nil {
			return err
		}
	}
	if len(conflicts) > 0 && !f.force {
		args := "ambit connect " + tool + " --url " + url + " --model " + f.model + " --force"
		if tool == connect.ClaudeDesktopMCP {
			args = "ambit connect " + tool + " --url " + url + " --force"
		}
		if f.mcpURL != "" {
			args += " --mcp-url " + f.mcpURL
		}
		err := a.failLines(conflicts, args)
		p.Line("  --force replaces these entries. The old value is kept so that ambit disconnect puts it back; a secret is kept in the backup file only.")
		return err
	}
	already := true
	for _, j := range jobs {
		already = already && j.plan.Already
	}
	for _, j := range drops {
		_, changes := j.change()
		already = already && !changes
	}

	// 4: the key.
	var key string
	reused := false
	if !f.dryRun {
		if key, reused, err = a.resolveKey(ctx, d, st, url, f); err != nil {
			return err
		}
		for _, j := range jobs {
			if j.secret { // the header file holds the key, so it is planned only now
				j.plan.After = connect.SecretFile(tool, key)
				j.plan.Record.SHA256 = connect.FileSum(j.plan.After)
				if string(j.before) != string(j.plan.After) {
					j.plan.Already, already = false, false
				}
			}
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
			if j.secret && !j.plan.Already {
				p.Line("%s: holds the key, readable only by you (mode 0600)", j.path)
				continue
			}
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
		for _, j := range drops {
			if _, changes := j.change(); changes {
				p.Line("%s: opencode 2 does not use the opencode function; ambit removes it", j.path)
				p.Line("%s", diffText(j.path, j.before, true, j.undo.After, j.undo.Delete))
			}
		}
	}
	for _, j := range drops {
		for _, n := range j.undo.Notes {
			p.Line("%s %s", p.Mark("warn"), n)
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
	if shell == connect.PowerShell && d.ScriptPolicy != nil {
		// Windows PowerShell 5.1 starts with Restricted: it does not load the profile that holds the function.
		// The policy is the person's to change, so connect only says how.
		if pol, err := d.ScriptPolicy(); err == nil && connect.PolicyBlocksProfile(pol) {
			p.Line("%s PowerShell does not run profile scripts (execution policy %s), so the opencode function will not load", p.Mark("warn"), pol)
			p.Line("  next: Set-ExecutionPolicy -Scope CurrentUser RemoteSigned")
		}
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

	// 7: save the plan marked pending, store the key, write the files, then save the state file again without the
	// mark. A crash or a failed save in between leaves a record that disconnect (and connect) can finish or undo.
	// A failed write takes a key stored in this run out again, so the key, the files and the state file agree.
	var changes []connect.FileChange
	var written []*job
	for _, j := range jobs {
		if c, ok := j.change(); ok {
			changes, written = append(changes, c), append(written, j)
		}
	}
	var removed []*job
	for _, j := range drops {
		if c, ok := j.change(); ok {
			changes, removed = append(changes, c), append(removed, j)
		}
	}
	newStore := ""
	oldTool, hadTool := st.Tools[tool]
	oldKey, hadKey := st.Keys[url]
	if !reused {
		newStore = f.keyStore
		if newStore == "" {
			newStore = connect.StoreKeychain
		}
		st.SetKey(url, connect.KeyRecord{Store: newStore})
	}
	if f.consoleURL != "" {
		kr := st.Keys[url]
		kr.ConsoleURL = strings.TrimRight(f.consoleURL, "/")
		st.SetKey(url, kr)
	}
	st.Tools[tool] = toolRecord(st, tool, url, jobs, drops, true)
	if err := d.save(st); err != nil {
		return err
	}
	undoPending := func() { // the plan is not on disk any more: put the state file back as it was
		if hadTool {
			st.Tools[tool] = oldTool
		} else {
			delete(st.Tools, tool)
		}
		if hadKey {
			st.Keys[url] = oldKey
		} else {
			delete(st.Keys, url)
		}
		_ = d.save(st)
	}
	if !reused {
		store, warning, err := connect.Put(d.Keychain, d.File, d.GOOS, f.keyStore, url, key)
		if err != nil {
			e := &out.Error{What: "store the key", Why: err.Error(), Next: "unlock your keychain, or run the command again with --key-store file", Code: out.ExitFailed}
			var te *connect.TimeoutError
			if errors.As(err, &te) {
				e.Why += "; the key may still be stored"
				e.Next = "run ambit disconnect " + tool + " to remove it, then run connect again (or use --key-store file)"
				return e // the plan stays pending, so that disconnect finds the key
			}
			undoPending()
			return e
		}
		if warning != "" {
			p.Line("%s %s", p.Mark("warn"), warning)
		}
		if store != newStore {
			kr := st.Keys[url]
			kr.Store, newStore = store, store
			st.SetKey(url, kr)
		}
	}
	results, err := connect.WriteAll(changes)
	if err != nil {
		if newStore != "" {
			if s, ok := d.store(st, url); ok {
				if derr := s.Delete(url); derr != nil {
					return fmt.Errorf("%w; the key stored in this run could not be deleted again (%v); ambit disconnect %s removes it", err, derr, tool)
				}
			}
		}
		undoPending()
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
	for _, j := range removed {
		p.Line("%s removed the opencode function from %s", p.Mark("ok"), j.path)
	}
	st.Tools[tool] = toolRecord(st, tool, url, jobs, drops, false)
	if err := d.save(st); err != nil {
		return err
	}
	if old := oldTool.URL; hadTool && old != url && !st.UsesURL(old, "") {
		if kr, ok := st.Keys[old]; ok && !kr.Set { // no tool uses the key of the old address any more
			if err := a.deleteKey(d, st, old, "", oldTool.Pending, true); err != nil {
				p.Line("%s %s; ambit disconnect --all deletes it later", p.Mark("warn"), err.Error())
			}
		}
	}

	// 8: the test call.
	if !f.noTest && tool != connect.ClaudeDesktopMCP {
		if err := a.testCall(ctx, d, tool, url, f.model, key); err != nil {
			return err
		}
	}
	if oc2 {
		p.Line("opencode 2 reads the key from %s when its background service starts.", jobs[1].path)
		p.Line("If opencode is running, restart the service so that it reads the new settings: opencode service restart")
		return nil
	}
	a.nextSteps(tool, url, shell)
	return nil
}

// openCode2 reads `opencode --version`: opencode 2 gets the key file, opencode 1 the shell function. When opencode does
// not answer, connect sets up opencode 2, the version a new install gets, and says so.
func openCode2(d *connectDeps) (bool, string) {
	v, err := d.OpenCodeVersion()
	if err != nil {
		return true, "opencode did not answer opencode --version (" + err.Error() + "); ambit sets it up for opencode 2. For opencode 1, make sure opencode is on your PATH and run the command again"
	}
	switch m := connect.OpenCodeMajor(v); {
	case m == 1:
		return false, ""
	case m >= 2:
		return true, ""
	}
	return true, "opencode --version printed " + strconv.Quote(v) + "; ambit sets it up for opencode 2"
}

// openCodeFunctions are the jobs that take the opencode function of an earlier connect out of the shell profiles:
// the recorded ones, and any other profile with an Ambit block.
func (a *app) openCodeFunctions(d *connectDeps, st *connect.State) ([]*job, error) {
	var jobs []*job
	for _, rec := range st.Tools[connect.OpenCode].Files {
		if len(rec.Blocks) == 0 && len(rec.Wrote) == 0 {
			continue
		}
		j := &job{path: rec.Path, rec: rec}
		var err error
		if j.before, j.exists, err = readConfig(j.path); err != nil {
			return nil, err
		}
		if j.undo, err = connect.UnplanBlocks(&rec, j.before, j.exists); err != nil {
			return nil, planErr(err)
		}
		jobs = append(jobs, j)
	}
	more, err := a.unrecordedBlocks(d, connect.OpenCode)
	if err != nil {
		return nil, err
	}
	for _, j := range more {
		if !containsPath(jobs, j.path) {
			jobs = append(jobs, j)
		}
	}
	return jobs, nil
}

// toolRecord is what the state file says about tool after this connect. A file of an earlier connect that this one
// does not use is still Ambit's, unless this connect takes Ambit's lines out of it (drops) and none stay. While
// pending, the record keeps it: the write may not have happened.
func toolRecord(st *connect.State, tool, url string, jobs, drops []*job, pending bool) connect.ToolRecord {
	rec := connect.ToolRecord{URL: url, Pending: pending}
	for _, j := range jobs {
		rec.Files = append(rec.Files, j.plan.Record)
	}
	gone := func(path string) bool {
		for _, j := range drops {
			if j.path == path && len(j.undo.Notes) == 0 {
				return true
			}
		}
		return false
	}
	for _, old := range st.Tools[tool].Files {
		if !containsPath(jobs, old.Path) && (pending || !gone(old.Path)) {
			rec.Files = append(rec.Files, old)
		}
	}
	return rec
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

var testFormats = map[string]local.Format{connect.ClaudeCode: local.FormatMessages, connect.Codex: local.FormatResponses, connect.OpenCode: local.FormatChat,
	connect.Hermes: local.FormatChat, connect.KiloCode: local.FormatChat, connect.Pi: local.FormatChat}

func (a *app) testCall(ctx context.Context, d *connectDeps, tool, url, model, key string) error {
	p := a.printer()
	start := time.Now()
	ans, cut, err := local.CallWith(ctx, d.HTTP(url), url, key, model, testFormats[tool], d.CallLimit, d.CallEvery)
	if err == nil && cut {
		p.Line("%s test call: the gateway and %s answered (%s)", p.Mark("ok"), model, time.Since(start).Round(100*time.Millisecond))
		p.Line("  note: %s", local.NoTextNote)
		return nil
	}
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
	case connect.ClaudeDesktopMCP:
		p.Line("Quit Claude Desktop completely and start it again, then check Connectors for \"ambit\". Node.js (npx) must be installed.")
	case connect.Hermes:
		p.Line("Start hermes and send one prompt. If the Hermes gateway service runs, restart it: hermes gateway restart. Hermes runs ambit key print to read the key.")
	case connect.KiloCode:
		p.Line("Restart Kilo Code (in VS Code: Developer: Reload Window; the CLI: start kilo again), so that it reads the settings and the key file.")
	case connect.Pi:
		p.Line("Start pi and send one prompt. pi runs ambit key print to read the key.")
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
		return out.Usage("unknown tool "+args[0], toolChoice, "ambit disconnect --help")
	case f.revoke && f.keepKey:
		return out.Usage("--revoke and --keep-key", "cannot be used together: a revoked key is of no use", "ambit disconnect --help")
	}
	d, err := a.connectEnv(f.service)
	if err != nil {
		return err
	}
	unlock, err := connect.LockState(d.StatePath)
	if err != nil {
		return &out.Error{What: "cannot change " + d.StatePath, Why: err.Error(), Code: out.ExitFailed}
	}
	defer unlock()
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
	deleting := keysToDelete(st, tools, f)
	if f.revoke {
		for _, u := range deleting {
			if consoleFor(f.consoleURL, st.Keys[u]) == "" {
				return out.Usage("--revoke", "needs the console address for the key of "+u, "ambit disconnect --revoke --console-url https://console.example.com")
			}
		}
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
				j.secret = rec.Secret
				switch {
				case rec.Secret:
					j.undo = connect.UnplanSecretFile(rec, j.before, j.exists)
				case len(rec.Blocks) > 0 || len(rec.Wrote) > 0:
					j.undo, err = connect.UnplanBlocks(&rec, j.before, j.exists)
				default:
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
				if j.secret {
					p.Line("%s: holds the key; ambit deletes it", j.path)
				} else {
					p.Line("%s", j.path)
					p.Line("%s", diffText(j.path, j.before, true, j.undo.After, j.undo.Delete))
				}
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
		for _, u := range deleting {
			if f.revoke {
				p.Line("would revoke the key for %s at %s", u, consoleFor(f.consoleURL, st.Keys[u]))
			}
		}
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
	pendingURLs := map[string]bool{} // an interrupted connect: the key may be in either store
	for _, tj := range all {
		if tr, ok := st.Tools[tj.tool]; ok {
			pendingURLs[tr.URL] = pendingURLs[tr.URL] || tr.Pending
			delete(st.Tools, tj.tool)
		}
	}
	if showCursor {
		st.CursorShown = false
	}
	if f.all {
		if len(tools) == 0 {
			p.Line("%s no tool is connected (nothing recorded)", p.Mark("ok"))
		}
	}
	// The state file goes first: if deleting a key fails, it lists the key that is still stored and no tool.
	if err := st.Save(d.StatePath); err != nil {
		return err
	}
	for _, u := range deleting {
		if f.revoke {
			if err := a.revokeKey(ctx, d, st, u, consoleFor(f.consoleURL, st.Keys[u])); err != nil {
				return err
			}
		}
		if err := a.deleteKey(d, st, u, f.consoleURL, pendingURLs[u], !f.revoke); err != nil {
			return err
		}
	}
	return nil
}

// keysToDelete is the addresses whose stored key disconnect deletes: keys of the disconnected tools (with --all,
// every stored key) that no remaining tool uses. A key from ambit key set goes only with --all.
func keysToDelete(st *connect.State, tools []string, f disconnectFlags) []string {
	if f.keepKey {
		return nil
	}
	urls := map[string]bool{}
	for _, t := range tools {
		if tr, ok := st.Tools[t]; ok {
			urls[tr.URL] = true
		}
	}
	if f.all { // also a key that no tool uses any more, left by a failed deletion or an earlier update
		for u := range st.Keys {
			urls[u] = true
		}
	}
	var list []string
	for u := range urls {
		kr, ok := st.Keys[u]
		if !ok || (kr.Set && !f.all) {
			continue
		}
		used := false
		for name, t := range st.Tools {
			used = used || (t.URL == u && !slices.Contains(tools, name))
		}
		if !used {
			list = append(list, u)
		}
	}
	sort.Strings(list)
	return list
}

func consoleFor(flag string, kr connect.KeyRecord) string {
	if c := strings.TrimRight(flag, "/"); c != "" {
		return c
	}
	return kr.ConsoleURL
}

// revokeKey sends the stored key of u to the console's DELETE /api/keys/self, which deletes that key. A key the
// console does not know (revoked or stopped already) is only a warning; any other failure keeps the stored key.
func (a *app) revokeKey(ctx context.Context, d *connectDeps, st *connect.State, u, console string) error {
	p := a.printer()
	s, ok := d.store(st, u)
	if !ok {
		return nil
	}
	key, err := s.Get(u)
	if errors.Is(err, connect.ErrNotFound) {
		p.Line("%s no key is stored for %s; nothing to revoke", p.Mark("warn"), u)
		return nil
	}
	if err != nil {
		return &out.Error{What: "read the stored key of " + u, Why: err.Error(), Next: "unlock your keychain, then run the command again", Code: out.ExitFailed}
	}
	p.Redactor().Add(key)
	fail := func(why string) error {
		return &out.Error{What: "revoke the key for " + u, Why: why + "; the key is still stored",
			Next: "run ambit disconnect --all --revoke again, or revoke the key in the console: " + console + "/keys", Code: out.ExitFailed}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, console+"/api/keys/self", nil)
	if err != nil {
		return fail(err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+key)
	hc := *d.HTTP(console)
	// The request carries the key: never follow a redirect, which could send it to another address.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return fail(err.Error())
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		p.Line("%s key for %s revoked", p.Mark("ok"), u)
	case http.StatusUnauthorized:
		p.Line("%s the console does not know the key for %s (revoked or stopped already)", p.Mark("warn"), u)
	default:
		return fail("the console answered " + resp.Status)
	}
	return nil
}

// deleteKey deletes the stored key of u (also from the other store when an interrupted connect may have put it
// there), takes it out of the state file, and prints the line and the revoke reminder at once, so that a later
// failure does not hide what was already deleted.
func (a *app) deleteKey(d *connectDeps, st *connect.State, u, consoleURL string, pending, remind bool) error {
	p := a.printer()
	kr := st.Keys[u]
	stores := []connect.Store{}
	if s, ok := d.store(st, u); ok {
		stores = append(stores, s)
	}
	if pending {
		stores = append(stores, d.Keychain, d.File)
	}
	for _, s := range stores {
		if err := s.Delete(u); err != nil {
			return &out.Error{What: "delete the stored key of " + u, Why: err.Error(), Next: "unlock your keychain, then run ambit disconnect --all", Code: out.ExitFailed}
		}
	}
	delete(st.Keys, u)
	if err := d.save(st); err != nil {
		return err
	}
	p.Line("%s key for %s deleted from %s", p.Mark("ok"), u, storeName(d.GOOS, kr.Store, d.File, u))
	console := consoleFor(consoleURL, kr)
	switch {
	case !remind:
	case console != "":
		p.Line("  next: revoke the key in the console: %s/keys (deleting it here does not revoke it)", console)
	default:
		p.Line("  next: revoke the key in the console (Settings > Keys); deleting it here does not revoke it")
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
	case connect.Hermes:
		paths = append(paths, connect.HermesConfigPath(d.GOOS, d.Getenv, d.Home))
	case connect.OpenCode: // every shell's profile, not only $SHELL's: the person may have changed shells since
		for _, sh := range []connect.Shell{connect.Bash, connect.Zsh, connect.Fish, connect.PowerShell} {
			if sh == connect.PowerShell && d.GOOS != "windows" {
				continue
			}
			if sp, err := connect.ShellFile(sh, d.GOOS, d.Getenv, d.Home, d.Profile); err == nil && !slices.Contains(paths, sp) {
				paths = append(paths, sp)
			}
		}
		if d.GOOS != "windows" { // the bash file of the other system, for a home directory shared or copied
			paths = append(paths, filepath.Join(d.Home, ".bashrc"), filepath.Join(d.Home, ".bash_profile"))
			slices.Sort(paths)
			paths = slices.Compact(paths)
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

// planSecretFile plans the header file before the key is known: it is Ambit's when it still holds what an earlier
// connect wrote. After and the hash are filled in once the key is read.
func planSecretFile(j *job) *connect.Plan {
	p := &connect.Plan{Record: connect.FileRecord{Path: j.path, Secret: true, Created: !j.exists}}
	if j.prev != nil {
		p.Record.Created, p.Record.Dirs, p.Record.Backup = j.prev.Created, j.prev.Dirs, j.prev.Backup
		p.Already = j.exists && connect.FileSum(j.before) == j.prev.SHA256
	}
	if p.Already {
		p.After, p.Record.SHA256 = j.before, j.prev.SHA256
	}
	return p
}
