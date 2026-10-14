package local

import (
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strings"

	"github.com/craftsman-apps/ambit/internal/cli/checks"
)

// DockerInfo is what preflight needs from `docker info`.
type DockerInfo struct {
	Reachable     bool
	Err           string // why Docker is not reachable
	ServerVersion string
	OS            string // e.g. "Docker Desktop", "OrbStack", "Ubuntu 24.04 LTS"
	MemTotal      int64  // bytes available to Docker (the Docker Desktop VM's memory on macOS and Windows)
	Rootless      bool
	Podman        bool
}

// ReadDockerInfo runs `docker info`. kind itself runs the docker command, so it must be on PATH.
func ReadDockerInfo() DockerInfo {
	raw, err := exec.Command("docker", "info", "--format", "{{json .}}").Output()
	if err != nil {
		msg := err.Error()
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		if _, lookErr := exec.LookPath("docker"); lookErr != nil {
			msg = "the docker command is not installed"
		}
		return DockerInfo{Err: msg}
	}
	return ParseDockerInfo(raw)
}

// ParseDockerInfo reads the JSON of `docker info --format '{{json .}}'`.
func ParseDockerInfo(raw []byte) DockerInfo {
	var v struct {
		ServerVersion   string
		OperatingSystem string
		MemTotal        int64
		SecurityOptions []string
		ServerErrors    []string
		Host            json.RawMessage `json:"host"` // only in Podman's own format
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return DockerInfo{Err: "docker info returned no JSON: " + err.Error()}
	}
	d := DockerInfo{ServerVersion: v.ServerVersion, OS: v.OperatingSystem, MemTotal: v.MemTotal,
		Podman: v.Host != nil || strings.Contains(strings.ToLower(string(raw)), "podman")}
	for _, o := range v.SecurityOptions {
		if strings.Contains(o, "rootless") {
			d.Rootless = true
		}
	}
	switch {
	case len(v.ServerErrors) > 0:
		d.Err = strings.Join(v.ServerErrors, "; ")
	case v.ServerVersion == "" && !d.Podman:
		d.Err = "docker info has no server version"
	default:
		d.Reachable = true
	}
	return d
}

// PreflightInput is everything preflight looks at. PortFree reports whether 127.0.0.1:port can be opened.
type PreflightInput struct {
	Name                string
	HTTPPort, HTTPSPort int
	SkipChecks          bool
	Docker              DockerInfo
	Ownership           Ownership
	PortFree            func(port int) bool
}

// PortFree reports whether a TCP listener can be opened on 127.0.0.1:port, where kind maps the ports.
func PortFree(port int) bool {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

const gb = 1_000_000_000 // decimal: a Docker Desktop VM set to "6 GB" reports about 6.2e9 bytes

// Preflight returns the local checks L1-L5 (spec 4.1 step 1, and `check --pre --local`).
func Preflight(in PreflightInput) []checks.Result {
	r := func(id string, s checks.Status, msg, next, source string) checks.Result {
		return checks.Result{ID: id, Group: checks.GroupLocal, Status: s, Message: msg, Next: next, Source: source}
	}
	soften := func(s checks.Status) checks.Status {
		if s == checks.Fail && in.SkipChecks {
			return checks.Warn
		}
		return s
	}
	var out []checks.Result
	d := in.Docker
	if !d.Reachable {
		next := "start Docker Desktop"
		if runtime.GOOS == "linux" {
			next = "start Docker: sudo systemctl start docker"
		}
		out = append(out, r("L1", checks.Fail, "Docker is not running: "+d.Err, next, "docker"))
		return out // without Docker nothing else can be checked
	}
	out = append(out, r("L1", checks.Pass, "Docker "+d.ServerVersion+" is running", "", "docker"))

	switch {
	case d.Podman:
		out = append(out, r("L2", checks.Fail, "Podman is not supported in this version (kind supports it only as an experiment)",
			"use Docker Desktop, OrbStack, Colima or Docker Engine", "docker"))
	case d.Rootless:
		out = append(out, r("L2", checks.Fail, "rootless Docker is not supported in this version (kind supports it only as an experiment)",
			"use rootful Docker Engine", "docker"))
	default:
		out = append(out, r("L2", checks.Pass, "container runtime: "+d.OS, "", "docker"))
	}

	mem := fmt.Sprintf("%.1f GB of memory available to Docker", float64(d.MemTotal)/gb)
	switch {
	case d.MemTotal < 4*gb:
		out = append(out, r("L3", soften(checks.Fail), mem+"; Ambit needs at least 4 GB, 6 GB recommended",
			"give Docker more memory (Docker Desktop: Settings, Resources), or pass --skip-checks", "docker"))
	case d.MemTotal < 6*gb:
		out = append(out, r("L3", checks.Warn, mem+"; 6 GB is recommended, so pods can be slow to start", "", "docker"))
	default:
		out = append(out, r("L3", checks.Pass, mem, "", "docker"))
	}

	if in.Ownership.Verdict == Owned {
		out = append(out, r("L4", checks.NA, fmt.Sprintf("ports %d and %d belong to the existing cluster", in.HTTPPort, in.HTTPSPort), "", "host"))
	} else {
		var busy []string
		for _, p := range []int{in.HTTPPort, in.HTTPSPort} {
			if !in.PortFree(p) {
				busy = append(busy, fmt.Sprint(p))
			}
		}
		if len(busy) > 0 {
			out = append(out, r("L4", checks.Fail, "port "+strings.Join(busy, " and ")+" on 127.0.0.1 is in use",
				fmt.Sprintf("ambit local up --port %d --https-port %d", in.HTTPPort+10000, in.HTTPSPort+10000), "host"))
		} else {
			out = append(out, r("L4", checks.Pass, fmt.Sprintf("ports %d and %d are free", in.HTTPPort, in.HTTPSPort), "", "host"))
		}
	}

	switch in.Ownership.Verdict {
	case Absent:
		out = append(out, r("L5", checks.Pass, fmt.Sprintf("no kind cluster named %q yet", in.Name), "", "docker"))
	case Owned:
		out = append(out, r("L5", checks.Pass, fmt.Sprintf("kind cluster %q was created by ambit local up; it will be repaired", in.Name), "", "docker"))
	default:
		e := in.Ownership.Refusal(in.Name)
		out = append(out, r("L5", checks.Fail, e.What+": "+e.Why, e.Next, "docker"))
	}
	return out
}
