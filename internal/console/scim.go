package console

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SCIM 2.0 (RFC 7643, 7644) receiver for offboarding (journey A4). The identity provider pushes users;
// when one is deactivated or deleted, Ambit revokes every SaaS grant they authorised at its provider,
// ends their console sessions, blocks their still-valid gateway tokens, and keeps a record as proof.
//
// Only what Okta and Entra ID send is implemented: create, get, filter by userName, replace (PUT),
// PATCH of active, delete, and the discovery documents. Groups are not provisioned here; roles and
// access come from the groups claim in each sign-in.

const (
	scimUserSchema = "urn:ietf:params:scim:schemas:core:2.0:User"
	scimListSchema = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	scimErrSchema  = "urn:ietf:params:scim:api:messages:2.0:Error"
	scimPatchOp    = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	scimLabel      = "ambitproject.dev/scim-user"
	recordLabel    = "ambitproject.dev/offboarding"
	// OffboardedConfigMap lists deactivated users' emails; the controller turns it into a gateway Deny rule.
	OffboardedConfigMap = "ambit-offboarded"
)

type SCIM struct {
	Client    client.Client // users and records in Store.Namespace; the offboarded list in Namespace
	Namespace string
	Token     string // bearer token the identity provider presents
	Conns     *Connections
	Sessions  *SessionStore
	Now       func() time.Time
}

// SCIMUser is what Ambit keeps of a provisioned user.
type SCIMUser struct {
	ID         string    `json:"id"`
	UserName   string    `json:"userName"`
	Email      string    `json:"email"`
	ExternalID string    `json:"externalId,omitempty"`
	Active     bool      `json:"active"`
	Updated    time.Time `json:"updated"`
}

// GrantResult is one line of offboarding evidence.
type GrantResult struct {
	Connection        string `json:"connection"`
	RevokedAtProvider bool   `json:"revokedAtProvider"`
	Error             string `json:"error,omitempty"`
}

// OffboardingRecord proves what happened when a user was deactivated.
type OffboardingRecord struct {
	Email           string        `json:"email"`
	At              time.Time     `json:"at"`
	Trigger         string        `json:"trigger"` // "scim PATCH active=false", "scim DELETE", ...
	Grants          []GrantResult `json:"grants"`
	SessionsEnded   int           `json:"sessionsEnded"`
	GatewayBlocked  bool          `json:"gatewayBlocked"`
	CompleteSuccess bool          `json:"completeSuccess"`
}

func (s *SCIM) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *SCIM) register(mux *http.ServeMux) {
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || s.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
				scimErr(w, http.StatusUnauthorized, "a valid SCIM bearer token is required")
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /scim/v2/ServiceProviderConfig", auth(s.providerConfig))
	mux.HandleFunc("GET /scim/v2/ResourceTypes", auth(s.resourceTypes))
	mux.HandleFunc("GET /scim/v2/Schemas", auth(func(w http.ResponseWriter, _ *http.Request) {
		scimJSON(w, http.StatusOK, map[string]any{"schemas": []string{scimListSchema}, "totalResults": 0, "Resources": []any{}})
	}))
	mux.HandleFunc("GET /scim/v2/Users", auth(s.list))
	mux.HandleFunc("POST /scim/v2/Users", auth(s.create))
	mux.HandleFunc("GET /scim/v2/Users/{id}", auth(s.get))
	mux.HandleFunc("PUT /scim/v2/Users/{id}", auth(s.replace))
	mux.HandleFunc("PATCH /scim/v2/Users/{id}", auth(s.patch))
	mux.HandleFunc("DELETE /scim/v2/Users/{id}", auth(s.remove))
}

func scimJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/scim+json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func scimErr(w http.ResponseWriter, code int, detail string) {
	scimJSON(w, code, map[string]any{"schemas": []string{scimErrSchema}, "status": fmt.Sprint(code), "detail": detail})
}

func (s *SCIM) providerConfig(w http.ResponseWriter, _ *http.Request) {
	scimJSON(w, http.StatusOK, map[string]any{
		"schemas":        []string{"urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"},
		"patch":          map[string]bool{"supported": true},
		"bulk":           map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":         map[string]any{"supported": true, "maxResults": 200},
		"changePassword": map[string]bool{"supported": false},
		"sort":           map[string]bool{"supported": false},
		"etag":           map[string]bool{"supported": false},
		"authenticationSchemes": []map[string]any{{"type": "oauthbearertoken", "name": "Bearer token",
			"description": "The token shown on the console's Settings / Offboarding page"}},
	})
}

