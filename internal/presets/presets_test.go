package presets

import (
	"os"
	"strings"
	"testing"
)

const sample = `
- id: salesforce
  version: 1
  displayName: Salesforce
  category: Sales
  server: { url: "https://api.salesforce.com/platform/mcp/v1/platform/sobject-all", transport: streamable-http }
  authorizationURL: https://{myDomain}.my.salesforce.com/services/oauth2/authorize
  tokenURL: https://{myDomain}.my.salesforce.com/services/oauth2/token
  revocationURL: https://{myDomain}.my.salesforce.com/services/oauth2/revoke
  resource: default
  scopes: [mcp_api, refresh_token]
  placeholders: [{ name: myDomain, label: "My Domain", pattern: "^[a-z0-9-]{1,80}$", example: acme }]
  setup: "1. Callback URL {callbackURL}"
  docs: https://developer.salesforce.com/docs
  live: { tool: soql_query, arguments: { query: "SELECT Id FROM Organization LIMIT 1" } }
  verified: true
`

func TestResolveSubstitutesAndBinds(t *testing.T) {
	ps, err := parse([]byte(sample))
	if err != nil || len(Validate(ps)) != 0 {
		t.Fatalf("err %v problems %v", err, Validate(ps))
	}
	r, err := ps[0].Resolve(map[string]string{"myDomain": "acme"}, "3MVG", "https://ambit.acme.test/connections/callback")
	if err != nil {
		t.Fatal(err)
	}
	if r.Connection.AuthorizationURL != "https://acme.my.salesforce.com/services/oauth2/authorize" ||
		r.Connection.ServerURL != r.Server.URL || r.Connection.Resource != r.Server.URL ||
		!strings.Contains(r.Setup, "https://ambit.acme.test/connections/callback") {
		t.Fatalf("resolved %+v", r)
	}
}

func TestPlaceholderCannotChangeHost(t *testing.T) {
	ps, _ := parse([]byte(sample))
	for _, v := range []string{"acme.evil.test/x", "acme@evil.test", "", "ACME", strings.Repeat("a", 81)} {
		if _, err := ps[0].Resolve(map[string]string{"myDomain": v}, "id", "https://cb"); err == nil {
			t.Fatalf("value %q accepted", v)
		}
	}
}

// A catalog pattern that is too loose (or unanchored) must not be the only defence.
func TestLoosePatternStillCannotChangeHost(t *testing.T) {
	for _, v := range []string{"acme.evil.test/x", "acme@evil.test", "acme/../x", "..", "a..b", "acme%2Fx", "acme#x", "acme?x=1", "acme:443", "acme\\x", "acme x", "acme\nx", ".evil", "evil.", "-x"} {
		ps, _ := parse([]byte(sample))
		ps[0].Placeholders[0].Pattern = ".*"
		if _, err := ps[0].Resolve(map[string]string{"myDomain": v}, "id", "https://cb"); err == nil {
			t.Fatalf("value %q accepted with a loose pattern", v)
		}
	}
}

func TestUndeclaredPlaceholderIsRefused(t *testing.T) {
	ps, _ := parse([]byte(sample))
	p := ps[0]
	p.TokenURL = "https://{other}.my.salesforce.com/token"
	if _, err := p.Resolve(map[string]string{"myDomain": "acme", "other": "evil.test/x"}, "id", "https://cb"); err == nil {
		t.Fatal("a placeholder the preset does not declare was filled from caller values")
	}
	if probs := strings.Join(Validate([]Preset{p}), "\n"); !strings.Contains(probs, "other") {
		t.Fatalf("Validate does not report the undeclared placeholder: %s", probs)
	}
}

func TestClientIDCannotChangeHost(t *testing.T) {
	ps, _ := parse([]byte(sample))
	p := ps[0]
	p.RevocationURL = "https://acme.my.salesforce.com/revoke/{clientID}"
	r, err := p.Resolve(map[string]string{"myDomain": "acme"}, "a/b@evil.test?x#y", "https://cb")
	if err != nil {
		t.Fatal(err)
	}
	if r.Connection.RevocationURL != "https://acme.my.salesforce.com/revoke/a%2Fb@evil.test%3Fx%23y" {
		t.Fatalf("client ID not escaped: %s", r.Connection.RevocationURL)
	}
	if _, err := p.Resolve(map[string]string{"myDomain": "acme"}, "..", "https://cb"); err == nil {
		t.Fatal("client ID .. accepted into a path")
	}
}

func TestValidateCatchesBadPresets(t *testing.T) {
	ps, _ := parse([]byte(sample))
	bad := ps[0]
	bad.AuthorizeParams = map[string]string{"redirect_uri": "x"}
	bad.Setup = "no callback here"
	bad.TokenURL = "http://plain.example.com/token"
	probs := strings.Join(Validate([]Preset{bad, ps[0]}), "\n")
	for _, want := range []string{"redirect_uri", "{callbackURL}", "duplicate id"} {
		if !strings.Contains(probs, want) {
			t.Fatalf("missing %q in %s", want, probs)
		}
	}
}

