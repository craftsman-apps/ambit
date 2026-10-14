package connect

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/craftsman-apps/ambit/internal/cli/out"
)

// FileChange is one config file to write. Before is what the file held when it was parsed (Exists false: it did
// not exist); the write is refused if the file is not like that right before the rename.
type FileChange struct {
	Path       string
	Before     []byte
	Exists     bool
	After      []byte
	Delete     bool     // remove the file instead of writing After
	RemoveDirs []string // with Delete: directories to remove afterwards when they are empty
}

// FileResult says what WriteAll did to one file.
type FileResult struct {
	Path    string
	Backup  string   // the .ambit.bak file, empty when the file did not exist
	Created bool     // the file did not exist
	Dirs    []string // directories made for it, outermost first
}

// writeHook runs after the temporary file is complete and before the check; tests use it to change the file at
// the worst moment.
var writeHook func()

func renameFile(from, to string) error {
	return renameWithRetry(runtime.GOOS, os.Rename, func() { time.Sleep(333 * time.Millisecond) }, from, to)
}

// renameWithRetry retries on Windows, where a tool that holds the file open makes the rename fail. Three tries
// over about one second, then a message that says what to do.
func renameWithRetry(goos string, rename func(a, b string) error, sleep func(), from, to string) error {
	err := rename(from, to)
	if goos != "windows" {
		return err
	}
	for i := 0; err != nil && i < 2; i++ {
		sleep()
		err = rename(from, to)
	}
	if err != nil {
		return &out.Error{What: "cannot replace " + to, Why: err.Error(), Next: "quit the tool that uses the file, then run the command again", Code: out.ExitFailed}
	}
	return nil
}

// WriteAll writes the files in order. Each existing file is first copied to <file>.ambit.bak (mode 0600, only the
// latest backup is kept). A symlink is written at its target. If a write fails, the files written before it are
// put back, and files and directories made in this run are removed.
func WriteAll(changes []FileChange) ([]FileResult, error) {
	var done []FileResult
	for i, c := range changes {
		r, err := writeOne(c)
		if err != nil {
			rollback(changes[:i], done)
			return nil, err
		}
		done = append(done, r)
	}
	return done, nil
}

func writeOne(c FileChange) (FileResult, error) {
	r := FileResult{Path: c.Path}
	target := c.Path
	if t, err := filepath.EvalSymlinks(c.Path); err == nil {
		target = t
	}
	if !c.Exists {
		dirs, err := makeDirs(filepath.Dir(target))
		if err != nil {
			return r, writeError(c.Path, err)
		}
		r.Created, r.Dirs = true, dirs
	}
	perm := os.FileMode(0o600)
	if c.Exists {
		fi, err := os.Stat(target)
		if err != nil {
			return r, writeError(c.Path, err)
		}
		perm = fi.Mode().Perm()
		if perm&0o200 == 0 {
			return r, &out.Error{What: "cannot write " + c.Path, Why: "the file is read-only", Next: readOnlyNext(c.Path), Code: out.ExitFailed}
		}
		if !c.Delete { // a file that is deleted holds only what connect made; there is nothing to keep
			r.Backup = c.Path + ".ambit.bak"
			if err := atomicWrite(r.Backup, c.Before, 0o600); err != nil {
				return r, writeError(r.Backup, err)
			}
		}
	}
	unchanged := func() error {
		if writeHook != nil {
			writeHook()
		}
		now, err := os.ReadFile(target)
		switch {
		case !c.Exists && errors.Is(err, fs.ErrNotExist), c.Exists && err == nil && bytes.Equal(now, c.Before):
			return nil
		}
		return &out.Error{What: "cannot write " + c.Path, Why: "the file changed while ambit worked on it", Next: "run the command again", Code: out.ExitFailed}
	}
	if c.Delete {
		if err := unchanged(); err != nil {
			return r, err
		}
		if err := os.Remove(target); err != nil {
			return r, writeError(c.Path, err)
		}
		for _, d := range c.RemoveDirs {
			os.Remove(d) // fails, and so keeps the directory, when something else is in it
		}
		return r, nil
	}
	if err := atomicWriteChecked(target, c.After, perm, unchanged); err != nil {
		var oe *out.Error
		if errors.As(err, &oe) {
			return r, err
		}
		return r, writeError(c.Path, err)
	}
	return r, nil
}

// makeDirs creates dir and its missing parents (0700) and returns the ones it made, outermost first.
func makeDirs(dir string) ([]string, error) {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			break
		}
		missing = append([]string{d}, missing...)
		if filepath.Dir(d) == d {
			break
		}
	}
	for _, d := range missing {
		if err := os.Mkdir(d, 0o700); err != nil {
			return nil, err
		}
	}
	return missing, nil
}

// rollback puts the files written so far back as they were.
func rollback(changes []FileChange, done []FileResult) {
	for i := len(done) - 1; i >= 0; i-- {
		c, r := changes[i], done[i]
		target := c.Path
		if t, err := filepath.EvalSymlinks(c.Path); err == nil {
			target = t
		}
		switch {
		case r.Created:
			os.Remove(target)
			for j := len(r.Dirs) - 1; j >= 0; j-- {
				os.Remove(r.Dirs[j])
			}
		default: // an updated file, or a deleted one: put the old bytes back
			atomicWrite(target, c.Before, 0o600)
		}
	}
}

func readOnlyNext(path string) string {
	if runtime.GOOS == "windows" {
		return "clear the read-only attribute of " + path
	}
	return "chmod u+w " + path
}

// writeError turns an OS error into the What, Why and next lines of spec section 7.
func writeError(path string, err error) *out.Error {
	e := &out.Error{What: "cannot write " + path, Why: err.Error(), Code: out.ExitFailed}
	var pe *fs.PathError
	if errors.Is(err, fs.ErrPermission) {
		e.Why = "permission denied"
		blocked := path
		if errors.As(err, &pe) && pe.Path != path {
			blocked = filepath.Dir(path) // the temporary file could not be made: the directory is read-only
		}
		e.Next = readOnlyNext(blocked)
	} else {
		e.Next = fmt.Sprintf("check %s, then run the command again", path)
	}
	return e
}
