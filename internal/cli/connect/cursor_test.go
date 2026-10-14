package connect

import (
	"os"
	"strings"
	"testing"
)

func TestConnectCursor(t *testing.T) {
	g := ConnectCursor("https://llm.example.com", "a/b:free", false)
	if g.Values[1] != [2]string{"Override OpenAI Base URL", "https://llm.example.com/v1"} || g.Values[2] != [2]string{"Model name", "a/b:free"} ||
		len(g.Steps) != 3 || g.Warning != "" || g.KeyHint != "" {
		t.Fatalf("%+v", g)
	}
	if g := ConnectCursor("https://llm.example.com", "", true); g.Values[2][1] != "<model-id>" || g.KeyHint != "Copy the key with: ambit key print --url https://llm.example.com --reveal" {
		t.Fatalf("%+v", g)
	}
	w := ConnectCursor("http://llm.localtest.me:8080", "m", false).Warning
	if !strings.Contains(w, "The LLM address http://llm.localtest.me:8080 is not a public HTTPS address") {
		t.Fatalf("warning = %q", w)
	}
	if len(DisconnectCursor()) == 0 {
		t.Fatal("no steps to undo")
	}
}

// The texts are the ones of the console's Cursor tab, so the two cannot drift.
func TestCursorTextsMatchTheConsole(t *testing.T) {
	src, err := os.ReadFile("../../../web/src/lib/tool-setup.ts")
	if err != nil {
		t.Skip("web/src/lib/tool-setup.ts is not here:", err)
	}
	g := ConnectCursor("http://llm.localtest.me", "m", false)
	texts := append(append([]string{}, g.Steps...), strings.Replace(g.Warning, "http://llm.localtest.me", "${llm}", 1))
	for _, label := range g.Values {
		texts = append(texts, label[0])
	}
	for _, s := range texts {
		if !strings.Contains(string(src), strings.ReplaceAll(s, `"`, `\"`)) {
			t.Errorf("not in tool-setup.ts: %s", s)
		}
	}
}
