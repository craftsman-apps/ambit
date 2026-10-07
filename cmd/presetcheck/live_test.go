package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/craftsman-apps/ambit/internal/console"
	"github.com/craftsman-apps/ambit/internal/presets"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeVendor: a TLS vendor with /token (refresh rotates rt1 → rt2) and /mcp (go-sdk server, tool "whoami",
// requires the renewed access token). onCall runs at each MCP request.
func fakeVendor(t *testing.T, toolError bool, onCall func()) (*httptest.Server, presets.Preset) {
	return fakeVendorWith(t, "rt1", "at2", "rt2", toolError, "alice", onCall)
}

// fakeVendorWith is fakeVendor with chosen tokens: refresh rotates oldRT → newRT and issues access; the tool
// answers text.
func fakeVendorWith(t *testing.T, oldRT, access, newRT string, toolError bool, text string, onCall func()) (*httptest.Server, presets.Preset) {
	s := mcp.NewServer(&mcp.Implementation{Name: "v", Version: "1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "whoami"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{IsError: toolError, Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
	})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			r.ParseForm()
			if r.PostForm.Get("refresh_token") != oldRT {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			w.Write([]byte(`{"access_token":"` + access + `","refresh_token":"` + newRT + `","expires_in":3600}`))
		case "/mcp":
			if onCall != nil {
				onCall()
			}
			if r.Header.Get("Authorization") != "Bearer "+access {
				w.WriteHeader(401)
				return
			}
			h.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, presets.Preset{ID: "v", Version: 1, Server: presets.ServerRef{URL: srv.URL + "/mcp", Transport: "streamable-http"},
		AuthorizationURL: srv.URL + "/authorize", TokenURL: srv.URL + "/token", Live: presets.Live{Tool: "whoami"}}
}

func TestLiveSavesRotatedTokenBeforeMCPCalls(t *testing.T) {
	pc := &PresetCreds{ClientID: "id", ClientSecret: "s", RefreshToken: "rt1"}
	saved := ""
	srv, p := fakeVendor(t, false, func() {
		if saved != "rt2" {
			t.Error("MCP call before the rotated token was saved")
		}
	})
	res := liveCheck(t.Context(), srv.Client(), p, pc, func() error { saved = pc.RefreshToken; return nil })
	if !res.Pass || pc.RefreshToken != "rt2" || len(res.Tools) != 1 {
		t.Fatalf("result %+v token %s", res, pc.RefreshToken)
	}
}

func TestLiveFailsOnToolErrorWithoutLeakingTokens(t *testing.T) {
	srv, p := fakeVendor(t, true, nil)
	res := liveCheck(t.Context(), srv.Client(), p, &PresetCreds{ClientID: "id", ClientSecret: "s", RefreshToken: "rt1"}, func() error { return nil })
	if res.Pass || !strings.Contains(res.Error, "isError") || strings.Contains(res.Error, "rt1") || strings.Contains(res.Error, "at2") {
		t.Fatalf("%+v", res)
	}
}

func TestLiveWithoutRefreshTokenUsesAccessTokenAndReports(t *testing.T) {
	srv, p := fakeVendor(t, false, nil)
	pc := &PresetCreds{ClientID: "id", ClientSecret: "s", AccessToken: "at2", Expiry: time.Now().Add(time.Hour)}
	res := liveCheck(t.Context(), srv.Client(), p, pc, func() error { return nil })
	if !res.Pass || !res.NoRefreshToken {
		t.Fatalf("%+v", res)
	}
}

// A zero expiry (no expires_in and no refresh token, as GitHub OAuth Apps answer) means the token does not expire.
func TestLiveWithNonExpiringAccessTokenUsesIt(t *testing.T) {
	srv, p := fakeVendor(t, false, nil)
	pc := &PresetCreds{ClientID: "id", ClientSecret: "s", AccessToken: "at2"}
	res := liveCheck(t.Context(), srv.Client(), p, pc, func() error { return nil })
	if !res.Pass || !res.NoRefreshToken {
		t.Fatalf("%+v", res)
	}
}

// A token that does not expire and that the vendor cancelled: the error says how to sign in again.
func TestLiveRefusedNonExpiringTokenSaysHowToSignInAgain(t *testing.T) {
	srv, p := fakeVendor(t, false, nil)
	pc := &PresetCreds{ClientID: "id", ClientSecret: "s", AccessToken: "cancelled-token"}
	res := liveCheck(t.Context(), srv.Client(), p, pc, func() error { return nil })
	want := `remove "accessToken" from presets.v in ~/.config/ambit/presetcheck.json, then run: make preset-live APP=v`
	if res.Pass || !strings.Contains(res.Error, "sign in again") || !strings.Contains(res.Error, want) || strings.Contains(res.Error, "cancelled-token") {
		t.Fatalf("%+v", res)
	}
}

func TestLiveRenewErrorKeepsTheVendorCode(t *testing.T) {
	srv, p := fakeVendor(t, false, nil)
	res := liveCheck(t.Context(), srv.Client(), p, &PresetCreds{ClientID: "id", ClientSecret: "s", RefreshToken: "stale"}, func() error { return nil })
	if res.Pass || !strings.Contains(res.Error, "invalid_grant") {
		t.Fatalf("%+v", res)
	}
}

func TestLiveSaveFailureStopsBeforeMCP(t *testing.T) {
	called := false
	srv, p := fakeVendor(t, false, func() { called = true })
	res := liveCheck(t.Context(), srv.Client(), p, &PresetCreds{ClientID: "id", ClientSecret: "s", RefreshToken: "rt1"}, func() error { return errors.New("disk full") })
	if res.Pass || called || !strings.Contains(res.Error, "save") {
		t.Fatalf("%+v called=%v", res, called)
	}
}

func TestSecondAccountDiffAndToolFile(t *testing.T) {
	got := fingerprintDiff(map[string]string{"a": "1", "b": "2", "c": "3"}, map[string]string{"a": "1", "b": "9", "d": "4"})
	if strings.Join(got, ",") != "b,c,d" {
		t.Fatalf("diff %v", got)
	}
	path := filepath.Join(t.TempDir(), "tools", "v.txt")
	if err := reportToolChanges(path, []string{"whoami", "search"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "search\nwhoami\n" {
		t.Fatalf("file %q", b)
	}
	if redact("token abcdefgh1234 and s", "abcdefgh1234", "s", "") != "token [secret] and s" {
		t.Fatal("redact")
	}
}

func TestLiveRedactsLongTokensInToolErrorText(t *testing.T) {
	const oldRT, access, newRT = "refresh-old-1234567", "access-token-7654321", "refresh-new-1234567"
	srv, p := fakeVendorWith(t, oldRT, access, newRT, true, "bad token "+access+" after "+oldRT+" now "+newRT, nil)
	res := liveCheck(t.Context(), srv.Client(), p, &PresetCreds{ClientID: "id", ClientSecret: "s", RefreshToken: oldRT}, func() error { return nil })
	if res.Pass || !strings.Contains(res.Error, "[secret]") {
		t.Fatalf("%+v", res)
	}
	for _, tok := range []string{oldRT, access, newRT} {
		if strings.Contains(res.Error, tok) {
			t.Fatalf("token in error: %s", res.Error)
		}
	}
}

func TestSecondAccountDiffOnlyWhenItPassed(t *testing.T) {
	first := Result{Pass: true, fingerprints: map[string]string{"a": "1"}}
	r := withSecond(first, Result{Error: "renew: invalid_grant"})
	if r.Pass || r.Error != "second account: renew: invalid_grant" || len(r.DiffersBySecondAccount) != 0 {
		t.Fatalf("%+v", r)
	}
	r = withSecond(first, Result{Pass: true, fingerprints: map[string]string{"a": "2"}})
	if !r.Pass || strings.Join(r.DiffersBySecondAccount, ",") != "a" {
		t.Fatalf("%+v", r)
	}
}

// A preset with no live.arguments still sends `"arguments": {}`: GitHub refuses `null` ("arguments must be a JSON object").
func TestCallToolSendsAnEmptyObjectWithoutArguments(t *testing.T) {
	var raw string
	s := mcp.NewServer(&mcp.Implementation{Name: "v", Version: "1"}, nil)
	s.AddTool(&mcp.Tool{Name: "get_me", InputSchema: &jsonschema.Schema{Type: "object"}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		raw = string(req.Params.Arguments)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})
	srv := httptest.NewTLSServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true}))
	defer srv.Close()
	if err := callTool(t.Context(), srv.Client(), srv.URL, "streamable-http", "at", "get_me", nil); err != nil {
		t.Fatal(err)
	}
	if raw != "{}" {
		t.Fatalf("arguments sent as %q, want {}", raw)
	}
}

// A saved entry with an empty client ID or secret (a paste that did not register) is asked for again.
func TestCredsWithAnEmptySecretAreAskedAgain(t *testing.T) {
	for _, c := range []struct {
		pc   *PresetCreds
		want bool
	}{
		{nil, false},
		{&PresetCreds{ClientID: "d6a5579187ebec82d5ab9970516c549f"}, false},
		{&PresetCreds{ClientSecret: "s3cret-value"}, false},
		{&PresetCreds{ClientID: "d6a5579187ebec82d5ab9970516c549f", ClientSecret: "s3cret-value"}, true},
	} {
		if got := credsComplete(c.pc); got != c.want {
			t.Fatalf("credsComplete(%+v) = %v", c.pc, got)
		}
	}
}

// A vendor that refuses the saved client (invalid_client) makes the run forget the secret, so the next run asks.
func TestRefusedClientIsForgotten(t *testing.T) {
	pc := &PresetCreds{ClientID: "xh1655l48q5vjiw", ClientSecret: "wrong-wrong-wrong", RefreshToken: "rt"}
	refused := fmt.Errorf("sign-in: %w", &console.TokenError{Status: 400, Code: "invalid_client", Description: "Invalid client_id or client_secret"})
	if !forgetRefusedClient(pc, refused) || pc.ClientSecret != "" || pc.RefreshToken != "" || credsComplete(pc) {
		t.Fatalf("refused client kept: %+v", pc)
	}
	other := &PresetCreds{ClientID: "id", ClientSecret: "s3cret-value"}
	if forgetRefusedClient(other, fmt.Errorf("sign-in: %w", &console.TokenError{Status: 400, Code: "invalid_grant"})) || other.ClientSecret == "" {
		t.Fatal("an invalid_grant answer must not forget the client secret")
	}
}
