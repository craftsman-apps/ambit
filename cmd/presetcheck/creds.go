package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type PresetCreds struct {
	ClientID     string            `json:"clientID"`
	ClientSecret string            `json:"clientSecret"`
	RefreshToken string            `json:"refreshToken,omitempty"`
	AccessToken  string            `json:"accessToken,omitempty"` // kept for vendors that issue no refresh token
	Expiry       time.Time         `json:"expiry,omitzero"`
	Values       map[string]string `json:"values,omitempty"`
	Second       *PresetCreds      `json:"second,omitempty"` // a second test account, to compare tool definitions
}

type Creds struct {
	Presets map[string]*PresetCreds `json:"presets"`
}

// lockCreds allows one presetcheck at a time: a second run would renew the same refresh tokens.
func lockCreds(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "presetcheck.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another presetcheck is running")
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func loadCreds(path string) (*Creds, error) {
	// One handle for the mode check and the read; O_NOFOLLOW refuses a symlink in place of the file.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return &Creds{Presets: map[string]*PresetCreds{}}, nil
	} else if errors.Is(err, syscall.ELOOP) {
		return nil, fmt.Errorf("%s is a symlink; keep the credentials in a plain file", path)
	} else if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a plain file", path)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by others; run chmod 0600 on it", path)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	c := &Creds{}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, err
	}
	if c.Presets == nil {
		c.Presets = map[string]*PresetCreds{}
	}
	return c, nil
}

// saveCreds replaces the file atomically, so a crash leaves either the old or the new tokens.
func saveCreds(path string, c *Creds) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	// A stale tmp file (from a crash) may have a wider mode; remove it and create a fresh one.
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	// fsync the directory so the rename itself survives a crash.
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
