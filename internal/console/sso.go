package console

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// Connect SSO (decision C, option A): connectors always live in Helm values. The console checks the
// identity provider, keeps the client secret in a Kubernetes Secret, and writes the values to add.
// It never changes Dex itself, so Git (or helm upgrade) stays the one source of sign-in settings.

var connectorIDRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,28}[a-z0-9])?$`)

type IssuerCheck struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorizationEndpoint"`
	TokenEndpoint         string   `json:"tokenEndpoint"`
	GroupsClaim           bool     `json:"groupsClaim"` // the provider advertises a "groups" claim or scope
	RedirectURI           string   `json:"redirectURI"` // register this at the identity provider
	ScopesSupported       []string `json:"scopesSupported"`
}

type SSOInput struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Issuer       string `json:"issuer"`
	ClientID     string `json:"clientID"`
	ClientSecret string `json:"clientSecret"`
	// Groups asks the provider for group membership, which Ambit's roles and access rules use.
	Groups bool `json:"groups"`
}

type SSOResult struct {
	SecretName  string `json:"secretName"`
	RedirectURI string `json:"redirectURI"`
	// Values to add to the Ambit chart values (sso.connectors and dex.envVars).
	Values string `json:"values"`
}

func (c *Catalog) redirectURI() string { return strings.TrimSuffix(c.PublicURL, "/") + "/dex/callback" }

// checkIssuer reads the provider's discovery document the way Dex will, and fails with a reason an
// admin can act on. OpenID Connect requires the document's issuer to equal the URL exactly.
func (c *Catalog) checkIssuer(r *http.Request, issuer string) (IssuerCheck, error) {
	u, err := url.Parse(issuer)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return IssuerCheck{}, fmt.Errorf("the issuer must be a URL like https://login.example.com/realms/acme")
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, strings.TrimSuffix(issuer, "/")+"/.well-known/openid-configuration", nil)
	resp, err := client.Do(req)
	if err != nil {
		return IssuerCheck{}, fmt.Errorf("the console could not reach %s: %v", issuer, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return IssuerCheck{}, fmt.Errorf("%s has no OpenID Connect discovery document (HTTP %d); check the issuer URL", issuer, resp.StatusCode)
	}
	var doc struct {
		Issuer                string   `json:"issuer"`
		AuthorizationEndpoint string   `json:"authorization_endpoint"`
		TokenEndpoint         string   `json:"token_endpoint"`
		ScopesSupported       []string `json:"scopes_supported"`
		ClaimsSupported       []string `json:"claims_supported"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return IssuerCheck{}, fmt.Errorf("%s returned a discovery document that is not valid JSON", issuer)
	}
	if doc.Issuer != issuer {
		return IssuerCheck{}, fmt.Errorf("the provider calls itself %q, not %q; use exactly its value (a trailing slash counts)", doc.Issuer, issuer)
	}
	return IssuerCheck{Issuer: doc.Issuer, AuthorizationEndpoint: doc.AuthorizationEndpoint, TokenEndpoint: doc.TokenEndpoint,
		GroupsClaim: slices.Contains(doc.ClaimsSupported, "groups") || slices.Contains(doc.ScopesSupported, "groups"),
		RedirectURI: c.redirectURI(), ScopesSupported: nonNil(doc.ScopesSupported)}, nil
}

func (c *Catalog) checkSSO(w http.ResponseWriter, r *http.Request, _ Session) {
	var in struct {
		Issuer string `json:"issuer"`
	}
	if !decodeInto(w, r, &in) {
		return
	}
	res, err := c.checkIssuer(r, in.Issuer)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func envName(id string) string {
	return "SSO_" + strings.ToUpper(strings.ReplaceAll(id, "-", "_")) + "_CLIENT_SECRET"
}

// connectSSO stores the client secret and returns the values to add. It is the only place the secret
// passes through the console, and it is never written back in a response or a log line.
func (c *Catalog) connectSSO(w http.ResponseWriter, r *http.Request, s Session) {
	var in SSOInput
	if !decodeInto(w, r, &in) {
		return
	}
	switch {
	case !connectorIDRE.MatchString(in.ID):
		writeErr(w, http.StatusBadRequest, "the ID must be lowercase letters, digits and dashes, at most 30 characters")
		return
	case in.ID == "local":
		writeErr(w, http.StatusBadRequest, `"local" is the bootstrap admin's sign-in; choose another ID`)
		return
	case slices.ContainsFunc(c.Connectors, func(x Connector) bool { return x.ID == in.ID }):
		writeErr(w, http.StatusConflict, "a sign-in option with the ID "+in.ID+" is already configured")
		return
	case in.ClientID == "" || in.ClientSecret == "" || strings.TrimSpace(in.Name) == "":
		writeErr(w, http.StatusBadRequest, "name, client ID and client secret are all required")
		return
	}
	idp, err := c.checkIssuer(r, in.Issuer)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	secretName := "ambit-sso-" + in.ID
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: c.Namespace,
			Labels:      map[string]string{"app.kubernetes.io/managed-by": "ambit-console", "ambitproject.dev/sso-connector": in.ID},
			Annotations: map[string]string{createdByKey: s.Email}},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{"clientSecret": in.ClientSecret},
	}
	if err := c.Client.Create(r.Context(), secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// ponytail: create-only, so the console's Role needs no update on Secrets it cannot name
			// in advance; rotating a secret is a kubectl step, shown here.
			writeErr(w, http.StatusConflict, fmt.Sprintf("the Secret %s already exists. To replace the client secret: kubectl -n %s delete secret %s, then save again", secretName, c.Namespace, secretName))
			return
		}
		apiErr(w, err)
		return
	}
	scopes := []string{"profile", "email"}
	// Ask for a "groups" scope only where the provider has one (Okta does). Others, such as Keycloak,
	// add groups through a claim mapper and refuse an unknown scope, which would break sign-in.
	if in.Groups && slices.Contains(idp.ScopesSupported, "groups") {
		scopes = append(scopes, "groups")
	}
	cfg := map[string]any{"issuer": in.Issuer, "clientID": in.ClientID, "clientSecret": "$" + envName(in.ID),
		"redirectURI": c.redirectURI(), "scopes": scopes}
	if in.Groups {
		cfg["insecureEnableGroups"] = true // Dex's name for "read the groups claim"; it is safe with a trusted issuer
	}
	values, err := yaml.Marshal(map[string]any{
		"sso": map[string]any{"connectors": []any{map[string]any{"type": "oidc", "id": in.ID, "name": in.Name, "config": cfg}}},
		"dex": map[string]any{"envVars": []any{map[string]any{"name": envName(in.ID),
			"valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": secretName, "key": "clientSecret"}}}}},
	})
	if err != nil {
		serverErr(w, err)
		return
	}
	slog.Info("SSO connector prepared", "id", in.ID, "issuer", in.Issuer, "secret", secretName, "by", s.Email)
	writeJSON(w, http.StatusCreated, SSOResult{SecretName: secretName, RedirectURI: c.redirectURI(), Values: string(values)})
}
