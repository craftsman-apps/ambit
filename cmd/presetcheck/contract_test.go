package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/craftsman-apps/ambit/internal/presets"
)

// vendor serves an MCP endpoint, protected-resource and authorization-server metadata, and a token endpoint.
func vendor(t *testing.T, mutate func(path string, w http.ResponseWriter) bool) (*httptest.Server, presets.Preset) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mutate != nil && mutate(r.URL.Path, w) {
			return
		}
		switch r.URL.Path {
		case "/mcp":
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+srv.URL+`/.well-known/oauth-protected-resource/mcp"`)
			w.WriteHeader(401)
		case "/.well-known/oauth-protected-resource/mcp":
			w.Write([]byte(`{"resource":"` + srv.URL + `/mcp","authorization_servers":["` + srv.URL + `"]}`))
		case "/.well-known/oauth-authorization-server":
			w.Write([]byte(`{"issuer":"` + srv.URL + `","authorization_endpoint":"` + srv.URL + `/authorize","token_endpoint":"` + srv.URL + `/token"}`))
		case "/token":
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"invalid_request"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, presets.Preset{ID: "v", Server: presets.ServerRef{URL: srv.URL + "/mcp", Transport: "streamable-http"},
		AuthorizationURL: srv.URL + "/authorize", TokenURL: srv.URL + "/token"}
}

func TestContractAcceptsHealthyVendor(t *testing.T) {
	srv, p := vendor(t, nil)
	if probs := checkContract(t.Context(), srv.Client(), p, nil); len(probs) != 0 {
		t.Fatal(probs)
	}
}

func TestContractCatchesChangedEndpoint(t *testing.T) {
	srv, p := vendor(t, nil)
	p.TokenURL = srv.URL + "/oauth/token" // the vendor moved it; metadata says /token
	if !strings.Contains(strings.Join(checkContract(t.Context(), srv.Client(), p, nil), "\n"), "token_endpoint") {
		t.Fatal("moved token endpoint not reported")
	}
}

func TestContractCatchesOpenServerAndNonOAuthTokenPage(t *testing.T) {
	srv, p := vendor(t, func(path string, w http.ResponseWriter) bool {
		switch path {
		case "/mcp":
			w.WriteHeader(200) // no authentication demanded
			return true
		case "/token":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>login</html>"))
			return true
		}
		return false
	})
	probs := strings.Join(checkContract(t.Context(), srv.Client(), p, nil), "\n")
	if !strings.Contains(probs, "did not ask for authentication") || !strings.Contains(probs, "token URL") {
		t.Fatal(probs)
	}
}

func TestContractTokenAnswers(t *testing.T) {
	for body, ok := range map[string]bool{
		`{"ok":false,"error":"invalid_client_id"}`: true,  // Slack: HTTP 200, still an OAuth refusal
		`{"ok":true}`:                              false, // no error indicator
		`{"access_token":"x"}`:                     false, // a token for an empty request is not an OAuth endpoint
	} {
		srv, p := vendor(t, func(path string, w http.ResponseWriter) bool {
			if path == "/token" {
				w.Write([]byte(body))
				return true
			}
			return false
		})
		got := !strings.Contains(strings.Join(checkContract(t.Context(), srv.Client(), p, nil), "\n"), "token URL")
		if got != ok {
			t.Fatalf("%s: ok=%v", body, got)
		}
	}
}

func TestContractUsesSSEHandshakeForSSEServers(t *testing.T) {
	srv, p := vendor(t, func(path string, w http.ResponseWriter) bool { return false })
	p.Server = presets.ServerRef{URL: srv.URL + "/sse", Transport: "sse"}
	var method string
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sse" {
			method = r.Method
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid_request"}`))
	})
	if probs := checkContract(t.Context(), srv.Client(), p, nil); len(probs) != 0 || method != http.MethodGet {
		t.Fatalf("method %s problems %v", method, probs)
	}
}

func TestContractHonoursDocumentedUnauthenticatedAnswer(t *testing.T) {
	srv, p := vendor(t, func(path string, w http.ResponseWriter) bool {
		if path == "/mcp" {
			w.WriteHeader(403)
			return true
		}
		return false
	})
	if len(checkContract(t.Context(), srv.Client(), p, nil)) == 0 {
		t.Fatal("403 accepted without being documented")
	}
	p.Contract.Unauthenticated = []int{403}
	if probs := checkContract(t.Context(), srv.Client(), p, nil); len(probs) != 0 {
		t.Fatal(probs)
	}
}

// tenantVendor: the authorization server's issuer has a path, so RFC 8414 puts the metadata under
// /.well-known/oauth-authorization-server/tenant.
func tenantVendor(t *testing.T, issuerInMetadata string) (*httptest.Server, presets.Preset) {
	var srv *httptest.Server
	srv, p := vendor(t, func(path string, w http.ResponseWriter) bool {
		switch path {
		case "/.well-known/oauth-protected-resource/mcp":
			w.Write([]byte(`{"authorization_servers":["` + srv.URL + `/tenant"]}`))
		case "/.well-known/oauth-authorization-server/tenant":
			iss := issuerInMetadata
			if iss == "" {
				iss = srv.URL + "/tenant"
			}
			w.Write([]byte(`{"issuer":"` + iss + `","authorization_endpoint":"` + srv.URL + `/authorize","token_endpoint":"` + srv.URL + `/token"}`))
		default:
			return false
		}
		return true
	})
	return srv, p
}

func TestContractIssuerWithPath(t *testing.T) {
	srv, p := tenantVendor(t, "")
	if probs := checkContract(t.Context(), srv.Client(), p, nil); len(probs) != 0 {
		t.Fatal(probs)
	}
	srv, p = tenantVendor(t, "https://elsewhere.example.test/tenant")
	if !strings.Contains(strings.Join(checkContract(t.Context(), srv.Client(), p, nil), "\n"), "issuer") {
		t.Fatal("wrong issuer not reported")
	}
}

func TestContractRefusesResourceMetadataOnAnotherHost(t *testing.T) {
	var other int
	otherSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { other++ }))
	t.Cleanup(otherSrv.Close)
	srv, p := vendor(t, func(path string, w http.ResponseWriter) bool {
		if path == "/mcp" {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+otherSrv.URL+`/prm"`)
			w.WriteHeader(401)
			return true
		}
		return false
	})
	probs := strings.Join(checkContract(t.Context(), srv.Client(), p, nil), "\n")
	if !strings.Contains(probs, "another host") || other != 0 {
		t.Fatalf("other host got %d requests; problems: %s", other, probs)
	}
}

func TestContractRefusesRedirectToAnotherHost(t *testing.T) {
	for _, redirected := range []string{"/mcp", "/token"} {
		var other int
		otherSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { other++ }))
		t.Cleanup(otherSrv.Close)
		srv, p := vendor(t, func(path string, w http.ResponseWriter) bool {
			if path == redirected {
				http.Redirect(w, &http.Request{}, otherSrv.URL+"/x", http.StatusTemporaryRedirect)
				return true
			}
			return false
		})
		hc := srv.Client() // the test servers share one certificate, so this client trusts both
		hc.CheckRedirect = contractClient().CheckRedirect
		if len(checkContract(t.Context(), hc, p, nil)) == 0 || other != 0 {
			t.Fatalf("%s: redirect not refused (other host got %d requests)", redirected, other)
		}
	}
}
