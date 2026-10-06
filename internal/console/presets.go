package console

// Presets (docs/presets.md): an admin picks a verified preset, registers the OAuth app at the vendor with
// the steps shown, and Ambit creates the Connection and its Server together. Every addition has an
// operation ID; nothing is deleted automatically, and "unfinished additions" lets the admin finish or remove.
//
// The client secret goes only into Secret ambit-conn-<name> (grants namespace). The Secret also records the
// operation, its branch (GitOps) and the input without the secret, so a retry or "finish" repeats the same
// addition and a half-done one is always listed, never hidden.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/controller"
	"github.com/craftsman-apps/ambit/internal/presets"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	operationKey    = "ambitproject.dev/operation"
	presetKey       = "ambitproject.dev/preset"
	presetValuesKey = "ambitproject.dev/preset-values"
	branchKey       = "ambitproject.dev/branch"
	presetInputKey  = "ambitproject.dev/preset-input"
	appSecretLabel  = "ambitproject.dev/app-secret"
)

var operationRE = regexp.MustCompile(`^[a-z0-9]{6,32}$`)

type PresetView struct {
	ID                string                `json:"id"`
	Version           int                   `json:"version"`
	DisplayName       string                `json:"displayName"`
	Category          string                `json:"category"`
	ServerHost        string                `json:"serverHost"`
	ServerURL         string                `json:"serverURL"` // with example values, for "Advanced"
	Scopes            []string              `json:"scopes"`
	Setup             string                `json:"setup"`
	Placeholders      []presets.Placeholder `json:"placeholders"`
	ClientIDLabel     string                `json:"clientIDLabel"`
	ClientSecretLabel string                `json:"clientSecretLabel"`
	Requires          string                `json:"requires,omitempty"`
	Preview           bool                  `json:"preview"`
	Revokes           bool                  `json:"revokes"`
	TestedOn          string                `json:"testedOn,omitempty"`
	Docs              string                `json:"docs"`
}

// PresetOverrides come from "Advanced"; any of them turns the addition into a custom app.
type PresetOverrides struct {
	ServerURL        string   `json:"serverURL,omitempty"`
	AuthorizationURL string   `json:"authorizationURL,omitempty"`
	TokenURL         string   `json:"tokenURL,omitempty"`
	RevocationURL    string   `json:"revocationURL,omitempty"`
	Scopes           []string `json:"scopes,omitempty"`
}

type PresetAddInput struct {
	Operation    string            `json:"operation"`
	Preset       string            `json:"preset"`
	Version      int               `json:"version"`
	Name         string            `json:"name"`
	DisplayName  string            `json:"displayName"`
	ClientID     string            `json:"clientID"`
	ClientSecret string            `json:"clientSecret,omitempty"`
	Values       map[string]string `json:"values"`
	Overrides    *PresetOverrides  `json:"overrides,omitempty"`
}

type Unfinished struct {
	Operation   string            `json:"operation"`
	App         string            `json:"app"`
	Missing     []string          `json:"missing"`
	PullRequest *PullRequestState `json:"pullRequest,omitempty"`
}

type NewerPreset struct {
	Version int      `json:"version"`
	Changes []string `json:"changes"`
}

func (c *Catalog) preset(id string) (presets.Preset, bool) {
	for _, p := range c.Presets {
		if p.ID == id && p.Verified {
			return p, true
		}
	}
	return presets.Preset{}, false
}

