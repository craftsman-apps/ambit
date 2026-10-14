package local

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/craftsman-apps/ambit/internal/cli/checks"
)

const (
	desktopInfo  = `{"ServerVersion":"29.1.0","OperatingSystem":"Docker Desktop","MemTotal":8217030656,"SecurityOptions":["name=seccomp,profile=builtin","name=cgroupns"]}`
	rootlessInfo = `{"ServerVersion":"29.1.0","OperatingSystem":"Ubuntu 24.04 LTS","MemTotal":16000000000,"SecurityOptions":["name=seccomp,profile=builtin","name=rootless","name=cgroupns"]}`
	podmanInfo   = `{"host":{"arch":"arm64","os":"linux"},"version":{"Version":"5.4.0"}}`
	downInfo     = `{"ServerErrors":["Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?"]}`
)

func statuses(rs []checks.Result) map[string]checks.Status {
	m := map[string]checks.Status{}
	for _, r := range rs {
		m[r.ID] = r.Status
	}
	return m
}

func input(info string) PreflightInput {
	return PreflightInput{Name: "ambit", HTTPPort: 8080, HTTPSPort: 8443, Docker: ParseDockerInfo([]byte(info)),
		Ownership: Ownership{Verdict: Absent}, PortFree: func(int) bool { return true }}
}

func TestParseDockerInfo(t *testing.T) {
	d := ParseDockerInfo([]byte(desktopInfo))
	if !d.Reachable || d.OS != "Docker Desktop" || d.MemTotal != 8217030656 || d.Rootless || d.Podman {
		t.Fatalf("desktop = %+v", d)
	}
	if d := ParseDockerInfo([]byte(rootlessInfo)); !d.Rootless {
		t.Fatal("rootless not detected")
	}
	if d := ParseDockerInfo([]byte(podmanInfo)); !d.Podman {
		t.Fatal("podman not detected")
	}
	if d := ParseDockerInfo([]byte(downInfo)); d.Reachable || !strings.Contains(d.Err, "Is the docker daemon running") {
		t.Fatalf("down = %+v", d)
	}
}

func TestPreflightAllGood(t *testing.T) {
	got := statuses(Preflight(input(desktopInfo)))
	for _, id := range []string{"L1", "L2", "L3", "L4", "L5"} {
		if got[id] != checks.Pass {
			t.Errorf("%s = %s, want pass", id, got[id])
		}
	}
}

func TestPreflightDockerDown(t *testing.T) {
	rs := Preflight(input(downInfo))
	if len(rs) != 1 || rs[0].ID != "L1" || rs[0].Status != checks.Fail || !strings.HasPrefix(rs[0].Message, "Docker is not running") || rs[0].Next == "" {
		t.Fatalf("results = %+v", rs)
	}
}

func TestPreflightUnsupportedRuntimes(t *testing.T) {
	for _, info := range []string{rootlessInfo, podmanInfo} {
		if s := statuses(Preflight(input(info)))["L2"]; s != checks.Fail {
			t.Errorf("L2 = %s, want fail", s)
		}
	}
}

func TestPreflightMemory(t *testing.T) {
	for _, c := range []struct {
		mem  string
		skip bool
		want checks.Status
	}{
		{"3500000000", false, checks.Fail}, {"3500000000", true, checks.Warn},
		{"5000000000", false, checks.Warn}, {"6200000000", false, checks.Pass},
	} {
		in := input(`{"ServerVersion":"29.1.0","OperatingSystem":"OrbStack","MemTotal":` + c.mem + `}`)
		in.SkipChecks = c.skip
		if s := statuses(Preflight(in))["L3"]; s != c.want {
			t.Errorf("mem %s skip %v: L3 = %s, want %s", c.mem, c.skip, s, c.want)
		}
	}
}

func TestPreflightBusyPort(t *testing.T) {
	in := input(desktopInfo)
	in.PortFree = func(p int) bool { return p != 8080 }
	for _, r := range Preflight(in) {
		if r.ID == "L4" && (r.Status != checks.Fail || r.Message != "port 8080 on 127.0.0.1 is in use" ||
			r.Next != "ambit local up --port 18080 --https-port 18443") {
			t.Fatalf("L4 = %+v", r)
		}
	}
	in.Ownership = Ownership{Verdict: Owned}
	if s := statuses(Preflight(in))["L4"]; s != checks.NA {
		t.Fatalf("repair mode: L4 = %s, want n/a (the ports are the cluster's own)", s)
	}
}

func TestPreflightForeignCluster(t *testing.T) {
	in := input(desktopInfo)
	in.Ownership = Ownership{Verdict: NotOwned, Reason: "there is no state file /x/ambit.json"}
	for _, r := range Preflight(in) {
		if r.ID == "L5" && (r.Status != checks.Fail || !strings.Contains(r.Message, "was not created by ambit local up") || !strings.HasPrefix(r.Next, "ambit local up --name ambit-try")) {
			t.Fatalf("L5 = %+v", r)
		}
	}
}

func TestParseDockerInfoPodmanOnlyFromIdentity(t *testing.T) {
	// A working Docker whose host name, labels and paths mention podman is still Docker.
	docker := `{"ServerVersion":"29.1.0","OperatingSystem":"Docker Desktop","Name":"podman-box","MemTotal":8217030656,` +
		`"DockerRootDir":"/home/podman/docker","Labels":["owner=podman-team"]}`
	if d := ParseDockerInfo([]byte(docker)); d.Podman || !d.Reachable {
		t.Fatalf("docker with podman in name and path = %+v", d)
	}
	if s := statuses(Preflight(input(docker)))["L2"]; s != checks.Pass {
		t.Fatalf("L2 = %s, want pass", s)
	}
	engine := `{"ServerVersion":"5.4.0","Components":[{"Name":"Podman Engine","Version":"5.4.0"}]}`
	if d := ParseDockerInfo([]byte(engine)); !d.Podman {
		t.Fatal("Podman Engine component not detected")
	}
}

func TestReadDockerInfoTimesOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake docker command is a shell script")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "docker")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	d := readDockerInfo(fake, 200*time.Millisecond)
	if time.Since(start) > 5*time.Second {
		t.Fatal("did not stop at the timeout")
	}
	if d.Reachable || d.Err != "Docker did not answer in 200ms" {
		t.Fatalf("info = %+v", d)
	}
	rs := Preflight(PreflightInput{Name: "ambit", Docker: d})
	if len(rs) != 1 || rs[0].Status != checks.Fail || !strings.Contains(rs[0].Message, "Docker did not answer in") {
		t.Fatalf("results = %+v", rs)
	}
}

func TestPreflightBusyPortKeepsName(t *testing.T) {
	in := input(desktopInfo)
	in.Name = "demo"
	in.PortFree = func(p int) bool { return p != 8080 }
	for _, r := range Preflight(in) {
		if r.ID == "L4" && r.Next != "ambit local up --name demo --port 18080 --https-port 18443" {
			t.Fatalf("L4 next = %q", r.Next)
		}
	}
}
