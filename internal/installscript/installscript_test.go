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
	"sync"
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

// addRelease serves /download/<tag>/<files>. With badSum the checksum is wrong.
func addRelease(mux *http.ServeMux, tag string, files map[string][]byte, badSum bool) {
	var sums strings.Builder
	for name, b := range files {
		sum := sha256.Sum256(b)
		if badSum {
			sum = sha256.Sum256([]byte("other"))
		}
		fmt.Fprintf(&sums, "%x  %s\n", sum, name)
	}
	prefix := "/download/" + tag + "/"
	mux.HandleFunc(prefix+"checksums.txt", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(sums.String())) })
	for name, b := range files {
		b := b
		mux.HandleFunc(prefix+name, func(w http.ResponseWriter, _ *http.Request) { w.Write(b) })
	}
}

// release plays GitHub: /download/v0.2.0/<files>, and /latest, which redirects to /tag/v0.2.0 (404 when there is
// no file, that is no release). The scripts must not use /latest/download: a second request there can reach a
// newer release than the first.
func release(t *testing.T, files map[string][]byte, badSum bool) string {
	mux := http.NewServeMux()
	addRelease(mux, "v0.2.0", files, badSum)
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		if len(files) == 0 {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/tag/v0.2.0", http.StatusFound)
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s.URL
}

// racingRelease is a repository where v0.3.0 becomes the latest release right after the first request about
// "latest". It returns the base URL and the paths requested.
func racingRelease(t *testing.T, v2, v3 map[string][]byte) (string, func() []string) {
	var mu sync.Mutex
	var paths []string
	latest := "v0.2.0"
	mux := http.NewServeMux()
	addRelease(mux, "v0.2.0", v2, false)
	addRelease(mux, "v0.3.0", v3, false)
	mux.HandleFunc("/latest/download/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tag := latest
		latest = "v0.3.0"
		mu.Unlock()
		http.Redirect(w, r, "/download/"+tag+strings.TrimPrefix(r.URL.Path, "/latest/download"), http.StatusFound)
	})
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tag := latest
		latest = "v0.3.0"
		mu.Unlock()
		http.Redirect(w, r, "/tag/"+tag, http.StatusFound)
	})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)
	return s.URL, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), paths...) }
}

// onlyTag fails when a script asked for a path outside /latest and /download/<tag>/.
func onlyTag(t *testing.T, paths []string, tag string) {
	t.Helper()
	for _, p := range paths {
		if p != "/latest" && !strings.HasPrefix(p, "/download/"+tag+"/") {
			t.Errorf("request %s: the script must read /latest once and then only /download/%s/", p, tag)
		}
	}
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

// A release that becomes the latest between the two downloads must not give a false checksum failure: the
// script resolves the tag once and reads the archive and checksums.txt from that tag.
func TestInstallShLatestIsResolvedOnce(t *testing.T) {
	v2 := map[string][]byte{unixArchive(): targz(t, "ambit", fakeBinary)}
	v3 := map[string][]byte{unixArchive(): targz(t, "ambit", "#!/bin/sh\necho newer ambit\n")}
	base, paths := racingRelease(t, v2, v3)
	out, dir, err := runSh(t, base)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "ambit")); string(b) != fakeBinary {
		t.Fatalf("installed %q, want the binary of v0.2.0", b)
	}
	onlyTag(t, paths(), "v0.2.0")
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

func TestInstallPs1LatestIsResolvedOnce(t *testing.T) {
	v2 := map[string][]byte{"ambit_windows_amd64.zip": zipped(t, "ambit.exe", "MZ v2")}
	v3 := map[string][]byte{"ambit_windows_amd64.zip": zipped(t, "ambit.exe", "MZ v3")}
	base, paths := racingRelease(t, v2, v3)
	out, dir, err := runPS(t, base)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "ambit.exe")); string(b) != "MZ v2" {
		t.Fatalf("installed %q, want the binary of v0.2.0", b)
	}
	onlyTag(t, paths(), "v0.2.0")
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

func TestInstallShLatestUnreachable(t *testing.T) {
	out, _, err := runSh(t, "http://127.0.0.1:1")
	if err == nil || !strings.Contains(out, "cannot reach http://127.0.0.1:1/latest") || strings.Contains(out, "No Ambit CLI release") {
		t.Fatalf("err %v, output:\n%s", err, out)
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

const noAnswer = "GitHub did not answer; try again"

// githubDown plays GitHub when it fails: /latest answers with status, or never answers when status is 0.
func githubDown(t *testing.T, status int) string {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status == 0 {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(s.Close)
	return s.URL
}

func TestInstallShGitHubServerError(t *testing.T) {
	for _, status := range []int{500, 502, 503, 429} {
		out, dir, err := runSh(t, githubDown(t, status))
		if err == nil || !strings.Contains(out, noAnswer) || strings.Contains(out, "No Ambit CLI release") {
			t.Fatalf("HTTP %d: err %v, output:\n%s", status, err, out)
		}
		if _, err := os.Stat(filepath.Join(dir, "ambit")); err == nil {
			t.Fatal("a binary was installed")
		}
	}
}

func TestInstallShGitHubTimeout(t *testing.T) {
	out, _, err := runSh(t, githubDown(t, 0), "AMBIT_LATEST_TIMEOUT=1")
	if err == nil || !strings.Contains(out, noAnswer) || strings.Contains(out, "No Ambit CLI release") {
		t.Fatalf("err %v, output:\n%s", err, out)
	}
}

func TestInstallPs1GitHubServerError(t *testing.T) {
	out, _, err := runPS(t, githubDown(t, 503))
	if err == nil || !strings.Contains(out, noAnswer) || strings.Contains(out, "No Ambit CLI release") {
		t.Fatalf("err %v, output:\n%s", err, out)
	}
}

func TestInstallPs1GitHubTimeout(t *testing.T) {
	out, _, err := runPS(t, githubDown(t, 0), "AMBIT_LATEST_TIMEOUT=1")
	if err == nil || !strings.Contains(out, noAnswer) || strings.Contains(out, "No Ambit CLI release") {
		t.Fatalf("err %v, output:\n%s", err, out)
	}
}