func (s *SCIM) resourceTypes(w http.ResponseWriter, _ *http.Request) {
	scimJSON(w, http.StatusOK, map[string]any{"schemas": []string{scimListSchema}, "totalResults": 1, "Resources": []any{map[string]any{
		"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:ResourceType"}, "id": "User", "name": "User",
		"endpoint": "/Users", "schema": scimUserSchema}}})
}

func (s *SCIM) resource(u *SCIMUser) map[string]any {
	return map[string]any{"schemas": []string{scimUserSchema}, "id": u.ID, "userName": u.UserName, "externalId": u.ExternalID,
		"active": u.Active, "emails": []any{map[string]any{"value": u.Email, "primary": true, "type": "work"}},
		"meta": map[string]any{"resourceType": "User", "lastModified": u.Updated.UTC().Format(time.RFC3339), "location": "/scim/v2/Users/" + u.ID}}
}

// --- storage: one Secret per user in the grants namespace (emails are personal data) ---

func (s *SCIM) store() *GrantStore { return s.Conns.Store }

func (s *SCIM) load(ctx context.Context, id string) (*SCIMUser, error) {
	var sec corev1.Secret
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.store().Namespace, Name: "scim-" + id}, &sec); err != nil {
		return nil, err
	}
	plain, err := s.store().open(sec.Data["user"])
	if err != nil {
		return nil, err
	}
	var u SCIMUser
	return &u, json.Unmarshal(plain, &u)
}

func (s *SCIM) save(ctx context.Context, u *SCIMUser) error {
	u.Updated = s.now()
	plain, _ := json.Marshal(u)
	sealed, err := s.store().seal(plain)
	if err != nil {
		return err
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "scim-" + u.ID, Namespace: s.store().Namespace,
		Labels: map[string]string{scimLabel: "true", grantUserLabel: userHash(u.Email)}}, Data: map[string][]byte{"user": sealed}}
	err = s.Client.Create(ctx, sec)
	if apierrors.IsAlreadyExists(err) {
		var cur corev1.Secret
		if err := s.Client.Get(ctx, client.ObjectKeyFromObject(sec), &cur); err != nil {
			return err
		}
		cur.Data, cur.Labels = sec.Data, sec.Labels
		return s.Client.Update(ctx, &cur)
	}
	return err
}

func (s *SCIM) all(ctx context.Context) ([]*SCIMUser, error) {
	var l corev1.SecretList
	if err := s.Client.List(ctx, &l, client.InNamespace(s.store().Namespace), client.MatchingLabels{scimLabel: "true"}); err != nil {
		return nil, err
	}
	var out []*SCIMUser
	for i := range l.Items {
		plain, err := s.store().open(l.Items[i].Data["user"])
		if err != nil {
			return nil, err
		}
		var u SCIMUser
		if err := json.Unmarshal(plain, &u); err != nil {
			return nil, err
		}
		out = append(out, &u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserName < out[j].UserName })
	return out, nil
}

// --- request bodies ---

type scimBody struct {
	UserName   string `json:"userName"`
	ExternalID string `json:"externalId"`
	Active     *bool  `json:"active"`
	Emails     []struct {
		Value   string `json:"value"`
		Primary bool   `json:"primary"`
	} `json:"emails"`
}

func (b scimBody) email() string {
	for _, e := range b.Emails {
		if e.Primary {
			return strings.ToLower(e.Value)
		}
	}
	if len(b.Emails) > 0 {
		return strings.ToLower(b.Emails[0].Value)
	}
	if strings.Contains(b.UserName, "@") {
		return strings.ToLower(b.UserName)
	}
	return ""
}