func (c *Catalog) listPresets(w http.ResponseWriter, _ *http.Request, _ Session) {
	out := []PresetView{}
	for _, p := range c.Presets {
		if !p.Verified {
			continue
		}
		ex := map[string]string{}
		for _, ph := range p.Placeholders {
			ex[ph.Name] = ph.Example
		}
		res, err := p.Resolve(ex, "", c.callbackURL())
		if err != nil {
			slog.Error("preset does not resolve", "preset", p.ID, "err", err) // Validate keeps this from happening
			continue
		}
		host := ""
		if u, err := url.Parse(res.Server.URL); err == nil {
			host = u.Host
		}
		v := PresetView{ID: p.ID, Version: p.Version, DisplayName: p.DisplayName, Category: p.Category, ServerHost: host, ServerURL: res.Server.URL,
			Scopes: nonNil(res.Connection.Scopes), Setup: res.Setup, Placeholders: append([]presets.Placeholder{}, p.Placeholders...), // never null in JSON
			ClientIDLabel: p.ClientIDLabel, ClientSecretLabel: p.ClientSecretLabel,
			Requires: p.Requires, Preview: p.Preview, Revokes: p.RevocationURL != "", TestedOn: p.VerifiedOn, Docs: p.Docs}
		if v.ClientIDLabel == "" {
			v.ClientIDLabel = "Client ID"
		}
		if v.ClientSecretLabel == "" {
			v.ClientSecretLabel = "Client secret"
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// build resolves the input into the objects to create; it never touches the cluster.
func (c *Catalog) build(in PresetAddInput, by string) (*v1.Connection, *v1.Server, error) {
	p, ok := c.preset(in.Preset)
	switch {
	case !ok:
		return nil, nil, fmt.Errorf("no verified preset named %s", in.Preset)
	case p.Version != in.Version:
		return nil, nil, fmt.Errorf("the preset changed (now version %d); reload the page", p.Version)
	case !operationRE.MatchString(in.Operation):
		return nil, nil, errors.New("missing operation ID; reload the page")
	}
	res, err := p.Resolve(in.Values, in.ClientID, c.callbackURL())
	if err != nil {
		return nil, nil, err
	}
	tag := p.ID + "@" + strconv.Itoa(p.Version)
	if o := in.Overrides; o != nil {
		// Only a value that differs from the preset's own makes the app custom.
		custom := false
		set := func(dst *string, v string) {
			if v != "" && v != *dst {
				*dst, custom = v, true
			}
		}
		serverURL := o.ServerURL
		if serverURL == res.Server.URL {
			serverURL = ""
		}
		set(&res.Server.URL, serverURL)
		set(&res.Connection.AuthorizationURL, o.AuthorizationURL)
		set(&res.Connection.TokenURL, o.TokenURL)
		set(&res.Connection.RevocationURL, o.RevocationURL)
		if len(o.Scopes) > 0 && !slices.Equal(o.Scopes, res.Connection.Scopes) {
			res.Connection.Scopes, custom = o.Scopes, true
		}
		if custom {
			tag = "custom from " + tag
		}
		if serverURL != "" {
			if !strings.HasPrefix(serverURL, "https://") || !validURL(serverURL) {
				return nil, nil, errors.New("the server URL must be an https URL, without a query string or fragment")
			}
			res.Connection.ServerURL = serverURL
			if p.Resource == "" || p.Resource == "default" { // "none" and an explicit resource stay as Resolve set them
				res.Connection.Resource = serverURL
			}
		}
	}
	// Resolve checked the preset's URLs; checkAppWith checks them again with any overrides. Only a test
	// preset (presets.LoadTesting) may use plain http to the kind test hosts.
	if err := checkAppWith(AppInput{Name: in.Name, DisplayName: in.DisplayName, AuthorizationURL: res.Connection.AuthorizationURL,
		TokenURL: res.Connection.TokenURL, RevocationURL: res.Connection.RevocationURL, ClientID: in.ClientID, ClientSecret: "unchanged",
		Scopes: res.Connection.Scopes, AuthorizeParams: res.Connection.AuthorizeParams}, p.Testing); err != nil {
		return nil, nil, err
	}
	values, _ := json.Marshal(in.Values)
	ann := map[string]string{createdByKey: by, operationKey: in.Operation, presetKey: tag, presetValuesKey: string(values)}
	conn := &v1.Connection{ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: c.Namespace, Annotations: ann}, Spec: res.Connection}
	conn.Spec.DisplayName, conn.Spec.ClientID = strings.TrimSpace(in.DisplayName), in.ClientID
	conn.Spec.ClientSecretRef = v1.SecretRef{Name: "ambit-conn-" + in.Name}
	srv := &v1.Server{ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: c.Namespace, Annotations: ann}, Spec: res.Server}
	srv.Spec.DisplayName, srv.Spec.Connection = strings.TrimSpace(in.DisplayName), in.Name
	return conn, srv, nil
}

func sameInput(a, b PresetAddInput) bool {
	a.ClientSecret, b.ClientSecret = "", ""
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func branchFor(in PresetAddInput) string { return branchPrefix + "add-" + in.Name + "-" + in.Operation }

func (c *Catalog) addFromPreset(w http.ResponseWriter, r *http.Request, s Session) {
	var in PresetAddInput
	if !decodeInto(w, r, &in) {
		return
	}
	if in.ClientSecret == "" {
		writeErr(w, http.StatusBadRequest, "the client secret is required")
		return
	}
	conn, srv, err := c.build(in, s.Email)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// One operation is one addition, whatever name the browser sends now.
	if _, prev, err := c.operationSecret(r.Context(), in.Operation); err == nil && !sameInput(prev, in) {
		writeErr(w, http.StatusConflict, "this addition was started with other values; reload the page to start a new one")
		return
	} else if err != nil && !apierrors.IsNotFound(err) {
		serverErr(w, err)
		return
	}
	// Refuse a name in use before the Secret exists: a Secret for an addition that can never finish
	// would be left behind. Objects of this same operation (a retry) are fine.
	for _, o := range []client.Object{&v1.Connection{}, &v1.Server{}} {
		err := c.Client.Get(r.Context(), client.ObjectKey{Namespace: c.Namespace, Name: in.Name}, o)
		if err == nil && o.GetAnnotations()[operationKey] != in.Operation {
			writeErr(w, http.StatusConflict, "an app or server named "+in.Name+" already exists; choose another name")
			return
		}
		if err != nil && !apierrors.IsNotFound(err) {
			serverErr(w, err)
			return
		}
	}
	if err := c.ensureAppSecret(r.Context(), s, in); err != nil {
		if errors.Is(err, errOtherAddition) {
			writeErr(w, http.StatusConflict, err.Error())
		} else {
			apiErr(w, err)
		}
		return
	}
	c.apply(w, r, s, in, conn, srv)
}

// apply creates the objects (Direct) or proposes them (GitOps). Both are safe to repeat for one operation.
func (c *Catalog) apply(w http.ResponseWriter, r *http.Request, s Session, in PresetAddInput, conn *v1.Connection, srv *v1.Server) {
	ctx := r.Context()
	if c.GitOps != nil {
		cm, err1 := Manifest("Connection", conn.Name, nil, conn.Annotations, conn.Spec)
		sm, err2 := Manifest("Server", srv.Name, nil, srv.Annotations, srv.Spec)
		if err := errors.Join(err1, err2); err != nil {
			serverErr(w, err)
			return
		}
		objs := []ProposedObject{{Kind: "Connection", Plural: "connections", Name: conn.Name, Manifest: cm}, {Kind: "Server", Plural: "servers", Name: srv.Name, Manifest: sm}}
		title := "Ambit: add app " + in.Name
		branch := branchFor(in)
		if sec, _, err := c.operationSecret(ctx, in.Operation); err == nil && sec.Annotations[branchKey] != "" {
			branch = sec.Annotations[branchKey] // a closed pull request moved the operation to a new branch
		}
		pr, err := c.GitOps.ProposeObjects(ctx, branch, title, s.Email, objs)
		if errors.Is(err, ErrPullRequestClosed) {
			// Closed without merging: open a fresh branch and record it on the Secret.
			branch = fmt.Sprintf("%s-r%d", branchFor(in), time.Now().Unix())
			if err = c.setBranch(ctx, in.Operation, branch); err == nil {
				pr, err = c.GitOps.ProposeObjects(ctx, branch, title, s.Email, objs)
			}
		}
		var invalid *InvalidChange
		if errors.As(err, &invalid) {
			writeErr(w, http.StatusBadRequest, invalid.Error())
			return
		}
		if err != nil {
			slog.Error("add from preset: pull request", "app", in.Name, "operation", in.Operation, "err", err)
			writeErr(w, http.StatusBadGateway, "could not open the pull request; it may still have been created. Finish it under Unfinished additions: "+err.Error())
			return
		}
		slog.Info("app from preset proposed", "app", in.Name, "preset", in.Preset, "version", in.Version, "by", s.Email, "url", pr.URL)
		writeJSON(w, http.StatusAccepted, Proposed{PullRequest: pr})
		return
	}
	code := http.StatusCreated
	for _, o := range []client.Object{conn, srv} {
		err := c.Client.Create(ctx, o)
		if apierrors.IsAlreadyExists(err) {
			cur := o.DeepCopyObject().(client.Object)
			if gerr := c.Client.Get(ctx, client.ObjectKeyFromObject(o), cur); gerr == nil && cur.GetAnnotations()[operationKey] == in.Operation {
				code = http.StatusOK // the same operation, retried
				continue
			}
			writeErr(w, http.StatusConflict, "an object named "+o.GetName()+" already exists; remove this addition under Unfinished additions")
			return
		}
		if err != nil {
			slog.Error("add from preset", "app", in.Name, "operation", in.Operation, "err", err)
			writeErr(w, http.StatusBadGateway, "the app was only partly added; finish it under Unfinished additions: "+err.Error())
			return
		}
	}
	slog.Info("app added from preset", "app", in.Name, "preset", in.Preset, "version", in.Version, "by", s.Email)
	v := appView(*conn)
	v.Preset, v.Bound = conn.Annotations[presetKey], conn.Spec.ServerURL != ""
	writeJSON(w, code, v)
}

var errOtherAddition = errors.New("this addition was started with other values; reload the page to start a new one")

func (c *Catalog) ensureAppSecret(ctx context.Context, s Session, in PresetAddInput) error {
	stored := in
	stored.ClientSecret = ""
	input, _ := json.Marshal(stored)
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ambit-conn-" + in.Name, Namespace: c.Conns.Store.Namespace,
		// Not grantConnLabel: grant listings select on that label and would try to open this as a grant.
		Labels:      map[string]string{"app.kubernetes.io/managed-by": "ambit-console", appSecretLabel: in.Name},
		Annotations: map[string]string{createdByKey: s.Email, operationKey: in.Operation, branchKey: branchFor(in), presetInputKey: string(input)}},
		Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"clientSecret": []byte(in.ClientSecret)}}
	err := c.Conns.Store.Client.Create(ctx, sec)
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	var cur corev1.Secret
	if err := c.Conns.Store.Client.Get(ctx, client.ObjectKeyFromObject(sec), &cur); err != nil {
		return err
	}
	if cur.Annotations[operationKey] != in.Operation {
		return fmt.Errorf("the secret for an app named %s already exists; choose another name, or remove it under Unfinished additions: %w", in.Name, errOtherAddition)
	}
	if cur.Annotations[presetInputKey] != string(input) || string(cur.Data["clientSecret"]) != in.ClientSecret {
		return errOtherAddition
	}
	return nil // the same operation, retried with the same input
}