func TestValidateReportsPlainHTTPURL(t *testing.T) {
	ps, _ := parse([]byte(sample))
	ps[0].TokenURL = "http://plain.example.com/token"
	want := `the token URL "http://plain.example.com/token" is not a valid https URL`
	if probs := strings.Join(Validate(ps), "\n"); !strings.Contains(probs, want) {
		t.Fatalf("missing %q in %s", want, probs)
	}
}

func TestAuthorizeParamsFollowCRDRules(t *testing.T) {
	for _, k := range reserved {
		ps, _ := parse([]byte(sample))
		ps[0].AuthorizeParams = map[string]string{k: "x"}
		if _, err := ps[0].Resolve(map[string]string{"myDomain": "acme"}, "id", "https://cb"); err == nil {
			t.Fatalf("Resolve accepted reserved key %s", k)
		}
		if len(Validate(ps)) == 0 {
			t.Fatalf("Validate accepted reserved key %s", k)
		}
	}
	ps, _ := parse([]byte(sample))
	ps[0].AuthorizeParams = map[string]string{}
	for i := 0; i < 17; i++ {
		ps[0].AuthorizeParams[string(rune('a'+i))] = "x"
	}
	if _, err := ps[0].Resolve(map[string]string{"myDomain": "acme"}, "id", "https://cb"); err == nil {
		t.Fatal("17 authorize params accepted")
	}
	if len(Validate(ps)) == 0 {
		t.Fatal("Validate accepted 17 authorize params")
	}
	delete(ps[0].AuthorizeParams, "q")
	if _, err := ps[0].Resolve(map[string]string{"myDomain": "acme"}, "id", "https://cb"); err != nil {
		t.Fatalf("16 authorize params refused: %v", err)
	}
}

func TestTokenRequestFormatMustBeFormOrJSON(t *testing.T) {
	ps, _ := parse([]byte(sample))
	ps[0].TokenRequestFormat = "XML"
	if len(Validate(ps)) == 0 {
		t.Fatal("Validate accepted XML")
	}
	ps[0].TokenRequestFormat = "JSON"
	if len(Validate(ps)) != 0 {
		t.Fatal("JSON refused")
	}
}

func TestResolveRefusesBadPatternsResourceAndMissingURLs(t *testing.T) {
	ps, _ := parse([]byte(sample))
	p := ps[0]
	p.Placeholders[0].Pattern = "([" // invalid: must be an error, never a panic
	if _, err := p.Resolve(map[string]string{"myDomain": "acme"}, "id", "https://cb"); err == nil {
		t.Fatal("invalid pattern accepted")
	}
	ps, _ = parse([]byte(sample))
	p = ps[0]
	p.Resource = "http://{myDomain}.plain.test/mcp"
	if _, err := p.Resolve(map[string]string{"myDomain": "acme"}, "id", "https://cb"); err == nil {
		t.Fatal("plain-http resource accepted")
	}
	p.Resource = "https://{myDomain}.res.test/mcp"
	r, err := p.Resolve(map[string]string{"myDomain": "acme"}, "id", "https://cb")
	if err != nil || r.Connection.Resource != "https://acme.res.test/mcp" {
		t.Fatalf("explicit resource %q err %v", r.Connection.Resource, err)
	}
	p.TokenURL = ""
	if _, err := p.Resolve(map[string]string{"myDomain": "acme"}, "id", "https://cb"); err == nil {
		t.Fatal("missing token URL accepted")
	}
}

func TestTestingPresetsMayUsePlainHTTPToTestHostsOnly(t *testing.T) {
	ps, err := LoadTesting()
	if err != nil || len(ps) == 0 || !ps[0].Testing {
		t.Fatalf("testing presets %v %v", ps, err)
	}
	if _, err := ps[0].Resolve(nil, "id", "http://cb"); err != nil {
		t.Fatalf("test preset refused: %v", err)
	}
	p := ps[0]
	p.Server.URL = "http://whoami.example.com/mcp"
	if _, err := p.Resolve(nil, "id", "http://cb"); err == nil {
		t.Fatal("plain http to a public host accepted for a test preset")
	}
	for _, h := range []string{"evil.test", "localtest.me.evil.test", "x.svc.evil.test", "evil-localhost"} {
		p.Server.URL = "http://" + h + "/mcp"
		if _, err := p.Resolve(nil, "id", "http://cb"); err == nil {
			t.Fatalf("plain http to %s accepted", h)
		}
	}
	ps2, _ := parse([]byte(sample))
	ps2[0].Server.URL = "http://whoami.ambit-system.svc:8080/mcp"
	if _, err := ps2[0].Resolve(map[string]string{"myDomain": "acme"}, "id", "https://cb"); err == nil {
		t.Fatal("a catalog preset may never use plain http")
	}
}

