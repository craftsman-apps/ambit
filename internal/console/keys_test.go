package console

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNewSecretKey(t *testing.T) {
	plain, prefix, hash, err := NewSecretKey()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^ambit_[0-9A-Za-z]{43}$`).MatchString(plain) || prefix != plain[:10] {
		t.Fatalf("plain %q prefix %q", plain, prefix)
	}
	sum := sha256.Sum256([]byte(plain))
	if hash != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("hash %q", hash)
	}
	other, _, _, _ := NewSecretKey()
	if other == plain {
		t.Fatal("two keys are equal")
	}
}

func TestKeySecretShape(t *testing.T) {
	now := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	k := Key{ID: "7q2m4x9k1c3d", Name: "laptop", Prefix: "ambit_9fK2", Email: "alice@acme.com", Groups: []string{"eng"},
		Scope: "both", Created: now, Expires: now.AddDate(0, 0, 90), SignedIn: now, Ceiling: now.AddDate(0, 0, 90),
		Until: now.AddDate(0, 0, 30), Hash: "sha256:ab"}
	s := k.Secret("ambit-system")
	if len(s.Data) != 1 || len(s.Data["key"]) == 0 || len(s.StringData) != 0 {
		t.Fatalf("exactly one entry in Data, none in StringData: Data=%v StringData=%v", s.Data, s.StringData)
	}
	var e struct {
		KeyHash  string         `json:"keyHash"`
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(s.Data["key"], &e); err != nil {
		t.Fatal(err)
	}
	if e.KeyHash != "sha256:ab" || len(e.Metadata) != 4 || e.Metadata["until"] != "2026-11-10T09:00:00Z" || e.Metadata["keyId"] != "7q2m4x9k1c3d" {
		t.Fatalf("entry %+v", e)
	}
	for l, v := range map[string]string{"ambitproject.dev/key": "true", "ambitproject.dev/key-llm": "true", "ambitproject.dev/key-mcp": "true", "ambitproject.dev/key-owner": ownerHash("alice@acme.com")} {
		if s.Labels[l] != v {
			t.Errorf("label %s = %q", l, s.Labels[l])
		}
	}
	if s.Name != "ambit-key-7q2m4x9k1c3d" || s.Annotations["ambitproject.dev/key-name"] != "laptop" || s.Annotations["ambitproject.dev/key-scope"] != "both" {
		t.Fatalf("meta %v %v", s.Name, s.Annotations)
	}
	// Round trip (as read back from the API server: Data, not StringData).
	back, err := KeyFromSecret(s)
	if err != nil || back.Email != k.Email || !back.Until.Equal(k.Until) || back.Scope != "both" || back.Name != "laptop" {
		t.Fatalf("round trip %+v %v", back, err)
	}
}

func TestScopeLabels(t *testing.T) {
	llm := Key{ID: "a", Email: "a@b.c", Scope: "llm"}.Secret("ns")
	if llm.Labels["ambitproject.dev/key-llm"] != "true" || llm.Labels["ambitproject.dev/key-mcp"] != "" {
		t.Fatal(llm.Labels)
	}
}

func TestUntilIsTheEarlierAndUTC(t *testing.T) {
	ceiling := time.Date(2027, 1, 9, 9, 0, 0, 0, time.UTC)
	signed := time.Date(2026, 10, 11, 18, 0, 0, 0, time.FixedZone("KST", 9*3600))
	u := until(ceiling, signed, 30)
	if u.Location() != time.UTC || !u.Equal(signed.AddDate(0, 0, 30)) {
		t.Fatalf("until %v", u)
	}
	if !until(ceiling, ceiling, 30).Equal(ceiling) {
		t.Fatal("ceiling must win when earlier")
	}
}

func TestOwnerSecretIsNotAGatewayKey(t *testing.T) {
	s := Owner{Email: "alice@acme.com", Groups: []string{"eng"}}.Secret("ambit-system")
	if s.Labels["ambitproject.dev/key-owner-record"] != "true" || s.Labels["ambitproject.dev/key"] != "" || s.Labels["ambitproject.dev/key-llm"] != "" {
		t.Fatalf("labels %v", s.Labels)
	}
	if s.Name != "ambit-key-owner-"+ownerHash("alice@acme.com") {
		t.Fatal(s.Name)
	}
}

func TestKeyFromSecretRejectsGarbage(t *testing.T) {
	if _, err := KeyFromSecret(nilSafeSecret(map[string][]byte{"key": []byte("{")})); err == nil {
		t.Fatal("garbage accepted")
	}
	if _, err := KeyFromSecret(nilSafeSecret(map[string][]byte{"key": []byte(`{"keyHash":"x","metadata":{}}`), "extra": []byte("y")})); err == nil {
		t.Fatal("a second entry must be refused")
	}
}

func nilSafeSecret(d map[string][]byte) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ambit-key-x", Labels: map[string]string{"ambitproject.dev/key": "true"}}, Data: d}
}
