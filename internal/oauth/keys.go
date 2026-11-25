package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sync/atomic"
	"time"
)

// SigningSets are Hydra's key sets Ambit keeps at exactly one key each. Hydra creates a missing set by itself at the
// first request for it, including the first fetch of its public JWKS; two Hydra pods that start together on an empty
// database then each create a key, and half of the access tokens carry the wrong key ID (docs/oauth.md, "Signing keys").
var SigningSets = []string{"hydra.jwt.access-token", "hydra.openid.id-token"}

// KeyChecker keeps Hydra's signing sets at one key, through Hydra's admin API (the pod's own, 127.0.0.1).
type KeyChecker struct {
	Admin  string // for example http://127.0.0.1:4445
	Client *http.Client
	Sets   []string
	// Jitter is the longest wait before a missing set is created: two pods that see the same gap rarely create together.
	Jitter time.Duration
}

type keySet struct {
	Keys []json.RawMessage `json:"keys"`
}

// count is the number of keys in a set: -1 when the set does not exist.
func (k *KeyChecker) count(ctx context.Context, set string) (int, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, k.Admin+"/admin/keys/"+set, nil)
	resp, err := k.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusNotFound:
		return -1, nil
	case http.StatusOK:
		var s keySet
		if err := json.Unmarshal(body, &s); err != nil {
			return 0, fmt.Errorf("key set %s: the answer is not JSON", set)
		}
		return len(s.Keys), nil
	}
	return 0, fmt.Errorf("key set %s: HTTP %d", set, resp.StatusCode)
}

func (k *KeyChecker) create(ctx context.Context, set string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, k.Admin+"/admin/keys/"+set, bytes.NewReader([]byte(`{"alg":"RS256","use":"sig"}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := k.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("create key set %s: HTTP %d", set, resp.StatusCode)
	}
	return nil
}

// Ensure creates a missing set with one key and fails when a set holds any other number of keys. It never deletes a key:
// which one to keep is an operator's decision (docs/oauth.md).
func (k *KeyChecker) Ensure(ctx context.Context) error {
	for _, set := range k.Sets {
		n, err := k.count(ctx, set)
		if err != nil {
			return err
		}
		if n == -1 {
			if k.Jitter > 0 {
				select {
				case <-time.After(rand.N(k.Jitter)):
				case <-ctx.Done():
					return ctx.Err()
				}
				// Another pod may have created it meanwhile.
				if n, err = k.count(ctx, set); err != nil {
					return err
				}
			}
			if n == -1 {
				if err := k.create(ctx, set); err != nil {
					return err
				}
				if n, err = k.count(ctx, set); err != nil {
					return err
				}
			}
		}
		if n != 1 {
			return fmt.Errorf("key set %s holds %d keys, it must hold exactly 1: tokens would carry the wrong key ID (docs/oauth.md, \"Signing keys\")", set, n)
		}
	}
	return nil
}

// Ready reports Hydra's admin API ready (it checks the database).
func (k *KeyChecker) hydraReady(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, k.Admin+"/health/ready", nil)
	resp, err := k.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Hydra is not ready: HTTP %d", resp.StatusCode)
	}
	return nil
}

// Watch checks every interval until ctx ends and keeps ready true only while the last check passed. The pod's readiness
// is this flag: no pod serves tokens while a signing set is missing or holds two keys.
func (k *KeyChecker) Watch(ctx context.Context, interval time.Duration, ready *atomic.Bool, logf func(msg string, err error)) {
	for {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := k.hydraReady(cctx)
		if err == nil {
			err = k.Ensure(cctx)
		}
		cancel()
		if was := ready.Swap(err == nil); (err == nil) != was || err != nil {
			logf("signing keys", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// ErrNotReady is what the readiness handler reports.
var ErrNotReady = errors.New("a signing key set is missing or does not hold exactly one key")

// ReadyHandler answers 200 while ready is true.
func ReadyHandler(ready *atomic.Bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, ErrNotReady.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
}
