package connect

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

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

// OpenCodeKeyPath is the file that opencode 2 reads the key from: <config base>/ambit/opencode-key, under the same
// config base as opencode's own directory. opencode 2 sends model calls from a background service that does not see
// the environment of the shell the person starts opencode in, so the key cannot come from a shell function.
func OpenCodeKeyPath(getenv func(string) string, home string) string {
	base := filepath.Join(home, ".config")
	if x := getenv("XDG_CONFIG_HOME"); x != "" {
		base = x
	}
	return filepath.Join(base, "ambit", "opencode-key")
}

// OpenCodeKey is the content of the key file: the key alone, without a line break, so that the header opencode
// sends is right whether or not it trims the file.
func OpenCodeKey(key string) []byte { return []byte(key) }

// fileRef is the opencode variable that reads path. opencode resolves an absolute path on every system; on Windows
// the path is written with forward slashes, so the JSON string holds no backslash escapes.
func fileRef(path, goos string) string {
	if goos == "windows" {
		path = strings.ReplaceAll(path, `\`, "/")
	}
	return "{file:" + path + "}"
}

var versionNumber = regexp.MustCompile(`(\d+)\.\d+`)

// OpenCodeMajor is the major version in the output of `opencode --version` ("opencode v2.2.5", "1.18.34"), or 0.
func OpenCodeMajor(version string) int {
	m := versionNumber.FindStringSubmatch(version)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// OpenCodeVersion runs `opencode --version`. It does not start opencode's background service.
func OpenCodeVersion() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "opencode", "--version").Output()
	return strings.TrimSpace(string(b)), err
}

// opencodeV2 reports whether a config uses the opencode v2 names: a top-level `providers`, or a provider with
// `package` or `settings`. opencode 1 does not read them.
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

// opencodeContainer is where opencode 2's provider goes: `providers` (the v2 format) when the file has it or has
// neither, else the older `provider`, which opencode 2 still reads. opencode 1 reads only `provider`.
func opencodeContainer(d *Doc, in Input) string {
	if in.KeyFile == "" {
		return "provider"
	}
	if _, ok := d.Get("providers"); !ok {
		if _, ok := d.Get("provider"); ok {
			return "provider"
		}
	}
	return "providers"
}

func opencodeSets(in Input, container string) []Set {
	apiKey := "{env:AMBIT_API_KEY}" // opencode 1: the shell function sets it
	if in.KeyFile != "" {
		apiKey = fileRef(in.KeyFile, in.GOOS)
	}
	models := `"models":{` + marshalString(in.Model) + `:{"name":` + marshalString(in.Model) + `}}`
	provider := `{"npm":"@ai-sdk/openai-compatible","name":"Ambit","options":{"baseURL":` + marshalString(in.URL+"/v1") +
		`,"apiKey":` + marshalString(apiKey) + `},` + models + `}`
	if container == "providers" {
		provider = `{"name":"Ambit","package":"@opencode/ai/providers/openai-compatible","settings":{"baseURL":` +
			marshalString(in.URL+"/v1") + `,"apiKey":` + marshalString(apiKey) + `},` + models + `}`
	}
	return []Set{
		{Path: []string{container, "ambit"}, Value: provider},
		{Path: []string{"model"}, Value: marshalString("ambit/" + in.Model)},
	}
}

// PlanOpenCode plans the entries of section 5.3 for the JSON file. in.KeyFile set means opencode 2: the provider
// reads the key from that file, in the `providers` or the `provider` format. Without it (opencode 1) a v2 config is
// an error. A file that does not parse is an error. Errors change nothing.
func PlanOpenCode(in Input) (*Plan, error) {
	d, seed, err := parseOrNew(in)
	if err != nil {
		return nil, fmt.Errorf("%s is not valid JSON — %w", in.Path, err)
	}
	if in.KeyFile == "" && opencodeV2(d) {
		return nil, &out.Error{What: in.Path + " uses the opencode v2 config format", Why: "opencode 1 does not read it",
			Next: "update opencode to version 2 and run the command again, or add the Ambit provider by hand (see docs/guides/coding-tools.md)", Code: out.ExitFailed}
	}
	sets := opencodeSets(in, opencodeContainer(d, in))
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
	for _, c := range []string{"provider", "providers"} {
		if _, ok := d.Get(c, "ambit"); ok {
			p.Conflicts = append(p.Conflicts, Conflict{What: c + ".ambit already exists", Why: "Ambit adds a provider of that name"})
			removed = append(removed, "/"+c+"/ambit") // it may hold a plain-text key: it stays in the backup only
		}
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
