package console

import (
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

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/controller"
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
}

type RuleView struct {
	Name        string   `json:"name"`
	Groups      []string `json:"groups"`
	AllSignedIn bool     `json:"allSignedIn"`
	Models      []string `json:"models"`
	Ready       string   `json:"ready"`
	Reason      string   `json:"reason"`
	Message     string   `json:"message"`
	GitManaged  bool     `json:"gitManaged"`
	CreatedBy   string   `json:"createdBy"`
}

// Effective is the policy the gateway enforces for one model, after merging every rule.
type Effective struct {
	Model     string `json:"model"`
	ModelName string `json:"modelName"`
	CEL       string `json:"cel"`
}

type RuleDetail struct {
	RuleView
	Effective []Effective `json:"effective"`
}

type RuleInput struct {
	Name        string   `json:"name"`
	Groups      []string `json:"groups"`
	AllSignedIn bool     `json:"allSignedIn"`
	Models      []string `json:"models"`
}

const (
	createdByKey = "ambitproject.dev/created-by"
	updatedByKey = "ambitproject.dev/updated-by"
)

var (
	nameRE  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	groupRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@/-]*$`) // same pattern as the CRD
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
	write := func(h func(http.ResponseWriter, *http.Request, Session)) http.HandlerFunc {
		return read(func(w http.ResponseWriter, r *http.Request, s Session) {
			if s.Role != RoleAdmin {
				writeErr(w, http.StatusForbidden, "only admins can change access rules")
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
	mux.HandleFunc("GET /api/models", read(c.listModels))
	mux.HandleFunc("GET /api/access-rules", read(c.listRules))
	mux.HandleFunc("GET /api/access-rules/{name}", read(c.getRule))
	mux.HandleFunc("POST /api/access-rules", write(c.createRule))
	mux.HandleFunc("PUT /api/access-rules/{name}", write(c.updateRule))
	mux.HandleFunc("DELETE /api/access-rules/{name}", write(c.deleteRule))
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
			AllowedGroups: nonNil(m.Status.AllowedGroups), AllSignedIn: m.Status.AllowAllSignedIn, Ready: "Unknown"}
		if cond != nil {
			v.Ready, v.Message = string(cond.Status), cond.Message
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func ruleView(ar v1.AccessRule) RuleView {
	v := RuleView{Name: ar.Name, Groups: nonNil(ar.Spec.Groups), AllSignedIn: ar.Spec.AllSignedIn, Models: nonNil(ar.Spec.Models),
		Ready: "Unknown", GitManaged: gitManaged(ar.Annotations), CreatedBy: ar.Annotations[createdByKey]}
	if cond := meta.FindStatusCondition(ar.Status.Conditions, "Ready"); cond != nil {
		v.Ready, v.Reason, v.Message = string(cond.Status), cond.Reason, cond.Message
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
	d := RuleDetail{RuleView: ruleView(rs[i]), Effective: []Effective{}}
	for _, name := range rs[i].Spec.Models {
		mi := slices.IndexFunc(ms, func(m v1.Model) bool { return m.Name == name })
		if mi < 0 {
			continue
		}
		d.Effective = append(d.Effective, Effective{Model: name, ModelName: ms[mi].Spec.Model, CEL: controller.CEL(res.Access[name])})
	}
	writeJSON(w, http.StatusOK, d)
}

// validate checks a rule from the form. hasServers is true when the rule already grants MCP tools,
// which the form does not edit yet; such a rule may have no models.
func (c *Catalog) validate(r *http.Request, in RuleInput, hasServers bool) error {
	switch {
	case (len(in.Groups) > 0) == in.AllSignedIn:
		return errors.New("choose either groups or everyone signed in, not both")
	case len(in.Models) == 0 && !hasServers:
		return errors.New("choose at least one model")
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
	if err := c.validate(r, in, false); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ar := &v1.AccessRule{
		ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: c.Namespace, Annotations: map[string]string{createdByKey: s.Email}},
		Spec:       v1.AccessRuleSpec{Groups: in.Groups, AllSignedIn: in.AllSignedIn, Models: in.Models},
	}
	if err := c.Client.Create(r.Context(), ar); err != nil {
		if apierrors.IsAlreadyExists(err) {
			writeErr(w, http.StatusConflict, "an access rule named "+in.Name+" already exists")
			return
		}
		apiErr(w, err)
		return
	}
	slog.Info("access rule created", "rule", in.Name, "by", s.Email, "groups", in.Groups, "allSignedIn", in.AllSignedIn, "models", in.Models)
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
	if gitManaged(ar.Annotations) {
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
	if err := c.validate(r, in, len(ar.Spec.Servers) > 0); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ar.Spec = v1.AccessRuleSpec{Groups: in.Groups, AllSignedIn: in.AllSignedIn, Models: in.Models, Servers: ar.Spec.Servers}
	if ar.Annotations == nil {
		ar.Annotations = map[string]string{}
	}
	ar.Annotations[updatedByKey] = s.Email
	if err := c.Client.Update(r.Context(), ar); err != nil {
		apiErr(w, err)
		return
	}
	slog.Info("access rule updated", "rule", ar.Name, "by", s.Email, "groups", in.Groups, "allSignedIn", in.AllSignedIn, "models", in.Models)
	writeJSON(w, http.StatusOK, ruleView(*ar))
}

func (c *Catalog) deleteRule(w http.ResponseWriter, r *http.Request, s Session) {
	ar, ok := c.editable(w, r)
	if !ok {
		return
	}
	if err := c.Client.Delete(r.Context(), ar); err != nil && !apierrors.IsNotFound(err) {
		apiErr(w, err)
		return
	}
	slog.Info("access rule deleted", "rule", ar.Name, "by", s.Email)
	w.WriteHeader(http.StatusNoContent)
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

func writeErr(w http.ResponseWriter, code int, msg string) { writeJSON(w, code, map[string]string{"error": msg}) }

func serverErr(w http.ResponseWriter, err error) {
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