func TestEmbeddedCatalogIsValid(t *testing.T) {
	ps, err := All()
	if err != nil {
		t.Fatal(err)
	}
	if p := Validate(ps); len(p) != 0 {
		t.Fatal(p)
	}
	for _, p := range ps {
		if p.Verified && p.Live.Tool == "" {
			t.Fatalf("%s is verified without a live check", p.ID)
		}
	}
	ts, err := LoadTesting()
	if err != nil || len(Validate(ts)) != 0 {
		t.Fatalf("testing catalog: %v %v", err, Validate(ts))
	}
}

func TestHashIgnoresVerified(t *testing.T) {
	ps, _ := parse([]byte(sample))
	a := ps[0]
	b := a
	b.Verified = !a.Verified
	b.VerifiedOn = "2026-11-01"
	if Hash(a) != Hash(b) {
		t.Fatal("verified changes the hash")
	}
	b.Scopes = []string{"mcp_api"}
	if Hash(a) == Hash(b) {
		t.Fatal("scopes do not change the hash")
	}
}

func TestResolveRefusesURLsTheCRDRefuses(t *testing.T) {
	for _, u := range []string{"HTTPS://{myDomain}.x.test/a", "https://{myDomain}.x.test/a?", "https://{myDomain}.x.test/a#", "Https://{myDomain}.x.test/a"} {
		ps, _ := parse([]byte(sample))
		ps[0].TokenURL = u
		if _, err := ps[0].Resolve(map[string]string{"myDomain": "acme"}, "id", "https://cb"); err == nil {
			t.Fatalf("%q accepted", u)
		}
	}
}

func TestValidateCatalogAuthorSafety(t *testing.T) {
	cases := map[string]func(p *Preset){
		"pattern must start with ^": func(p *Preset) { p.Placeholders[0].Pattern = "[a-z0-9-]{1,80}" },
		"end with $":                func(p *Preset) { p.Placeholders[0].Pattern = "^[a-z0-9-]{1,80}" },
		"displayName is required":   func(p *Preset) { p.DisplayName = "" },
		"duplicate placeholder myDomain": func(p *Preset) {
			p.Placeholders = append(p.Placeholders, p.Placeholders[0])
		},
		"verified without a live check": func(p *Preset) { p.Live.Tool = ""; p.Placeholders[0].Pattern = "([" },
	}
	for want, mutate := range cases {
		ps, _ := parse([]byte(sample))
		mutate(&ps[0])
		if probs := strings.Join(Validate(ps), "\n"); !strings.Contains(probs, want) {
			t.Fatalf("missing %q in %s", want, probs)
		}
	}
}

// docMissing lists the presets that have no row in the vendor table of the document.
func docMissing(doc string, ps []Preset) []string {
	var m []string
	for _, p := range ps {
		if !strings.Contains(doc, "| "+p.ID+" |") {
			m = append(m, p.ID)
		}
	}
	return m
}

func TestDocsListEveryPreset(t *testing.T) {
	doc, err := os.ReadFile("../../docs/presets.md") // a missing file fails the test
	if err != nil {
		t.Fatal(err)
	}
	ps, err := All()
	if err != nil {
		t.Fatal(err)
	}
	if m := docMissing(string(doc), ps); len(m) > 0 {
		t.Fatalf("docs/presets.md has no row for %v", m)
	}
}

func TestDocMissingFindsAnAbsentID(t *testing.T) {
	doc := "| id | Vendor |\n|---|---|\n| github | GitHub |\n"
	got := docMissing(doc, []Preset{{ID: "github"}, {ID: "linear"}})
	if len(got) != 1 || got[0] != "linear" {
		t.Fatalf("missing = %v, want [linear]", got)
	}
	if len(docMissing(doc, []Preset{{ID: "github"}})) != 0 {
		t.Fatal("a listed id was reported missing")
	}
}

// The Google presets share one OAuth client: a second one must still enable its own MCP API (step 3) and
// add its scopes (step 4), then reuse the client instead of creating another.
func TestGoogleSetupReusesTheClientAfterEnablingTheProduct(t *testing.T) {
	ps, err := All()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range ps {
		if p.Category != "Google Workspace" {
			continue
		}
		n++
		if strings.Contains(p.Setup, "go to step 5") || !strings.Contains(p.Setup, "do steps 3 and 4") || !strings.Contains(p.Setup, "skip steps 5 and 6") {
			t.Errorf("%s: a second Google preset must do steps 3 and 4, then reuse the client:\n%s", p.ID, p.Setup)
		}
	}
	if n != 5 {
		t.Fatalf("%d Google Workspace presets, want 5", n)
	}
}

// Presets whose scopes allow writing say so in plain words, so the admin knows before adding them.
func TestWriteScopesAreStatedInSetup(t *testing.T) {
	ps, err := All()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"dropbox": true, "airtable": true, "box": true, "webex": true, "docusign": true, "github": true}
	for _, p := range ps {
		if want[p.ID] {
			delete(want, p.ID)
			if !strings.Contains(p.Setup, "These scopes let the agent change data in") {
				t.Errorf("%s: setup does not say that its scopes allow changing data", p.ID)
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("presets not found: %v", want)
	}
}
