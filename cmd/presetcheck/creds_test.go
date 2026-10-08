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
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("stat %v err %v", st, err)
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

func TestLoadCredsRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	if err := saveCreds(real, &Creds{Presets: map[string]*PresetCreds{}}); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "presetcheck.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCreds(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink accepted: %v", err)
	}
	if _, err := loadCreds(real); err != nil {
		t.Fatal(err)
	}
}

func TestSaveCredsReplacesAWideStaleTmpFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "presetcheck.json")
	if err := os.WriteFile(path+".tmp", []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveCreds(path, &Creds{Presets: map[string]*PresetCreds{}}); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("stat %v err %v", st, err)
	}
}
