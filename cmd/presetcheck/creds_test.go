package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredsArePrivateAndOneRunAtATime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "presetcheck.json")
	c := &Creds{Presets: map[string]*PresetCreds{"gh": {ClientID: "id", RefreshToken: "rt"}}}
	if err := saveCreds(path, c); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	unlock, err := lockCreds(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockCreds(dir); err == nil || !strings.Contains(err.Error(), "another presetcheck") {
		t.Fatalf("second run not refused: %v", err)
	}
	// Saving while locked replaces the file atomically; the lock is on another file and survives.
	c.Presets["gh"].RefreshToken = "rt2"
	if err := saveCreds(path, c); err != nil {
		t.Fatal(err)
	}
	if _, err := lockCreds(dir); err == nil {
		t.Fatal("lock lost after saving")
	}
	unlock()
	got, _ := loadCreds(path)
	if got.Presets["gh"].RefreshToken != "rt2" {
		t.Fatal("rotated token not saved")
	}
	os.Chmod(path, 0o644)
	if _, err := loadCreds(path); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("readable file accepted: %v", err)
	}
}
