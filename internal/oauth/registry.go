package oauth

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ValkeyRegistry keeps the set of dynamic client IDs in Valkey, so that every replica counts the same clients.
type ValkeyRegistry struct {
	C      redis.Cmdable
	Prefix string
}

func (v *ValkeyRegistry) key() string { return v.Prefix + "dcr-clients" }

// The set is a sorted set: the score is the time of the registration, so that Reset does not drop a client that was registered
// while the janitor was listing Hydra's clients.
func (v *ValkeyRegistry) Add(ctx context.Context, id string) (int, error) {
	pipe := v.C.TxPipeline()
	pipe.ZAdd(ctx, v.key(), redis.Z{Score: float64(time.Now().UnixNano()), Member: id})
	n := pipe.ZCard(ctx, v.key())
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return int(n.Val()), nil
}
func (v *ValkeyRegistry) Remove(ctx context.Context, id string) error {
	return v.C.ZRem(ctx, v.key(), id).Err()
}
func (v *ValkeyRegistry) Count(ctx context.Context) (int, error) {
	n, err := v.C.ZCard(ctx, v.key()).Result()
	return int(n), err
}

// Reset makes the registry match the clients that Hydra listed (ids). since is the time before the listing began: an entry added
// after it is kept, because the listing may not have seen it.
func (v *ValkeyRegistry) Reset(ctx context.Context, ids []string, since time.Time) error {
	old, err := v.C.ZRangeByScore(ctx, v.key(), &redis.ZRangeBy{Min: "-inf", Max: strconv.FormatInt(since.UnixNano(), 10)}).Result()
	if err != nil {
		return err
	}
	listed := map[string]bool{}
	for _, id := range ids {
		listed[id] = true
	}
	var stale []any
	for _, id := range old {
		if !listed[id] {
			stale = append(stale, id)
		}
	}
	pipe := v.C.TxPipeline()
	if len(stale) > 0 {
		pipe.ZRem(ctx, v.key(), stale...)
	}
	for _, id := range ids { // listed but not known (registered by another replica, or the registry was lost): known from now on
		pipe.ZAddNX(ctx, v.key(), redis.Z{Score: float64(since.UnixNano()), Member: id})
	}
	_, err = pipe.Exec(ctx)
	return err
}
func (v *ValkeyRegistry) Once(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return v.C.SetNX(ctx, v.Prefix+"once:"+key, "1", ttl).Result()
}

// MemRegistry is the same in memory (tests).
type MemRegistry struct {
	mu   sync.Mutex
	ids  map[string]time.Time // the time of the registration
	once map[string]time.Time
	Now  func() time.Time
}

func (m *MemRegistry) init() {
	if m.ids == nil {
		m.ids, m.once = map[string]time.Time{}, map[string]time.Time{}
	}
}
func (m *MemRegistry) Add(_ context.Context, id string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.ids[id] = time.Now()
	return len(m.ids), nil
}
func (m *MemRegistry) Remove(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	delete(m.ids, id)
	return nil
}
func (m *MemRegistry) Count(context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.ids), nil
}
func (m *MemRegistry) Reset(_ context.Context, ids []string, since time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	listed := map[string]bool{}
	for _, id := range ids {
		listed[id] = true
		if _, ok := m.ids[id]; !ok {
			m.ids[id] = since
		}
	}
	for id, t := range m.ids {
		if !listed[id] && !t.After(since) {
			delete(m.ids, id)
		}
	}
	return nil
}
func (m *MemRegistry) Once(_ context.Context, key string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	now := time.Now()
	if m.Now != nil {
		now = m.Now()
	}
	if t, ok := m.once[key]; ok && now.Before(t) {
		return false, nil
	}
	m.once[key] = now.Add(ttl)
	return true, nil
}

// Janitor deletes the dynamic clients that nobody used for Config.UnusedClientDays and makes the registry match Hydra.
// Replicas may run it together: deleting a deleted client is not an error. It returns how many clients it deleted.
func (s *Server) Janitor(ctx context.Context) (int, error) {
	cutoff := s.now().Add(-time.Duration(s.Cfg.UnusedClientDays) * 24 * time.Hour)
	began := time.Now() // the registry's own clock: a client registered after this moment may be missing from the listing
	var keep []string
	deleted := 0
	err := s.Hydra.ListClients(ctx, func(c Client) error {
		if c.Metadata["registered_via"] != "dcr" {
			return nil
		}
		last := c.CreatedAt
		if v, ok := c.Metadata["last_used"].(string); ok {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				last = t
			}
		}
		if !last.IsZero() && last.Before(cutoff) {
			if err := s.Hydra.DeleteClient(ctx, c.ID); err != nil {
				return err
			}
			deleted++
			return nil
		}
		keep = append(keep, c.ID)
		return nil
	})
	if err != nil {
		return deleted, err
	}
	return deleted, s.Clients.Reset(ctx, keep, began)
}

// RunJanitor runs the janitor every interval until ctx ends.
func (s *Server) RunJanitor(ctx context.Context, interval time.Duration) {
	for {
		if n, err := s.Janitor(ctx); err != nil {
			s.log().Warn("client clean-up failed", "err", err)
		} else if n > 0 {
			s.log().Info("unused clients deleted", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
