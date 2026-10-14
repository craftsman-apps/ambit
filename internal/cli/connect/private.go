package connect

import (
	"os"
	"path/filepath"
)

// atomicWrite writes data to a temporary file in the same directory and renames it over path. A file that must
// be private (perm without group or other bits) is restricted to the current user, which on Windows needs an ACL.
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	fail := func(err error) error { tmp.Close(); os.Remove(tmp.Name()); return err }
	if err := tmp.Chmod(perm); err != nil && !isWindowsChmodError(err) {
		return fail(err)
	}
	if perm&0o077 == 0 {
		if err := restrictToUser(tmp.Name()); err != nil {
			return fail(err)
		}
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}
