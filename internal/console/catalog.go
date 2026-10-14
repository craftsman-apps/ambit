package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/controller"
	"github.com/craftsman-apps/ambit/internal/presets"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Catalog serves models and access rules. In Direct mode admins edit rules here;
// rules managed in Git (Argo CD tracking annotation) are read-only.
type Catalog struct {
	Client    client.Client
	Namespace string
	// GitOps is set in GitOps mode: changes become pull requests instead of cluster writes.
	GitOps *GitOps
	// ArgoApplication (namespace/name) is the Argo CD Application that syncs the repository; the change
	// timeline reads its sync status. Empty skips that step.
	ArgoApplication string
	containsCache   sync.Map // "commit...revision" pairs known to be contained
	// PublicURL and Connectors let the Connect SSO page build the callback URL and refuse IDs in use.
	PublicURL  string
	Connectors []Connector
	HTTP       *http.Client // for reaching identity providers; nil uses a 10-second client
	// Keys serves personal API keys; nil leaves the key routes out. LLMURL and MCPURL are the gateway URLs shown in
	// the setup text for each tool, which the web UI builds from them. LastUsed overrides the audit lookup of each key's last use (tests).
	Keys     *KeyService
	LLMURL   string
	MCPURL   string
	LastUsed func(ctx context.Context, ids []string, from time.Time) (map[string]time.Time, error)
	// Conns serves "My connections" and the connect flow; nil when no grants namespace is configured.
	Conns *Connections
	// SCIM receives users from the identity provider and offboards deactivated ones; nil without Conns.
	SCIM *SCIM
	// Audit searches the gateway's access logs; nil when evidence is off.
	Audit *Audit
	// Budgets shows today's use of each Budget; nil when limits are off.
	Budgets *Budgets
	// Dependencies reports what Ambit relies on and whether it answers; nil hides the page.
	Dependencies *Dependencies
	// Prices suggests model prices (LiteLLM's list); nil offers no suggestions.
	Prices *PriceSource
	// Tools reads per-user servers' tools with one consenting admin's grant; nil without Conns.
	Tools *ToolReader
	// Presets is the app preset catalog (presets.All, plus the test presets when enabled).
	Presets []presets.Preset
}

// Config tells the console UI how changes are applied.
type Config struct {
	Mode       string `json:"mode"` // "direct" or "gitops"
	Repository string `json:"repository,omitempty"`
	Branch     string `json:"branch,omitempty"`
}

// Proposed is the answer to a change in GitOps mode (HTTP 202): nothing changed yet.
type Proposed struct {
	PullRequest PullRequest `json:"pullRequest"`
}

type ModelView struct {
	Name          string   `json:"name"`
	DisplayName   string   `json:"displayName"`
	Model         string   `json:"model"`
	Provider      string   `json:"provider"`
	AllowedGroups []string `json:"allowedGroups"`
	AllSignedIn   bool     `json:"allSignedIn"`
	Ready         string   `json:"ready"`
	Message       string   `json:"message"`
	// Price in dollars per million tokens; nil when the model has none.
	Price *v1.ModelPrice `json:"price"`
}

type RuleView struct {
	Name        string            `json:"name"`
	Groups      []string          `json:"groups"`
	AllSignedIn bool              `json:"allSignedIn"`
	Models      []string          `json:"models"`
	Servers     []v1.ServerAccess `json:"servers"`
	Ready       string            `json:"ready"`
	Reason      string            `json:"reason"`
	Message     string            `json:"message"`
	GitManaged  bool              `json:"gitManaged"`
	CreatedBy   string            `json:"createdBy"`
}

// Effective is the policy the gateway enforces for one model, after merging every rule.
type Effective struct {
	Model     string `json:"model"`
	ModelName string `json:"modelName"`
	CEL       string `json:"cel"`
}

// EffectiveServer is the part of the MCP policy the gateway enforces for one server.
type EffectiveServer struct {
	Server string `json:"server"`
	CEL    string `json:"cel"`
}

type RuleDetail struct {
	RuleView
	Effective        []Effective       `json:"effective"`
	EffectiveServers []EffectiveServer `json:"effectiveServers"`
}

type RuleInput struct {
	Name        string   `json:"name"`
	Groups      []string `json:"groups"`
	AllSignedIn bool     `json:"allSignedIn"`
	Models      []string `json:"models"`
	// Servers replaces the rule's servers; left out (nil) keeps them, for clients that edit models only.
	Servers []v1.ServerAccess `json:"servers"`
}