// operationSecret finds the Secret an operation created, with the input it stored.
func (c *Catalog) operationSecret(ctx context.Context, op string) (*corev1.Secret, PresetAddInput, error) {
	var secs corev1.SecretList
	if err := c.Conns.Store.Client.List(ctx, &secs, client.InNamespace(c.Conns.Store.Namespace), client.HasLabels{appSecretLabel}); err != nil {
		return nil, PresetAddInput{}, err
	}
	for i := range secs.Items {
		if secs.Items[i].Annotations[operationKey] == op {
			var in PresetAddInput
			err := json.Unmarshal([]byte(secs.Items[i].Annotations[presetInputKey]), &in)
			return &secs.Items[i], in, err
		}
	}
	return nil, PresetAddInput{}, apierrors.NewNotFound(corev1.Resource("secrets"), op)
}

// missing lists which of the app's objects are absent from the cluster; an object of another operation
// counts as missing (it is not this addition's).
func (c *Catalog) missing(ctx context.Context, app, op string) ([]string, error) {
	var out []string
	for i, o := range []client.Object{&v1.Connection{}, &v1.Server{}} {
		err := c.Client.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: app}, o)
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
		if err != nil || o.GetAnnotations()[operationKey] != op {
			out = append(out, []string{"Connection", "Server"}[i])
		}
	}
	return out, nil
}

