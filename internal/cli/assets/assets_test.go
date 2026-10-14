package assets

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/craftsman-apps/ambit/internal/validate"
)

// The release workflow builds crds.yaml with: for f in deploy/crds/*.yaml; do cat "$f"; printf '\n---\n'; done
func TestCRDBundleEqualsReleaseConcatenation(t *testing.T) {
	files, err := filepath.Glob("../../../deploy/crds/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v %v", files, err)
	}
	var want bytes.Buffer
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		want.Write(b)
		want.WriteString("\n---\n")
	}
	if !bytes.Equal(CRDBundle(), want.Bytes()) {
		t.Fatalf("embedded bundle (%d bytes) differs from the release concatenation (%d bytes)", len(CRDBundle()), want.Len())
	}
}

func TestQuickstartValuesSetsConsoleURL(t *testing.T) {
	v, err := QuickstartValues("http://console.localtest.me:18080")
	if err != nil {
		t.Fatal(err)
	}
	c := v["console"].(map[string]any)
	if c["url"] != "http://console.localtest.me:18080" || c["host"] != "console.localtest.me" {
		t.Fatalf("console = %v", c)
	}
	if g := v["gateway"].(map[string]any)["service"].(map[string]any); g["nodePort"] != float64(30080) {
		t.Fatalf("gateway.service = %v; the container side must stay NodePort 30080", g)
	}
}

func TestTestModelManifest(t *testing.T) {
	want := map[string]bool{"ConfigMap/ambit-local-model": false, "Deployment/ambit-local-model": false,
		"Service/ambit-local-model": false, "Secret/ambit-llm-local": false, "Model/local-test": false}
	for _, doc := range bytes.Split(TestModel(), []byte("\n---\n")) {
		var o struct {
			Kind     string
			Metadata struct {
				Name   string
				Labels map[string]string
			}
		}
		if err := yaml.Unmarshal(doc, &o); err != nil {
			t.Fatal(err)
		}
		if o.Metadata.Labels["ambitproject.dev/created-by"] != "ambit-cli" {
			t.Errorf("%s/%s has no created-by label", o.Kind, o.Metadata.Name)
		}
		want[o.Kind+"/"+o.Metadata.Name] = true
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("missing %s", k)
		}
	}
	if !bytes.Contains(TestModel(), []byte("python@sha256:79e7a9b9ff1cbceff819f856fb374477792a5967759d94df266de7b7b4120e6f")) {
		t.Error("the image is not the pinned python:3.13-alpine digest")
	}
	v, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	problems, n, err := v.File(context.Background(), bytes.NewReader(TestModel()))
	if err != nil || n != 1 || len(problems) != 0 {
		t.Fatalf("validate: %d Ambit objects, problems %v, err %v", n, problems, err)
	}
}

// The spec (4.1 step 5) requires a test that runs the script with Python and checks one plain and one streamed answer.
func TestTestModelServer(t *testing.T) {
	py := ""
	for _, name := range []string{"python3", "python"} {
		if p, err := exec.LookPath(name); err == nil {
			py = p
			break
		}
	}
	if py == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("Python is required in CI for this test")
		}
		t.Skip("no Python on PATH")
	}
	var cm struct{ Data map[string]string }
	if err := yaml.Unmarshal(bytes.Split(TestModel(), []byte("\n---\n"))[0], &cm); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "server.py")
	if err := os.WriteFile(script, []byte(cm.Data["server.py"]), 0o600); err != nil {
		t.Fatal(err)
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	cmd := exec.Command(py, script)
	cmd.Env = append(os.Environ(), fmt.Sprintf("PORT=%d", port))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", port)
	post := func(body string) *http.Response {
		deadline := time.Now().Add(10 * time.Second)
		for {
			r, err := http.Post(url, "application/json", strings.NewReader(body))
			if err == nil {
				return r
			}
			if time.Now().After(deadline) {
				t.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	r := post(`{"model":"local/echo","messages":[{"role":"system","content":"be short"},{"role":"user","content":"hello there"}]}`)
	var plain struct {
		Choices []struct{ Message struct{ Content string } }
		Usage   struct{ PromptTokens, CompletionTokens, TotalTokens int } `json:"usage"`
	}
	json.NewDecoder(r.Body).Decode(&plain)
	r.Body.Close()
	if r.StatusCode != 200 || plain.Choices[0].Message.Content != "You said: hello there" {
		t.Fatalf("plain answer: %d %+v", r.StatusCode, plain)
	}

	r = post(`{"model":"local/echo","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)
	defer r.Body.Close()
	var text strings.Builder
	usage, done := false, false
	s := bufio.NewScanner(r.Body)
	for s.Scan() {
		line, ok := strings.CutPrefix(s.Text(), "data: ")
		if !ok {
			continue
		}
		if line == "[DONE]" {
			done = true
			break
		}
		var c struct {
			Choices []struct{ Delta struct{ Content string } }
			Usage   *struct{ TotalTokens int } `json:"usage"`
		}
		json.Unmarshal([]byte(line), &c)
		for _, ch := range c.Choices {
			text.WriteString(ch.Delta.Content)
		}
		usage = usage || c.Usage != nil
	}
	if text.String() != "You said: hi" || !usage || !done {
		t.Fatalf("stream: text %q, usage %v, done %v", text.String(), usage, done)
	}
}
