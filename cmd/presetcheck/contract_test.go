package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
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
		`{"ok":false,"error":"invalid_client_id"}`: true, // Slack: HTTP 200, still an OAuth refusal
		`{"ok":true}`:          false, // no error indicator
		`{"access_token":"x"}`: false, // a token for an empty request is not an OAuth endpoint
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
			w.Write([]byte(`{"resource":"` + srv.URL + `/mcp","authorization_servers":["` + srv.URL + `/tenant"]}`))
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
}

// Webex publishes issuer "https://webexapis.com" under the authorization server URL
// "https://mcp.webexapis.com/mcp/webex-messaging" (RFC 8414 section 3.3 asks for equality). A preset holds no
// issuer, so the mismatch cannot be fixed there: it is printed, and the endpoints are still compared.
func TestContractIssuerMismatchIsNotAFailureButEndpointsAre(t *testing.T) {
	srv, p := tenantVendor(t, "https://elsewhere.example.test/tenant")
	if probs := checkContract(t.Context(), srv.Client(), p, nil); len(probs) != 0 {
		t.Fatal(probs)
	}
	p.TokenURL = srv.URL + "/oauth/token"
	if !strings.Contains(strings.Join(checkContract(t.Context(), srv.Client(), p, nil), "\n"), "token_endpoint") {
		t.Fatal("moved token endpoint not reported when the issuer also differs")
	}
}

// Linear and Canva publish the endpoints of their MCP server's own registration server; a preset for an app made
// in the vendor's developer portal uses the portal's endpoints on purpose (the portal's client ids are unknown to
// the MCP server's). Those are not compared, but the token URL must still answer like a token endpoint.
func TestContractAppEndpointsAreNotComparedWithMCPRegistrationServer(t *testing.T) {
	srv, p := vendor(t, func(path string, w http.ResponseWriter) bool {
		if path == "/portal/token" {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"invalid_client"}`))
			return true
		}
		return false
	})
	p.AuthorizationURL, p.TokenURL = srv.URL+"/portal/authorize", srv.URL+"/portal/token"
	if len(checkContract(t.Context(), srv.Client(), p, nil)) == 0 {
		t.Fatal("different endpoints accepted without being declared")
	}
	p.Contract.AppEndpoints = true
	if probs := checkContract(t.Context(), srv.Client(), p, nil); len(probs) != 0 {
		t.Fatal(probs)
	}
	p.TokenURL = srv.URL + "/gone" // still probed
	if !strings.Contains(strings.Join(checkContract(t.Context(), srv.Client(), p, nil), "\n"), "token URL") {
		t.Fatal("token URL not probed")
	}
}

// A preset with placeholders (ServiceNow) cannot be checked without a real tenant. A fixture host that does not
// exist is reported as skipped, not as a vendor failure; a vendor host that does not exist still fails.
func TestContractSkipsTenantHostThatDoesNotExist(t *testing.T) {
	_, p := vendor(t, nil)
	p.Server.URL = "https://{instance}.invalid/mcp"
	p.TokenURL = "https://{instance}.invalid/token"
	vals := map[string]string{"instance": "nobody"}
	if probs := checkContract(t.Context(), http.DefaultClient, presetWith(p, false), vals); len(probs) == 0 {
		t.Fatal("unreachable host not reported")
	}
	probs, skipped := checkContractOrSkip(t.Context(), http.DefaultClient, presetWith(p, true), vals)
	if len(probs) != 0 || skipped == "" {
		t.Fatalf("problems %v skipped %q", probs, skipped)
	}
	// without placeholders the host belongs to the vendor: the failure stays
	if probs, skipped := checkContractOrSkip(t.Context(), http.DefaultClient, presetWith(p, false), vals); len(probs) == 0 || skipped != "" {
		t.Fatalf("problems %v skipped %q", probs, skipped)
	}
}

func presetWith(p presets.Preset, placeholder bool) presets.Preset {
	if placeholder {
		p.Placeholders = []presets.Placeholder{{Name: "instance", Label: "x", Pattern: "^[a-z]+$", Example: "example"}}
	} else {
		p.Server.URL = strings.ReplaceAll(p.Server.URL, "{instance}", "nobody")
		p.TokenURL = strings.ReplaceAll(p.TokenURL, "{instance}", "nobody")
		p.Placeholders = nil
	}
	return p
}

// RFC 9728: the server may name its resource metadata on another host (Zoom). It counts only if the metadata's
// "resource" is the server URL (section 3.3); otherwise another host could speak for the server.
func TestContractResourceMetadataOnAnotherHost(t *testing.T) {
	for _, c := range []struct {
		name     string
		resource func(serverURL string) string
		want     string // "" = the contract passes
	}{
		{"names the server", func(s string) string { return s }, ""},
		{"names the server with a slash", func(s string) string { return s + "/" }, ""},
		{"names the server's origin", func(s string) string { u, _ := url.Parse(s); return u.Scheme + "://" + u.Host }, ""},
		{"names a path that is not a parent", func(s string) string { return s + "x" }, "not for the server"},
		{"names another resource", func(string) string { return "https://other.example/mcp" }, "not for the server"},
		{"names no resource", func(string) string { return "" }, "not for the server"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, p := vendor(t, nil) // the vendor's own metadata stays unused: the server points elsewhere
			meta := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`{"resource":"` + c.resource(p.Server.URL) + `","authorization_servers":["` + srv.URL + `"]}`))
			}))
			t.Cleanup(meta.Close)
			srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/mcp":
					w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+meta.URL+`/prm"`)
					w.WriteHeader(401)
				case "/.well-known/oauth-authorization-server":
					w.Write([]byte(`{"issuer":"` + srv.URL + `","authorization_endpoint":"` + srv.URL + `/authorize","token_endpoint":"` + srv.URL + `/token"}`))
				case "/token":
					w.WriteHeader(400)
					w.Write([]byte(`{"error":"invalid_request"}`))
				}
			})
			hc := srv.Client() // the test servers share one certificate
			probs := strings.Join(checkContract(t.Context(), hc, p, nil), "\n")
			if (c.want == "") != (probs == "") || !strings.Contains(probs, c.want) {
				t.Fatalf("want %q, problems: %s", c.want, probs)
			}
		})
	}
}

