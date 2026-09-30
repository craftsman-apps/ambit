package console

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Grant is one user's authorisation for one Connection: the tokens the provider issued to them.
type Grant struct {
	Email        string    `json:"email"`
	Connection   string    `json:"connection"`
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken,omitempty"`
	Expiry       time.Time `json:"expiry"`
	// State is "connected" or "broken" (a refresh failed; the user must reconnect).
	State       string    `json:"state"`
	Error       string    `json:"error,omitempty"`
	ConnectedAt time.Time `json:"connectedAt"`
	CheckedAt   time.Time `json:"checkedAt"`
}

const (
	grantConnectedState = "connected"
	grantBrokenState    = "broken"
	grantUserLabel      = "ambitproject.dev/user"
	grantConnLabel      = "ambitproject.dev/connection"
)

// GrantStore keeps each grant in its own Secret in a namespace that holds nothing else, so the
// console's Secret rights there cannot reach other Secrets. The payload is also sealed with
// AES-256-GCM, so a copy of the Secret without the console's key reveals nothing.
// ponytail: one Secret per grant is fine to tens of thousands of grants; move to PostgreSQL beyond.
type GrantStore struct {
	Client    client.Client
	Namespace string
	Key       []byte // 32 bytes, from ambit-console-keys
}

// userHash names a user in labels and object names without putting their email there.
func userHash(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(email)))
	return hex.EncodeToString(sum[:])[:32]
}

func grantName(email, conn string) string { return "grant-" + conn + "-" + userHash(email) }

func (s *GrantStore) seal(plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.Key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, nil), nil
}

func (s *GrantStore) open(sealed []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.Key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("grant: sealed data too short")
	}
	return gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
}

func (s *GrantStore) decode(sec *corev1.Secret) (*Grant, error) {
	plain, err := s.open(sec.Data["grant"])
	if err != nil {
		return nil, fmt.Errorf("grant %s: %w", sec.Name, err)
	}
	var g Grant
	return &g, json.Unmarshal(plain, &g)
}

// Get returns the grant, or nil when the user has not connected this app.
func (s *GrantStore) Get(ctx context.Context, email, conn string) (*Grant, error) {
	var sec corev1.Secret
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: grantName(email, conn)}, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return s.decode(&sec)
}

// Put creates or replaces the grant.
func (s *GrantStore) Put(ctx context.Context, g *Grant) error {
	plain, err := json.Marshal(g)
	if err != nil {
		return err
	}
	sealed, err := s.seal(plain)
	if err != nil {
		return err
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: grantName(g.Email, g.Connection), Namespace: s.Namespace,
			Labels: map[string]string{grantUserLabel: userHash(g.Email), grantConnLabel: g.Connection, "app.kubernetes.io/managed-by": "ambit-console"}},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"grant": sealed},
	}
	err = s.Client.Create(ctx, sec)
	if apierrors.IsAlreadyExists(err) {
		var cur corev1.Secret
		if err := s.Client.Get(ctx, client.ObjectKeyFromObject(sec), &cur); err != nil {
			return err
		}
		cur.Data = sec.Data
		return s.Client.Update(ctx, &cur)
	}
	return err
}

func (s *GrantStore) Delete(ctx context.Context, email, conn string) error {
	err := s.Client.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: grantName(email, conn)}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// List returns every grant of one user, or of everyone when email is empty.
func (s *GrantStore) List(ctx context.Context, email string) ([]*Grant, error) {
	opts := []client.ListOption{client.InNamespace(s.Namespace), client.HasLabels{grantConnLabel}}
	if email != "" {
		opts = append(opts, client.MatchingLabels{grantUserLabel: userHash(email)})
	}
	var l corev1.SecretList
	if err := s.Client.List(ctx, &l, opts...); err != nil {
		return nil, err
	}
	out := make([]*Grant, 0, len(l.Items))
	for i := range l.Items {
		g, err := s.decode(&l.Items[i])
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}
