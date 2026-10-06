package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
			probs = append(probs, checkMetadata(ctx, hc, res.Server.URL, m[1], res.Connection.AuthorizationURL, res.Connection.TokenURL, res.Connection.RevocationURL)...)
		}
	}
	a, err = do(ctx, hc, http.MethodPost, res.Connection.TokenURL, "application/x-www-form-urlencoded", "")
	if err != nil {
		probs = append(probs, "token URL: "+err.Error())
	} else {
		var o struct {
			Error string `json:"error"`
			OK    *bool  `json:"ok"`
		}
		isOAuthError := json.Unmarshal(a.body, &o) == nil && (o.Error != "" || (o.OK != nil && !*o.OK))
		if !isOAuthError || a.status == http.StatusNotFound {
			probs = append(probs, fmt.Sprintf("token URL did not answer like an OAuth token endpoint (HTTP %d)", a.status))
		}
	}
	return probs
}

// checkMetadata compares the vendor's published OAuth metadata with the preset (stable fields only).
func checkMetadata(ctx context.Context, hc *http.Client, serverURL, prmURL, authURL, tokenURL, revURL string) []string {
	if !sameHost(serverURL, prmURL) {
		return []string{"resource metadata is on another host: " + prmURL}
	}
	var prm struct {
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := getJSON(ctx, hc, prmURL, &prm); err != nil {
		return []string{fmt.Sprintf("resource metadata %s: %v", prmURL, err)}
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
	cmp("issuer", strings.TrimSuffix(as.Issuer, "/"), strings.TrimSuffix(issuer, "/"))
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

func sameHost(a, b string) bool {
	ua, e1 := url.Parse(a)
	ub, e2 := url.Parse(b)
	return e1 == nil && e2 == nil && ua.Host == ub.Host && ub.Scheme == "https"
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
		probs := checkContract(ctx, hc, p, fixtures[p.ID])
		out[p.ID] = probs
		if len(probs) == 0 {
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
