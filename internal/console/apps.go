package console

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Apps are Connections as an admin manages them (journey U1 without kubectl): the OAuth app a user
// connects with their own account. The client secret goes into Secret ambit-conn-<name> in the grants
// namespace; the Connection only names it, so Git never holds it.

type AppInput struct {
	Name             string   `json:"name"`
	DisplayName      string   `json:"displayName"`
	AuthorizationURL string   `json:"authorizationURL"`
	TokenURL         string   `json:"tokenURL"`
	RevocationURL    string   `json:"revocationURL"`
	ClientID         string   `json:"clientID"`
	ClientSecret     string   `json:"clientSecret"`
	Scopes           []string `json:"scopes"`
}

type AppView struct {
	Name             string   `json:"name"`
	DisplayName      string   `json:"displayName"`
	AuthorizationURL string   `json:"authorizationURL"`
	TokenURL         string   `json:"tokenURL"`
	RevocationURL    string   `json:"revocationURL"`
	ClientID         string   `json:"clientID"`
	Scopes           []string `json:"scopes"`
	Ready            string   `json:"ready"`
	Message          string   `json:"message"`
	GitManaged       bool     `json:"gitManaged"`
	CreatedBy        string   `json:"createdBy"`
}

var (
	appNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$`) // ambit-conn-<name> must stay a valid name
	scopeRE   = regexp.MustCompile(`^[\x21\x23-\x5B\x5D-\x7E]+$`)          // RFC 6749 scope-token
)

func (c *Catalog) callbackURL() string {
	return strings.TrimSuffix(c.PublicURL, "/") + "/connections/callback"
}

func appView(x v1.Connection) AppView {
	v := AppView{Name: x.Name, DisplayName: x.Spec.DisplayName, AuthorizationURL: x.Spec.AuthorizationURL, TokenURL: x.Spec.TokenURL,
		RevocationURL: x.Spec.RevocationURL, ClientID: x.Spec.ClientID, Scopes: nonNil(x.Spec.Scopes), Ready: "Unknown",
		GitManaged: gitManaged(x.Annotations), CreatedBy: x.Annotations[createdByKey]}
	if cond := meta.FindStatusCondition(x.Status.Conditions, "Ready"); cond != nil {
		v.Ready, v.Message = string(cond.Status), cond.Message
	}
	return v
}

func (c *Catalog) listApps(w http.ResponseWriter, r *http.Request, _ Session) {
	var l v1.ConnectionList
	if err := c.Client.List(r.Context(), &l, client.InNamespace(c.Namespace)); err != nil {
		serverErr(w, err)
		return
	}
	sort.Slice(l.Items, func(i, j int) bool { return l.Items[i].Name < l.Items[j].Name })
	out := []AppView{}
	for _, x := range l.Items {
		out = append(out, appView(x))
	}
	writeJSON(w, http.StatusOK, map[string]any{"callbackURL": c.callbackURL(), "apps": out})
}

func checkApp(in AppInput) error {
	switch {
	case !appNameRE.MatchString(in.Name):
		return errors.New("name must be lowercase letters, digits and dashes, at most 40 characters")
	case strings.TrimSpace(in.DisplayName) == "" || len(in.DisplayName) > 200:
		return errors.New("give the app a display name, as users will see it")
	case in.ClientID == "" || in.ClientSecret == "":
		return errors.New("the client ID and client secret from the app's OAuth settings are both required")
	}
	for label, u := range map[string]string{"authorization URL": in.AuthorizationURL, "token URL": in.TokenURL, "revocation URL": in.RevocationURL} {
		if u == "" && label == "revocation URL" {
			continue
		}
		if !validURL(u) {
			return fmt.Errorf("the %s must look like https://host/path, without a query string or fragment", label)
		}
		if strings.HasPrefix(u, "http://") && !plainHTTPOK(u) {
			return fmt.Errorf("the %s must use https: the client secret and users' tokens travel over it", label)
		}
	}
	for _, sc := range in.Scopes {
		if !scopeRE.MatchString(sc) {
			return fmt.Errorf("scope %q: one scope per entry, without spaces or quotes", sc)
		}
	}
	return nil
}

func (c *Catalog) createApp(w http.ResponseWriter, r *http.Request, s Session) {
	var in AppInput
	if !decodeInto(w, r, &in) {
		return
	}
	if err := checkApp(in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var existing v1.Connection
	if err := c.Client.Get(r.Context(), client.ObjectKey{Namespace: c.Namespace, Name: in.Name}, &existing); err == nil {
		writeErr(w, http.StatusConflict, "an app named "+in.Name+" already exists")
		return
	} else if !apierrors.IsNotFound(err) {
		serverErr(w, err)
		return
	}
	secretName := "ambit-conn-" + in.Name
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: c.Conns.Store.Namespace,
		// Not grantConnLabel: grant listings select on that label and would try to open this as a grant.
		Labels:      map[string]string{"app.kubernetes.io/managed-by": "ambit-console", "ambitproject.dev/app-secret": in.Name},
		Annotations: map[string]string{createdByKey: s.Email}},
		Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"clientSecret": []byte(in.ClientSecret)}}
	// Create-only: an existing secret belongs to an app still waiting in a pull request (or one removed
	// by pull request); replacing it would pair that app's client ID with this secret.
	if err := c.Conns.Store.Client.Create(r.Context(), sec); apierrors.IsAlreadyExists(err) {
		writeErr(w, http.StatusConflict, fmt.Sprintf("the secret for an app named %s already exists (an app waiting in a pull request, or one removed earlier); choose another name, or delete Secret %s in namespace %s", in.Name, secretName, c.Conns.Store.Namespace))
		return
	} else if err != nil {
		apiErr(w, err)
		return
	}
	conn := &v1.Connection{ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: c.Namespace, Annotations: map[string]string{createdByKey: s.Email}},
		Spec: v1.ConnectionSpec{DisplayName: strings.TrimSpace(in.DisplayName), AuthorizationURL: in.AuthorizationURL, TokenURL: in.TokenURL,
			RevocationURL: in.RevocationURL, ClientID: in.ClientID, ClientSecretRef: v1.SecretRef{Name: secretName}, Scopes: in.Scopes}}
	if c.GitOps != nil {
		c.propose(w, r, s, "add", "Connection", "connections", in.Name, conn.Spec, conn.Annotations)
		return
	}
	if err := c.Client.Create(r.Context(), conn); err != nil {
		apiErr(w, err)
		return
	}
	slog.Info("app added", "connection", in.Name, "tokenURL", in.TokenURL, "secret", secretName, "by", s.Email)
	writeJSON(w, http.StatusCreated, appView(*conn))
}

// deleteApp revokes every user's grant for the app at the provider, then removes the Connection and
// its client secret.
func (c *Catalog) deleteApp(w http.ResponseWriter, r *http.Request, s Session) {
	var conn v1.Connection
	if err := c.Client.Get(r.Context(), client.ObjectKey{Namespace: c.Namespace, Name: r.PathValue("name")}, &conn); err != nil {
		if apierrors.IsNotFound(err) {
			writeErr(w, http.StatusNotFound, "no app named "+r.PathValue("name"))
		} else {
			serverErr(w, err)
		}
		return
	}
	if c.GitOps == nil && gitManaged(conn.Annotations) {
		writeErr(w, http.StatusConflict, "this app is managed in Git; remove it with a pull request")
		return
	}
	if c.GitOps != nil {
		// ponytail: after the pull request merges, the app's grants and Secret ambit-conn-<name> stay
		// until someone removes them (Token already refuses a removed app); a controller could clean up.
		c.propose(w, r, s, "delete", "Connection", "connections", conn.Name, nil, nil)
		return
	}
	// Revoke every user's grant at the provider while the app (its URLs and secret) still exists.
	grants, err := c.Conns.Store.List(r.Context(), "")
	if err != nil {
		serverErr(w, err)
		return
	}
	for _, g := range grants {
		if g.Connection != conn.Name {
			continue
		}
		if revoked, err := c.Conns.Revoke(r.Context(), g); err != nil {
			slog.Error("app removal: revoke failed; forgetting the grant anyway", "connection", conn.Name, "err", err)
			if err := c.Conns.Store.Delete(r.Context(), g.Email, g.Connection); err != nil {
				serverErr(w, err)
				return
			}
		} else {
			slog.Info("app removal: grant ended", "connection", conn.Name, "revokedAtProvider", revoked)
		}
	}
	if err := c.Client.Delete(r.Context(), &conn); err != nil && !apierrors.IsNotFound(err) {
		apiErr(w, err)
		return
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: conn.Spec.ClientSecretRef.Name, Namespace: c.Conns.Store.Namespace}}
	if err := c.Conns.Store.Client.Delete(r.Context(), sec); err != nil && !apierrors.IsNotFound(err) {
		slog.Error("app removed but its client secret was not", "secret", sec.Name, "err", err)
	}
	slog.Info("app removed", "connection", conn.Name, "by", s.Email)
	w.WriteHeader(http.StatusNoContent)
}
