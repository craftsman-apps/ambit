package console

// Change a model's key (#166) and delete a model (#167), docs/providers.md.
//
// Only a console key Secret is changed or deleted: Secret ambit-llm-<model> that Add model created for this model
// (its labels say so). Any other Secret is managed elsewhere (kubectl, External Secrets) and the console leaves it.
// The key is written with a merge patch on a metadata-only object, so the console never reads the old key.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	keyChangedByKey = "ambitproject.dev/key-changed-by"
	keyChangedAtKey = "ambitproject.dev/key-changed-at"
	deleteBranchKey = "ambitproject.dev/delete-branch" // the GitOps pull request that deletes this Secret's Model
)

// keyEntries are every Secret entry a Model's key can use (api/v1alpha1/types.go). A key change sets the provider's
// entries and removes the others, so an old entry (a session token) does not stay.
var keyEntries = []string{"Authorization", "accessKey", "secretKey", "sessionToken", "credentials.json"}

type ModelKeyInput struct {
	APIKey string `json:"apiKey,omitempty"`
}

// entries are the Secret entries for the provider, and the values redact must remove from any message.
func (in *ModelKeyInput) entries(provider string) (map[string]string, []string, error) {
	in.APIKey = strings.TrimSpace(in.APIKey)
	if cloudOnly(provider) {
		return nil, []string{in.APIKey}, errors.New("keys for " + provider + " are not changed in the console; change the Secret where it is managed")
	}
	if in.APIKey == "" {
		return nil, nil, errors.New("the API key is empty")
	}
	return map[string]string{"Authorization": in.APIKey}, []string{in.APIKey}, nil
}

// isConsoleKey reports whether the Secret is the one Add model created for this model.
func isConsoleKey(sec metav1.Object, model string) bool {
	return sec.GetName() == llmSecretPrefix+model && sec.GetLabels()["app.kubernetes.io/managed-by"] == "ambit-console" &&
		sec.GetLabels()[modelSecretLabel] == model && sec.GetAnnotations()[operationKey] != ""
}

// consoleKeySecret is the Model's console key Secret (metadata only), or a message for the admin.
func (c *Catalog) consoleKeySecret(ctx context.Context, m *v1.Model) (*metav1.PartialObjectMetadata, string, error) {
	ref := m.Spec.CredentialsRef
	if ref == nil {
		return nil, "this model uses the gateway's cloud identity; it has no key to change", nil
	}
	sec, err := c.secretMeta(ctx, ref.Name)
	if apierrors.IsNotFound(err) {
		return nil, "the Secret " + ref.Name + " does not exist; create it, or delete and add the model again", nil
	} else if err != nil {
		return nil, "", err
	}
	if !isConsoleKey(sec, m.Name) {
		return nil, "the Secret " + ref.Name + " was not created by the console for this model; change it where it is managed", nil
	}
	return sec, "", nil
}

// getModel reads the Model named in the path; on false the answer is written.
func (c *Catalog) getModel(w http.ResponseWriter, r *http.Request) (*v1.Model, bool) {
	var m v1.Model
	if err := c.Client.Get(r.Context(), client.ObjectKey{Namespace: c.Namespace, Name: r.PathValue("name")}, &m); err != nil {
		if apierrors.IsNotFound(err) {
			writeErr(w, http.StatusNotFound, "no model named "+r.PathValue("name"))
		} else {
			serverErr(w, err)
		}
		return nil, false
	}
	return &m, true
}

