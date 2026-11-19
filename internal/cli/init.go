package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/craftsman-apps/ambit/internal/cli/out"
)

// prompter asks one question and returns the answer; Enter takes def. app.testPrompter replaces it in tests.
type prompter interface {
	Ask(question, def string) (string, error)
}

// linePrompter asks on the terminal: the question on standard error (so it stays on the terminal when standard
// output is redirected), the answer one line from standard input.
type linePrompter struct {
	a *app
	w io.Writer
}

func (l linePrompter) Ask(question, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(l.w, "%s [%s]: ", question, def)
	} else {
		fmt.Fprintf(l.w, "%s: ", question)
	}
	s, err := l.a.stdin().ReadString('\n')
	if err != nil && (s == "" || !errors.Is(err, io.EOF)) {
		return "", errors.New("the input ended before every question was answered")
	}
	if s = strings.TrimSpace(s); s == "" {
		return def, nil
	}
	return s, nil
}

type initFlags struct {
	out, gitops string
	force       bool
}

func (a *app) initCmd() *cobra.Command {
	var f initFlags
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write values.yaml for ambit install by asking questions (never a secret value)",
		Long: `Asks for the addresses, the company sign-in, the gateway tokens, TLS, the audit log, budgets, tool
protection and GitOps, and writes a values file for ambit install. It never asks for a secret value: it asks for
the name of each Secret and prints the kubectl create secret command to run. It needs a terminal.`,
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			return a.init(cmd.Context(), f, cmd.Flags().Changed("out"))
		}),
	}
	fl := cmd.Flags()
	fl.StringVar(&f.out, "out", "values.yaml", "file to write; an existing file needs --force")
	fl.StringVar(&f.gitops, "gitops", "", "also write the Argo CD files of a configuration repository: argocd")
	fl.BoolVar(&f.force, "force", false, "overwrite --out and the Argo CD files")
	return cmd
}

// valueKeys is the mapping table: every values path that ambit init can write. TestInitKeysExistInTheChart fails
// when one of them is missing from charts/ambit/values.yaml, so a chart change cannot silently break init.
var valueKeys = []string{
	"console.url", "console.host",
	"llm.enabled", "llm.host", "llm.jwt.dex", "llm.jwt.issuer", "llm.jwt.audiences", "llm.jwt.jwksURL",
	"mcp.enabled", "mcp.host", "mcp.jwt.dex", "mcp.jwt.issuer", "mcp.jwt.audiences", "mcp.jwt.jwksURL",
	"sso.connectors", "dex.envVars",
	"gateway.replicas", "gateway.tls.enabled", "gateway.tls.certificateSecret",
	"evidence.enabled", "evidence.retention", "evidence.clickhouse.external.host", "evidence.clickhouse.external.passwordSecret",
	"limits.enabled", "limits.failureMode", "limits.valkey.external.address", "limits.valkey.external.passwordSecret",
	"protection.enabled",
	"gitops.enabled", "gitops.repository", "gitops.branch", "gitops.path", "gitops.github.appID", "gitops.github.privateKeySecret",
}

// initSecret is a Secret the values name; init prints the command that creates it, with placeholders only.
type initSecret struct{ name, kind, key, hint string }

// initResult is what the questions produced.
type initResult struct {
	vals     map[string]any
	settings int
	secrets  []initSecret
	warnings []string
	gitops   *gitopsAnswers
}

type gitopsAnswers struct {
	repository, branch, path string
	parent                   bool
}

func set(m map[string]any, path string, v any) {
	keys := strings.Split(path, ".")
	for _, k := range keys[:len(keys)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[k] = next
		}
		m = next
	}
	m[keys[len(keys)-1]] = v
}

