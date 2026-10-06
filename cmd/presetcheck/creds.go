package main

import (
	"encoding/json"
	"errors"
	"fmt"
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
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Creds{Presets: map[string]*PresetCreds{}}, nil
	} else if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by others; run chmod 0600 on it", path)
	}
	b, err := os.ReadFile(path)
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
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