// changeModelKey answers PUT /api/models/{name}/key. The same in Direct and GitOps mode: the key is never in Git.
func (c *Catalog) changeModelKey(w http.ResponseWriter, r *http.Request, s Session) {
	var in ModelKeyInput
	if !decodeInto(w, r, &in) {
		return
	}
	m, ok := c.getModel(w, r)
	if !ok {
		return
	}
	data, secrets, err := in.entries(m.Spec.Provider)
	if err != nil {
		writeErr(w, http.StatusBadRequest, redact(err.Error(), secrets...))
		return
	}
	ctx := r.Context()
	sec, refused, err := c.consoleKeySecret(ctx, m)
	if err != nil {
		serverErr(w, err)
		return
	}
	if refused != "" {
		writeErr(w, http.StatusConflict, refused)
		return
	}
	patchData := map[string]any{}
	for _, k := range keyEntries {
		patchData[k] = nil
	}
	for k, v := range data {
		patchData[k] = []byte(v) // encoded as base64 by json.Marshal
	}
	// uid and resourceVersion make the patch fail on a Secret that was replaced or changed since the read.
	patch, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": sec.UID, "resourceVersion": sec.ResourceVersion,
			"annotations": map[string]string{keyChangedByKey: s.Email, keyChangedAtKey: time.Now().UTC().Format(time.RFC3339)}},
		"data": patchData,
	})
	if err := c.Client.Patch(ctx, sec, client.RawPatch(types.MergePatchType, patch)); err != nil {
		switch {
		case apierrors.IsConflict(err) || apierrors.IsNotFound(err):
			writeErr(w, http.StatusConflict, "the Secret "+sec.Name+" changed while saving; try again")
		default:
			slog.Error("change model key", "model", m.Name, "secret", sec.Name, "err", redact(err.Error(), secrets...))
			apiErrRedacted(w, err, secrets...)
		}
		return
	}
	slog.Info("model key changed", "model", m.Name, "secret", sec.Name, "by", s.Email)
	w.WriteHeader(http.StatusNoContent)
}

// modelReferences names what refers to the model (decision D6): access rules, model routers, and other Models'
// fallback lists. It reads the cluster; in GitOps mode a reference only in an open pull request is not seen.
func (c *Catalog) modelReferences(ctx context.Context, name string) (string, error) {
	var rules v1.AccessRuleList
	var routers v1.ModelRouterList
	var models v1.ModelList
	for _, l := range []client.ObjectList{&rules, &routers, &models} {
		if err := c.Client.List(ctx, l, client.InNamespace(c.Namespace)); err != nil {
			return "", err
		}
	}
	var ruleNames, routerNames, modelNames []string
	for _, ar := range rules.Items {
		if slices.Contains(ar.Spec.Models, name) {
			ruleNames = append(ruleNames, ar.Name)
		}
	}
	for _, mr := range routers.Items {
		used := mr.Spec.Default == name || slices.ContainsFunc(mr.Spec.Routes, func(rt v1.RouterRoute) bool { return rt.Model == name }) ||
			mr.Spec.Classifier.LLM != nil && mr.Spec.Classifier.LLM.Model == name
		if used {
			routerNames = append(routerNames, mr.Name)
		}
	}
	for _, m := range models.Items {
		if m.Name != name && slices.Contains(m.Spec.Fallback, name) {
			modelNames = append(modelNames, m.Name)
		}
	}
	var parts []string
	if len(ruleNames) > 0 {
		parts = append(parts, "the access rules "+strings.Join(ruleNames, ", "))
	}
	if len(routerNames) > 0 {
		parts = append(parts, "the model routers "+strings.Join(routerNames, ", "))
	}
	if len(modelNames) > 0 {
		parts = append(parts, "the models "+strings.Join(modelNames, ", ")+" (as a fallback)")
	}
	if len(parts) == 0 {
		return "", nil
	}
	return strings.Join(parts, " and ") + " name this model; remove it from them first", nil
}

// ModelDeleted answers a Direct mode delete: which Secret the Model used and whether it was removed.
type ModelDeleted struct {
	Secret        string `json:"secret"`
	SecretRemoved bool   `json:"secretRemoved"`
	SecretNote    string `json:"secretNote,omitempty"`
}

var tsRE = regexp.MustCompile(`^[0-9]+$`)

