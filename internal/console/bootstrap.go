package console

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"

	"github.com/dexidp/dex/api/v2"
	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	ClientID    = "ambit-console"
	KeysSecret  = "ambit-console-keys"
	AdminSecret = "ambit-initial-admin"
)

// EnsureClickHousePassword creates the audit store's password once (Secret ambit-clickhouse, key
// password), for ClickHouse, the collector and the console. The chart never renders it (GitOps rule 6).
func EnsureClickHousePassword(ctx context.Context, kube kubernetes.Interface, ns string) (string, error) {
	secrets := kube.CoreV1().Secrets(ns)
	sec, err := secrets.Get(ctx, "ambit-clickhouse", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		sec, err = secrets.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ambit-clickhouse"},
			Data: map[string][]byte{"password": []byte(randomPassword(40))}}, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			sec, err = secrets.Get(ctx, "ambit-clickhouse", metav1.GetOptions{})
		}
	}
	if err != nil {
		return "", fmt.Errorf("ambit-clickhouse: %w", err)
	}
	return string(sec.Data["password"]), nil
}

type Keys struct {
	SessionKey   []byte
	ClientSecret string
	GrantKey     []byte // AES-256 key for users' SaaS tokens (Connections); added on upgrade if missing
	SCIMToken    string // bearer token the identity provider uses for SCIM offboarding
}

// Bootstrap prepares everything the chart deliberately does not generate (GitOps rule 6):
// the session key, the Dex OAuth client and the bootstrap admin. Safe to run on every start.
// ponytail: assumes one console replica; add leader election before scaling out.
// With bootstrapAdmin false it removes the bootstrap admin from Dex and deletes its password Secret.
func Bootstrap(ctx context.Context, kube kubernetes.Interface, dex api.DexClient, ns, publicURL string, bootstrapAdmin bool) (Keys, error) {
	secrets := kube.CoreV1().Secrets(ns)
	redirect := strings.TrimSuffix(publicURL, "/") + "/auth/callback"

	keysSec, err := secrets.Get(ctx, KeysSecret, metav1.GetOptions{})
	newKeys := apierrors.IsNotFound(err)
	if err != nil && !newKeys {
		return Keys{}, fmt.Errorf("read %s: %w", KeysSecret, err)
	}
	if newKeys {
		keysSec = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: KeysSecret}, Data: map[string][]byte{
			"session-key": randomBytes(32), "client-secret": []byte(randomPassword(40)), "grant-key": randomBytes(32),
		}}
		if keysSec, err = secrets.Create(ctx, keysSec, metav1.CreateOptions{}); err != nil {
			return Keys{}, fmt.Errorf("create %s: %w", KeysSecret, err)
		}
	}
	if len(keysSec.Data["grant-key"]) != 32 || len(keysSec.Data["scim-token"]) == 0 { // installs from before steps 5 and 6
		if len(keysSec.Data["grant-key"]) != 32 {
			keysSec.Data["grant-key"] = randomBytes(32)
		}
		if len(keysSec.Data["scim-token"]) == 0 {
			keysSec.Data["scim-token"] = []byte(randomPassword(48))
		}
		if keysSec, err = secrets.Update(ctx, keysSec, metav1.UpdateOptions{}); err != nil {
			return Keys{}, fmt.Errorf("add keys to %s: %w", KeysSecret, err)
		}
	}
	keys := Keys{SessionKey: keysSec.Data["session-key"], ClientSecret: string(keysSec.Data["client-secret"]),
		GrantKey: keysSec.Data["grant-key"], SCIMToken: string(keysSec.Data["scim-token"])}

	client := &api.Client{Id: ClientID, Name: "Ambit", Secret: keys.ClientSecret, RedirectUris: []string{redirect}}
	resp, err := dex.CreateClient(ctx, &api.CreateClientReq{Client: client})
	if err != nil {
		return Keys{}, fmt.Errorf("dex CreateClient: %w", err)
	}
	if resp.AlreadyExists {
		if newKeys { // Dex cannot change a client's secret, so replace the client.
			if _, err := dex.DeleteClient(ctx, &api.DeleteClientReq{Id: ClientID}); err != nil {
				return Keys{}, fmt.Errorf("dex DeleteClient: %w", err)
			}
			if _, err := dex.CreateClient(ctx, &api.CreateClientReq{Client: client}); err != nil {
				return Keys{}, fmt.Errorf("dex CreateClient: %w", err)
			}
		} else if _, err := dex.UpdateClient(ctx, &api.UpdateClientReq{Id: ClientID, Name: "Ambit", RedirectUris: []string{redirect}}); err != nil {
			return Keys{}, fmt.Errorf("dex UpdateClient: %w", err)
		}
	}

	if !bootstrapAdmin {
		if _, err := dex.DeletePassword(ctx, &api.DeletePasswordReq{Email: BootstrapEmail}); err != nil {
			return Keys{}, fmt.Errorf("dex DeletePassword: %w", err)
		}
		if err := secrets.Delete(ctx, AdminSecret, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return Keys{}, fmt.Errorf("delete %s: %w", AdminSecret, err)
		}
		return keys, nil
	}

	_, err = secrets.Get(ctx, AdminSecret, metav1.GetOptions{})
	if err == nil {
		return keys, nil
	}
	if !apierrors.IsNotFound(err) {
		return Keys{}, fmt.Errorf("read %s: %w", AdminSecret, err)
	}
	pw := randomPassword(24)
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return Keys{}, err
	}
	// Dex first, Secret second: if the Secret write fails, the next start resets the password again.
	pr, err := dex.CreatePassword(ctx, &api.CreatePasswordReq{Password: &api.Password{
		Email: BootstrapEmail, Hash: hash, Username: "admin", UserId: "ambit-bootstrap-admin"}})
	if err != nil {
		return Keys{}, fmt.Errorf("dex CreatePassword: %w", err)
	}
	if pr.AlreadyExists {
		if _, err := dex.UpdatePassword(ctx, &api.UpdatePasswordReq{Email: BootstrapEmail, NewHash: hash}); err != nil {
			return Keys{}, fmt.Errorf("dex UpdatePassword: %w", err)
		}
	}
	_, err = secrets.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: AdminSecret},
		Data: map[string][]byte{"email": []byte(BootstrapEmail), "password": []byte(pw)}}, metav1.CreateOptions{})
	if err != nil {
		return Keys{}, fmt.Errorf("create %s: %w", AdminSecret, err)
	}
	return keys, nil
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// randomPassword avoids characters that are easy to misread when copied from a terminal.
func randomPassword(n int) string {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, n)
	for i := range b {
		j, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b[i] = alphabet[j.Int64()]
	}
	return string(b)
}
