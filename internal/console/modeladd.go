package console

// Add model (docs/providers.md): an admin adds a Model in the console. The provider API key is typed in the
// console, or the Model names an existing Secret ambit-llm-*, or (Bedrock, VertexAI) the gateway's cloud identity.
//
// A typed key goes only into Secret ambit-llm-<name>. The Secret also records the operation and the GitOps
// branch, so a retry repeats the same addition and a half-done one is listed ("unfinished"), never hidden.
// The key is never read back, logged, returned or put in Git. redact() is a second guard on every message that
// can pass near it.

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	llmSecretPrefix  = "ambit-llm-"
	modelSecretLabel = "ambitproject.dev/model-secret" // not one of the ambit-keys-writer selector labels (ambitproject.dev/key*)
	modelInputKey    = "ambitproject.dev/model-input"
)

var branchOpRE = regexp.MustCompile(`^[a-z0-9]{6,32}(-r[0-9]+)?$`)

type ModelAddInput struct {
	Operation string `json:"operation"`
	Name      string `json:"name"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	BaseURL   string `json:"baseURL,omitempty"`
	// Formats only with provider OpenAI and a baseURL (self-hosted).
	Formats  []v1.ModelFormat     `json:"formats,omitempty"`
	Azure    *v1.AzureSettings    `json:"azure,omitempty"`
	Bedrock  *v1.BedrockSettings  `json:"bedrock,omitempty"`
	VertexAI *v1.VertexAISettings `json:"vertexai,omitempty"`
	// Credentials is "entered" (APIKey), "existing" (SecretName) or "cloud" (Bedrock and VertexAI only).
	Credentials string `json:"credentials"`
	APIKey      string `json:"apiKey,omitempty"`
	SecretName  string `json:"secretName,omitempty"`
}

// UnfinishedModel is a console-created key Secret whose Model does not exist.
type UnfinishedModel struct {
	Secret      string            `json:"secret"`
	Model       string            `json:"model"`
	Operation   string            `json:"operation"`
	PullRequest *PullRequestState `json:"pullRequest,omitempty"`
}

func cloudOnly(provider string) bool { return provider == "Bedrock" || provider == "VertexAI" }

// redact removes the key (and its base64 form, which a Secret dump shows) from a message.
func redact(msg, key string) string {
	if key == "" {
		return msg
	}
	msg = strings.ReplaceAll(msg, key, "[key]")
	return strings.ReplaceAll(msg, base64.StdEncoding.EncodeToString([]byte(key)), "[key]")
}

func modelBranch(in ModelAddInput) string {
	return branchPrefix + "add-model-" + in.Name + "-" + in.Operation
}

// storedInput is the input without the key: what the Secret records for a retry.
func (in ModelAddInput) storedInput() string {
	in.APIKey = ""
	b, _ := json.Marshal(in)
	return string(b)
}

// check validates what needs no cluster; the CRD's own rules run later, in the dry-run create.
func (in *ModelAddInput) check() error {
	in.APIKey = strings.TrimSpace(in.APIKey)
	switch {
	case !operationRE.MatchString(in.Operation):
		return errors.New("missing operation ID; reload the page")
	case !nameRE.MatchString(in.Name) || len(in.Name) > 63:
		return errors.New("name must be lowercase letters, digits and dashes, starting and ending with a letter or digit")
	case in.Provider == "" || strings.TrimSpace(in.Model) == "":
		return errors.New("choose a provider and give the model ID")
	}
	switch in.Credentials {
	case "entered":
		if cloudOnly(in.Provider) {
			return errors.New("keys for " + in.Provider + " are not entered in the console; use an existing Secret or the gateway's cloud identity")
		}
		if in.APIKey == "" {
			return errors.New("the API key is empty")
		}
	case "existing":
		if !strings.HasPrefix(in.SecretName, llmSecretPrefix) {
			return errors.New("the Secret's name must start with " + llmSecretPrefix)
		}
	case "cloud":
		if !cloudOnly(in.Provider) {
			return errors.New("the gateway's cloud identity is only for Bedrock and VertexAI")
		}
	default:
		return errors.New("choose how the API key is given: enter it, use an existing Secret, or the gateway's cloud identity")
	}
	if in.Credentials != "entered" && in.APIKey != "" || in.Credentials != "existing" && in.SecretName != "" {
		return errors.New("an API key or Secret name was sent that the chosen credential does not use")
	}
	return nil
}

func (c *Catalog) modelFrom(in ModelAddInput, by string) *v1.Model {
	m := &v1.Model{ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: c.Namespace,
		Annotations: map[string]string{createdByKey: by, operationKey: in.Operation}},
		Spec: v1.ModelSpec{Provider: in.Provider, Model: strings.TrimSpace(in.Model), BaseURL: in.BaseURL, Formats: in.Formats,
			Azure: in.Azure, Bedrock: in.Bedrock, VertexAI: in.VertexAI}}
	switch in.Credentials {
	case "entered":
		m.Spec.CredentialsRef = &v1.SecretRef{Name: llmSecretPrefix + in.Name}
	case "existing":
		m.Spec.CredentialsRef = &v1.SecretRef{Name: in.SecretName}
	}
	return m
}

func (c *Catalog) addModel(w http.ResponseWriter, r *http.Request, s Session) {
	var in ModelAddInput
	if !decodeInto(w, r, &in) {
		return
	}
	key := strings.TrimSpace(in.APIKey)
	if err := in.check(); err != nil {
		writeErr(w, http.StatusBadRequest, redact(err.Error(), key))
		return
	}
	ctx := r.Context()
	m := c.modelFrom(in, s.Email)

	// A Model of this name: the same operation retried (done), or another Model (refused).
	var cur v1.Model
	switch err := c.Client.Get(ctx, client.ObjectKeyFromObject(m), &cur); {
	case err == nil && cur.Annotations[operationKey] == in.Operation:
		writeJSON(w, http.StatusOK, modelView(cur))
		return
	case err == nil:
		writeErr(w, http.StatusConflict, "a model named "+in.Name+" already exists; choose another name")
		return
	case !apierrors.IsNotFound(err):
		serverErr(w, err)
		return
	}
	if in.Credentials == "existing" {
		var sec metav1.PartialObjectMetadata
		sec.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
		if err := c.Client.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: in.SecretName}, &sec); apierrors.IsNotFound(err) {
			writeErr(w, http.StatusBadRequest, "no Secret named "+in.SecretName+" in "+c.Namespace)
			return
		} else if err != nil {
			serverErr(w, err)
			return
		}
	}
	if c.GitOps != nil {
		prs, err := c.openModelPRs(ctx, in)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "could not read pull requests from "+c.GitOps.Repository+": "+err.Error())
			return
		}
		for _, pr := range prs {
			if strings.HasPrefix(pr.Branch, modelBranch(in)) {
				writeJSON(w, http.StatusAccepted, Proposed{PullRequest: pr}) // this operation's own pull request
				return
			}
		}
		if len(prs) > 0 {
			writeErr(w, http.StatusConflict, "an open pull request already adds a model named "+in.Name+": "+prs[0].URL)
			return
		}
	}
	// Every CRD rule applies exactly as on create; nothing is written before this passes.
	if err := c.Client.Create(ctx, m.DeepCopy(), client.DryRunAll); err != nil {
		if apierrors.IsAlreadyExists(err) {
			writeErr(w, http.StatusConflict, "a model named "+in.Name+" already exists; choose another name")
		} else {
			apiErrRedacted(w, err, key)
		}
		return
	}

	var created *corev1.Secret // set only when this request created the Secret
	if in.Credentials == "entered" {
		var status int
		var err error
		if created, status, err = c.ensureModelSecret(ctx, s, in); err != nil {
			slog.Error("add model: key secret", "model", in.Name, "operation", in.Operation, "err", redact(err.Error(), key))
			writeErr(w, status, redact(err.Error(), key))
			return
		}
	}
	if c.GitOps != nil {
		c.proposeModel(w, r, s, in, m)
		return
	}
	if err := c.Client.Create(ctx, m); err != nil {
		c.dropSecret(created, in)
		slog.Error("add model", "model", in.Name, "operation", in.Operation, "err", redact(err.Error(), key))
		if apierrors.IsAlreadyExists(err) {
			writeErr(w, http.StatusConflict, "a model named "+in.Name+" already exists; choose another name")
		} else {
			apiErrRedacted(w, err, key)
		}
		return
	}
	logModelAdded(in, s, "direct")
	writeJSON(w, http.StatusCreated, modelView(*m))
}

// apiErrRedacted is apiErr for a request that carries a key: the message is cleaned before it is shown.
func apiErrRedacted(w http.ResponseWriter, err error, key string) {
	switch {
	case apierrors.IsInvalid(err) || apierrors.IsBadRequest(err):
		writeErr(w, http.StatusBadRequest, redact(strings.TrimSpace(err.Error()), key))
	case apierrors.IsForbidden(err):
		writeErr(w, http.StatusForbidden, "the console is not allowed to do that in Kubernetes: "+redact(err.Error(), key))
	default:
		slog.Error("add model", "err", redact(err.Error(), key))
		writeErr(w, http.StatusInternalServerError, "the console could not reach Kubernetes; try again")
	}
}

func logModelAdded(in ModelAddInput, s Session, mode string) {
	slog.Info("model added", "model", in.Name, "provider", in.Provider, "modelID", in.Model, "credentials", in.Credentials, "mode", mode, "by", s.Email)
}

func modelView(m v1.Model) ModelView {
	return ModelView{Name: m.Name, DisplayName: m.Spec.DisplayName, Model: m.Spec.Model, Provider: m.Spec.Provider,
		AllowedGroups: nonNil(m.Status.AllowedGroups), Ready: "Unknown", Price: m.Spec.Price}
}

// dropSecret deletes the Secret this request created, and only that exact one (UID precondition).
func (c *Catalog) dropSecret(sec *corev1.Secret, in ModelAddInput) {
	if sec == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 10*time.Second)
	defer cancel()
	if err := c.Client.Delete(ctx, sec, client.Preconditions{UID: ptrUID(sec.UID)}); err != nil && !apierrors.IsNotFound(err) {
		slog.Error("add model: the key secret was not removed; it is listed under Unfinished additions", "secret", sec.Name, "operation", in.Operation, "err", redact(err.Error(), in.APIKey))
	}
}

// openModelPRs lists the open console pull requests that add a Model of this name.
func (c *Catalog) openModelPRs(ctx context.Context, in ModelAddInput) ([]PullRequest, error) {
	prs, err := c.GitOps.Repo.OpenPullRequests(ctx)
	if err != nil {
		return nil, err
	}
	head := branchPrefix + "add-model-" + in.Name + "-"
	var out []PullRequest
	for _, pr := range prs {
		if rest, ok := strings.CutPrefix(pr.Branch, head); ok && branchOpRE.MatchString(rest) {
			out = append(out, pr)
		}
	}
	return out, nil
}

// ensureModelSecret creates Secret ambit-llm-<name>. It never overwrites: a Secret that is not this operation's
// is refused (409). created is nil when the same operation already made it (a retry).
func (c *Catalog) ensureModelSecret(ctx context.Context, s Session, in ModelAddInput) (created *corev1.Secret, status int, err error) {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: llmSecretPrefix + in.Name, Namespace: c.Namespace,
		Labels:      map[string]string{"app.kubernetes.io/managed-by": "ambit-console", modelSecretLabel: in.Name},
		Annotations: map[string]string{createdByKey: s.Email, operationKey: in.Operation, branchKey: modelBranch(in), modelInputKey: in.storedInput()}},
		Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"Authorization": []byte(in.APIKey)}}
	err = c.Client.Create(ctx, sec)
	if err == nil {
		return sec, 0, nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return nil, http.StatusBadGateway, fmt.Errorf("could not store the key in Secret %s: %w", sec.Name, err)
	}
	var cur corev1.Secret
	if gerr := c.Client.Get(ctx, client.ObjectKeyFromObject(sec), &cur); gerr != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("could not read Secret %s: %w", sec.Name, gerr)
	}
	// The same operation, same input, same key: a retry. The comparison stays inside this process.
	if cur.Annotations[operationKey] == in.Operation && cur.Annotations[modelInputKey] == in.storedInput() &&
		subtle.ConstantTimeCompare(cur.Data["Authorization"], []byte(in.APIKey)) == 1 {
		return nil, 0, nil
	}
	return nil, http.StatusConflict, fmt.Errorf("a Secret named %s already exists; choose Use an existing Secret, or another name", sec.Name)
}

// proposeModel opens the pull request with the Model manifest only. The key is not in it.
func (c *Catalog) proposeModel(w http.ResponseWriter, r *http.Request, s Session, in ModelAddInput, m *v1.Model) {
	ctx := r.Context()
	manifest, err := Manifest("Model", m.Name, nil, m.Annotations, m.Spec)
	if err != nil {
		serverErr(w, err)
		return
	}
	objs := []ProposedObject{{Kind: "Model", Plural: "models", Name: m.Name, Manifest: manifest}}
	title := "Ambit: add model " + in.Name
	branch := modelBranch(in)
	sec, _ := c.modelSecret(ctx, llmSecretPrefix+in.Name)
	if sec != nil && sec.Annotations[operationKey] == in.Operation && sec.Annotations[branchKey] != "" {
		branch = sec.Annotations[branchKey] // a closed pull request moved the operation to a new branch
	}
	pr, err := c.GitOps.ProposeObjects(ctx, branch, title, s.Email, objs)
	if errors.Is(err, ErrPullRequestClosed) {
		branch = fmt.Sprintf("%s-r%d", modelBranch(in), time.Now().Unix())
		if sec != nil && sec.Annotations[operationKey] == in.Operation {
			sec.Annotations[branchKey] = branch
			err = c.Client.Update(ctx, sec)
		} else {
			err = nil
		}
		if err == nil {
			pr, err = c.GitOps.ProposeObjects(ctx, branch, title, s.Email, objs)
		}
	}
	var invalid *InvalidChange
	if errors.As(err, &invalid) {
		writeErr(w, http.StatusBadRequest, redact(invalid.Error(), in.APIKey))
		return
	}
	if err != nil {
		slog.Error("add model: pull request", "model", in.Name, "operation", in.Operation, "err", redact(err.Error(), in.APIKey))
		writeErr(w, http.StatusBadGateway, "could not open the pull request; retry, or remove the key under Unfinished additions: "+redact(err.Error(), in.APIKey))
		return
	}
	logModelAdded(in, s, "gitops")
	writeJSON(w, http.StatusAccepted, Proposed{PullRequest: pr})
}

func (c *Catalog) modelSecret(ctx context.Context, name string) (*corev1.Secret, error) {
	var sec corev1.Secret
	if err := c.Client.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: name}, &sec); err != nil {
		return nil, err
	}
	return &sec, nil
}

// secretNames answers GET /api/model-secrets: the names of Secrets ambit-llm-*, from metadata only.
func (c *Catalog) secretNames(w http.ResponseWriter, r *http.Request, _ Session) {
	var l metav1.PartialObjectMetadataList
	l.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("SecretList"))
	if err := c.Client.List(r.Context(), &l, client.InNamespace(c.Namespace)); err != nil {
		serverErr(w, err)
		return
	}
	out := []string{}
	for _, it := range l.Items {
		if strings.HasPrefix(it.Name, llmSecretPrefix) {
			out = append(out, it.Name)
		}
	}
	slices.Sort(out)
	writeJSON(w, http.StatusOK, out)
}

// unfinishedModels lists key Secrets the console made whose Model is absent; nothing is removed automatically.
func (c *Catalog) unfinishedModels(w http.ResponseWriter, r *http.Request, _ Session) {
	ctx := r.Context()
	var secs corev1.SecretList
	if err := c.Client.List(ctx, &secs, client.InNamespace(c.Namespace), client.HasLabels{modelSecretLabel}); err != nil {
		serverErr(w, err)
		return
	}
	out := []UnfinishedModel{}
	for _, sec := range secs.Items {
		op, name := sec.Annotations[operationKey], sec.Labels[modelSecretLabel]
		if op == "" || c.GitOps == nil && time.Since(sec.CreationTimestamp.Time) < 10*time.Second {
			continue // an addition still in progress: its request is creating the Model now
		}
		var m v1.Model
		err := c.Client.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: name}, &m)
		if err == nil && m.Annotations[operationKey] == op {
			continue // complete
		}
		if err != nil && !apierrors.IsNotFound(err) {
			serverErr(w, err)
			return
		}
		u := UnfinishedModel{Secret: sec.Name, Model: name, Operation: op}
		if c.GitOps != nil {
			// ponytail: one GitHub lookup per addition not yet in the cluster, on every listing.
			st, found, err := c.GitOps.Repo.PullRequestByBranch(ctx, sec.Annotations[branchKey])
			if err != nil {
				writeErr(w, http.StatusBadGateway, "could not list unfinished additions: "+err.Error())
				return
			}
			if found && !st.Closed {
				continue // open, or merged and waiting for the sync
			}
			if found {
				u.PullRequest = &st
			}
		}
		out = append(out, u)
	}
	writeJSON(w, http.StatusOK, out)
}

// removeModelKey deletes one Secret, only if the console made it for an addition that did not finish and no Model uses it.
func (c *Catalog) removeModelKey(w http.ResponseWriter, r *http.Request, s Session) {
	ctx := r.Context()
	sec, err := c.modelSecret(ctx, r.PathValue("secret"))
	if apierrors.IsNotFound(err) {
		writeErr(w, http.StatusNotFound, "no Secret named "+r.PathValue("secret"))
		return
	} else if err != nil {
		serverErr(w, err)
		return
	}
	if sec.Labels[modelSecretLabel] == "" || sec.Annotations[operationKey] == "" || sec.Labels["app.kubernetes.io/managed-by"] != "ambit-console" {
		writeErr(w, http.StatusConflict, "the console did not create this Secret for a model; it will not remove it")
		return
	}
	var ms v1.ModelList
	if err := c.Client.List(ctx, &ms, client.InNamespace(c.Namespace)); err != nil {
		serverErr(w, err)
		return
	}
	for _, m := range ms.Items {
		if m.Spec.CredentialsRef != nil && m.Spec.CredentialsRef.Name == sec.Name {
			writeErr(w, http.StatusConflict, "the model "+m.Name+" uses this Secret")
			return
		}
	}
	if c.GitOps != nil && sec.Annotations[branchKey] != "" {
		st, found, err := c.GitOps.Repo.PullRequestByBranch(ctx, sec.Annotations[branchKey])
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		if found && !st.Closed {
			writeErr(w, http.StatusConflict, "its pull request is open or merged; close it first")
			return
		}
		if err := c.GitOps.Repo.DeleteBranch(ctx, sec.Annotations[branchKey]); err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	if err := c.Client.Delete(ctx, sec, client.Preconditions{UID: ptrUID(sec.UID)}); err != nil && !apierrors.IsNotFound(err) {
		apiErr(w, err)
		return
	}
	slog.Info("model key removed", "secret", sec.Name, "by", s.Email)
	w.WriteHeader(http.StatusNoContent)
}
