package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/craftsman-apps/ambit/internal/presets"
	"sigs.k8s.io/yaml"
)

var resourceMetadataRE = regexp.MustCompile(`resource_metadata="([^"]+)"`)

func checkContract(ctx context.Context, hc *http.Client, p presets.Preset, values map[string]string) []string {
	res, err := p.Resolve(values, "presetcheck", "https://ambit.example.test/connections/callback")
	if err != nil {
		return []string{err.Error()}
	}
	var probs []string
	want := p.Contract.Unauthenticated
	if len(want) == 0 {
		want = []int{http.StatusUnauthorized}
	}
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"presetcheck","version":"1"}}}`
	var a answer
	if res.Server.Transport == "sse" { // SSE: the handshake is a GET for the event stream
		a, err = do(ctx, hc, http.MethodGet, res.Server.URL, "text/event-stream", "")
	} else {
		a, err = do(ctx, hc, http.MethodPost, res.Server.URL, "application/json", initialize)
	}
	switch {
	case err != nil:
		probs = append(probs, "server: "+err.Error())
	case !slices.Contains(want, a.status):
		probs = append(probs, fmt.Sprintf("server did not ask for authentication (HTTP %d)", a.status))
	default:
		if m := resourceMetadataRE.FindStringSubmatch(a.header.Get("WWW-Authenticate")); m != nil {
			probs = append(probs, checkMetadata(ctx, hc, res.Server.URL, m[1], res.Connection.AuthorizationURL, res.Connection.TokenURL, res.Connection.RevocationURL, p.Contract.AppEndpoints)...)
		}
	}
	// A dummy grant, not an empty body: some vendors (GitHub) answer 404 when client_id is missing.
	a, err = do(ctx, hc, http.MethodPost, res.Connection.TokenURL, "application/x-www-form-urlencoded", tokenProbe)
	if err != nil {
		probs = append(probs, "token URL: "+err.Error())
	} else {
		var o struct {
			Error   string `json:"error"`
			OK      *bool  `json:"ok"`
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		// RFC 6749 "error", Slack's {"ok":false}, or (Canva, Webex) {"code"|"message"} with a client-error status.
		isOAuthError := json.Unmarshal(a.body, &o) == nil && (o.Error != "" || (o.OK != nil && !*o.OK) ||
			((o.Code != "" || o.Message != "") && (a.status == http.StatusBadRequest || a.status == http.StatusUnauthorized)))
		if !isOAuthError { // judged by the JSON error, at any status: a wrong URL answers with HTML
			probs = append(probs, fmt.Sprintf("token URL did not answer like an OAuth token endpoint (HTTP %d)", a.status))
		}
	}
	return probs
}

// checkContractOrSkip: a preset with placeholders is checked against a tenant from fixtures.yaml. If that
// tenant's host does not exist there is nothing to check, so the preset is skipped, not failed.
func checkContractOrSkip(ctx context.Context, hc *http.Client, p presets.Preset, values map[string]string) (probs []string, skipped string) {
	probs = checkContract(ctx, hc, p, values)
	if len(p.Placeholders) > 0 && len(probs) > 0 && slices.ContainsFunc(probs, func(s string) bool { return strings.Contains(s, "no such host") }) {
		return nil, "the fixture tenant in test/presets/fixtures.yaml does not exist; add a real test tenant"
	}
	return probs, ""
}

// checkMetadata compares the vendor's published OAuth metadata with the preset (stable fields only).
// tokenProbe is an authorization-code request no vendor can accept; a token endpoint answers it with an OAuth error.
const tokenProbe = "grant_type=authorization_code&client_id=ambit-contract-check&code=ambit-contract-check&redirect_uri=https%3A%2F%2F127.0.0.1%2Fambit-contract"

func checkMetadata(ctx context.Context, hc *http.Client, serverURL, prmURL, authURL, tokenURL, revURL string, appEndpoints bool) []string {
	// RFC 9728 lets the server name its resource metadata on any host (Zoom: zoom.us for mcp.zoom.us). The
	// protection is section 3.3: the metadata's "resource" must be the server URL, or its origin or a parent path (as
	// Airtable and Slack publish). https only.
	if u, err := url.Parse(prmURL); err != nil || u.Scheme != "https" || u.Host == "" {
		return []string{"resource metadata is not an https URL: " + prmURL}
	}
	var prm struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := getJSON(ctx, hc, prmURL, &prm); err != nil {
		return []string{fmt.Sprintf("resource metadata %s: %v", prmURL, err)}
	}
	if !resourceCovers(prm.Resource, serverURL) {
		return []string{fmt.Sprintf("resource metadata %s is for %q, not for the server %s", prmURL, prm.Resource, serverURL)}
	}
	if len(prm.AuthorizationServers) == 0 {
		return []string{fmt.Sprintf("resource metadata %s names no authorization server", prmURL)}
	}
	issuer := prm.AuthorizationServers[0]
	var as struct {
		Issuer     string `json:"issuer"`
		Authorize  string `json:"authorization_endpoint"`
		Token      string `json:"token_endpoint"`
		Revocation string `json:"revocation_endpoint"`
	}
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return []string{"authorization server is not https: " + issuer}
	}
	path := strings.TrimSuffix(u.Path, "/")
	tried := []string{
		u.Scheme + "://" + u.Host + "/.well-known/oauth-authorization-server" + path, // RFC 8414 path insertion
		u.Scheme + "://" + u.Host + "/.well-known/openid-configuration" + path,
		strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration",
	}
	found := false
	for _, m := range tried {
		if getJSON(ctx, hc, m, &as) == nil && as.Issuer != "" {
			found = true
			break
		}
	}
	if !found {
		return []string{"no authorization server metadata found for " + issuer}
	}
	var probs []string
	cmp := func(field, got, want string) {
		if got != want {
			probs = append(probs, fmt.Sprintf("%s: vendor says %q, preset has %q", field, got, want))
		}
	}
	if strings.TrimSuffix(as.Issuer, "/") != strings.TrimSuffix(issuer, "/") {
		// The preset holds no issuer and Ambit does not use one, so this cannot be fixed in the preset (Webex).
		fmt.Printf("note: %s publishes issuer %q (RFC 8414 section 3.3 asks for the same URL)\n", issuer, as.Issuer)
	}
	if appEndpoints {
		return probs
	}
	cmp("authorization_endpoint", as.Authorize, authURL)
	cmp("token_endpoint", as.Token, tokenURL)
	switch {
	case revURL != "" && as.Revocation != "":
		cmp("revocation_endpoint", as.Revocation, revURL)
	case revURL != "":
		// Many vendors do not publish it; not a failure, but printed so the maintainer re-checks the docs.
		fmt.Printf("note: %s publishes no revocation_endpoint; the preset's %s is from vendor docs\n", issuer, revURL)
	}
	return probs
}

type answer struct {
	status int
	header http.Header
	body   []byte
}

// do makes one bounded request with two retries on network errors and 5xx.
func do(ctx context.Context, hc *http.Client, method, u, ctype, body string) (answer, error) {
	if !strings.HasPrefix(u, "https://") {
		return answer{}, fmt.Errorf("%s is not https", u)
	}
	var last error
	for i := 0; i < 3; i++ {
		req, err := http.NewRequestWithContext(ctx, method, u, strings.NewReader(body))
		if err != nil {
			return answer{}, err
		}
		req.Header.Set("Content-Type", ctype)
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := hc.Do(req)
		if err == nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return answer{resp.StatusCode, resp.Header, b}, nil
			}
			last = fmt.Errorf("HTTP %d", resp.StatusCode)
		} else {
			last = err
			if dnsErr := (*net.DNSError)(nil); errors.As(err, &dnsErr) && dnsErr.IsNotFound {
				break // the name does not exist; retrying cannot help
			}
		}
		if i == 2 {
			break
		}
		select {
		case <-ctx.Done():
			return answer{}, ctx.Err()
		case <-time.After(time.Duration(i+1) * 2 * time.Second):
		}
	}
	return answer{}, last
}

func getJSON(ctx context.Context, hc *http.Client, u string, out any) error {
	a, err := do(ctx, hc, http.MethodGet, u, "application/json", "")
	if err != nil {
		return err
	}
	if a.status != http.StatusOK {
		return fmt.Errorf("HTTP %d", a.status)
	}
	return json.Unmarshal(a.body, out)
}

// resourceCovers: resource is serverURL itself or a parent of it (same origin, path prefix at a "/").
func resourceCovers(resource, serverURL string) bool {
	r, e1 := url.Parse(resource)
	s, e2 := url.Parse(serverURL)
	if e1 != nil || e2 != nil || r.Scheme != "https" || r.Scheme != s.Scheme || !strings.EqualFold(r.Host, s.Host) {
		return false
	}
	rp := strings.TrimSuffix(r.Path, "/")
	return rp == "" || s.Path == rp || strings.HasPrefix(s.Path, rp+"/")
}

// contractClient: bounded, https only, no redirects to another host.
func contractClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if r.URL.Scheme != "https" || r.URL.Host != via[0].URL.Host || len(via) > 5 {
			return fmt.Errorf("redirect to %s refused", r.URL)
		}
		return nil
	}}
}

// contract runs checkContract for every preset (or the ids given) and writes {id: [problems]} to jsonPath.
func contract(ctx context.Context, jsonPath string, ids []string) error {
	all, err := presets.All()
	if err != nil {
		return err
	}
	var fixtures map[string]map[string]string
	b, err := os.ReadFile("test/presets/fixtures.yaml")
	if err != nil {
		return err
	}
	if err := yaml.UnmarshalStrict(b, &fixtures); err != nil {
		return fmt.Errorf("test/presets/fixtures.yaml: %w", err)
	}
	for _, id := range ids {
		if !slices.ContainsFunc(all, func(p presets.Preset) bool { return p.ID == id }) {
			return fmt.Errorf("no preset %q", id)
		}
	}
	out := map[string][]string{}
	failed := false
	hc := contractClient()
	for _, p := range all {
		if len(ids) > 0 && !slices.Contains(ids, p.ID) {
			continue
		}
		probs, skipped := checkContractOrSkip(ctx, hc, p, fixtures[p.ID])
		out[p.ID] = probs
		if skipped != "" {
			fmt.Printf("SKIP %s: %s\n", p.ID, skipped)
		} else if len(probs) == 0 {
			fmt.Println("PASS", p.ID)
		} else {
			failed = true
			fmt.Printf("FAIL %s:\n  %s\n", p.ID, joinLines(probs))
		}
	}
	if jsonPath != "" {
		j, _ := json.MarshalIndent(out, "", "  ")
		if err := os.WriteFile(jsonPath, j, 0o644); err != nil {
			return err
		}
	}
	if failed {
		return fmt.Errorf("contract check failed")
	}
	return nil
}
