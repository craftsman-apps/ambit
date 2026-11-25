package oauth

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ClientAddrHeader carries the caller's address from the public Gateway to ambit-auth. The Gateway sets it itself from
// the connection (or the PROXY protocol header), replacing any value the caller sent (policy ambit-auth-public-headers),
// and only the Gateway's pods may reach port 8080 (NetworkPolicy ambit-auth). X-Forwarded-For and the like are never read.
const ClientAddrHeader = "X-Ambit-Client-Address"

// ClientKey is the address that limits count: an IPv4 address as it is, an IPv6 address by its /64 (a customer owns a
// /64, so one more address in it is not a new caller). With no usable header it falls back to the peer's address, which
// is the Gateway pod: the limits then count every caller together, which is safe and loud (the log says so).
func ClientKey(r *http.Request) (key string, fromGateway bool) {
	if ip := net.ParseIP(strings.TrimSpace(r.Header.Get(ClientAddrHeader))); ip != nil {
		return prefixKey(ip), true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil {
		return prefixKey(ip), false
	}
	return "unknown", false
}

func prefixKey(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// Limiter counts events in fixed windows. The count is shared by every replica when it lives in Valkey.
type Limiter interface {
	// Allow counts one event for key in bucket and reports whether it is within limit per window.
	Allow(ctx context.Context, bucket, key string, limit int, window time.Duration) (bool, error)
}

// ValkeyLimiter keeps the counters in Valkey (or Redis): INCR on a key made of the bucket, the caller and the window.
type ValkeyLimiter struct {
	C      redis.Cmdable
	Prefix string
	Now    func() time.Time
}

func (l *ValkeyLimiter) Allow(ctx context.Context, bucket, key string, limit int, window time.Duration) (bool, error) {
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	slot := now().Unix() / int64(window/time.Second)
	k := l.Prefix + "rl:" + bucket + ":" + key + ":" + itoa(slot)
	pipe := l.C.TxPipeline()
	incr := pipe.Incr(ctx, k)
	pipe.Expire(ctx, k, 2*window)
	if _, err := pipe.Exec(ctx); err != nil {
		return false, err
	}
	return incr.Val() <= int64(limit), nil
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// MemLimiter counts in this process only: tests, and the fallback when Valkey does not answer (then a replica limits
// what it sees itself, so a Valkey outage never removes a limit).
type MemLimiter struct {
	mu  sync.Mutex
	m   map[string]int
	Now func() time.Time
}

func (l *MemLimiter) Allow(_ context.Context, bucket, key string, limit int, window time.Duration) (bool, error) {
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]int{}
	}
	slot := now().Unix() / int64(window/time.Second)
	k := bucket + ":" + key + ":" + itoa(slot)
	if len(l.m) > 50000 { // a flood of distinct callers must not grow memory without bound: start over
		l.m = map[string]int{}
	}
	l.m[k]++
	return l.m[k] <= limit, nil
}

// Limits wraps a shared limiter with the process-local one for when the shared one fails. The bucket defaults are the
// first values of 0.6.0; they are measured and may change (docs/oauth.md, "Limits").
type Limits struct {
	Shared Limiter
	Local  Limiter
	Rates  map[string]Rate
}

// Rate is a limit in one bucket.
type Rate struct {
	Limit  int
	Window time.Duration
}

// Bucket names. Separate buckets, so a flood of one kind never uses up another: sign-in pages, token calls, discovery,
// registration.
const (
	BucketDiscovery = "discovery"
	BucketSignIn    = "signin"
	BucketRegister  = "register"
)

// DefaultRates are per caller address.
func DefaultRates() map[string]Rate {
	return map[string]Rate{
		BucketDiscovery: {Limit: 600, Window: time.Minute},
		BucketSignIn:    {Limit: 300, Window: time.Minute},
		BucketRegister:  {Limit: 10, Window: time.Hour},
	}
}

// Allow reports whether the caller may go on. shared=false means the shared counter failed and the local one answered.
func (l *Limits) Allow(ctx context.Context, bucket, key string) (ok, shared bool) {
	rate := l.Rates[bucket]
	if l.Shared != nil {
		if ok, err := l.Shared.Allow(ctx, bucket, key, rate.Limit, rate.Window); err == nil {
			return ok, true
		}
	}
	ok, _ = l.Local.Allow(ctx, bucket, key, rate.Limit, rate.Window)
	return ok, false
}