func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(v); err != nil {
		scimErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

// --- handlers ---

var filterRE = regexp.MustCompile(`(?i)^\s*userName\s+eq\s+"([^"]*)"\s*$`)

func (s *SCIM) list(w http.ResponseWriter, r *http.Request) {
	users, err := s.all(r.Context())
	if err != nil {
		scimErr(w, http.StatusInternalServerError, "could not read users")
		return
	}
	if f := r.URL.Query().Get("filter"); f != "" {
		m := filterRE.FindStringSubmatch(f)
		if m == nil {
			scimErr(w, http.StatusBadRequest, `only filter=userName eq "..." is supported`)
			return
		}
		var keep []*SCIMUser
		for _, u := range users {
			if strings.EqualFold(u.UserName, m[1]) {
				keep = append(keep, u)
			}
		}
		users = keep
	}
	res := []any{}
	for _, u := range users {
		res = append(res, s.resource(u))
	}
	scimJSON(w, http.StatusOK, map[string]any{"schemas": []string{scimListSchema}, "totalResults": len(res), "itemsPerPage": len(res), "startIndex": 1, "Resources": res})
}

func (s *SCIM) create(w http.ResponseWriter, r *http.Request) {
	var b scimBody
	if !readBody(w, r, &b) {
		return
	}
	if b.UserName == "" || b.email() == "" {
		scimErr(w, http.StatusBadRequest, "userName and an email are required")
		return
	}
	users, err := s.all(r.Context())
	if err != nil {
		scimErr(w, http.StatusInternalServerError, "could not read users")
		return
	}
	for _, u := range users {
		if strings.EqualFold(u.UserName, b.UserName) {
			scimJSON(w, http.StatusConflict, map[string]any{"schemas": []string{scimErrSchema}, "status": "409", "scimType": "uniqueness", "detail": "userName already exists"})
			return
		}
	}
	u := &SCIMUser{ID: scimID(), UserName: b.UserName, Email: b.email(), ExternalID: b.ExternalID, Active: b.Active == nil || *b.Active}
	if err := s.save(r.Context(), u); err != nil {
		slog.Error("scim: save user", "err", err)
		scimErr(w, http.StatusInternalServerError, "could not save the user")
		return
	}
	slog.Info("scim: user provisioned", "email", u.Email, "active", u.Active)
	if !u.Active {
		s.Offboard(r.Context(), u.Email, "scim POST active=false")
	}
	scimJSON(w, http.StatusCreated, s.resource(u))
}

func (s *SCIM) get(w http.ResponseWriter, r *http.Request) {
	u, err := s.load(r.Context(), r.PathValue("id"))
	if err != nil {
		scimErr(w, http.StatusNotFound, "no such user")
		return
	}
	scimJSON(w, http.StatusOK, s.resource(u))
}

// setActive applies an activation change; deactivation offboards, reactivation lifts the gateway block.
func (s *SCIM) setActive(ctx context.Context, u *SCIMUser, active bool, trigger string) {
	was := u.Active
	u.Active = active
	switch {
	case was && !active:
		s.Offboard(ctx, u.Email, trigger)
	case !was && active:
		if err := s.setBlocked(ctx, u.Email, false); err != nil {
			slog.Error("scim: unblock", "email", u.Email, "err", err)
		}
		slog.Info("scim: user reactivated", "email", u.Email)
	}
}

func (s *SCIM) replace(w http.ResponseWriter, r *http.Request) {
	u, err := s.load(r.Context(), r.PathValue("id"))
	if err != nil {
		scimErr(w, http.StatusNotFound, "no such user")
		return
	}
	var b scimBody
	if !readBody(w, r, &b) {
		return
	}
	if b.UserName != "" {
		u.UserName = b.UserName
	}
	if e := b.email(); e != "" {
		u.Email = e
	}
	u.ExternalID = b.ExternalID
	if b.Active != nil {
		s.setActive(r.Context(), u, *b.Active, "scim PUT active=false")
	}
	if err := s.save(r.Context(), u); err != nil {
		scimErr(w, http.StatusInternalServerError, "could not save the user")
		return
	}
	scimJSON(w, http.StatusOK, s.resource(u))
}

// patch handles {"Operations":[{"op":"replace","path":"active","value":false}]} and the forms Entra ID
// sends ("Replace", "False" as a string, or {"active": false} as the value with no path).
func (s *SCIM) patch(w http.ResponseWriter, r *http.Request) {
	u, err := s.load(r.Context(), r.PathValue("id"))
	if err != nil {
		scimErr(w, http.StatusNotFound, "no such user")
		return
	}
	var body struct {
		Operations []struct {
			Op    string          `json:"op"`
			Path  string          `json:"path"`
			Value json.RawMessage `json:"value"`
		} `json:"Operations"`
	}
	if !readBody(w, r, &body) {
		return
	}
	for _, op := range body.Operations {
		if !strings.EqualFold(op.Op, "replace") && !strings.EqualFold(op.Op, "add") {
			continue
		}
		var active *bool
		if strings.EqualFold(op.Path, "active") {
			active = parseBool(op.Value)
		} else if op.Path == "" {
			var m map[string]json.RawMessage
			if json.Unmarshal(op.Value, &m) == nil {
				if v, ok := m["active"]; ok {
					active = parseBool(v)
				}
			}
		}
		if active != nil {
			s.setActive(r.Context(), u, *active, fmt.Sprintf("scim PATCH active=%t", *active))
		}
	}
	if err := s.save(r.Context(), u); err != nil {
		scimErr(w, http.StatusInternalServerError, "could not save the user")
		return
	}
	scimJSON(w, http.StatusOK, s.resource(u))
}

// scimID is a user id that is also valid in a Kubernetes object name (lowercase hex).
func scimID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func parseBool(raw json.RawMessage) *bool {
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return &b
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		v := strings.EqualFold(str, "true")
		if v || strings.EqualFold(str, "false") {
			return &v
		}
	}
	return nil
}

