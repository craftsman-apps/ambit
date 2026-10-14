package connect

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/craftsman-apps/ambit/internal/cli/out"
)

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func skipUnlessPOSIXUser(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file modes do not stop this user")
	}
}

func TestWriteAllCreatesMissingFileAndDirectories(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".claude", "deep", "settings.json")
	res, err := WriteAll([]FileChange{{Path: p, After: []byte("{}\n")}})
	if err != nil {
		t.Fatal(err)
	}
	if read(t, p) != "{}\n" || !res[0].Created || res[0].Backup != "" || len(res[0].Dirs) != 2 {
		t.Fatalf("result %+v", res[0])
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(filepath.Dir(p)); fi.Mode().Perm() != 0o700 {
			t.Fatalf("directory mode %v", fi.Mode().Perm())
		}
	}
}

func TestWriteAllBackupHoldsThePreviousContentAndOnlyTheLatestIsKept(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(p, []byte("one"), 0o644)
	if _, err := WriteAll([]FileChange{{Path: p, Before: []byte("one"), Exists: true, After: []byte("two")}}); err != nil {
		t.Fatal(err)
	}
	res, err := WriteAll([]FileChange{{Path: p, Before: []byte("two"), Exists: true, After: []byte("three")}})
	if err != nil {
		t.Fatal(err)
	}
	bak := p + ".ambit.bak"
	if res[0].Backup != bak || read(t, bak) != "two" || read(t, p) != "three" {
		t.Fatalf("backup %q holds %q", res[0].Backup, read(t, bak))
	}
	if entries, _ := os.ReadDir(filepath.Dir(p)); len(entries) != 2 {
		t.Fatalf("expected the file and one backup, found %v", entries)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(bak); fi.Mode().Perm() != 0o600 {
			t.Fatalf("backup mode %v", fi.Mode().Perm())
		}
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o644 {
			t.Fatalf("the file's own mode changed to %v", fi.Mode().Perm())
		}
	}
}

func TestWriteAllRefusesAFileChangedWhileWeWorked(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(p, []byte("one"), 0o600)
	writeHook = func() { os.WriteFile(p, []byte("the tool wrote this"), 0o600) }
	defer func() { writeHook = nil }()
	_, err := WriteAll([]FileChange{{Path: p, Before: []byte("one"), Exists: true, After: []byte("two")}})
	var e *out.Error
	if !errors.As(err, &e) || !strings.Contains(e.Why, "changed") || e.Next != "run the command again" {
		t.Fatalf("err = %#v", err)
	}
	if read(t, p) != "the tool wrote this" {
		t.Fatal("the other program's change was overwritten")
	}
	if entries, _ := os.ReadDir(filepath.Dir(p)); len(entries) != 2 { // the file and its backup, no temporary file
		t.Fatalf("leftovers: %v", entries)
	}
}

func TestWriteAllRefusesAFileThatAppearedWhileWeWorked(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	writeHook = func() { os.WriteFile(p, []byte("new"), 0o600) }
	defer func() { writeHook = nil }()
	if _, err := WriteAll([]FileChange{{Path: p, After: []byte("{}")}}); err == nil || read(t, p) != "new" {
		t.Fatalf("err = %v", err)
	}
}

func TestWriteAllWritesThroughASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "settings.json")
	os.MkdirAll(filepath.Dir(target), 0o755)
	os.WriteFile(target, []byte("one"), 0o644)
	link := filepath.Join(dir, "settings.json")
	os.Symlink(target, link)
	if _, err := WriteAll([]FileChange{{Path: link, Before: []byte("one"), Exists: true, After: []byte("two")}}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced by a file")
	}
	if read(t, target) != "two" {
		t.Fatal("the target was not written")
	}
}