// interview asks the questions of spec D2 3.1 in order. It never asks for a secret value.
func interview(p prompter, gitopsFiles bool) (*initResult, error) {
	r := &initResult{vals: map[string]any{}}
	put := func(path string, v any) { set(r.vals, path, v); r.settings++ }
	ask := p.Ask
	choose := func(q, def string, options ...string) (string, error) {
		prompt := q + " (" + strings.Join(options, ", ") + ")"
		for {
			a, err := ask(prompt, def)
			if err != nil {
				return "", err
			}
			for _, o := range options {
				if strings.EqualFold(a, o) {
					return o, nil
				}
			}
			prompt = "Please answer one of " + strings.Join(options, ", ") + ". " + q
		}
	}
	yes := func(q string, def bool) (bool, error) {
		d := "n"
		if def {
			d = "y"
		}
		a, err := choose(q, d, "y", "n")
		return a == "y", err
	}
	// secretName asks for the name of a Secret until the answer is a valid Kubernetes name, so nothing but a name
	// (never a value with '=' or spaces) reaches the printed kubectl command.
	secretName := func(q, def string) (string, error) {
		for {
			a, err := ask(q, def)
			if err != nil || secretNameRE.MatchString(a) {
				return a, err
			}
			q = "A Secret name has lower-case letters, digits, '-' and '.'. " + strings.TrimPrefix(q, "A Secret name has lower-case letters, digits, '-' and '.'. ")
		}
	}
	var err error
	must := func(s string, e error) string {
		if err == nil {
			err = e
		}
		return s
	}
	mustYes := func(b bool, e error) bool {
		if err == nil {
			err = e
		}
		return b
	}

	// 1. Console address.
	console := must(ask("Console address", "https://console.example.com"))
	if err != nil {
		return nil, err
	}
	u, perr := url.Parse(console)
	for perr != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		if console, err = ask("The console address must be a full URL, for example https://console.example.com. Console address", "https://console.example.com"); err != nil {
			return nil, err
		}
		u, perr = url.Parse(console)
	}
	console = strings.TrimSuffix(console, "/")
	put("console.url", console)
	put("console.host", u.Hostname())
	domain := u.Hostname()
	if i := strings.Index(domain, "."); i > 0 {
		domain = domain[i+1:]
	}

	// 2. Gateways.
	put("llm.enabled", true)
	put("llm.host", must(ask("LLM gateway host", "llm."+domain)))
	mcpOn := mustYes(yes("MCP gateway on?", true))
	put("mcp.enabled", mcpOn)
	if mcpOn {
		put("mcp.host", must(ask("MCP gateway host", "mcp."+domain)))
	}
	if err != nil {
		return nil, err
	}

	// 3. Company sign-in.
	sso, err := choose("Company sign-in", "later", "oidc", "github", "saml", "ldap", "later")
	if err != nil {
		return nil, err
	}
	if sso != "later" {
		secret := must(secretName("Name of the Secret that holds the sign-in secret (you create it; the value is never asked here)", "ambit-sso"))
		redirect := console + "/dex/callback"
		conn := map[string]any{"type": sso, "id": "company", "name": "Company sign-in"}
		var cfg map[string]any
		var envs []map[string]any
		env := func(name, key, hint string) string {
			envs = append(envs, map[string]any{"name": name, "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": secret, "key": key}}})
			r.secrets = append(r.secrets, initSecret{name: secret, kind: "generic", key: key, hint: hint})
			return "$" + name
		}
		switch sso {
		case "oidc":
			cfg = map[string]any{"issuer": must(ask("OIDC issuer URL", "https://login."+domain)), "clientID": must(ask("OIDC client ID", "ambit")),
				"redirectURI": redirect, "insecureEnableGroups": true}
			cfg["clientSecret"] = env("SSO_CLIENT_SECRET", "clientSecret", "<client secret>")
		case "github":
			cfg = map[string]any{"clientID": must(ask("GitHub OAuth app client ID", "")), "redirectURI": redirect,
				"orgs": []any{map[string]any{"name": must(ask("GitHub organization whose members may sign in", ""))}}, "loadAllGroups": false}
			cfg["clientSecret"] = env("SSO_CLIENT_SECRET", "clientSecret", "<client secret>")
		case "saml":
			cfg = map[string]any{"ssoURL": must(ask("SAML single sign-on URL of the identity provider", "")), "redirectURI": redirect,
				"entityIssuer": redirect, "usernameAttr": "name", "emailAttr": "email", "groupsAttr": "groups"}
			cfg["caData"] = env("SSO_SAML_CA", "caData", "<base64 of the identity provider's signing certificate>")
		case "ldap":
			cfg = map[string]any{"host": must(ask("LDAP host:port", "ldap."+domain+":636")), "bindDN": must(ask("Bind DN", "cn=ambit,dc=example,dc=com")),
				"userSearch": map[string]any{"baseDN": must(ask("Base DN of the users", "ou=people,dc=example,dc=com")), "filter": "(objectClass=person)",
					"username": "mail", "idAttr": "DN", "emailAttr": "mail", "nameAttr": "cn"},
				"groupSearch": map[string]any{"baseDN": must(ask("Base DN of the groups", "ou=groups,dc=example,dc=com")), "filter": "(objectClass=groupOfNames)",
					"userMatchers": []any{map[string]any{"userAttr": "DN", "groupAttr": "member"}}, "nameAttr": "cn"}}
			cfg["bindPW"] = env("SSO_BIND_PASSWORD", "bindPassword", "<bind password>")
		}
		if err != nil {
			return nil, err
		}
		conn["config"] = cfg
		put("sso.connectors", []any{conn})
		var envList []any
		for _, e := range envs {
			envList = append(envList, e)
		}
		put("dex.envVars", envList)
	}

	// 4. Gateway tokens.
	tokens, err := choose("Gateway tokens from: idp (your identity provider) or dex (Ambit's Dex, for tests)", "idp", "idp", "dex")
	if err != nil {
		return nil, err
	}
	gws := []string{"llm"}
	if mcpOn {
		gws = append(gws, "mcp")
	}
	if tokens == "dex" {
		for _, g := range gws {
			put(g+".jwt.dex", true)
		}
		r.warnings = append(r.warnings, "Gateway tokens from Ambit's Dex are meant for tests, not for a company")
	} else {
		issuer := must(ask("Token issuer URL", "https://login."+domain))
		aud := must(ask("Token audience", "ambit"))
		jwks := must(ask("JWKS URL of the issuer", strings.TrimSuffix(issuer, "/")+"/keys"))
		if err != nil {
			return nil, err
		}
		for _, g := range gws {
			put(g+".jwt.issuer", issuer)
			put(g+".jwt.audiences", []any{aud})
			put(g+".jwt.jwksURL", jwks)
		}
	}

	// 5. TLS.
	var tls string
	for {
		if tls, err = ask("Name of the TLS Secret (kubernetes.io/tls), or none", "ambit-tls"); err != nil {
			return nil, err
		}
		if strings.EqualFold(tls, "none") || secretNameRE.MatchString(tls) {
			break
		}
	}
	if strings.EqualFold(tls, "none") {
		put("gateway.tls.enabled", false)
		r.warnings = append(r.warnings, "No TLS: ambit check --production will fail (P3)")
	} else {
		put("gateway.tls.enabled", true)
		put("gateway.tls.certificateSecret", tls)
		r.secrets = append(r.secrets, initSecret{name: tls, kind: "tls"})
	}

	// 6. Audit log.
	if mustYes(yes("Audit log (every call recorded)?", true)) {
		put("evidence.enabled", true)
		put("evidence.retention", must(ask("How long to keep audit records", "8760h")))
		if mustYes(yes("Your own ClickHouse (not the one the chart installs)?", false)) {
			put("evidence.clickhouse.external.host", must(ask("ClickHouse host", "clickhouse."+domain)))
			s := must(secretName("Name of the Secret with the ClickHouse password", "ambit-clickhouse-password"))
			put("evidence.clickhouse.external.passwordSecret", s)
			r.secrets = append(r.secrets, initSecret{name: s, kind: "generic", key: "password", hint: "<password>"})
		}
	} else {
		put("evidence.enabled", false)
	}
	if err != nil {
		return nil, err
	}

	// 7. Budgets.
	if mustYes(yes("Budgets (daily request and spending limits)?", true)) {
		put("limits.enabled", true)
		put("limits.failureMode", must(choose("When the budget store does not answer", "FailOpen", "FailOpen", "FailClosed")))
		if mustYes(yes("Your own Valkey or Redis (keeps the counts on a restart)?", false)) {
			put("limits.valkey.external.address", must(ask("Valkey address host:port", "valkey."+domain+":6379")))
			s := must(secretName("Name of the Secret with the Valkey password", "ambit-valkey-password"))
			put("limits.valkey.external.passwordSecret", s)
			r.secrets = append(r.secrets, initSecret{name: s, kind: "generic", key: "password", hint: "<password>"})
		}
	} else {
		put("limits.enabled", false)
	}
	if err != nil {
		return nil, err
	}

	// 8. Tool protection.
	put("protection.enabled", mustYes(yes("Tool protection (masks and blocks in MCP tool calls)?", true)))

	// 9. GitOps.
	if mustYes(yes("GitOps mode (the console opens pull requests in your configuration repository)?", gitopsFiles)) {
		g := &gitopsAnswers{repository: must(ask("Configuration repository (owner/name)", "")), branch: must(ask("Branch Argo CD watches", "main")),
			path: must(ask("Directory of Ambit in the repository", "ambit"))}
		put("gitops.enabled", true)
		put("gitops.repository", g.repository)
		put("gitops.branch", g.branch)
		put("gitops.path", g.path)
		put("gitops.github.appID", must(ask("GitHub App ID", "")))
		s := must(secretName("Name of the Secret with the GitHub App's private key", "ambit-github-app"))
		put("gitops.github.privateKeySecret", s)
		r.secrets = append(r.secrets, initSecret{name: s, kind: "file", key: "private-key.pem", hint: "<the App's .pem file>"})
		if gitopsFiles {
			g.parent = mustYes(yes("One parent Argo CD Application for the four (app of apps)?", false))
		}
		r.gitops = g
	} else if gitopsFiles {
		g := &gitopsAnswers{repository: must(ask("Configuration repository for the Argo CD files (owner/name)", "")),
			branch: must(ask("Branch Argo CD watches", "main")), path: must(ask("Directory of Ambit in the repository", "ambit"))}
		g.parent = mustYes(yes("One parent Argo CD Application for the four (app of apps)?", false))
		r.gitops = g
	}
	if err != nil {
		return nil, err
	}

	// 10. Gateway replicas.
	for {
		s, err := ask("Gateway replicas", "2")
		if err != nil {
			return nil, err
		}
		if n, cerr := strconv.Atoi(s); cerr == nil && n >= 1 {
			put("gateway.replicas", n)
			break
		}
	}
	return r, nil
}

