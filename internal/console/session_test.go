package console

import (
	"testing"
	"time"
)

func TestCodecRoundTrip(t *testing.T) {
	c := Codec{Key: []byte("0123456789abcdef0123456789abcdef")}
	in := Session{Email: "user01@acme.com", Name: "User01", Groups: []string{"finance"}, Role: RoleViewer, Exp: time.Now().Add(time.Hour).Unix()}
	var out Session
	if err := c.Decode(c.Encode(in), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Email != in.Email || out.Role != in.Role || len(out.Groups) != 1 {
		t.Fatalf("round trip mismatch: %+v", out)
	}
}

func TestCodecRejectsTamperingAndWrongKey(t *testing.T) {
	c := Codec{Key: []byte("0123456789abcdef0123456789abcdef")}
	tok := c.Encode(Session{Email: "user01@acme.com", Role: RoleViewer})
	var s Session
	cases := map[string]string{
		"empty":         "",
		"no signature":  "eyJlbWFpbCI6ImEifQ",
		"tampered body": "eyJlbWFpbCI6ImV2ZUBhY21lLmNvbSIsInJvbGUiOiJhZG1pbiJ9" + tok[len(tok)-44:],
		"garbage":       "!!!.???",
	}
	for name, v := range cases {
		if err := c.Decode(v, &s); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	other := Codec{Key: []byte("ffffffffffffffffffffffffffffffff")}
	if err := other.Decode(tok, &s); err == nil {
		t.Error("wrong key: expected error")
	}
}

func TestRoleFor(t *testing.T) {
	cases := []struct {
		name, connector, email string
		groups                 []string
		want                   string
	}{
		{"bootstrap admin", "local", BootstrapEmail, nil, RoleAdmin},
		{"other local user is not admin", "local", "user02@acme.com", nil, RoleViewer},
		{"bootstrap email from SSO is not bootstrap", "okta", BootstrapEmail, nil, RoleViewer},
		{"admin group", "okta", "user10@acme.com", []string{"eng", "ambit-admins"}, RoleAdmin},
		{"approver group", "okta", "user11@acme.com", []string{"ambit-approvers"}, RoleApprover},
		{"admin wins over approver", "okta", "user11@acme.com", []string{"ambit-approvers", "ambit-admins"}, RoleAdmin},
		{"no groups", "okta", "user01@acme.com", nil, RoleViewer},
	}
	for _, c := range cases {
		if got := roleFor(c.connector, c.email, c.groups); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}
