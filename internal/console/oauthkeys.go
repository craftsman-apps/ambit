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

var rotating sync.Mutex // one rotation at a time (one console replica; a second console would read the list that the first wrote)

type signingKeyView struct {
	Set  string   `json:"set"`
	KIDs []string `json:"kids"`
}

func (c *Catalog) registerOAuthKeys(mux *http.ServeMux, allow func(string, func(http.ResponseWriter, *http.Request, Session), ...string) http.HandlerFunc) {
	mux.HandleFunc("GET /api/admin/oauth/signing-key", allow("only admins can see the signing keys", c.getSigningKey, RoleAdmin))
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
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: oauth.RotationConfigMap, Namespace: c.Namespace}, Data: map[string]string{"since": at.UTC().Format(time.RFC3339)}}
	err := c.Client.Create(r.Context(), cm)
	if apierrors.IsAlreadyExists(err) {
		var cur corev1.ConfigMap
		if err = c.Client.Get(r.Context(), client.ObjectKeyFromObject(cm), &cur); err != nil {
			return err
		}
		cur.Data = cm.Data
		return c.Client.Update(r.Context(), &cur)
	}
	return err
}

func (c *Catalog) getSigningKey(w http.ResponseWriter, r *http.Request, _ Session) {
	if c.Hydra == nil {
		writeErr(w, http.StatusNotFound, "OAuth sign-in is not turned on")
		return
	}
	out := []signingKeyView{}
	for _, set := range oauth.SigningSets {
		keys, err := c.Hydra.KeySet(r.Context(), set)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "the OAuth server did not answer")
			return
		}
		v := signingKeyView{Set: set, KIDs: []string{}}
		for _, k := range keys {
			v.KIDs = append(v.KIDs, oauth.KeyID(k)) // the key ID only: the keys themselves are private
		}
		out = append(out, v)
	}
	at, err := c.readRotation(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "the time of the last rotation is unreadable")
		return
	}
	res := map[string]any{"sets": out}
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
	writeJSON(w, http.StatusOK, rot)
}