// deleteModel answers DELETE /api/models/{name}.
func (c *Catalog) deleteModel(w http.ResponseWriter, r *http.Request, s Session) {
	ctx := r.Context()
	m, ok := c.getModel(w, r)
	if !ok {
		return
	}
	if c.GitOps == nil && gitManaged(m.Annotations) {
		writeErr(w, http.StatusConflict, "this model is managed in Git; delete it with a pull request")
		return
	}
	if refs, err := c.modelReferences(ctx, m.Name); err != nil {
		serverErr(w, err)
		return
	} else if refs != "" {
		writeErr(w, http.StatusConflict, refs)
		return
	}
	var sec *metav1.PartialObjectMetadata // the console key Secret, when the Model has one
	if m.Spec.CredentialsRef != nil {
		got, err := c.secretMeta(ctx, m.Spec.CredentialsRef.Name)
		if err != nil && !apierrors.IsNotFound(err) {
			serverErr(w, err)
			return
		}
		if err == nil && isConsoleKey(got, m.Name) {
			sec = got
		}
	}
	if c.GitOps != nil {
		c.proposeModelDelete(w, r, s, m, sec)
		return
	}
	if err := c.Client.Delete(ctx, m, client.Preconditions{UID: ptrUID(m.UID)}); err != nil && !apierrors.IsNotFound(err) {
		apiErr(w, err)
		return
	}
	out := ModelDeleted{}
	if m.Spec.CredentialsRef != nil {
		out.Secret = m.Spec.CredentialsRef.Name
	}
	switch user, err := c.modelUsing(ctx, out.Secret); {
	case out.Secret == "":
	case sec == nil:
		out.SecretNote = "not created by the console; it is kept"
	case err != nil:
		slog.Error("delete model: check the Secret's users", "secret", out.Secret, "err", err)
		out.SecretNote = "could not be checked, so it is kept; it is listed under Keys without a model"
	case user != "":
		out.SecretNote = "used by the model " + user + "; it is kept"
	default:
		if err := c.Client.Delete(ctx, sec, client.Preconditions{UID: ptrUID(sec.UID)}); err != nil && !apierrors.IsNotFound(err) {
			slog.Error("delete model: delete key secret", "secret", sec.Name, "err", err)
			out.SecretNote = "could not be removed; it is listed under Keys without a model"
			break
		}
		out.SecretRemoved = true
	}
	slog.Info("model deleted", "model", m.Name, "secret", out.Secret, "secretRemoved", out.SecretRemoved, "by", s.Email)
	writeJSON(w, http.StatusOK, out)
}

// proposeModelDelete opens (or returns the open) pull request that deletes the Model's manifest, and marks the
// console key Secret with its branch so it is listed for removal once the Model is gone.
func (c *Catalog) proposeModelDelete(w http.ResponseWriter, r *http.Request, s Session, m *v1.Model, sec *metav1.PartialObjectMetadata) {
	ctx := r.Context()
	prs, err := c.GitOps.Repo.OpenPullRequests(ctx)
	if err != nil {
		slog.Error("delete model: list pull requests", "model", m.Name, "err", err)
		writeErr(w, http.StatusBadGateway, "could not read pull requests from "+c.GitOps.Repository)
		return
	}
	head := branchPrefix + "delete-model-" + m.Name + "-"
	var pr *PullRequest
	for i := range prs {
		if rest, ok := strings.CutPrefix(prs[i].Branch, head); ok && tsRE.MatchString(rest) {
			pr = &prs[i]
			break
		}
	}
	if pr == nil {
		opened, err := c.GitOps.Propose(ctx, "delete", "Model", "models", m.Name, s.Email, nil)
		if err != nil {
			slog.Error("delete model: pull request", "model", m.Name, "err", err)
			writeErr(w, http.StatusBadGateway, "could not open a pull request in "+c.GitOps.Repository+": "+err.Error())
			return
		}
		pr = &opened
		slog.Info("pull request opened", "kind", "Model", "name", m.Name, "verb", "delete", "by", s.Email, "url", pr.URL)
	}
	if sec != nil && sec.Annotations[deleteBranchKey] != pr.Branch {
		patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"uid": sec.UID, "annotations": map[string]string{deleteBranchKey: pr.Branch}}})
		if err := c.Client.Patch(ctx, sec, client.RawPatch(types.MergePatchType, patch)); err != nil {
			// The pull request is open; without the mark the Secret is not listed after the merge. Say so.
			slog.Error("delete model: mark key secret", "secret", sec.Name, "err", err)
			writeErr(w, http.StatusBadGateway, fmt.Sprintf("the pull request %s is open, but the Secret %s could not be marked for removal; try again", pr.URL, sec.Name))
			return
		}
	}
	writeJSON(w, http.StatusAccepted, Proposed{PullRequest: *pr})
}