func (a *app) init(_ context.Context, f initFlags, outGiven bool) error {
	p := a.printer()
	if f.gitops != "" && f.gitops != "argocd" {
		return out.Usage("--gitops "+f.gitops, "only argocd is supported", "ambit init --gitops argocd")
	}
	gitopsFiles := f.gitops == "argocd"
	if gitopsFiles && outGiven {
		return out.Usage("--out and --gitops argocd", "with --gitops argocd the values file is <GitOps path>/values.yaml in the current directory, the repository root", "ambit init --gitops argocd")
	}
	if gitopsFiles && version == "dev" {
		return out.Usage("development build", "the Argo CD files name the chart version, and this ambit has none", "use a released ambit")
	}
	pr := a.testPrompter
	if pr == nil {
		if !a.stdinTTY() {
			return out.Usage("no terminal", "ambit init asks questions and needs a terminal", "run it in a terminal, or write values.yaml yourself (docs/install.md)")
		}
		pr = linePrompter{a: a, w: a.errw}
	}
	if !gitopsFiles && !f.force {
		if _, err := os.Stat(f.out); err == nil {
			return &out.Error{What: f.out + " exists", Why: "ambit init does not overwrite a file", Next: "ambit init --force, or --out <another file>"}
		}
	}
	r, err := interview(pr, gitopsFiles)
	if err != nil {
		return &out.Error{What: "ambit init stopped", Why: err.Error(), Next: "run ambit init again"}
	}
	ns := a.g.namespace
	header := fmt.Sprintf("# Values for ambit install, written by ambit init %s on %s.\n# Edit freely; ambit init does not read this file back. Every setting: the chart's values.yaml.\n",
		version, time.Now().UTC().Format("2006-01-02"))
	body, err := yaml.Marshal(r.vals)
	if err != nil {
		return err
	}
	files := map[string][]byte{}
	valuesPath := f.out
	if gitopsFiles {
		valuesPath = filepath.Join(r.gitops.path, "values.yaml")
		for name, b := range argoFiles(r.gitops, ns) {
			files[name] = b
		}
	}
	files[valuesPath] = append([]byte(header), body...)
	if !f.force {
		for name := range files {
			if _, err := os.Stat(name); err == nil {
				return &out.Error{What: name + " exists", Why: "ambit init does not overwrite a file", Next: "ambit init --gitops argocd --force (review the diff before you commit)"}
			}
		}
	}
	for _, name := range sortedKeys(files) {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(name, files[name], 0o644); err != nil {
			return err
		}
	}
	for _, w := range r.warnings {
		p.Line("%s %s", p.Mark("warn"), w)
	}
	p.Line("Wrote %s (%d settings).", valuesPath, r.settings)
	if gitopsFiles {
		printArgoNext(p, r.gitops)
	}
	if len(r.secrets) > 0 {
		p.Line("Create these Secrets before ambit install (values are never written to files by ambit init):")
		var fromFile []string
		for _, s := range dedupSecrets(r.secrets) {
			switch s.kind {
			case "tls":
				p.Line("  kubectl -n %s create secret tls %s --cert=<file> --key=<file>", ns, s.name)
				fromFile = append(fromFile, s.name+"/tls.crt=<file>", s.name+"/tls.key=<file>")
			case "file":
				p.Line("  kubectl -n %s create secret generic %s --from-file=%s=<file>", ns, s.name, s.key)
				fromFile = append(fromFile, s.name+"/"+s.key+"=<file>")
			default:
				p.Line("  kubectl -n %s create secret generic %s --from-literal=%s=%s", ns, s.name, s.key, s.hint)
				fromFile = append(fromFile, s.name+"/"+s.key+"=<file>")
			}
		}
		if !gitopsFiles {
			p.Line("Or let ambit install create them from files: ambit install -f %s --secret-file %s", valuesPath, strings.Join(fromFile, " --secret-file "))
		}
	}
	if !gitopsFiles {
		p.Line("Next: ambit install -f %s", valuesPath)
	}
	return nil
}

// dedupSecrets drops a repeated Secret key (two answers that name the same Secret and key).
func dedupSecrets(in []initSecret) []initSecret {
	var list []initSecret
	seen := map[string]bool{}
	for _, s := range in {
		if !seen[s.name+"/"+s.key] {
			seen[s.name+"/"+s.key] = true
			list = append(list, s)
		}
	}
	return list
}
