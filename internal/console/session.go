package console

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

const (
	SessionCookie  = "ambit_session"
	txCookie       = "ambit_login"
	BootstrapEmail = "admin@ambit.local"

	RoleAdmin    = "admin"
	RoleApprover = "approver"
	RoleViewer   = "viewer"
)

// Session is what the console remembers about a signed-in person. It lives in a signed cookie.
type Session struct {
	ID        string   `json:"id"`
	Sub       string   `json:"sub"`
	Email     string   `json:"email"`
	Name      string   `json:"name"`
	Groups    []string `json:"groups,omitempty"`
	Role      string   `json:"role"`
	Bootstrap bool     `json:"bootstrap"`
	Exp       int64    `json:"exp"`
	SignedIn  int64    `json:"signedIn"` // unix seconds of the last verified sign-in (set at sign-in)
}

// Codec signs values with HMAC-SHA256. Values are readable by the holder but cannot be changed.
type Codec struct{ Key []byte }

var errBadToken = errors.New("invalid signed value")

func (c Codec) Encode(v any) string {
	b, _ := json.Marshal(v)
	body := base64.RawURLEncoding.EncodeToString(b)
	return body + "." + base64.RawURLEncoding.EncodeToString(c.mac(body))
}

func (c Codec) Decode(s string, v any) error {
	body, sig, ok := strings.Cut(s, ".")
	if !ok || body == "" {
		return errBadToken
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, c.mac(body)) {
		return errBadToken
	}
	b, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return errBadToken
	}
	return json.Unmarshal(b, v)
}

func (c Codec) mac(body string) []byte {
	m := hmac.New(sha256.New, c.Key)
	m.Write([]byte(body))
	return m.Sum(nil)
}

// roleFor maps an identity to a console role. Only the bootstrap user of Dex's local
// connector is an admin without a group; the same email arriving through SSO is not.
func roleFor(connector, email string, groups []string) string {
	switch {
	case connector == "local" && email == BootstrapEmail:
		return RoleAdmin
	case slices.Contains(groups, "ambit-admins"):
		return RoleAdmin
	case slices.Contains(groups, "ambit-approvers"):
		return RoleApprover
	default:
		return RoleViewer
	}
}