func TestWriteAllRestoresTheFirstFileWhenTheSecondFails(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.json")
	os.WriteFile(first, []byte("one"), 0o600)
	created := filepath.Join(dir, "new", "created.json")
	blocker := filepath.Join(dir, "blocker")
	os.WriteFile(blocker, []byte("x"), 0o600) // a file where the second path needs a directory
	_, err := WriteAll([]FileChange{
		{Path: first, Before: []byte("one"), Exists: true, After: []byte("two")},
		{Path: created, After: []byte("c")},
		{Path: filepath.Join(blocker, "x.json"), After: []byte("y")},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if read(t, first) != "one" {
		t.Fatalf("the first file was not restored: %q", read(t, first))
	}
	if _, err := os.Stat(filepath.Dir(created)); !os.IsNotExist(err) {
		t.Fatal("a file and directory created in this run were not removed")
	}
}

func TestWriteAllReadOnlyFileAndDirectory(t *testing.T) {
	skipUnlessPOSIXUser(t)
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro.json")
	os.WriteFile(ro, []byte("one"), 0o400)
	_, err := WriteAll([]FileChange{{Path: ro, Before: []byte("one"), Exists: true, After: []byte("two")}})
	var e *out.Error
	if !errors.As(err, &e) || e.Code != out.ExitFailed || !strings.Contains(e.What, ro) || e.Next != "chmod u+w "+ro || read(t, ro) != "one" {
		t.Fatalf("read-only file: err = %#v", err)
	}
	rodir := filepath.Join(dir, "rodir")
	os.Mkdir(rodir, 0o700)
	f := filepath.Join(rodir, "a.json")
	os.WriteFile(f, []byte("one"), 0o600)
	os.Chmod(rodir, 0o500)
	defer os.Chmod(rodir, 0o700)
	_, err = WriteAll([]FileChange{{Path: f, Before: []byte("one"), Exists: true, After: []byte("two")}})
	if !errors.As(err, &e) || e.Next != "chmod u+w "+rodir || read(t, f) != "one" {
		t.Fatalf("read-only directory: err = %#v", err)
	}
}

func TestWriteAllDeletesAFileAndTheDirectoriesWeMade(t *testing.T) {
	dir := t.TempDir()
	made := filepath.Join(dir, "made")
	p := filepath.Join(made, "settings.json")
	os.MkdirAll(made, 0o700)
	os.WriteFile(p, []byte("{}\n"), 0o600)
	res, err := WriteAll([]FileChange{{Path: p, Before: []byte("{}\n"), Exists: true, Delete: true, RemoveDirs: []string{made}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(made); !os.IsNotExist(err) || res[0].Backup != "" {
		t.Fatalf("directory still there or backup written: %+v", res[0])
	}
	// A directory that is not empty stays.
	os.MkdirAll(made, 0o700)
	os.WriteFile(p, []byte("{}\n"), 0o600)
	os.WriteFile(filepath.Join(made, "other"), []byte("x"), 0o600)
	if _, err := WriteAll([]FileChange{{Path: p, Before: []byte("{}\n"), Exists: true, Delete: true, RemoveDirs: []string{made}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(made, "other")); err != nil {
		t.Fatal("a directory with the person's files was removed")
	}
}

func TestRenameRetriesOnWindows(t *testing.T) {
	calls := 0
	rename := func(a, b string) error {
		if calls++; calls < 3 {
			return errors.New("The process cannot access the file because it is being used by another process.")
		}
		return nil
	}
	if err := renameWithRetry("windows", rename, func() {}, "a", "b"); err != nil || calls != 3 {
		t.Fatalf("err %v after %d calls", err, calls)
	}
	calls = -10
	err := renameWithRetry("windows", rename, func() {}, "a", "b")
	var e *out.Error
	if !errors.As(err, &e) || !strings.Contains(e.Next, "quit") || calls != -7 {
		t.Fatalf("err = %#v after %d calls", err, calls+10)
	}
	calls = -10
	if err := renameWithRetry("linux", rename, func() {}, "a", "b"); err == nil || calls != -9 {
		t.Fatalf("no retry outside Windows: err %v, calls %d", err, calls+10)
	}
}

func TestWriteAllRollbackKeepsTheModeOfTheFile(t *testing.T) {
	skipUnlessPOSIXUser(t)
	dir := t.TempDir()
	first := filepath.Join(dir, "first.json")
	os.WriteFile(first, []byte("one"), 0o644)
	os.Chmod(first, 0o644)
	blocker := filepath.Join(dir, "blocker")
	os.WriteFile(blocker, []byte("x"), 0o600)
	_, err := WriteAll([]FileChange{
		{Path: first, Before: []byte("one"), Exists: true, After: []byte("two")},
		{Path: filepath.Join(blocker, "x.json"), After: []byte("y")},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if fi, _ := os.Stat(first); read(t, first) != "one" || fi.Mode().Perm() != 0o644 {
		t.Fatalf("restored %q with mode %v", read(t, first), fi.Mode().Perm())
	}
}

func TestWriteAllRollbackMakesTheRemovedDirectoriesAgain(t *testing.T) {
	dir := t.TempDir()
	outer := filepath.Join(dir, "a")
	inner := filepath.Join(outer, "b")
	p := filepath.Join(inner, "settings.json")
	os.MkdirAll(inner, 0o700)
	os.WriteFile(p, []byte("{}\n"), 0o600)
	blocker := filepath.Join(dir, "blocker")
	os.WriteFile(blocker, []byte("x"), 0o600)
	_, err := WriteAll([]FileChange{
		{Path: p, Before: []byte("{}\n"), Exists: true, Delete: true, RemoveDirs: []string{outer, inner}}, // outermost first, as FileResult.Dirs
		{Path: filepath.Join(blocker, "x.json"), After: []byte("y")},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if read(t, p) != "{}\n" {
		t.Fatalf("the deleted file was not put back: %q", read(t, p))
	}
}

func TestWriteAllDeletesNestedDirectoriesInnermostFirst(t *testing.T) {
	dir := t.TempDir()
	outer := filepath.Join(dir, "a")
	inner := filepath.Join(outer, "b")
	p := filepath.Join(inner, "settings.json")
	os.MkdirAll(inner, 0o700)
	os.WriteFile(p, []byte("{}\n"), 0o600)
	if _, err := WriteAll([]FileChange{{Path: p, Before: []byte("{}\n"), Exists: true, Delete: true, RemoveDirs: []string{outer, inner}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(outer); !os.IsNotExist(err) {
		t.Fatal("the outer directory that connect made is still there")
	}
}

func TestWriteAllSaysWhatItCouldNotPutBack(t *testing.T) {
	skipUnlessPOSIXUser(t)
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	os.Mkdir(locked, 0o700)
	first := filepath.Join(locked, "first.json")
	second := filepath.Join(dir, "second.json")
	os.WriteFile(first, []byte("one"), 0o600)
	os.WriteFile(second, []byte("s"), 0o600)
	calls := 0
	writeHook = func() {
		if calls++; calls == 2 { // the second file is being checked: make the first one impossible to restore
			os.Chmod(locked, 0o500)
		}
	}
	defer func() { writeHook = nil; os.Chmod(locked, 0o700) }()
	_, err := WriteAll([]FileChange{
		{Path: first, Before: []byte("one"), Exists: true, After: []byte("two")},
		{Path: second, Before: []byte("different"), Exists: true, After: []byte("y")}, // fails: it changed
	})
	var e *out.Error
	if !errors.As(err, &e) || !strings.Contains(e.Why, "changed while") || !strings.Contains(e.Why, first) || !strings.Contains(e.Why, first+".ambit.bak") {
		t.Fatalf("err = %#v", err)
	}
}