func (s *SCIM) remove(w http.ResponseWriter, r *http.Request) {
	u, err := s.load(r.Context(), r.PathValue("id"))
	if err != nil {
		scimErr(w, http.StatusNotFound, "no such user")
		return
	}
	if u.Active {
		s.Offboard(r.Context(), u.Email, "scim DELETE")
	}
	_ = s.Client.Delete(r.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: s.store().Namespace, Name: "scim-" + u.ID}})
	w.WriteHeader(http.StatusNoContent)
}

// --- offboarding ---

// Offboard revokes every grant the user authorised at its provider, ends their console sessions,
// blocks their gateway tokens, and writes a record. It logs one line per grant as evidence.
func (s *SCIM) Offboard(ctx context.Context, email, trigger string) OffboardingRecord {
	rec := OffboardingRecord{Email: email, At: s.now(), Trigger: trigger, Grants: []GrantResult{}, CompleteSuccess: true}
	grants, err := s.Conns.Store.List(ctx, email)
	if err != nil {
		slog.Error("offboarding: list grants", "email", email, "err", err)
		rec.CompleteSuccess = false
	}
	for _, g := range grants {
		revoked, err := s.Conns.Revoke(ctx, g)
		res := GrantResult{Connection: g.Connection, RevokedAtProvider: revoked}
		if err != nil {
			res.Error = err.Error()
			rec.CompleteSuccess = false
		}
		rec.Grants = append(rec.Grants, res)
		slog.Info("offboarding: grant", "email", email, "connection", g.Connection, "revokedAtProvider", revoked, "err", res.Error)
	}
	if s.Sessions != nil {
		rec.SessionsEnded = s.Sessions.RevokeUser(email)
	}
	if err := s.setBlocked(ctx, email, true); err != nil {
		slog.Error("offboarding: block gateway tokens", "email", email, "err", err)
		rec.CompleteSuccess = false
	} else {
		rec.GatewayBlocked = true
	}
	s.saveRecord(ctx, rec)
	slog.Info("offboarding: done", "email", email, "trigger", trigger, "grants", len(rec.Grants), "sessionsEnded", rec.SessionsEnded, "complete", rec.CompleteSuccess)
	return rec
}

// setBlocked adds or removes the email in the ConfigMap the controller turns into a gateway Deny rule,
// so tokens the identity provider already issued stop working now rather than when they expire.
func (s *SCIM) setBlocked(ctx context.Context, email string, blocked bool) error {
	var cm corev1.ConfigMap
	err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: OffboardedConfigMap}, &cm)
	create := apierrors.IsNotFound(err)
	if err != nil && !create {
		return err
	}
	if create {
		cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: OffboardedConfigMap, Namespace: s.Namespace,
			Labels: map[string]string{"app.kubernetes.io/managed-by": "ambit-console"}}}
	}
	var emails []string
	_ = json.Unmarshal([]byte(cm.Data["emails"]), &emails)
	keep := emails[:0]
	for _, e := range emails {
		if !strings.EqualFold(e, email) {
			keep = append(keep, e)
		}
	}
	if blocked {
		keep = append(keep, strings.ToLower(email))
	}
	sort.Strings(keep)
	b, _ := json.Marshal(keep)
	cm.Data = map[string]string{"emails": string(b)}
	if create {
		return s.Client.Create(ctx, &cm)
	}
	return s.Client.Update(ctx, &cm)
}

func (s *SCIM) saveRecord(ctx context.Context, rec OffboardingRecord) {
	plain, _ := json.Marshal(rec)
	sealed, err := s.store().seal(plain)
	if err == nil {
		name := fmt.Sprintf("offboarding-%s-%d", userHash(rec.Email)[:12], rec.At.Unix())
		err = s.Client.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.store().Namespace,
			Labels: map[string]string{recordLabel: "true", grantUserLabel: userHash(rec.Email)}}, Data: map[string][]byte{"record": sealed}})
	}
	if err != nil {
		slog.Error("offboarding: save record", "email", rec.Email, "err", err)
	}
}

// Records returns offboarding records, newest first.
func (s *SCIM) Records(ctx context.Context) ([]OffboardingRecord, error) {
	var l corev1.SecretList
	if err := s.Client.List(ctx, &l, client.InNamespace(s.store().Namespace), client.MatchingLabels{recordLabel: "true"}); err != nil {
		return nil, err
	}
	out := []OffboardingRecord{}
	for i := range l.Items {
		plain, err := s.store().open(l.Items[i].Data["record"])
		if err != nil {
			return nil, err
		}
		var rec OffboardingRecord
		if json.Unmarshal(plain, &rec) == nil {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out, nil
}
