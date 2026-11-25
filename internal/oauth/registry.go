package oauth

import (
	"context"
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

func (v *ValkeyRegistry) Add(ctx context.Context, id string) (int, error) {
	pipe := v.C.TxPipeline()
	pipe.SAdd(ctx, v.key(), id)
	n := pipe.SCard(ctx, v.key())
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return int(n.Val()), nil
}
func (v *ValkeyRegistry) Remove(ctx context.Context, id string) error {
	return v.C.SRem(ctx, v.key(), id).Err()
}
func (v *ValkeyRegistry) Count(ctx context.Context) (int, error) {
	n, err := v.C.SCard(ctx, v.key()).Result()
	return int(n), err
}
func (v *ValkeyRegistry) Reset(ctx context.Context, ids []string) error {
	pipe := v.C.TxPipeline()
	pipe.Del(ctx, v.key())
	if len(ids) > 0 {
		args := make([]any, len(ids))
		for i, id := range ids {
			args[i] = id
		}
		pipe.SAdd(ctx, v.key(), args...)
	}
	_, err := pipe.Exec(ctx)
	return err
}
func (v *ValkeyRegistry) Once(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return v.C.SetNX(ctx, v.Prefix+"once:"+key, "1", ttl).Result()
}

// MemRegistry is the same in memory (tests).
type MemRegistry struct {
	mu   sync.Mutex
	ids  map[string]bool
	once map[string]time.Time
	Now  func() time.Time
}

func (m *MemRegistry) init() {
	if m.ids == nil {
		m.ids, m.once = map[string]bool{}, map[string]time.Time{}
	}
}
func (m *MemRegistry) Add(_ context.Context, id string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.ids[id] = true
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
func (m *MemRegistry) Reset(_ context.Context, ids []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ids, m.once = map[string]bool{}, map[string]time.Time{}
	for _, id := range ids {
		m.ids[id] = true
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
	return deleted, s.Clients.Reset(ctx, keep)
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