func (c *Catalog) listUnfinished(ctx context.Context) ([]Unfinished, error) {
	var secs corev1.SecretList
	if err := c.Conns.Store.Client.List(ctx, &secs, client.InNamespace(c.Conns.Store.Namespace), client.HasLabels{appSecretLabel}); err != nil {
		return nil, err
	}
	out := []Unfinished{}
	for _, sec := range secs.Items {
		op, app := sec.Annotations[operationKey], sec.Labels[appSecretLabel]
		if op == "" {
			continue // added before operation IDs; managed as before
		}
		if c.GitOps == nil && time.Since(sec.CreationTimestamp.Time) < 10*time.Second {
			continue // an addition still in progress: its request is creating the objects now
		}
		miss, err := c.missing(ctx, app, op)
		if err != nil || len(miss) == 0 {
			if err != nil {
				return nil, err
			}
			continue // complete
		}
		u := Unfinished{Operation: op, App: app, Missing: miss}
		if c.GitOps != nil {
			// ponytail: one GitHub lookup per addition not yet in the cluster, on every listing.
			st, found, err := c.GitOps.Repo.PullRequestByBranch(ctx, sec.Annotations[branchKey])
			if err != nil {
				return nil, err
			}
			if found && !st.Closed {
				continue // open, or merged and waiting for the sync: not unfinished
			}
			u.Missing = []string{"pull request"}
			if found {
				u.PullRequest = &st
			}
		}
		out = append(out, u)
	}
	return out, nil
}

