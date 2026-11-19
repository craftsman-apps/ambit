package local

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/craftsman-apps/ambit/internal/cli/out"
)

// TestLockHelperProcess is not a test: TestLockBetweenProcesses starts the test binary again with it, so a
// second process holds the lock. It takes the lock, says "locked", then exits or waits to be killed.
func TestLockHelperProcess(t *testing.T) {
	dir, mode := os.Getenv("AMBIT_LOCK_HELPER_DIR"), os.Getenv("AMBIT_LOCK_HELPER_MODE")
	if dir == "" {
		t.Skip("helper process only")
	}
	if _, err := (Store{Dir: dir}).Lock("x"); err != nil {
		os.Stdout.WriteString("error: " + err.Error() + "\n")
		os.Exit(3)
	}
	os.Stdout.WriteString("locked\n")
	if mode == "exit" {
		os.Exit(0) // the lock is never released by the code: the OS frees it when the process ends
	}
	time.Sleep(time.Minute)
	os.Exit(0)
}

func startHolder(t *testing.T, dir, mode string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockHelperProcess$")
	cmd.Env = append(os.Environ(), "AMBIT_LOCK_HELPER_DIR="+dir, "AMBIT_LOCK_HELPER_MODE="+mode)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	if line != "locked\n" {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("helper process: %q", line)
	}
	return cmd
}

// #107: two `ambit local up --name x` processes. The second fails at once; the lock is free when the first ends,
// also after kill -9 (spec 6.4).
func TestLockBetweenProcesses(t *testing.T) {
	dir := t.TempDir()
	s := Store{Dir: dir}

	holder := startHolder(t, dir, "wait")
	start := time.Now()
	_, err := s.Lock("x")
	var e *out.Error
	if !errors.As(err, &e) || !strings.Contains(e.What, `another ambit local up or down for "x" is running`) {
		t.Fatalf("second process: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the second run waited %s; it must fail at once", d)
	}
	if unlock, err := s.Lock("y"); err != nil {
		t.Fatalf("another name is not locked: %v", err)
	} else {
		unlock()
	}
	holder.Process.Kill() // SIGKILL on Unix, TerminateProcess on Windows
	holder.Wait()
	unlock, err := s.Lock("x")
	if err != nil {
		t.Fatalf("lock after kill -9 of the holder: %v", err)
	}
	unlock()

	holder = startHolder(t, dir, "exit")
	holder.Wait()
	unlock, err = s.Lock("x")
	if err != nil {
		t.Fatalf("lock after the holder ended: %v", err)
	}
	unlock()
}

// The lock is held for the whole of local up and local down.
func TestUpAndDownHoldTheLock(t *testing.T) {
	r := newRig(t, false)
	unlock, err := r.env.Store.Lock("ambit")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	for name, run := range map[string]func() error{
		"up": func() error { return Up(context.Background(), r.p, r.env, r.opts) },
		"down": func() error {
			return Down(context.Background(), r.p, r.env, "ambit", strings.NewReader(""), false, true)
		},
	} {
		err := run()
		var e *out.Error
		if !errors.As(err, &e) || !strings.Contains(e.What, "another ambit local up or down") {
			t.Errorf("%s while locked: %v", name, err)
		}
	}
	if r.kind.created != nil || len(r.ops.calls) != 0 {
		t.Fatalf("a locked run changed something: created %v, calls %v", r.kind.created != nil, r.ops.calls)
	}
}

// #108 S1: the state file's bytes reach the disk before the rename, so a power loss cannot leave an empty file.
func TestStoreWriteSyncsBeforeRename(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	synced := false
	old := syncFile
	syncFile = func(f *os.File) error {
		if _, err := os.Stat(s.Path("ambit")); err == nil {
			t.Error("the state file was renamed before Sync")
		}
		synced = true
		return f.Sync()
	}
	defer func() { syncFile = old }()
	if err := s.Write(State{Name: "ambit", Status: "ready"}); err != nil || !synced {
		t.Fatalf("Write = %v, synced %v", err, synced)
	}
	syncFile = func(*os.File) error { return errors.New("disk full") }
	if err := s.Write(State{Name: "ambit", Status: "creating"}); err == nil {
		t.Fatal("a failed Sync must fail Write")
	}
	if st, _ := s.Read("ambit"); st == nil || st.Status != "ready" {
		t.Fatalf("a failed write replaced the state file: %+v", st)
	}
}

// #108 S2: Windows reserved device names are refused on every system, so a name works the same everywhere.
func TestReservedWindowsNamesAreRefused(t *testing.T) {
	for _, name := range []string{"con", "prn", "aux", "nul", "com1", "com9", "lpt1", "lpt9", "nul.x", "con.local"} {
		var e *out.Error
		if err := checkName(name); !errors.As(err, &e) || e.Code != out.ExitUsage || !strings.Contains(e.Why, "Windows") {
			t.Errorf("checkName(%q) = %v, want a usage error that names Windows", name, err)
		}
	}
	for _, name := range []string{"console", "com10", "nulx", "auxiliary", "lpt", "ambit"} {
		if err := checkName(name); err != nil {
			t.Errorf("checkName(%q) = %v", name, err)
		}
	}
}