func TestContractRefusesResourceMetadataWithoutHTTPS(t *testing.T) {
	srv, p := vendor(t, func(path string, w http.ResponseWriter) bool {
		if path == "/mcp" {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="http://other.example/prm"`)
			w.WriteHeader(401)
			return true
		}
		return false
	})
	if probs := strings.Join(checkContract(t.Context(), srv.Client(), p, nil), "\n"); !strings.Contains(probs, "not an https URL") {
		t.Fatal(probs)
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

// The token endpoint is judged by its JSON OAuth error, at any status: GitHub answers an unknown client with
// 404 {"error":"Not Found"}, while a wrong URL answers with an HTML page.
func TestContractTokenAnswersByStatus(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		ok     bool
	}{
		{404, `{"error":"Not Found"}`, true},
		{404, `<!DOCTYPE html><html>Not Found</html>`, false},
		{400, `{"error":"invalid_client"}`, true},
		// Canva and Webex refuse with their own JSON shape, not RFC 6749's "error" member.
		{400, `{"code":"unauthorized_client","message":"Validating client failed for x"}`, true},
		{400, `{"message":"client_secret cannot be null or empty","errors":[{"description":"x"}]}`, true},
		{404, `{"message":"Not found"}`, false}, // a vendor-shaped error must be a client error about the request
		{200, `{"message":"hello"}`, false},
	} {
		srv, p := vendor(t, func(path string, w http.ResponseWriter) bool {
			if path == "/token" {
				w.WriteHeader(c.status)
				w.Write([]byte(c.body))
				return true
			}
			return false
		})
		got := !strings.Contains(strings.Join(checkContract(t.Context(), srv.Client(), p, nil), "\n"), "token URL")
		if got != c.ok {
			t.Fatalf("%d %s: ok=%v", c.status, c.body, got)
		}
	}
}

// The probe is a dummy authorization-code grant, not an empty body.
func TestContractTokenProbeSendsADummyGrant(t *testing.T) {
	var form url.Values
	srv, p := vendor(t, func(path string, w http.ResponseWriter) bool { return false })
	next := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			r.ParseForm()
			form = r.PostForm
		}
		next.ServeHTTP(w, r)
	})
	checkContract(t.Context(), srv.Client(), p, nil)
	if form.Get("grant_type") != "authorization_code" || form.Get("client_id") == "" || form.Get("code") == "" {
		t.Fatalf("probe form %v", form)
	}
}
