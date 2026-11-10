package connect

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
)

// MCPRemote is the mcp-remote version that docs/guides/claude-desktop-mcp.md was tested with.
const MCPRemote = "mcp-remote@0.14.3"

// ClaudeDesktopConfigPath is where Claude Desktop reads claude_desktop_config.json. Linux has no official app; the
// path is the one community builds use.
func ClaudeDesktopConfigPath(goos string, getenv func(string) string, home string) string {
	switch goos {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json")
	case "windows":
		if d := getenv("APPDATA"); d != "" {
			return filepath.Join(d, "Claude", "claude_desktop_config.json")
		}
		return filepath.Join(home, "AppData", "Roaming", "Claude", "claude_desktop_config.json")
	}
	if d := getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "Claude", "claude_desktop_config.json")
	}
	return filepath.Join(home, ".config", "Claude", "claude_desktop_config.json")
}

// MCPHeaderPath is the file that mcp-remote reads the Authorization header from (--header-file).
func MCPHeaderPath(home string) string { return filepath.Join(home, ".ambit", "mcp-headers.txt") }

// MCPHeader is the content of the header file. It holds the key: the file is written with mode 0600.
func MCPHeader(key string) []byte { return []byte("Authorization: Bearer " + key + "\n") }

// FileSum is the hex SHA-256 of a file's content. The state file records it for the header file instead of the
// content; a hash of a random 256-bit key does not reveal the key.
func FileSum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// needsAllowHTTP is mcp-remote's rule: http:// is refused unless the host is localhost or --allow-http is given.
func needsAllowHTTP(addr string) bool {
	u, err := url.Parse(addr)
	if err != nil || u.Scheme != "http" {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return false
	}
	return true
}

func claudeDesktopEntry(in Input, headerPath string) string {
	args := []string{marshalString("-y"), marshalString(MCPRemote), marshalString(in.URL)}
	if needsAllowHTTP(in.URL) {
		args = append(args, marshalString("--allow-http"))
	}
	args = append(args, marshalString("--header-file"), marshalString(headerPath))
	v := `{"command":"npx","args":[`
	for i, a := range args {
		if i > 0 {
			v += ","
		}
		v += a
	}
	return v + "]}"
}

// PlanClaudeDesktopMCP plans the entry mcpServers.ambit of claude_desktop_config.json. The header file is planned by
// the caller, once the key is known.
func PlanClaudeDesktopMCP(in Input, headerPath string) (*Plan, error) {
	d, seed, err := parseOrNew(in)
	if err != nil {
		return nil, fmt.Errorf("%s is not valid JSON — %w", in.Path, err)
	}
	sets := []Set{{Path: []string{"mcpServers", "ambit"}, Value: claudeDesktopEntry(in, headerPath)}}
	already := false
	if in.Prev != nil {
		already = in.Exists && claudeAlready(d, sets)
		if _, err := d.Remove(in.Prev.Entries); err != nil {
			return nil, err
		}
	}
	p := &Plan{}
	var removed []string
	if _, ok := d.Get("mcpServers", "ambit"); ok {
		p.Conflicts = append(p.Conflicts, Conflict{What: "mcpServers.ambit already exists", Why: "Ambit adds a server of that name"})
		removed = append(removed, "/mcpServers/ambit") // it may hold a key in env: it stays in the backup only
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

// headerLine is what a header file written by connect holds, with any key.
var headerLine = regexp.MustCompile(`^Authorization: Bearer ambit_[0-9A-Za-z]{43}\n$`)

// UnplanSecretFile plans the disconnect of a file that holds the key (rec.Secret). It is deleted only while it holds
// exactly what connect wrote; otherwise it stays, with a note.
func UnplanSecretFile(rec FileRecord, current []byte, exists bool) *Disconnected {
	r := &Disconnected{After: current}
	if !exists {
		return r
	}
	if FileSum(current) != rec.SHA256 || !headerLine.Match(current) {
		r.Notes = append(r.Notes, rec.Path+" was changed after connect; left as it is (it may hold a key: delete it when you no longer need it)")
		return r
	}
	r.Delete, r.RemoveDirs = true, rec.Dirs
	if rec.Backup != "" {
		r.Notes = append(r.Notes, "the file that "+rec.Path+" replaced at connect is in "+rec.Backup+" (it may hold a key)")
	}
	return r
}
