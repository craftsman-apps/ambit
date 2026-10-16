package connect

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/craftsman-apps/ambit/internal/cli/out"
	"github.com/tailscale/hujson"
)

// OpenCodePath is $XDG_CONFIG_HOME/opencode/opencode.json (or .jsonc, whichever exists), else
// ~/.config/opencode/... (opencode uses ~/.config on macOS and Windows too). Never a project file.
func OpenCodePath(getenv func(string) string, home string, exists func(string) bool) (string, error) {
	dir := filepath.Join(home, ".config", "opencode")
	if x := getenv("XDG_CONFIG_HOME"); x != "" {
		dir = filepath.Join(x, "opencode")
	}
	j, jc := filepath.Join(dir, "opencode.json"), filepath.Join(dir, "opencode.jsonc")
	switch ej, ejc := exists(j), exists(jc); {
	case ej && ejc:
		return "", &out.Error{What: "both " + j + " and " + jc + " exist", Why: "ambit cannot tell which one opencode reads",
			Next: "merge them into one file, then run the command again", Code: out.ExitFailed}
	case ejc:
		return jc, nil
	}
	return j, nil
}

// opencodeV2 reports whether a config uses the opencode v2 names: a top-level `providers`, or a provider with
// `package` or `settings`. This version of ambit writes the v1 format that was tested live.
func opencodeV2(d *Doc) bool {
	std, err := hujson.Standardize(append([]byte(nil), d.src...)) // Standardize blanks the comments in its argument
	if err != nil {
		return false
	}
	var m map[string]any
	if json.Unmarshal(std, &m) != nil {
		return false
	}
	if _, ok := m["providers"]; ok {
		return true
	}
	prov, _ := m["provider"].(map[string]any)
	for _, p := range prov {
		if o, ok := p.(map[string]any); ok {
			if _, ok := o["package"]; ok {
				return true
			}
			if _, ok := o["settings"]; ok {
				return true
			}
		}
	}
	return false
}

func opencodeSets(in Input) []Set {
	provider := `{"npm":"@ai-sdk/openai-compatible","name":"Ambit","options":{"baseURL":` + marshalString(in.URL+"/v1") +
		`,"apiKey":"{env:AMBIT_API_KEY}"},"models":{` + marshalString(in.Model) + `:{"name":` + marshalString(in.Model) + `}}}`
	return []Set{
		{Path: []string{"provider", "ambit"}, Value: provider},
		{Path: []string{"model"}, Value: marshalString("ambit/" + in.Model)},
	}
}

// PlanOpenCode plans the entries of section 5.3 for the JSON file. A file that does not parse, and a v2 config,
// are errors and change nothing.
func PlanOpenCode(in Input) (*Plan, error) {
	d, seed, err := parseOrNew(in)
	if err != nil {
		return nil, fmt.Errorf("%s is not valid JSON — %w", in.Path, err)
	}
	if opencodeV2(d) {
		return nil, &out.Error{What: in.Path + " uses the opencode v2 config format", Why: "this version of ambit writes the v1 format",
			Next: "add the Ambit provider by hand (see docs/guides/coding-tools.md), or wait for a newer ambit", Code: out.ExitFailed}
	}
	sets := opencodeSets(in)
	decidedModel := false
	already := false
	if in.Prev != nil {
		// Entries an earlier connect wrote are ours: take them out, and write them again below. The checks below
		// run also when the entries are unchanged.
		already = in.Exists && claudeAlready(d, sets)
		for _, e := range in.Prev.Entries {
			decidedModel = decidedModel || e.Pointer == "/model" && e.Previous != nil
		}
		if _, err := d.Remove(in.Prev.Entries); err != nil {
			return nil, err
		}
	}
	p := &Plan{}
	var removed []string
	if _, ok := d.Get("provider", "ambit"); ok {
		p.Conflicts = append(p.Conflicts, Conflict{What: "provider.ambit already exists", Why: "Ambit adds a provider of that name"})
		removed = append(removed, "/provider/ambit") // it may hold a plain-text key: it stays in the backup only
	}
	if m, ok := d.Get("model"); ok && !decidedModel && !jsonEqual(m, sets[1].Value) {
		p.Conflicts = append(p.Conflicts, Conflict{What: "model is set to another value", Why: "opencode would keep using it instead of Ambit's"})
	}
	if len(p.Conflicts) > 0 && !in.Force {
		return p, nil
	}
	if already && len(removed) == 0 {
		return &Plan{After: in.File, Record: *in.Prev, Already: true}, nil
	}
	if err := refuseSecondBackup(in, removed); err != nil {
		return nil, err
	}
	for _, ptr := range removed {
		d.Delete(splitPointer(ptr)...)
	}
	entries, err := d.Apply(sets)
	if err != nil {
		return nil, err
	}
	p.After = d.Bytes()
	p.Record = FileRecord{Path: in.Path, Entries: entries, Seed: seed, RemovedSecrets: removed, Created: !in.Exists}
	if in.Prev != nil && in.Exists {
		p.Record.Created, p.Record.Dirs, p.Record.Seed = in.Prev.Created, in.Prev.Dirs, in.Prev.Seed
		p.Record.RemovedSecrets = append(append([]string{}, in.Prev.RemovedSecrets...), removed...)
	}
	return p, nil
}
