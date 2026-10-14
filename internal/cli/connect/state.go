package connect

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// State is the file connect.json: what Ambit wrote, so disconnect removes exactly that. It never holds a secret.
type State struct {
	Version     int                   `json:"version"`
	CursorShown bool                  `json:"cursorShown"`
	Keys        map[string]KeyRecord  `json:"keys"`
	Tools       map[string]ToolRecord `json:"tools"`
}

// KeyRecord says where the key of a gateway address is stored, and where to revoke it.
type KeyRecord struct {
	Store      string `json:"store"` // StoreKeychain or StoreFile
	ConsoleURL string `json:"consoleURL,omitempty"`
}

type ToolRecord struct {
	URL   string       `json:"url"`
	Files []FileRecord `json:"files"`
}

// FileRecord is one config file a tool's connect wrote. Entries is for JSON files, Blocks for text files.
type FileRecord struct {
	Path    string        `json:"path"`
	Created bool          `json:"created,omitempty"` // connect made the file (and Dirs lists the directories it made)
	Dirs    []string      `json:"dirs,omitempty"`
	Backup  string        `json:"backup,omitempty"`
	Entries []Entry       `json:"entries,omitempty"`
	Blocks  []BlockRecord `json:"blocks,omitempty"`
	Wrote   []string      `json:"wrote,omitempty"` // the body of each block as connect wrote it, same order as Blocks
}

// Entry is one JSON member Ambit wrote. Wrote and Previous are JSON texts; Previous nil means the member did not
// exist. Inserted is the exact bytes added to the file, which lets disconnect restore the original bytes.
type Entry struct {
	Pointer          string  `json:"pointer"`
	Wrote            string  `json:"wrote"`
	Previous         *string `json:"previous"`
	CreatedContainer bool    `json:"createdContainer,omitempty"`
	Inserted         string  `json:"inserted,omitempty"`
}

func NewState() *State {
	return &State{Version: 1, Keys: map[string]KeyRecord{}, Tools: map[string]ToolRecord{}}
}

func (s *State) SetKey(url string, k KeyRecord) { s.Keys[url] = k }

// UsesURL reports whether a tool other than except is connected to url.
func (s *State) UsesURL(url, except string) bool {
	for name, t := range s.Tools {
		if name != except && t.URL == url {
			return true
		}
	}
	return false
}

// DefaultStatePath is <os.UserConfigDir>/ambit/connect.json.
func DefaultStatePath() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "ambit", "connect.json"), nil
}

// LoadState reads the state file; a missing file is an empty state.
func LoadState(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return NewState(), nil
	}
	if err != nil {
		return nil, err
	}
	s := NewState()
	if err := json.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	if s.Version > 1 {
		return nil, fmt.Errorf("%s was written by a newer ambit (version %d)", path, s.Version)
	}
	if s.Keys == nil {
		s.Keys = map[string]KeyRecord{}
	}
	if s.Tools == nil {
		s.Tools = map[string]ToolRecord{}
	}
	return s, nil
}

// Save writes the file atomically, readable by the current user only.
func (s *State) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(b, '\n'), 0o600)
}
