package connect

import "strings"

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
