package connect

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/craftsman-apps/ambit/internal/cli/out"
)

// planJSON plans sets in a JSON or JSONC file, the way PlanOpenCode does. An entry at a set path that Ambit did not
// write is a conflict: an object (a provider or server of Ambit's name, which may hold a key) is removed and stays
// in the backup only; any other value is replaced, and disconnect gives it back. A value the person let Ambit
// replace at an earlier connect (--force) is no conflict again. user names the tool in the conflict text.
func planJSON(in Input, sets []Set, user string) (*Plan, error) {
	d, seed, err := parseOrNew(in)
	if err != nil {
		return nil, fmt.Errorf("%s is not valid JSON — %w", in.Path, err)
	}
	already := false
	decided := map[string]bool{}
	if in.Prev != nil {
		// Entries an earlier connect wrote are ours: take them out, and write them again below.
		already = in.Exists && claudeAlready(d, sets)
		for _, e := range in.Prev.Entries {
			decided[e.Pointer] = decided[e.Pointer] || e.Previous != nil
		}
		if _, err := d.Remove(in.Prev.Entries); err != nil {
			return nil, err
		}
	}
	p := &Plan{}
	var removed []string
	for _, s := range sets {
		got, ok := d.Get(s.Path...)
		if !ok || jsonEqual(got, s.Value) {
			continue
		}
		ptr := pointer(s.Path)
		switch {
		case strings.HasPrefix(s.Value, "{"):
			p.Conflicts = append(p.Conflicts, Conflict{What: dotted(ptr) + " already exists", Why: "Ambit adds an entry of that name"})
			removed = append(removed, ptr)
		case !decided[ptr]:
			p.Conflicts = append(p.Conflicts, Conflict{What: dotted(ptr) + " is set to another value", Why: user + " would keep using it instead of Ambit's"})
		}
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
		return nil, fmt.Errorf("%s — %w", in.Path, err)
	}
	p.After = d.Bytes()
	p.Record = FileRecord{Path: in.Path, Entries: entries, Seed: seed, RemovedSecrets: removed, Created: !in.Exists}
	if in.Prev != nil && in.Exists {
		p.Record.Created, p.Record.Dirs, p.Record.Seed = in.Prev.Created, in.Prev.Dirs, in.Prev.Seed
		p.Record.RemovedSecrets = append(append([]string{}, in.Prev.RemovedSecrets...), removed...)
	}
	return p, nil
}

// KiloPath is the global config file of Kilo Code (CLI and VS Code extension): in $XDG_CONFIG_HOME/kilo, else
// ~/.config/kilo (on macOS and Windows too), the first file that exists of the ones Kilo itself writes to, in Kilo's
// order; kilo.jsonc when none exists. Never a project file.
func KiloPath(getenv func(string) string, home string, exists func(string) bool) string {
	dir := filepath.Join(home, ".config", "kilo")
	if x := getenv("XDG_CONFIG_HOME"); x != "" {
		dir = filepath.Join(x, "kilo")
	}
	for _, f := range []string{"kilo.jsonc", "kilo.json", "opencode.jsonc", "opencode.json", "config.json"} {
		if p := filepath.Join(dir, f); exists(p) {
			return p
		}
	}
	return filepath.Join(dir, "kilo.jsonc")
}

// KiloKeyPath is the file Kilo Code reads the key from, next to the opencode 2 key file. The VS Code extension
// starts Kilo as a background process (kilo serve) that does not see a terminal's environment.
func KiloKeyPath(getenv func(string) string, home string) string {
	return filepath.Join(filepath.Dir(OpenCodeKeyPath(getenv, home)), "kilo-key")
}

// PlanKilo plans Kilo Code's provider (the opencode 1 format, which Kilo, a fork of opencode, reads), the model and,
// with in.MCPURL, the MCP server. The key is a {file:...} reference to in.KeyFile.
func PlanKilo(in Input) (*Plan, error) {
	ref := fileRef(in.KeyFile, in.GOOS)
	sets := []Set{
		{Path: []string{"provider", "ambit"}, Value: `{"npm":"@ai-sdk/openai-compatible","name":"Ambit","options":{"baseURL":` +
			marshalString(in.URL+"/v1") + `,"apiKey":` + marshalString(ref) + `},"models":{` + marshalString(in.Model) + `:{"name":` + marshalString(in.Model) + `}}}`},
		{Path: []string{"model"}, Value: marshalString("ambit/" + in.Model)},
	}
	if in.MCPURL != "" {
		sets = append(sets, Set{Path: []string{"mcp", "ambit"}, Value: `{"type":"remote","url":` + marshalString(in.MCPURL) +
			`,"oauth":false,"headers":{"Authorization":` + marshalString("Bearer "+ref) + `}}`})
	}
	return planJSON(in, sets, "Kilo Code")
}

// PiDir is pi's agent directory: $PI_CODING_AGENT_DIR (a leading ~ is the home directory), else ~/.pi/agent.
func PiDir(getenv func(string) string, home string) string {
	switch d := getenv("PI_CODING_AGENT_DIR"); {
	case d == "~":
		return home
	case strings.HasPrefix(d, "~/"):
		return filepath.Join(home, d[2:])
	case d != "":
		return d
	}
	return filepath.Join(home, ".pi", "agent")
}

// piKeyCommand is the apiKey value with which pi runs ambit key print. pi reads $NAME in the value as a variable,
// so a $ is written as $$.
func piKeyCommand(in Input) string {
	return "!" + strings.ReplaceAll(QuoteCmd(in.GOOS, in.AmbitPath)+" key print --url "+QuoteCmd(in.GOOS, in.URL), "$", "$$")
}

// PlanPiModels plans the provider "ambit" in pi's models.json. pi runs the key command for each request.
func PlanPiModels(in Input) (*Plan, error) {
	if err := CheckCmdSafe(in.GOOS, "pi", in.AmbitPath, in.URL); err != nil {
		return nil, err
	}
	return planJSON(in, []Set{{Path: []string{"providers", "ambit"}, Value: `{"baseUrl":` + marshalString(in.URL+"/v1") +
		`,"api":"openai-completions","apiKey":` + marshalString(piKeyCommand(in)) + `,"models":[{"id":` + marshalString(in.Model) + `}]}`}}, "pi")
}

// PlanPiSettings plans the default provider and model in pi's settings.json.
func PlanPiSettings(in Input) (*Plan, error) {
	return planJSON(in, []Set{
		{Path: []string{"defaultProvider"}, Value: `"ambit"`},
		{Path: []string{"defaultModel"}, Value: marshalString(in.Model)},
	}, "pi")
}

// PlanPiMCP plans the server "ambit" in pi's user mcp.json. auth.provider makes pi send the key of the provider
// "ambit" (the key command of models.json) as the bearer token; pi allows it only over https, or http to localhost.
func PlanPiMCP(in Input) (*Plan, error) {
	if needsAllowHTTP(in.MCPURL) {
		return nil, out.Usage("--mcp-url "+in.MCPURL, "pi sends the key to an MCP server only over https, or over http to localhost",
			"use the https address of the MCP gateway")
	}
	return planJSON(in, []Set{{Path: []string{"mcpServers", "ambit"}, Value: `{"url":` + marshalString(in.MCPURL) + `,"auth":{"provider":"ambit"}}`}}, "pi")
}
