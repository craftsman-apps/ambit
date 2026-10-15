package connect

import (
	"strings"

	"github.com/craftsman-apps/ambit/internal/cli/out"
)

// Tool names, as the user types them.
const (
	ClaudeCode = "claude-code"
	Codex      = "codex"
	OpenCode   = "opencode"
	Cursor     = "cursor"
)

// Input is what planning a connect needs. The caller reads the file and the environment; planning changes nothing.
type Input struct {
	Path      string // the config file
	File      []byte // its content when Exists
	Exists    bool
	URL       string // normalised
	Model     string
	AmbitPath string // from BinEnv.Path
	GOOS      string
	Force     bool
	Getenv    func(string) string
	Prev      *FileRecord // what an earlier connect of this tool wrote to this file
	Shell     string      // the opencode wrapper: bash, zsh, fish or powershell
}

// Plan is the result of planning a connect.
type Plan struct {
	After     []byte
	Record    FileRecord
	Conflicts []Conflict
	Warnings  []string
	Already   bool // the file holds Ambit's entries with these values already; After is File
}

// Conflict is an existing entry that Ambit did not write and that would be replaced or would defeat Ambit's.
type Conflict struct {
	What string // "env.ANTHROPIC_BASE_URL is https://other.example.com"
	Why  string
}

// Disconnected is the result of planning a disconnect of one file.
type Disconnected struct {
	After      []byte
	Delete     bool     // connect made the file and nothing else is in it
	RemoveDirs []string // directories connect made, to remove when empty
	Notes      []string // the `!` lines: entries left in place, secrets that were removed
}

func (c Conflict) String() string { return c.What + " — " + c.Why }

func trimSpace(b []byte) []byte   { return []byte(strings.TrimSpace(string(b))) }
func contains(s, sub string) bool { return strings.Contains(s, sub) }

// refuseSecondBackup stops a re-connect that would remove another secret while the backup of an earlier connect is
// the only place that holds a secret. The backup is kept as it is, so the new value would be in no file at all.
func refuseSecondBackup(in Input, removed []string) error {
	if in.Prev == nil || len(in.Prev.RemovedSecrets) == 0 || len(removed) == 0 {
		return nil
	}
	return &out.Error{What: "cannot remove " + dotted(removed[0]) + " from " + in.Path,
		Why:  "the backup " + in.Prev.Backup + " holds an earlier removed value, and ambit keeps one backup per file",
		Next: "copy the value you need out of the file and the backup, remove the entry from " + in.Path + " by hand, then run the command again",
		Code: out.ExitFailed}
}