func (c *Catalog) unfinished(w http.ResponseWriter, r *http.Request, _ Session) {
	out, err := c.listUnfinished(r.Context())
	if errors.Is(err, context.Canceled) {
		return
	}
	if err != nil {
		slog.Error("list unfinished additions", "err", err)
		writeErr(w, http.StatusBadGateway, "could not list unfinished additions: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// finishUnfinished repeats the stored operation: missing objects are created, or the pull request opened
// from the same branch. The client secret is already in the Secret.
func (c *Catalog) finishUnfinished(w http.ResponseWriter, r *http.Request, s Session) {
	_, in, err := c.operationSecret(r.Context(), r.PathValue("op"))
	if apierrors.IsNotFound(err) {
		writeErr(w, http.StatusNotFound, "no unfinished addition "+r.PathValue("op"))
		return
	}
	if err != nil {
		serverErr(w, err)
		return
	}
	conn, srv, err := c.build(in, s.Email)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error()+" (remove this addition and add the app again)")
		return
	}
	c.apply(w, r, s, in, conn, srv)
}

// removeUnfinished deletes what an operation created. Refused for a complete addition (remove the app
// itself, which revokes grants) and while its pull request is open or merged: the Secret is in use or
// about to be.
func (c *Catalog) removeUnfinished(w http.ResponseWriter, r *http.Request, s Session) {
	ctx := r.Context()
	sec, in, err := c.operationSecret(ctx, r.PathValue("op"))
	if apierrors.IsNotFound(err) {
		writeErr(w, http.StatusNotFound, "no unfinished addition "+r.PathValue("op"))
		return
	}
	if err != nil {
		serverErr(w, err)
		return
	}
	miss, err := c.missing(ctx, in.Name, in.Operation)
	if err != nil {
		serverErr(w, err)
		return
	}
	if len(miss) == 0 {
		writeErr(w, http.StatusConflict, "this addition is complete; remove the app on the Apps page")
		return
	}
	if c.GitOps != nil {
		st, found, err := c.GitOps.Repo.PullRequestByBranch(ctx, sec.Annotations[branchKey])
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		if found && !st.Closed {
			writeErr(w, http.StatusConflict, "its pull request is open or merged; close it first, or remove the app itself")
			return
		}
		if err := c.GitOps.Repo.DeleteBranch(ctx, sec.Annotations[branchKey]); err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
	} else {
		for _, o := range []client.Object{&v1.Server{}, &v1.Connection{}} {
			err := c.Client.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: in.Name}, o)
			if apierrors.IsNotFound(err) || (err == nil && o.GetAnnotations()[operationKey] != in.Operation) {
				continue // absent, or another app's object: never touched
			}
			if err != nil {
				serverErr(w, err)
				return
			}
			if err := c.Client.Delete(ctx, o, client.Preconditions{UID: ptrUID(o.GetUID())}); err != nil && !apierrors.IsNotFound(err) {
				apiErr(w, err)
				return
			}
		}
	}
	if err := c.Conns.Store.Client.Delete(ctx, sec, client.Preconditions{UID: ptrUID(sec.UID)}); err != nil && !apierrors.IsNotFound(err) {
		apiErr(w, err)
		return
	}
	slog.Info("unfinished addition removed", "app", in.Name, "operation", in.Operation, "by", s.Email)
	w.WriteHeader(http.StatusNoContent)
}

func ptrUID(u types.UID) *types.UID { return &u }

func (c *Catalog) setBranch(ctx context.Context, op, branch string) error {
	sec, _, err := c.operationSecret(ctx, op)
	if err != nil {
		return err
	}
	sec.Annotations[branchKey] = branch
	return c.Conns.Store.Client.Update(ctx, sec)
}

