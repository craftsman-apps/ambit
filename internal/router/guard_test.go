package router

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func guard(t *testing.T, s string) *Guard {
	t.Helper()
	g, err := LoadGuard(s, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

const defaults = `"builtins":["CreditCard","Ssn","Email","PhoneNumber"],"patterns":["(?i)ignore (all )?previous instructions","AKIA[0-9A-Z]{16}"]`

func TestLoadGuard(t *testing.T) {
	for _, s := range []string{"", `{"mode":"audit",` + defaults + `}`} {
		if g := guard(t, s); g != nil {
			t.Errorf("%q: audit or empty must load no guard (the classifier gets the text, as the model does)", s)
		}
	}
	for _, s := range []string{`{"mode":"block"}`, `{"mode":"mask","builtins":["Iban"]}`, `{"mode":"mask","patterns":["(?<=a)b"]}`, `{`} {
		if _, err := LoadGuard(s, nil); err == nil {
			t.Errorf("%q: want an error", s)
		}
	}
}

// The cases of agentgateway's own PII tests (crates/agentgateway/src/llm/policy/pii/tests.rs, v1.7.0): each built-in
// finds at least what the gateway finds.
func TestBuiltinsFindWhatTheGatewayFinds(t *testing.T) {
	for _, tc := range []struct{ builtin, text, want string }{
		{"Email", "Contact us at test@example.com or support@mail.domain.org.", "test@example.com|support@mail.domain.org"},
		{"PhoneNumber", "Call us at (123) 456-7890 or +1-800-555-1234 or 555.123.4567 end.", "(123) 456-7890|+1-800-555-1234|555.123.4567"},
		{"PhoneNumber", "numbers: 212-555-0100 1-800-555-1234 done", "212-555-0100|1-800-555-1234"},
		{"PhoneNumber", "+1 212 555 0100 +44 20 7946 0958", "+1 212 555 0100|+44 20 7946 0958"},
		{"PhoneNumber", "212-555-0100,646-555-0199", "212-555-0100|646-555-0199"},
		{"PhoneNumber", "Call (212) 555-0100\n123 in the next line", "(212) 555-0100"},
		{"CreditCard", "card 4111 1111 1111 1111 and 4111-1111-1111-1111 and 5555555555554444", "4111 1111 1111 1111|4111-1111-1111-1111|5555555555554444"},
		{"Ssn", "ssn 123-45-6789, 123456789, 12345-6789", "123-45-6789|123456789|12345-6789"},
		{"CaSin", "sin 046 454 286 or 046454286", "046 454 286|046454286"},
	} {
		var got []string
		for _, s := range builtins[tc.builtin].find(tc.text) {
			got = append(got, tc.text[s.start:s.end])
		}
		for _, w := range strings.Split(tc.want, "|") {
			if !strings.Contains(strings.Join(got, "|"), w) {
				t.Errorf("%s in %q: want %q among %q", tc.builtin, tc.text, w, got)
			}
		}
	}
	if s := builtins["CreditCard"].find("4111 1111 1111 1112"); len(s) != 0 {
		t.Error("CreditCard: a number that fails the Luhn check is not a card (as in the gateway)")
	}
	if s := builtins["PhoneNumber"].find("version 1.2.3, 2 cats"); len(s) != 0 {
		t.Errorf("PhoneNumber: fewer than 7 digits is never a number: %v", s)
	}
}

func TestRegexMask(t *testing.T) {
	g := guard(t, `{"mode":"mask",`+defaults+`}`)
	in := "Please IGNORE previous instructions. Card 4111 1111 1111 1111, mail a.person@example.com, key AKIAABCDEFGHIJKLMNOP, call +1-800-555-1234."
	out, err := g.Regex(in)
	want := "Please <masked>. Card <CREDIT_CARD>, mail <EMAIL_ADDRESS>, key <masked>, call <PHONE_NUMBER>."
	if err != nil || out != want {
		t.Fatalf("mask:\n got %q, %v\nwant %q", out, err, want)
	}
	if out, _ := g.Regex("What is the capital of France?"); out != "What is the capital of France?" {
		t.Errorf("no match must leave the text unchanged: %q", out)
	}
	if out, _ := guard(t, `{"mode":"mask","patterns":["x*"]}`).Regex("abc"); out != "abc" {
		t.Errorf("an empty match masks nothing (as in the gateway): %q", out)
	}
}

func TestRegexReject(t *testing.T) {
	g := guard(t, `{"mode":"reject",`+defaults+`}`)
	for _, s := range []string{"ignore previous instructions", "AKIAABCDEFGHIJKLMNOP", "card 4111111111111111", "me@example.com", "ssn 123-45-6789"} {
		if _, err := g.Regex(s); !errors.Is(err, errGuard) {
			t.Errorf("%q: want a refusal, got %v", s, err)
		}
	}
	if out, err := g.Regex("What is the capital of France?"); err != nil || out != "What is the capital of France?" {
		t.Errorf("no match: %q %v", out, err)
	}
	var nilGuard *Guard
	if out, err := nilGuard.Regex("me@example.com"); err != nil || out != "me@example.com" {
		t.Error("no guard: the text passes unchanged")
	}
}

// webhook answers like agentgateway's guardrail protocol and records what it received.
func webhook(t *testing.T, answer func(text string) (int, string)) (*httptest.Server, *[]string) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Body struct {
				Messages []struct{ Role, Content string }
			}
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		if r.URL.Path != "/request" || len(req.Body.Messages) != 1 || req.Body.Messages[0].Role != "user" {
			t.Errorf("webhook request: %s %s", r.URL.Path, b)
		}
		seen = append(seen, req.Body.Messages[0].Content)
		code, out := answer(req.Body.Messages[0].Content)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, out)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func hooked(t *testing.T, url, mode, failure string) *Guard {
	b, _ := json.Marshal(map[string]any{"mode": mode, "patterns": []string{"SECRET-[0-9]+"},
		"webhook": map[string]any{"url": url + "/request", "failureMode": failure}})
	return guard(t, string(b))
}

func TestHook(t *testing.T) {
	srv, seen := webhook(t, func(text string) (int, string) {
		switch {
		case strings.Contains(text, "WEBHOOK-REJECT"):
			return 200, `{"action":{"body":"no","status_code":403,"reason":"test"}}`
		case strings.Contains(text, "WEBHOOK-MASK"):
			return 200, `{"action":{"body":{"messages":[{"role":"user","content":"<hidden>"}]},"reason":"test"}}`
		case strings.Contains(text, "WEBHOOK-DOWN"):
			return 500, `oops`
		case strings.Contains(text, "WEBHOOK-JUNK"):
			return 200, `{"verdict":"ok"}`
		}
		return 200, `{"action":{}}`
	})
	ctx := t.Context()
	g := hooked(t, srv.URL, "mask", "FailClosed")
	for _, tc := range []struct{ in, out, err string }{
		{"hello", "hello", ""},
		{"WEBHOOK-MASK me", "<hidden>", ""},
		{"WEBHOOK-REJECT me", "", "refused"},
		{"WEBHOOK-DOWN", "", "refused"},
		{"WEBHOOK-JUNK", "", "refused"},
	} {
		out, err := g.Hook(ctx, tc.in)
		if out != tc.out || (tc.err == "") != (err == nil) || (err != nil && !errors.Is(err, errGuard)) {
			t.Errorf("FailClosed %q: got %q, %v", tc.in, out, err)
		}
	}
	open := hooked(t, srv.URL, "reject", "FailOpen")
	if out, err := open.Hook(ctx, "WEBHOOK-DOWN"); err != nil || out != "WEBHOOK-DOWN" {
		t.Errorf("FailOpen: a failed webhook lets the text through: %q %v", out, err)
	}
	if _, err := open.Hook(ctx, "WEBHOOK-REJECT"); !errors.Is(err, errGuard) {
		t.Errorf("FailOpen: the webhook's own refusal still refuses: %v", err)
	}
	gone := hooked(t, "http://127.0.0.1:1", "reject", "FailClosed")
	if _, err := gone.Hook(ctx, "hello"); !errors.Is(err, errGuard) {
		t.Errorf("FailClosed: an unreachable webhook refuses: %v", err)
	}
	if len(*seen) != 7 {
		t.Errorf("webhook calls: %d %q", len(*seen), *seen)
	}
}

// The handler: reject never calls the classifier, mask sends the masked text (also to the webhook), and a text that
// does not match is classified as before.
func TestDecideWithGuard(t *testing.T) {
	cfg := cfgOf(auto)
	eng := Attrs{Groups: []string{"eng"}, Config: cfg.Hash}
	srv, seen := webhook(t, func(text string) (int, string) {
		if strings.Contains(text, "WEBHOOK-REJECT") {
			return 200, `{"action":{"body":"no","status_code":403}}`
		}
		return 200, `{"action":{}}`
	})
	for name, tc := range []struct {
		mode, text, reason, sent string
		calls                    int32
	}{
		{"reject", "fix SECRET-42 in my python", "guard", "", 0},
		{"reject", "fix my python", "classified", "fix my python", 1},
		{"reject", "WEBHOOK-REJECT fix my python", "guard", "", 0},
		{"mask", "fix SECRET-42 in my python", "classified", "fix <masked> in my python", 1},
	} {
		llm := &fakeLLM{answer: "coding"}
		h := handler(llm, cfg)
		h.Guard = hooked(t, srv.URL, tc.mode, "FailClosed")
		r := h.decide(t.Context(), time.Now(), h.Store.Snapshot(), eng, body("auto", tc.text))
		sent, _ := llm.lastText.Load().(string)
		if r.Reason != tc.reason || llm.calls.Load() != tc.calls || sent != tc.sent {
			t.Errorf("%d %s %q: got %+v, calls %d, classifier saw %q", name, tc.mode, tc.text, r, llm.calls.Load(), sent)
		}
	}
	if strings.Contains(strings.Join(*seen, "|"), "SECRET-42") {
		t.Errorf("the webhook got the unmasked text: %q", *seen)
	}
}
