package oauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHydra is the part of Hydra's admin API the checker uses: key sets, and the readiness.
type fakeHydra struct {
	mu      sync.Mutex
	sets    map[string]int
	ready   bool
	creates int
}

func (f *fakeHydra) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/health/ready" {
		if !f.ready {
			http.Error(w, "no", 503)
		}
		return
	}
	set := strings.TrimPrefix(r.URL.Path, "/admin/keys/")
	switch r.Method {
	case http.MethodGet:
		n, ok := f.sets[set]
		if !ok {
			http.Error(w, `{"error":"Not Found"}`, 404)
			return
		}
		keys := make([]map[string]string, n)
		for i := range keys {
			keys[i] = map[string]string{"kid": "k"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	case http.MethodPost:
		f.sets[set]++
		f.creates++
		w.WriteHeader(201)
	}
}

func checker(f *fakeHydra) (*KeyChecker, func()) {
	s := httptest.NewServer(f)
	return &KeyChecker{Admin: s.URL, Client: s.Client(), Sets: SigningSets}, s.Close
}

func TestEnsureCreatesMissingSetsOnce(t *testing.T) {
	f := &fakeHydra{sets: map[string]int{}, ready: true}
	k, stop := checker(f)
	defer stop()
	for i := 0; i < 3; i++ {
		if err := k.Ensure(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if f.creates != 2 || f.sets["hydra.jwt.access-token"] != 1 || f.sets["hydra.openid.id-token"] != 1 {
		t.Fatalf("want each of 2 sets created once, got %d creations %v", f.creates, f.sets)
	}
}

func TestEnsureRefusesTwoKeysAndNeverDeletes(t *testing.T) {
	f := &fakeHydra{sets: map[string]int{"hydra.jwt.access-token": 2, "hydra.openid.id-token": 1}, ready: true}
	k, stop := checker(f)
	defer stop()
	err := k.Ensure(t.Context())
	if err == nil || !strings.Contains(err.Error(), "holds 2 keys") || f.sets["hydra.jwt.access-token"] != 2 {
		t.Fatalf("got %v, sets %v", err, f.sets)
	}
}

func TestEnsureRefusesAnEmptySet(t *testing.T) {
	f := &fakeHydra{sets: map[string]int{"hydra.jwt.access-token": 0, "hydra.openid.id-token": 1}, ready: true}
	k, stop := checker(f)
	defer stop()
	if err := k.Ensure(t.Context()); err == nil || !strings.Contains(err.Error(), "holds 0 keys") {
		t.Fatalf("got %v", err)
	}
}

func TestEnsureErrorOnServerError(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", 500) }))
	defer s.Close()
	k := &KeyChecker{Admin: s.URL, Client: s.Client(), Sets: SigningSets}
	if err := k.Ensure(t.Context()); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("got %v", err)
	}
}

// Two pods that see the same gap: the jitter and the second look make one creation the usual outcome; if both create,
// the checker reports it and the pods stay out of the Service.
func TestEnsureJitterLooksAgain(t *testing.T) {
	f := &fakeHydra{sets: map[string]int{}, ready: true}
	k, stop := checker(f)
	defer stop()
	k.Jitter = 50 * time.Millisecond
	k.Sets = SigningSets[:1]
	go func() { time.Sleep(5 * time.Millisecond); f.mu.Lock(); f.sets[SigningSets[0]] = 1; f.mu.Unlock() }()
	// Often the other pod's key is there at the second look; either way the set ends with 1 key, or the error says 2.
	err := k.Ensure(t.Context())
	n := f.sets[SigningSets[0]]
	if (err == nil) != (n == 1) {
		t.Fatalf("error %v with %d keys", err, n)
	}
}

func TestWatchFollowsHydraAndTheKeys(t *testing.T) {
	f := &fakeHydra{sets: map[string]int{}, ready: false}
	k, stop := checker(f)
	defer stop()
	var ready atomic.Bool
	ctx, cancel := t.Context(), func() {}
	_ = cancel
	go k.Watch(ctx, 10*time.Millisecond, &ready, func(string, error) {})
	wait := func(want bool) {
		t.Helper()
		for i := 0; i < 300; i++ {
			if ready.Load() == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("ready did not become %v", want)
	}
	time.Sleep(50 * time.Millisecond)
	if ready.Load() || f.creates != 0 {
		t.Fatal("a pod whose Hydra is not ready must not create keys or be ready")
	}
	f.mu.Lock()
	f.ready = true
	f.mu.Unlock()
	wait(true)
	f.mu.Lock()
	f.sets[SigningSets[0]] = 2
	f.mu.Unlock() // somebody adds a second key
	wait(false)
	f.mu.Lock()
	f.sets[SigningSets[0]] = 1
	f.mu.Unlock()
	wait(true)
}

func TestReadyHandler(t *testing.T) {
	var ready atomic.Bool
	h := ReadyHandler(&ready)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 503 {
		t.Fatalf("not ready: %d", rec.Code)
	}
	ready.Store(true)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 200 {
		t.Fatalf("ready: %d", rec.Code)
	}
}