// newerPreset reports a newer catalog version of the preset an app came from, with what would change.
func (c *Catalog) newerPreset(conn v1.Connection, servers []v1.Server) *NewerPreset {
	id, ver, ok := strings.Cut(conn.Annotations[presetKey], "@")
	if !ok || strings.HasPrefix(id, "custom from ") {
		return nil
	}
	p, found := c.preset(id)
	if v, err := strconv.Atoi(ver); err != nil || !found || p.Version <= v {
		return nil
	}
	var values map[string]string
	_ = json.Unmarshal([]byte(conn.Annotations[presetValuesKey]), &values)
	res, err := p.Resolve(values, conn.Spec.ClientID, c.callbackURL())
	if err != nil {
		return &NewerPreset{Version: p.Version, Changes: []string{"the new version needs other values: " + err.Error()}}
	}
	ch := []string{}
	diff := func(what, old, now string) {
		if old != now {
			ch = append(ch, fmt.Sprintf("%s: %s → %s", what, old, now))
		}
	}
	diff("authorization URL", conn.Spec.AuthorizationURL, res.Connection.AuthorizationURL)
	diff("token URL", conn.Spec.TokenURL, res.Connection.TokenURL)
	diff("revocation URL", conn.Spec.RevocationURL, res.Connection.RevocationURL)
	diff("resource", conn.Spec.Resource, res.Connection.Resource)
	diff("scopes", strings.Join(conn.Spec.Scopes, " "), strings.Join(res.Connection.Scopes, " "))
	for _, s := range servers {
		if s.Name == conn.Name {
			diff("server URL", s.Spec.URL, res.Server.URL)
		}
	}
	return &NewerPreset{Version: p.Version, Changes: ch}
}

// serverURLs are the URLs of the Servers that name the Connection.
func serverURLs(conn string, servers []v1.Server) []string {
	var urls []string
	for _, srv := range servers {
		if srv.Spec.Connection == conn {
			urls = append(urls, srv.Spec.URL)
		}
	}
	return urls
}

// bindApp binds an older Connection to the one Server that uses it. Only serverURL is set: setting
// resource would make every existing grant ask its user to reconnect. In Direct mode the existing grants
// get the same serverURL (bindGrants), so their users keep working. In GitOps mode the controller applies
// the change after merge, when the console cannot update the grants at the same moment: those users
// connect again once (the UI says so before the admin binds).
func (c *Catalog) bindApp(w http.ResponseWriter, r *http.Request, s Session) {
	var conn v1.Connection
	if err := c.Client.Get(r.Context(), client.ObjectKey{Namespace: c.Namespace, Name: r.PathValue("name")}, &conn); apierrors.IsNotFound(err) {
		writeErr(w, http.StatusNotFound, "no app named "+r.PathValue("name"))
		return
	} else if err != nil {
		serverErr(w, err)
		return
	}
	if c.GitOps == nil && gitManaged(conn.Annotations) {
		writeErr(w, http.StatusConflict, "this app is managed in Git; change it with a pull request")
		return
	}
	var servers v1.ServerList
	if err := c.Client.List(r.Context(), &servers, client.InNamespace(c.Namespace)); err != nil {
		serverErr(w, err)
		return
	}
	urls := serverURLs(conn.Name, servers.Items)
	if len(urls) != 1 {
		writeErr(w, http.StatusConflict, "binding needs exactly one server using this app; found "+strconv.Itoa(len(urls)))
		return
	}
	conn.Spec.ServerURL = urls[0]
	if c.GitOps != nil {
		c.propose(w, r, s, "update", "Connection", "connections", conn.Name, conn.Spec, conn.Labels, consoleAnnotations(conn.Annotations))
		return
	}
	update := func() error { return c.Client.Update(r.Context(), &conn) }
	var err error
	if c.Conns != nil {
		err = c.Conns.bindGrants(r.Context(), &conn, urls[0], update)
	} else {
		err = update()
	}
	if err != nil {
		apiErr(w, err)
		return
	}
	slog.Info("app bound to its server", "connection", conn.Name, "serverURL", urls[0], "by", s.Email)
	v := appView(conn)
	v.Preset, v.Bound = conn.Annotations[presetKey], true
	writeJSON(w, http.StatusOK, v)
}

// serverAccess answers GET /api/servers/{name}/access from the same merge the gateway policy uses,
// over the Servers the reconciler keeps.
func (c *Catalog) serverAccess(w http.ResponseWriter, r *http.Request, _ Session) {
	_, rules, err := c.load(r)
	if err != nil {
		serverErr(w, err)
		return
	}
	servers, err := c.servers(r)
	if err != nil {
		serverErr(w, err)
		return
	}
	name := r.PathValue("name")
	if !slices.ContainsFunc(servers, func(s v1.Server) bool { return s.Name == name }) {
		writeErr(w, http.StatusNotFound, "no server named "+name)
		return
	}
	var conns v1.ConnectionList
	if err := c.Client.List(r.Context(), &conns, client.InNamespace(c.Namespace)); err != nil {
		serverErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, controller.ServerAccess(servers, conns.Items, rules, name))
}
