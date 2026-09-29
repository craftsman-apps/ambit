package console

import (
	"context"
	"testing"

	"github.com/dexidp/dex/api/v2"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// fakeDex records calls; embedding the interface panics on any call we did not expect.
type fakeDex struct {
	api.DexClient
	clients   map[string]*api.Client
	passwords map[string][]byte
	calls     []string
}

func newFakeDex() *fakeDex {
	return &fakeDex{clients: map[string]*api.Client{}, passwords: map[string][]byte{}}
}

func (f *fakeDex) CreateClient(_ context.Context, in *api.CreateClientReq, _ ...grpc.CallOption) (*api.CreateClientResp, error) {
	f.calls = append(f.calls, "CreateClient")
	if _, ok := f.clients[in.Client.Id]; ok {
		return &api.CreateClientResp{AlreadyExists: true}, nil
	}
	f.clients[in.Client.Id] = in.Client
	return &api.CreateClientResp{Client: in.Client}, nil
}

func (f *fakeDex) DeleteClient(_ context.Context, in *api.DeleteClientReq, _ ...grpc.CallOption) (*api.DeleteClientResp, error) {
	f.calls = append(f.calls, "DeleteClient")
	delete(f.clients, in.Id)
	return &api.DeleteClientResp{}, nil
}

func (f *fakeDex) UpdateClient(_ context.Context, in *api.UpdateClientReq, _ ...grpc.CallOption) (*api.UpdateClientResp, error) {
	f.calls = append(f.calls, "UpdateClient")
	f.clients[in.Id].RedirectUris = in.RedirectUris
	return &api.UpdateClientResp{}, nil
}

func (f *fakeDex) CreatePassword(_ context.Context, in *api.CreatePasswordReq, _ ...grpc.CallOption) (*api.CreatePasswordResp, error) {
	f.calls = append(f.calls, "CreatePassword")
	if _, ok := f.passwords[in.Password.Email]; ok {
		return &api.CreatePasswordResp{AlreadyExists: true}, nil
	}
	f.passwords[in.Password.Email] = in.Password.Hash
	return &api.CreatePasswordResp{}, nil
}

func (f *fakeDex) UpdatePassword(_ context.Context, in *api.UpdatePasswordReq, _ ...grpc.CallOption) (*api.UpdatePasswordResp, error) {
	f.calls = append(f.calls, "UpdatePassword")
	f.passwords[in.Email] = in.NewHash
	return &api.UpdatePasswordResp{}, nil
}

const ns = "ambit-system"
const pub = "http://console.localtest.me:8080"

func TestBootstrapFirstStart(t *testing.T) {
	kube, dex := fake.NewClientset(), newFakeDex()
	keys, err := Bootstrap(t.Context(), kube, dex, ns, pub)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys.SessionKey) != 32 || keys.ClientSecret == "" {
		t.Fatalf("keys not generated: %+v", keys)
	}
	c := dex.clients[ClientID]
	if c == nil || c.Secret != keys.ClientSecret || c.RedirectUris[0] != pub+"/auth/callback" {
		t.Fatalf("dex client wrong: %+v", c)
	}
	admin, err := kube.CoreV1().Secrets(ns).Get(t.Context(), AdminSecret, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pw := admin.Data["password"]
	if len(pw) != 24 || string(admin.Data["email"]) != BootstrapEmail {
		t.Fatalf("admin secret wrong: len=%d email=%s", len(pw), admin.Data["email"])
	}
	if err := bcrypt.CompareHashAndPassword(dex.passwords[BootstrapEmail], pw); err != nil {
		t.Fatalf("dex hash does not match stored password: %v", err)
	}
}

func TestBootstrapIsIdempotent(t *testing.T) {
	kube, dex := fake.NewClientset(), newFakeDex()
	k1, _ := Bootstrap(t.Context(), kube, dex, ns, pub)
	hash1 := string(dex.passwords[BootstrapEmail])
	dex.calls = nil
	k2, err := Bootstrap(t.Context(), kube, dex, ns, pub)
	if err != nil {
		t.Fatal(err)
	}
	if string(k1.SessionKey) != string(k2.SessionKey) || k1.ClientSecret != k2.ClientSecret {
		t.Fatal("keys changed on restart; every session and the Dex client would break")
	}
	if string(dex.passwords[BootstrapEmail]) != hash1 {
		t.Fatal("admin password changed on restart")
	}
	for _, c := range dex.calls {
		if c == "CreatePassword" || c == "UpdatePassword" || c == "DeleteClient" {
			t.Fatalf("unexpected %s on restart (calls %v)", c, dex.calls)
		}
	}
}

func TestBootstrapRecreatesClientWhenKeysSecretLost(t *testing.T) {
	kube, dex := fake.NewClientset(), newFakeDex()
	Bootstrap(t.Context(), kube, dex, ns, pub)
	kube.CoreV1().Secrets(ns).Delete(t.Context(), KeysSecret, metav1.DeleteOptions{})
	k, err := Bootstrap(t.Context(), kube, dex, ns, pub)
	if err != nil {
		t.Fatal(err)
	}
	if dex.clients[ClientID].Secret != k.ClientSecret {
		t.Fatal("Dex still has the old client secret; sign-in would fail")
	}
}

func TestBootstrapResetsDexPasswordWhenAdminSecretLost(t *testing.T) {
	kube, dex := fake.NewClientset(), newFakeDex()
	Bootstrap(t.Context(), kube, dex, ns, pub)
	kube.CoreV1().Secrets(ns).Delete(t.Context(), AdminSecret, metav1.DeleteOptions{})
	if _, err := Bootstrap(t.Context(), kube, dex, ns, pub); err != nil {
		t.Fatal(err)
	}
	admin, _ := kube.CoreV1().Secrets(ns).Get(t.Context(), AdminSecret, metav1.GetOptions{})
	if err := bcrypt.CompareHashAndPassword(dex.passwords[BootstrapEmail], admin.Data["password"]); err != nil {
		t.Fatal("new admin Secret does not match Dex; the admin is locked out")
	}
}

func TestBootstrapUpdatesRedirectWhenURLChanges(t *testing.T) {
	kube, dex := fake.NewClientset(), newFakeDex()
	Bootstrap(t.Context(), kube, dex, ns, pub)
	if _, err := Bootstrap(t.Context(), kube, dex, ns, "https://ambit.acme.com"); err != nil {
		t.Fatal(err)
	}
	if got := dex.clients[ClientID].RedirectUris[0]; got != "https://ambit.acme.com/auth/callback" {
		t.Fatalf("redirect not updated: %s", got)
	}
}
