package connect

import (
	"fmt"
	"path/filepath"
)

// ClaudeSettingsPath is $CLAUDE_CONFIG_DIR/settings.json, else ~/.claude/settings.json. Never a project file:
// those are shared through git.
func ClaudeSettingsPath(getenv func(string) string, home string) string {
	if d := getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "settings.json")
	}
	return filepath.Join(home, ".claude", "settings.json")
}

// claudeModelVars are the variables that name the model. The console snippet sets all four.
var claudeModelVars = []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL"}

// claudeSecretVars outrank apiKeyHelper in Claude Code. In settings.json they are a conflict.
var claudeSecretVars = []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY"}

func claudeSets(in Input) []Set {
	sets := []Set{
		{Path: []string{"apiKeyHelper"}, Value: marshalString(Quote(in.GOOS, in.AmbitPath) + " key print --url " + in.URL)},
		{Path: []string{"env", "ANTHROPIC_BASE_URL"}, Value: marshalString(in.URL)},
	}
	for _, v := range claudeModelVars {
		sets = append(sets, Set{Path: []string{"env", v}, Value: marshalString(in.Model)})
	}
	return sets
}

func parseOrNew(in Input) (*Doc, *string, error) {
	if !in.Exists {
		return NewJSON("\n"), nil, nil
	}
	if len(trimSpace(in.File)) == 0 { // an empty file is a file without settings
		seed := string(in.File)
		eol := "\n"
		if contains(seed, "\r\n") {
			eol = "\r\n"
		}
		return NewJSON(eol), &seed, nil
	}
	d, err := ParseJSON(in.File)
	return d, nil, err
}

// PlanClaudeCode plans the entries of section 5.1. A parse error is returned as an error and changes nothing.
func PlanClaudeCode(in Input) (*Plan, error) {
	d, seed, err := parseOrNew(in)
	if err != nil {
		return nil, fmt.Errorf("%s is not valid JSON — %w", in.Path, err)
	}
	sets := claudeSets(in)
	if in.Prev != nil {
		// Entries an earlier connect wrote are ours: take them out, and write them again below. The file then
		// ends up as if the earlier connect had not happened, so a disconnect restores the very first state.
		if in.Exists && claudeAlready(d, sets) {
			return &Plan{After: in.File, Record: *in.Prev, Already: true}, nil
		}
		if _, err := d.Remove(in.Prev.Entries); err != nil {
			return nil, err
		}
	}
	p := &Plan{}
	if cmd, ok := d.Get("apiKeyHelper"); ok && !jsonEqual(cmd, sets[0].Value) {
		p.Conflicts = append(p.Conflicts, Conflict{What: "apiKeyHelper is " + cmd, Why: "Claude Code would use that command instead of Ambit's key"})
	}
	if u, ok := d.Get("env", "ANTHROPIC_BASE_URL"); ok && !jsonEqual(u, sets[1].Value) {
		p.Conflicts = append(p.Conflicts, Conflict{What: "env.ANTHROPIC_BASE_URL is " + u, Why: "Claude Code would send its requests to that address"})
	}
	var removed []string
	for _, v := range claudeSecretVars {
		if _, ok := d.Get("env", v); ok {
			p.Conflicts = append(p.Conflicts, Conflict{What: "env." + v + " is set", Why: "it outranks Ambit's key helper, so Claude Code would keep using that key"})
			removed = append(removed, "/env/"+v)
		}
	}
	if len(p.Conflicts) > 0 && !in.Force {
		return p, nil
	}
	for _, ptr := range removed {
		d.Delete(splitPointer(ptr)...) // the value stays in the backup file only
	}
	entries, err := d.Apply(sets)
	if err != nil {
		return nil, err
	}
	p.After = d.Bytes()
	p.Record = FileRecord{Path: in.Path, Entries: entries, Seed: seed, RemovedSecrets: removed, Created: !in.Exists}
	if in.Getenv != nil {
		for _, v := range claudeSecretVars {
			if in.Getenv(v) != "" {
				p.Warnings = append(p.Warnings, v+" is set in your shell and outranks Ambit's key; remove it from your shell profile")
			}
		}
	}
	return p, nil
}

func claudeAlready(d *Doc, sets []Set) bool {
	for _, s := range sets {
		if got, ok := d.Get(s.Path...); !ok || !jsonEqual(got, s.Value) {
			return false
		}
	}
	return true
}

// UnplanJSON plans the disconnect of a JSON file (Claude Code, opencode): rec says what connect wrote.
// current is the file now; exists false means it is gone. Nothing is an error except a file that does not parse.
func UnplanJSON(rec FileRecord, current []byte, exists bool) (*Disconnected, error) {
	r := &Disconnected{}
	for _, p := range rec.RemovedSecrets {
		r.Notes = append(r.Notes, fmt.Sprintf("%s was removed at connect; its old value is in %s", dotted(p), rec.Backup))
	}
	if !exists {
		return r, nil
	}
	d, err := ParseJSON(current)
	if err != nil {
		return nil, fmt.Errorf("%s is not valid JSON — %w", rec.Path, err)
	}
	left, err := d.Remove(rec.Entries)
	if err != nil {
		return nil, err
	}
	for _, p := range left {
		r.Notes = append([]string{dotted(p) + " was changed after connect; left as it is"}, r.Notes...)
	}
	r.After = d.Bytes()
	if string(r.After) == string(NewJSON(d.eol).Bytes()) {
		switch {
		case rec.Created:
			r.Delete, r.RemoveDirs = true, rec.Dirs
		case rec.Seed != nil:
			r.After = []byte(*rec.Seed)
		}
	}
	return r, nil
}

// dotted turns a JSON pointer into the name people know: /env/ANTHROPIC_MODEL becomes env.ANTHROPIC_MODEL.
func dotted(ptr string) string {
	out := ""
	for i, p := range splitPointer(ptr) {
		if i > 0 {
			out += "."
		}
		out += p
	}
	return out
}
