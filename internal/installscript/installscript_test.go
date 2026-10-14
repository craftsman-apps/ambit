package installscript

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const fakeBinary = "#!/bin/sh\necho fake ambit\n"

func targz(t *testing.T, name, body string) []byte {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))})
	tw.Write([]byte(body))
	tw.Close()
	gz.Close()
	return b.Bytes()
}

func zipped(t *testing.T, name, body string) []byte {
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, _ := zw.Create(name)
	w.Write([]byte(body))
	zw.Close()
	return b.Bytes()
}

// release serves /download/v0.2.0/<files> and /latest/download/<files>. With badSum the checksum is wrong.
func release(t *testing.T, files map[string][]byte, badSum bool) string {
	var sums strings.Builder
	for name, b := range files {
		sum := sha256.Sum256(b)
		if badSum {
			sum = sha256.Sum256([]byte("other"))
		}
		fmt.Fprintf(&sums, "%x  %s\n", sum, name)
	}
	mux := http.NewServeMux()
	serve := func(prefix string) {
		mux.HandleFunc(prefix+"checksums.txt", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(sums.String())) })
		for name, b := range files {
			b := b
			mux.HandleFunc(prefix+name, func(w http.ResponseWriter, _ *http.Request) { w.Write(b) })
		}
	}
	serve("/download/v0.2.0/")
	serve("/latest/download/")
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s.URL
}

func need(t *testing.T, tool string) string {
	p, err := exec.LookPath(tool)
	if err != nil {
		if os.Getenv("CI") != "" && !(tool == "sh" && runtime.GOOS == "windows") {
			t.Fatalf("%s is required in CI", tool)
		}
		t.Skipf("no %s on PATH", tool)
	}
	return p
}

func unixArchive() string {
	arch := map[string]string{"amd64": "amd64", "arm64": "arm64"}[runtime.GOARCH]
	return fmt.Sprintf("ambit_%s_%s.tar.gz", runtime.GOOS, arch)
}

func runSh(t *testing.T, base string, env ...string) (string, string, error) {
	sh := need(t, "sh")
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is for macOS and Linux")
	}
	dir := t.TempDir()
	cmd := exec.Command(sh, "../../site/static/install.sh")
	cmd.Env = append(os.Environ(), append([]string{"AMBIT_DOWNLOAD_BASE=" + base, "AMBIT_INSTALL_DIR=" + dir}, env...)...)
	out, err := cmd.CombinedOutput()
	return string(out), dir, err
}

func TestInstallShPinnedVersion(t *testing.T) {
	base := release(t, map[string][]byte{unixArchive(): targz(t, "ambit", fakeBinary)}, false)
	out, dir, err := runSh(t, base, "AMBIT_VERSION=v0.2.0")
	if err != nil || !strings.Contains(out, "✓ installed ambit to "+filepath.Join(dir, "ambit")) || !strings.Contains(out, "Next: ambit local up") {
		t.Fatalf("err %v, output:\n%s", err, out)
	}
	if fi, err := os.Stat(filepath.Join(dir, "ambit")); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("binary: %v %v", fi, err)
	}
}

func TestInstallShLatest(t *testing.T) {
	base := release(t, map[string][]byte{unixArchive(): targz(t, "ambit", fakeBinary)}, false)
	if out, _, err := runSh(t, base); err != nil {
		t.Fatalf("latest: %v\n%s", err, out)
	}
}

func TestInstallShChecksumMismatch(t *testing.T) {
	base := release(t, map[string][]byte{unixArchive(): targz(t, "ambit", fakeBinary)}, true)
	out, dir, err := runSh(t, base, "AMBIT_VERSION=0.2.0")
	if err == nil || !strings.Contains(out, "does not match checksums.txt") {
		t.Fatalf("err %v, output:\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "ambit")); err == nil {
		t.Fatal("a binary with a wrong checksum was installed")
	}
}

func TestInstallShMissingVersion(t *testing.T) {
	base := release(t, map[string][]byte{}, false)
	out, _, err := runSh(t, base, "AMBIT_VERSION=9.9.9")
	if err == nil || !strings.Contains(out, "✗ cannot download") || !strings.Contains(out, "next: check AMBIT_VERSION") {
		t.Fatalf("err %v, output:\n%s", err, out)
	}
}

func runPS(t *testing.T, base string, env ...string) (string, string, error) {
	pwsh := need(t, "pwsh")
	dir := t.TempDir()
	cmd := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-File", "../../site/static/install.ps1")
	cmd.Env = append(os.Environ(), append([]string{"AMBIT_DOWNLOAD_BASE=" + base, "AMBIT_INSTALL_DIR=" + dir,
		"PROCESSOR_ARCHITECTURE=AMD64", "LOCALAPPDATA=" + t.TempDir()}, env...)...)
	out, err := cmd.CombinedOutput()
	return string(out), dir, err
}

func TestInstallPs1(t *testing.T) {
	base := release(t, map[string][]byte{"ambit_windows_amd64.zip": zipped(t, "ambit.exe", "MZ fake")}, false)
	out, dir, err := runPS(t, base, "AMBIT_VERSION=0.2.0")
	if err != nil || !strings.Contains(out, "Installed ambit to") {
		t.Fatalf("err %v, output:\n%s", err, out)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "ambit.exe")); err != nil || string(b) != "MZ fake" {
		t.Fatalf("ambit.exe: %q %v", b, err)
	}
}

func TestInstallPs1ChecksumMismatch(t *testing.T) {
	base := release(t, map[string][]byte{"ambit_windows_amd64.zip": zipped(t, "ambit.exe", "MZ fake")}, true)
	out, dir, err := runPS(t, base)
	if err == nil || !strings.Contains(out, "does not match checksums.txt") {
		t.Fatalf("err %v, output:\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "ambit.exe")); err == nil {
		t.Fatal("a binary with a wrong checksum was installed")
	}
}

const noRelease = "No Ambit CLI release is published yet (the latest release has no ambit_<os>_<arch> file). See https://github.com/craftsman-apps/ambit/releases"

func TestInstallShNoReleaseYet(t *testing.T) {
	base := release(t, map[string][]byte{}, false)
	out, dir, err := runSh(t, base)
	if err == nil || !strings.Contains(out, noRelease) {
		t.Fatalf("err %v, output:\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "ambit")); err == nil {
		t.Fatal("a binary was installed")
	}
}

func TestInstallPs1NoReleaseYet(t *testing.T) {
	base := release(t, map[string][]byte{}, false)
	out, dir, err := runPS(t, base)
	if err == nil || !strings.Contains(out, noRelease) {
		t.Fatalf("err %v, output:\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "ambit.exe")); err == nil {
		t.Fatal("a binary was installed")
	}
}