const (
	createdByKey = "ambitproject.dev/created-by"
	updatedByKey = "ambitproject.dev/updated-by"
)

var (
	nameRE  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	groupRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@/-]*$`) // same pattern as the CRD
	toolRE  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)        // same pattern as the CRD
)

func (c *Catalog) register(mux *http.ServeMux, auth *Auth) {
	read := func(h func(http.ResponseWriter, *http.Request, Session)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			s, ok := auth.Current(r)
			if !ok {
				writeErr(w, http.StatusUnauthorized, "not signed in")
				return
			}
			h(w, r, s)
		}
	}
	// allow wraps a change: the caller must hold one of roles (any signed-in user when none are
	// given) and send JSON.
	allow := func(denied string, h func(http.ResponseWriter, *http.Request, Session), roles ...string) http.HandlerFunc {
		return read(func(w http.ResponseWriter, r *http.Request, s Session) {
			if len(roles) > 0 && !slices.Contains(roles, s.Role) {
				writeErr(w, http.StatusForbidden, denied)
				return
			}
			if r.Method != http.MethodDelete {
				// A cross-site form cannot send application/json without a CORS preflight.
				if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
					writeErr(w, http.StatusUnsupportedMediaType, "send application/json")
					return
				}
			}
			h(w, r, s)
		})
	}
	c.registerKeys(mux, read, allow)
	write := func(h func(http.ResponseWriter, *http.Request, Session)) http.HandlerFunc {
		return allow("only admins can change access rules", h, RoleAdmin)
	}
	decide := func(h func(http.ResponseWriter, *http.Request, Session)) http.HandlerFunc {
		return allow("only approvers and admins can decide on server requests", h, RoleApprover, RoleAdmin)
	}
	mux.HandleFunc("GET /api/models", read(c.listModels))
	mux.HandleFunc("GET /api/access-rules", read(c.listRules))
	mux.HandleFunc("GET /api/access-rules/{name}", read(c.getRule))
	mux.HandleFunc("POST /api/access-rules", write(c.createRule))
	mux.HandleFunc("PUT /api/access-rules/{name}", write(c.updateRule))
	mux.HandleFunc("DELETE /api/access-rules/{name}", write(c.deleteRule))
	budget := func(h func(http.ResponseWriter, *http.Request, Session)) http.HandlerFunc {
		return allow("only admins can change budgets", h, RoleAdmin)
	}
	mux.HandleFunc("POST /api/budgets", budget(c.createBudget))
	mux.HandleFunc("PUT /api/budgets/{name}", budget(c.updateBudget))
	mux.HandleFunc("DELETE /api/budgets/{name}", budget(c.deleteBudget))
	mux.HandleFunc("GET /api/servers", read(c.listServers))
	mux.HandleFunc("POST /api/servers/{name}/approve-tools", write(c.approveTools))
	mux.HandleFunc("GET /api/server-requests", read(c.listRequests))
	mux.HandleFunc("POST /api/server-requests", allow("", c.createRequest))
	mux.HandleFunc("POST /api/server-requests/{name}/approve", decide(c.approveRequest))
	mux.HandleFunc("POST /api/server-requests/{name}/deny", decide(c.denyRequest))
	mux.HandleFunc("GET /api/me/access", read(c.myAccess))
	mux.HandleFunc("GET /api/config", read(c.config))
	mux.HandleFunc("GET /api/changes", read(c.changes))
	mux.HandleFunc("GET /api/changes/{number}", read(c.change))
	mux.HandleFunc("POST /api/sso/check", allow("only admins can connect sign-in providers", c.checkSSO, RoleAdmin))
	mux.HandleFunc("POST /api/sso/connectors", allow("only admins can connect sign-in providers", c.connectSSO, RoleAdmin))
	if c.Conns != nil {
		c.Conns.register(mux, auth)
		apps := func(h func(http.ResponseWriter, *http.Request, Session)) http.HandlerFunc {
			return allow("only admins can manage apps", h, RoleAdmin)
		}
		adminRead := func(h func(http.ResponseWriter, *http.Request, Session)) http.HandlerFunc {
			return read(func(w http.ResponseWriter, r *http.Request, s Session) {
				if s.Role != RoleAdmin {
					writeErr(w, http.StatusForbidden, "only admins can manage apps")
					return
				}
				h(w, r, s)
			})
		}
		mux.HandleFunc("GET /api/apps", adminRead(c.listApps))
		mux.HandleFunc("GET /api/presets", adminRead(c.listPresets))
		mux.HandleFunc("GET /api/apps/unfinished", adminRead(c.unfinished))
		mux.HandleFunc("GET /api/servers/{name}/access", adminRead(c.serverAccess))
		mux.HandleFunc("POST /api/apps/from-preset", apps(c.addFromPreset))
		mux.HandleFunc("POST /api/apps/unfinished/{op}/finish", apps(c.finishUnfinished))
		mux.HandleFunc("DELETE /api/apps/unfinished/{op}", apps(c.removeUnfinished))
		mux.HandleFunc("PUT /api/apps/{name}/bind", apps(c.bindApp))
		mux.HandleFunc("POST /api/apps", apps(c.createApp))
		mux.HandleFunc("DELETE /api/apps/{name}", apps(c.deleteApp))
	}
	admin := func(h func(http.ResponseWriter, *http.Request, Session)) http.HandlerFunc {
		return read(func(w http.ResponseWriter, r *http.Request, s Session) {
			if s.Role != RoleAdmin {
				writeErr(w, http.StatusForbidden, "only admins can see this")
				return
			}
			h(w, r, s)
		})
	}
	if c.Tools != nil {
		mux.HandleFunc("PUT /api/servers/{name}/tool-reader", allow("only admins can choose a tool reader", func(w http.ResponseWriter, r *http.Request, s Session) {
			if err := c.Tools.SetReader(r.Context(), s.Email, r.PathValue("name")); err != nil {
				writeConsentErr(w, err)
				return
			}
			slog.Info("tool reader set", "server", r.PathValue("name"), "reader", s.Email)
			w.WriteHeader(http.StatusNoContent)
		}, RoleAdmin))
		mux.HandleFunc("DELETE /api/servers/{name}/tool-reader", allow("only admins can clear a tool reader", func(w http.ResponseWriter, r *http.Request, s Session) {
			if err := c.Tools.ClearReader(r.Context(), r.PathValue("name")); err != nil {
				writeConsentErr(w, err)
				return
			}
			slog.Info("tool reader cleared", "server", r.PathValue("name"), "by", s.Email)
			w.WriteHeader(http.StatusNoContent)
		}, RoleAdmin))
	}
	if c.Audit != nil {
		c.Audit.register(mux, admin)
	}
	if c.Budgets != nil {
		c.Budgets.register(mux, admin)
	}
	if c.Dependencies != nil {
		c.Dependencies.register(mux, admin)
	}
	mux.HandleFunc("GET /api/models/{name}/price", admin(c.modelPrice))
	mux.HandleFunc("PUT /api/models/{name}/price", allow("only admins can change prices", c.setModelPrice, RoleAdmin))
	mux.HandleFunc("GET /api/prices/licence", read(func(w http.ResponseWriter, r *http.Request, _ Session) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(LiteLLMLicence))
	}))
	if c.SCIM != nil {
		c.SCIM.register(mux)
		mux.HandleFunc("GET /api/offboarding", admin(func(w http.ResponseWriter, r *http.Request, _ Session) {
			recs, err := c.SCIM.Records(r.Context())
			if err != nil {
				serverErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"scimURL": strings.TrimSuffix(c.PublicURL, "/") + "/scim/v2", "records": recs})
		}))
		mux.HandleFunc("GET /api/offboarding/token", admin(func(w http.ResponseWriter, _ *http.Request, s Session) {
			slog.Info("SCIM token shown", "to", s.Email)
			writeJSON(w, http.StatusOK, map[string]string{"token": c.SCIM.Token})
		}))
	}
}

func (c *Catalog) config(w http.ResponseWriter, _ *http.Request, _ Session) {
	if c.GitOps == nil {
		writeJSON(w, http.StatusOK, Config{Mode: "direct"})
		return
	}
	writeJSON(w, http.StatusOK, Config{Mode: "gitops", Repository: c.GitOps.Repository, Branch: c.GitOps.Branch})
}

// changes lists the console's open pull requests, so admins see what is waiting for review.
func (c *Catalog) changes(w http.ResponseWriter, r *http.Request, _ Session) {
	if c.GitOps == nil {
		writeJSON(w, http.StatusOK, []PullRequest{})
		return
	}
	prs, err := c.GitOps.Repo.OpenPullRequests(r.Context())
	if err != nil {
		slog.Error("list pull requests", "err", err)
		writeErr(w, http.StatusBadGateway, "could not read pull requests from "+c.GitOps.Repository+": "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, prs)
}

// propose opens a pull request for one object and answers 202; manifest nil deletes it.
func (c *Catalog) propose(w http.ResponseWriter, r *http.Request, s Session, verb, kind, plural, name string, spec any, labels, annotations map[string]string) {
	var manifest []byte
	if spec != nil {
		var err error
		if manifest, err = Manifest(kind, name, labels, annotations, spec); err != nil {
			serverErr(w, err)
			return
		}
	}
	pr, err := c.GitOps.Propose(r.Context(), verb, kind, plural, name, s.Email, manifest)
	var invalid *InvalidChange
	if errors.As(err, &invalid) {
		writeErr(w, http.StatusBadRequest, invalid.Error())
		return
	}
	if err != nil {
		slog.Error("open pull request", "kind", kind, "name", name, "err", err)
		writeErr(w, http.StatusBadGateway, "could not open a pull request in "+c.GitOps.Repository+": "+err.Error())
		return
	}
	slog.Info("pull request opened", "kind", kind, "name", name, "verb", verb, "by", s.Email, "url", pr.URL)
	writeJSON(w, http.StatusAccepted, Proposed{PullRequest: pr})
}

func (c *Catalog) load(r *http.Request) ([]v1.Model, []v1.AccessRule, error) {
	var ms v1.ModelList
	var rs v1.AccessRuleList
	if err := c.Client.List(r.Context(), &ms, client.InNamespace(c.Namespace)); err != nil {
		return nil, nil, err
	}
	if err := c.Client.List(r.Context(), &rs, client.InNamespace(c.Namespace)); err != nil {
		return nil, nil, err
	}
	sort.Slice(ms.Items, func(i, j int) bool { return ms.Items[i].Name < ms.Items[j].Name })
	sort.Slice(rs.Items, func(i, j int) bool { return rs.Items[i].Name < rs.Items[j].Name })
	return ms.Items, rs.Items, nil
}

func (c *Catalog) listModels(w http.ResponseWriter, r *http.Request, _ Session) {
	ms, _, err := c.load(r)
	if err != nil {
		serverErr(w, err)
		return
	}
	out := []ModelView{}
	for _, m := range ms {
		cond := meta.FindStatusCondition(m.Status.Conditions, "Ready")
		v := ModelView{Name: m.Name, DisplayName: m.Spec.DisplayName, Model: m.Spec.Model, Provider: m.Spec.Provider,
			AllowedGroups: nonNil(m.Status.AllowedGroups), AllSignedIn: m.Status.AllowAllSignedIn, Ready: "Unknown", Price: m.Spec.Price}
		if cond != nil {
			v.Ready, v.Message = string(cond.Status), cond.Message
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func ruleView(ar v1.AccessRule) RuleView {
	v := RuleView{Name: ar.Name, Groups: nonNil(ar.Spec.Groups), AllSignedIn: ar.Spec.AllSignedIn, Models: nonNil(ar.Spec.Models),
		Servers: ar.Spec.Servers,
		Ready:   "Unknown", GitManaged: gitManaged(ar.Annotations), CreatedBy: ar.Annotations[createdByKey]}
	if cond := meta.FindStatusCondition(ar.Status.Conditions, "Ready"); cond != nil {
		v.Ready, v.Reason, v.Message = string(cond.Status), cond.Reason, cond.Message
	}
	if v.Servers == nil {
		v.Servers = []v1.ServerAccess{}
	}
	return v
}

func (c *Catalog) listRules(w http.ResponseWriter, r *http.Request, _ Session) {
	_, rs, err := c.load(r)
	if err != nil {
		serverErr(w, err)
		return
	}
	out := []RuleView{}
	for _, ar := range rs {
		out = append(out, ruleView(ar))
	}
	writeJSON(w, http.StatusOK, out)
}

func (c *Catalog) getRule(w http.ResponseWriter, r *http.Request, _ Session) {
	ms, rs, err := c.load(r)
	if err != nil {
		serverErr(w, err)
		return
	}
	i := slices.IndexFunc(rs, func(ar v1.AccessRule) bool { return ar.Name == r.PathValue("name") })
	if i < 0 {
		writeErr(w, http.StatusNotFound, "no access rule named "+r.PathValue("name"))
		return
	}
	// The same Compile the controller runs, so the console shows exactly what the gateway enforces.
	res := controller.Compile(ms, nil, rs, controller.RouteRef{Name: "ambit-llm"})
	d := RuleDetail{RuleView: ruleView(rs[i]), Effective: []Effective{}, EffectiveServers: []EffectiveServer{}}
	if len(rs[i].Spec.Servers) > 0 {
		servers, err := c.servers(r)
		if err != nil {
			serverErr(w, err)
			return
		}
		for _, sa := range rs[i].Spec.Servers {
			d.EffectiveServers = append(d.EffectiveServers, EffectiveServer{Server: sa.Name, CEL: controller.ServerCEL(servers, rs, sa.Name)})
		}
	}
	for _, name := range rs[i].Spec.Models {
		mi := slices.IndexFunc(ms, func(m v1.Model) bool { return m.Name == name })
		if mi < 0 {
			continue
		}
		d.Effective = append(d.Effective, Effective{Model: name, ModelName: ms[mi].Spec.Model, CEL: controller.CEL(res.Access[name])})
	}
	writeJSON(w, http.StatusOK, d)
}

// validate checks a rule from the form. hasServers is true when the rule will grant MCP tools.
func (c *Catalog) validate(r *http.Request, in RuleInput, hasServers bool) error {
	switch {
	case (len(in.Groups) > 0) == in.AllSignedIn:
		return errors.New("choose either groups or everyone signed in, not both")
	case len(in.Models) == 0 && !hasServers:
		return errors.New("choose at least one model or server")
	}
	if len(in.Servers) > 0 {
		servers, err := c.servers(r)
		if err != nil {
			return err
		}
		for _, sa := range in.Servers {
			if !slices.ContainsFunc(servers, func(x v1.Server) bool { return x.Name == sa.Name }) {
				return fmt.Errorf("no server named %q", sa.Name)
			}
			if sa.Tools != nil && len(sa.Tools) == 0 {
				return fmt.Errorf("server %q: choose at least one tool, or all tools", sa.Name)
			}
			for _, t := range sa.Tools {
				if !toolRE.MatchString(t) {
					return fmt.Errorf("server %q: %q is not a valid tool name", sa.Name, t)
				}
			}
		}
	}
	for _, g := range in.Groups {
		if !groupRE.MatchString(g) {
			return fmt.Errorf("group %q may only contain letters, digits and . _ @ / -", g)
		}
	}
	ms, _, err := c.load(r)
	if err != nil {
		return err
	}
	for _, m := range in.Models {
		if !slices.ContainsFunc(ms, func(x v1.Model) bool { return x.Name == m }) {
			return fmt.Errorf("no model named %q", m)
		}
	}
	return nil
}

func decode(w http.ResponseWriter, r *http.Request) (RuleInput, bool) {
	var in RuleInput
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return in, false
	}
	return in, true
}

func (c *Catalog) createRule(w http.ResponseWriter, r *http.Request, s Session) {
	in, ok := decode(w, r)
	if !ok {
		return
	}
	if !nameRE.MatchString(in.Name) {
		writeErr(w, http.StatusBadRequest, "name must be lowercase letters, digits and dashes, starting and ending with a letter or digit")
		return
	}
	if err := c.validate(r, in, len(in.Servers) > 0); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ar := &v1.AccessRule{
		ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: c.Namespace, Annotations: map[string]string{createdByKey: s.Email}},
		Spec:       v1.AccessRuleSpec{Groups: in.Groups, AllSignedIn: in.AllSignedIn, Models: in.Models, Servers: in.Servers},
	}
	if c.GitOps != nil {
		var existing v1.AccessRule
		if err := c.Client.Get(r.Context(), client.ObjectKeyFromObject(ar), &existing); err == nil {
			writeErr(w, http.StatusConflict, "an access rule named "+in.Name+" already exists")
			return
		}
		c.propose(w, r, s, "create", "AccessRule", "accessrules", in.Name, ar.Spec, ar.Labels, ar.Annotations)
		return
	}
	if err := c.Client.Create(r.Context(), ar); err != nil {
		if apierrors.IsAlreadyExists(err) {
			writeErr(w, http.StatusConflict, "an access rule named "+in.Name+" already exists")
			return
		}
		apiErr(w, err)
		return
	}
	slog.Info("access rule created", "rule", in.Name, "by", s.Email, "groups", in.Groups, "allSignedIn", in.AllSignedIn, "models", in.Models, "servers", in.Servers)
	writeJSON(w, http.StatusCreated, ruleView(*ar))
}

func (c *Catalog) editable(w http.ResponseWriter, r *http.Request) (*v1.AccessRule, bool) {
	var ar v1.AccessRule
	if err := c.Client.Get(r.Context(), client.ObjectKey{Namespace: c.Namespace, Name: r.PathValue("name")}, &ar); err != nil {
		if apierrors.IsNotFound(err) {
			writeErr(w, http.StatusNotFound, "no access rule named "+r.PathValue("name"))
		} else {
			serverErr(w, err)
		}
		return nil, false
	}
	// In GitOps mode every change is a pull request, so Git-managed rules are editable here too.
	if c.GitOps == nil && gitManaged(ar.Annotations) {
		writeErr(w, http.StatusConflict, "this rule is managed in Git; change it with a pull request")
		return nil, false
	}
	return &ar, true
}

func (c *Catalog) updateRule(w http.ResponseWriter, r *http.Request, s Session) {
	in, ok := decode(w, r)
	if !ok {
		return
	}
	ar, ok := c.editable(w, r)
	if !ok {
		return
	}
	servers := ar.Spec.Servers
	if in.Servers != nil {
		servers = in.Servers
	}
	if err := c.validate(r, in, len(servers) > 0); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ar.Spec = v1.AccessRuleSpec{Groups: in.Groups, AllSignedIn: in.AllSignedIn, Models: in.Models, Servers: servers}
	if c.GitOps != nil {
		c.propose(w, r, s, "update", "AccessRule", "accessrules", ar.Name, ar.Spec, ar.Labels, consoleAnnotations(ar.Annotations))
		return
	}
	if ar.Annotations == nil {
		ar.Annotations = map[string]string{}
	}
	ar.Annotations[updatedByKey] = s.Email
	if err := c.Client.Update(r.Context(), ar); err != nil {
		apiErr(w, err)
		return
	}
	slog.Info("access rule updated", "rule", ar.Name, "by", s.Email, "groups", in.Groups, "allSignedIn", in.AllSignedIn, "models", in.Models, "servers", servers)
	writeJSON(w, http.StatusOK, ruleView(*ar))
}

func (c *Catalog) deleteRule(w http.ResponseWriter, r *http.Request, s Session) {
	ar, ok := c.editable(w, r)
	if !ok {
		return
	}
	if c.GitOps != nil {
		c.propose(w, r, s, "delete", "AccessRule", "accessrules", ar.Name, nil, nil, nil)
		return
	}
	if err := c.Client.Delete(r.Context(), ar); err != nil && !apierrors.IsNotFound(err) {
		apiErr(w, err)
		return
	}
	slog.Info("access rule deleted", "rule", ar.Name, "by", s.Email)
	w.WriteHeader(http.StatusNoContent)
}

// consoleAnnotations keeps only Ambit's own annotations for Git; Argo CD and kubectl add theirs.
func consoleAnnotations(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		if strings.HasPrefix(k, v1.GroupVersion.Group+"/") {
			out[k] = v
		}
	}
	return out
}

func gitManaged(annotations map[string]string) bool {
	_, ok := annotations["argocd.argoproj.io/tracking-id"]
	return ok
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func serverErr(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) {
		return // the browser left the page; nobody reads this answer
	}
	slog.Error("catalog", "err", err)
	writeErr(w, http.StatusInternalServerError, "the console could not reach Kubernetes; try again")
}

// apiErr passes the API server's validation message (e.g. the CRD's CEL rule) to the user.
func apiErr(w http.ResponseWriter, err error) {
	if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
		writeErr(w, http.StatusBadRequest, strings.TrimSpace(err.Error()))
		return
	}
	if apierrors.IsForbidden(err) {
		writeErr(w, http.StatusForbidden, "the console is not allowed to do that in Kubernetes: "+err.Error())
		return
	}
	serverErr(w, err)
}
