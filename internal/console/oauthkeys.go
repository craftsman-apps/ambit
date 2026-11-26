package console

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/craftsman-apps/ambit/internal/oauth"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The signing keys of OAuth sign-in (docs/oauth.md, "If a signing key may be stolen"), EXPERIMENTAL in 0.6.x. There is no scheduled rotation in 0.6.0: an
// admin rotates the keys in an emergency, and every connection of every platform has to be made again. Only with OAuth turned on.

// RotateConfirm is the sentence an admin sends to rotate: the call is not made by a stray request.
const RotateConfirm = "every connection must connect again"

// rotationLabel fences the ConfigMap in the admission policy ambit-keys-writer (by label, which also covers a delete of many objects, and by name).
const rotationLabel = "ambitproject.dev/oauth-rotation"

var rotating sync.Mutex // one rotation at a time (one console replica; a second console would read the list that the first wrote)

func (c *Catalog) registerOAuthKeys(mux *http.ServeMux, adminRead func(func(http.ResponseWriter, *http.Request, Session)) http.HandlerFunc,
	allow func(string, func(http.ResponseWriter, *http.Request, Session), ...string) http.HandlerFunc) {
	mux.HandleFunc("GET /api/admin/oauth/signing-key", adminRead(c.getSigningKey))
	mux.HandleFunc("POST /api/admin/oauth/signing-key/emergency-rotation", allow("only admins can rotate the signing keys", c.rotateSigningKey, RoleAdmin))
}

// readRotation is the time of the last rotation (zero: none).
func (c *Catalog) readRotation(r *http.Request) (time.Time, error) {
	var cm corev1.ConfigMap
	if err := c.Client.Get(r.Context(), client.ObjectKey{Namespace: c.Namespace, Name: oauth.RotationConfigMap}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	if v := cm.Data["since"]; v != "" {
		return time.Parse(time.RFC3339, v)
	}
	return time.Time{}, nil
}

// markRotation writes the time that the token hook reads. It is written BEFORE the keys change: from then on no grant that is older is refreshed.
func (c *Catalog) markRotation(r *http.Request, at time.Time) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: oauth.RotationConfigMap, Namespace: c.Namespace, Labels: map[string]string{rotationLabel: "true"}}, Data: map[string]string{"since": at.UTC().Format(time.RFC3339)}}
	err := c.Client.Create(r.Context(), cm)
	if apierrors.IsAlreadyExists(err) {
		var cur corev1.ConfigMap
		if err = c.Client.Get(r.Context(), client.ObjectKeyFromObject(cm), &cur); err != nil {
			return err
		}
		cur.Data = cm.Data
		if cur.Labels == nil {
			cur.Labels = map[string]string{}
		}
		cur.Labels[rotationLabel] = "true"
		return c.Client.Update(r.Context(), &cur)
	}
	return err
}

// getSigningKey shows the key IDs that Hydra publishes and the time of the last rotation. It reads the PUBLIC key list: the console never reads a private
// key for a status call (only the rotation handles private keys).
func (c *Catalog) getSigningKey(w http.ResponseWriter, r *http.Request, _ Session) {
	if c.Hydra == nil || c.HydraPublic == "" {
		writeErr(w, http.StatusNotFound, "OAuth sign-in is not turned on")
		return
	}
	ids, err := oauth.PublicKeyIDs(r.Context(), nil, c.HydraPublic)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "the OAuth server did not answer")
		return
	}
	at, err := c.readRotation(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "the time of the last rotation is unreadable")
		return
	}
	res := map[string]any{"published": ids}
	if !at.IsZero() {
		res["lastRotation"] = at
	}
	writeJSON(w, http.StatusOK, res)
}

// rotateSigningKey is the emergency rotation. Order: the time first (the hook then refuses every older grant), then the keys, then the
// record. It answers with the old and the new key ID of each set and two probe tokens for the check on every gateway pod.
func (c *Catalog) rotateSigningKey(w http.ResponseWriter, r *http.Request, s Session) {
	if c.Hydra == nil || c.OAuthIssuer == "" || c.OAuthResource == "" {
		writeErr(w, http.StatusNotFound, "OAuth sign-in is not turned on")
		return
	}
	var in struct {
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil || in.Confirm != RotateConfirm {
		writeErr(w, http.StatusBadRequest, `send {"confirm":"`+RotateConfirm+`"}: a rotation ends every OAuth connection of every platform`)
		return
	}
	if !rotating.TryLock() {
		writeErr(w, http.StatusConflict, "a rotation is running")
		return
	}
	defer rotating.Unlock()
	now := time.Now()
	if c.Keys != nil && c.Keys.Now != nil {
		now = c.Keys.Now()
	}
	if err := c.markRotation(r, now); err != nil {
		writeErr(w, http.StatusInternalServerError, "the time of the rotation could not be written: nothing was changed")
		return
	}
	rot, err := oauth.RotateSigningKeys(r.Context(), c.Hydra, oauth.ProbeClaims{Issuer: c.OAuthIssuer, Audience: c.OAuthResource}, now)
	if c.Keys != nil {
		detail := "ok"
		if err != nil {
			detail = "failed"
		}
		c.Keys.event(r.Context(), s.Email, "oauth-signing-key-rotation", "", "", detail)
	}
	if err != nil {
		// The time is written, so every older grant is already refused. Some sets may be rotated: run the call again. No key material is in the error.
		writeErr(w, http.StatusBadGateway, "the rotation did not finish ("+err.Error()+"): run it again; grants made before now are already refused")
		return
	}
	// A second time, after the keys have changed: a grant made between the first time and the replacement has a token of the old key, and is over too.
	if err := c.markRotation(r, time.Now()); err != nil {
		writeErr(w, http.StatusInternalServerError, "the keys are replaced, but the time could not be written again: run the rotation again")
		return
	}
	w.Header().Set("Cache-Control", "no-store") // the answer holds probe tokens that are signed with the real keys
	writeJSON(w, http.StatusOK, rot)
}
