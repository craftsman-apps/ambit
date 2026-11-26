package oauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// RotationConfigMap holds the time of the last emergency rotation of the signing keys (key "since", RFC 3339). The console writes it
// before it replaces the keys; the token hook refuses the refresh of every grant that is older (Hydra v26.2.0 gives a refreshed access
// token the key ID of the grant's first token, which no longer exists after a rotation: docs/oauth.md, "Signing keys").
const RotationConfigMap = "ambit-oauth-rotation"

// tempKeySet is the prefix of the set in which a new key is made before it replaces the signing key (never published: not in broadcast_keys).
// Every attempt has a set of its own, so that two attempts (a console that restarts while one runs) never share one.
const tempKeySet = "ambit.next"

// KeySet is the keys of a Hydra key set as Hydra holds them: PRIVATE keys. Never log them or send them anywhere.
func (h *HydraAdmin) KeySet(ctx context.Context, set string) ([]json.RawMessage, error) {
	var s keySet
	if _, err := h.do(ctx, "GET", "/admin/keys/"+set, nil, &s); err != nil {
		return nil, err
	}
	return s.Keys, nil
}

// CreateKeySet makes a set with one new RS256 key and returns the key.
func (h *HydraAdmin) CreateKeySet(ctx context.Context, set string) (json.RawMessage, error) {
	if _, err := h.do(ctx, "POST", "/admin/keys/"+set, map[string]string{"alg": "RS256", "use": "sig"}, nil); err != nil {
		return nil, err
	}
	keys, err := h.KeySet(ctx, set)
	if err != nil {
		return nil, err
	}
	if len(keys) != 1 {
		return nil, fmt.Errorf("the new key set %s holds %d keys", set, len(keys))
	}
	return keys[0], nil
}

// ReplaceKeySet makes the set hold exactly these keys, in one call (the set is never seen with two keys).
func (h *HydraAdmin) ReplaceKeySet(ctx context.Context, set string, keys []json.RawMessage) error {
	_, err := h.do(ctx, "PUT", "/admin/keys/"+set, keySet{Keys: keys}, nil)
	return err
}

// DeleteKeySet removes a set; a missing set is not an error.
func (h *HydraAdmin) DeleteKeySet(ctx context.Context, set string) error {
	_, err := h.do(ctx, "DELETE", "/admin/keys/"+set, nil, nil)
	if errors.Is(err, ErrUnknown) {
		return nil
	}
	return err
}

// KeyID is the "kid" of a JSON Web Key.
func KeyID(k json.RawMessage) string {
	var v struct {
		KID string `json:"kid"`
	}
	_ = json.Unmarshal(k, &v)
	return v.KID
}

// RotatedSet is one signing set after a rotation: the key that left and the key that signs now.
type RotatedSet struct {
	Set    string `json:"set"`
	OldKID string `json:"oldKid"`
	NewKID string `json:"newKid"`
}

// Rotation is the result of an emergency rotation. The probe tokens carry no agent, grant or scope, so no route accepts them: they
// only show which keys a gateway knows (docs/oauth.md, "If a signing key may be stolen"): signed by a key the gateway knows, the answer
// is 403 (the token is valid, the rules refuse it); signed by an unknown key, it is 401.
type Rotation struct {
	At    time.Time    `json:"rotatedAt"`
	Sets  []RotatedSet `json:"sets"`
	Probe struct {
		Old string `json:"old"` // signed by the key that left
		New string `json:"new"` // signed by the key that signs now
	} `json:"probe"`
}

// ProbeClaims says what the probe tokens name: the issuer and the audience of the public route.
type ProbeClaims struct{ Issuer, Audience string }

// RotateSigningKeys replaces the key of every signing set by a new key, set by set: the new key is made in a temporary set, the signing
// set is replaced in one call, the temporary set is removed, and the result is read back. A set that does not hold exactly one key is
// not touched (fix that first). On an error some sets may be rotated and some not; run it again.
func RotateSigningKeys(ctx context.Context, h *HydraAdmin, pc ProbeClaims, now time.Time) (Rotation, error) {
	rot := Rotation{At: now.UTC()}
	var oldAccess, newAccess json.RawMessage
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	temp := tempKeySet + "-" + hex.EncodeToString(rnd[:])
	for _, set := range SigningSets {
		old, err := h.KeySet(ctx, set)
		if err != nil {
			return rot, fmt.Errorf("read key set %s: %w", set, err)
		}
		if len(old) != 1 {
			return rot, fmt.Errorf("key set %s holds %d keys, it must hold exactly 1 (docs/oauth.md, \"Signing keys\")", set, len(old))
		}
		if err := h.DeleteKeySet(ctx, temp); err != nil { // a leftover of an earlier run
			return rot, fmt.Errorf("remove the temporary key set: %w", err)
		}
		fresh, err := h.CreateKeySet(ctx, temp)
		if err != nil {
			return rot, fmt.Errorf("make the new key for %s: %w", set, err)
		}
		if err := h.ReplaceKeySet(ctx, set, []json.RawMessage{fresh}); err != nil {
			_ = h.DeleteKeySet(ctx, temp)
			return rot, fmt.Errorf("replace key set %s: %w", set, err)
		}
		if err := h.DeleteKeySet(ctx, temp); err != nil {
			return rot, fmt.Errorf("remove the temporary key set: %w", err)
		}
		now, err := h.KeySet(ctx, set)
		if err != nil || len(now) != 1 || KeyID(now[0]) != KeyID(fresh) {
			return rot, fmt.Errorf("key set %s does not hold the new key after the replacement", set)
		}
		rot.Sets = append(rot.Sets, RotatedSet{Set: set, OldKID: KeyID(old[0]), NewKID: KeyID(fresh)})
		if set == SigningSets[0] {
			oldAccess, newAccess = old[0], fresh
		}
	}
	var err error
	if rot.Probe.Old, err = probeToken(oldAccess, pc, rot.At); err != nil {
		return rot, err
	}
	if rot.Probe.New, err = probeToken(newAccess, pc, rot.At); err != nil {
		return rot, err
	}
	return rot, nil
}

// probeToken signs a token that names the issuer and audience and nothing else, valid for an hour.
func probeToken(key json.RawMessage, pc ProbeClaims, now time.Time) (string, error) {
	var jwk jose.JSONWebKey
	if err := jwk.UnmarshalJSON(key); err != nil || jwk.IsPublic() {
		return "", errors.New("the signing key cannot be read as a private key")
	}
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jwk}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", jwk.KeyID))
	if err != nil {
		return "", errors.New("the signing key cannot sign")
	}
	tok, err := jwt.Signed(sig).Claims(jwt.Claims{Issuer: pc.Issuer, Subject: "ambit-rotation-probe", Audience: jwt.Audience{pc.Audience},
		IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(time.Hour))}).Serialize()
	if err != nil {
		return "", errors.New("the probe token cannot be signed")
	}
	return tok, nil
}
