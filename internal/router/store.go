// Package router is ambit-router: an agentgateway ExtProc that labels chat prompts for model routers of type LLM.
package router

import (
	"sync/atomic"
	"time"

	"github.com/craftsman-apps/ambit/internal/routerconfig"
	"golang.org/x/time/rate"
)

// Snapshot is one loaded configuration with its keys. A request uses one snapshot from start to end.
type Snapshot struct {
	Config routerconfig.Config
	keys   map[string]string
	limits map[string]*rate.Limiter // per router name
}

// Store holds the current snapshot; the watch replaces it, requests read it without locks.
type Store struct{ p atomic.Pointer[Snapshot] }

func (s *Store) Set(cfg routerconfig.Config, keys map[string]string) {
	old := s.p.Load()
	lim := map[string]*rate.Limiter{}
	for _, r := range cfg.Routers {
		if r.RatePerMinute <= 0 {
			continue
		}
		per := rate.Every(time.Minute / time.Duration(r.RatePerMinute))
		if old != nil && old.limits[r.Name] != nil && old.limits[r.Name].Limit() == per {
			lim[r.Name] = old.limits[r.Name] // the bucket survives a reload
		} else {
			lim[r.Name] = rate.NewLimiter(per, int(r.RatePerMinute))
		}
	}
	s.p.Store(&Snapshot{Config: cfg, keys: keys, limits: lim})
}

// Snapshot is nil until a configuration was loaded.
func (s *Store) Snapshot() *Snapshot { return s.p.Load() }
