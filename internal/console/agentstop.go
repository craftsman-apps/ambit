package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/craftsman-apps/ambit/internal/controller"
	"github.com/craftsman-apps/ambit/internal/identity"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Stopping an agent (docs/agents.md): a fail-closed state machine.
//
//	active -- Stop --> stopping -- S2 keys deleted, S3 grants revoked, S5 every Gateway acknowledged --> stopped -- Start --> active
//
// S1 writes the agent to the stop list (ConfigMap ambit-agent-stops, a new list generation) and records state stopping.
// S2 deletes the agent's keys. S3 revokes the agent's OAuth grants (RevokeGrants; none without OAuth). S4 and S5 are the
// controller's: it renders the list into Require policies on every MCP and LLM route and writes into
// ambit-agent-stops-ack which generation each Gateway has accepted. S6 is here: the agent is reported stopped only when
// S2 and S3 are done and every Gateway acknowledged the entry's generation. Every step can be repeated.

const (
	agentStopsLabel = "ambitproject.dev/agent-stops"
	defaultOrphan   = 7
	defaultRetain   = 25 * time.Minute
)

var ErrNotStopped = errors.New("only a stopped agent can be started")

// TakeStopWork reports (and clears) that a stop was requested since the last call.
func (s *KeyService) TakeStopWork() bool { return s.stopWork.Swap(false) }

func (s *KeyService) orphanDays() int {
	if s.Agents.OrphanDays > 0 {
		return s.Agents.OrphanDays
	}
	return defaultOrphan
}

func (s *KeyService) stopRetention() time.Duration {
	if s.Agents.StopRetention > 0 {
		return s.Agents.StopRetention
	}
	return defaultRetain
}

// stopList reads the list; a missing ConfigMap is an empty list (cm has no name). Call it holding s.mu.
func (s *KeyService) stopList(ctx context.Context) (controller.StopList, *corev1.ConfigMap, error) {
	var cm corev1.ConfigMap
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: controller.StopsConfigMap}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return controller.StopList{}, &corev1.ConfigMap{}, nil
		}
		return controller.StopList{}, nil, err
	}
	l, err := controller.ParseStopList(&cm)
	return l, &cm, err
}

// changeStopList applies change to the list and writes it when change says so, version-checked, retrying on conflict.
// change returns whether it changed the set of entries (which raises the generation); other changes of an entry are
// written without a new generation.
func (s *KeyService) changeStopList(ctx context.Context, change func(l *controller.StopList) (set, other bool)) (controller.StopList, error) {
	var out controller.StopList
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		l, cm, err := s.stopList(ctx)
		if err != nil {
			return err
		}
		set, other := change(&l)
		out = l
		if !set && !other {
			return nil
		}
		if set {
			l.Gen++
			out = l
		}
		b, _ := json.Marshal(l)
		if cm.Name == "" {
			cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: controller.StopsConfigMap, Namespace: s.Namespace, Labels: map[string]string{agentStopsLabel: "true"}}}
			cm.Data = map[string]string{"list.json": string(b)}
			return s.Client.Create(ctx, cm)
		}
		cm.Data = map[string]string{"list.json": string(b)}
		return s.Client.Update(ctx, cm)
	})
	return out, err
}

func entryIndex(l controller.StopList, name string) int {
	return slices.IndexFunc(l.Entries, func(e controller.StopEntry) bool { return e.Kind == "agent" && e.Name == name })
}

// StopAck reads the controller's acknowledgment; a missing ConfigMap is an empty one (no Gateway acknowledged).
func (s *KeyService) StopAck(ctx context.Context) (controller.StopAck, bool, error) {
	var cm corev1.ConfigMap
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: controller.StopsAckConfigMap}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return controller.StopAck{}, false, nil
		}
		return controller.StopAck{}, false, err
	}
	return controller.ParseStopAck(&cm), true, nil
}

// acknowledged: every Gateway the controller knows has accepted generation gen, and the list renders. With no ConfigMap
// the controller has not answered: not acknowledged. A controller that found no route to attach to answers with no
// Gateways: nothing can be reached, so the stop is complete.
func acknowledged(a controller.StopAck, have bool, gen int64) bool {
	if !have || a.Degraded != "" || a.Gen < gen {
		return false
	}
	for _, g := range a.Gateways {
		if g.AckedGen < gen {
			return false
		}
	}
	return true
}

// StopAgent stops an agent: S1 and S2 now; the controller's acknowledgment (S5) and S6 follow from AdvanceStops. The
// owner (person mode) or an admin may. Stopping needs no setting: it works with agents switched off too.
func (s *KeyService) StopAgent(ctx context.Context, by Session, name string) (Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel, err := s.opCtx(ctx)
	if err != nil {
		return Agent{}, err
	}
	defer cancel()
	actor, err := adminEmail(by)
	if err != nil {
		return Agent{}, err
	}
	a, err := s.GetAgent(ctx, name)
	if err != nil {
		return Agent{}, err
	}
	if !(by.Role == RoleAdmin || canManage(by, a, actor) == nil) {
		return Agent{}, ErrNoAgent
	}
	return s.stopLocked(ctx, actor, a)
}

// stopLocked runs S1 to S3 for a, and S6 when everything is acknowledged already. Idempotent. Call it holding s.mu, in a term.
func (s *KeyService) stopLocked(ctx context.Context, actor string, a Agent) (Agent, error) {
	switch a.State {
	case StateDeleting, StatePending:
		return a, ErrAgentNotActive
	case StateStopped:
		return a, nil
	}
	now := s.now()
	s.stopWork.Store(true)
	// S1: the stop list, then the record. A full list is not an error: the credentials go all the same.
	unlisted := false
	if _, err := s.changeStopList(ctx, func(l *controller.StopList) (bool, bool) {
		unlisted = false
		if entryIndex(*l, a.Name) >= 0 {
			return false, false
		}
		if len(l.Entries) >= controller.MaxStopEntries {
			unlisted = true
			return false, false
		}
		l.Entries = append(l.Entries, controller.StopEntry{Kind: "agent", Name: a.Name, Gen: l.Gen + 1, RequestedAt: now})
		return true, false
	}); err != nil {
		return a, err
	}
	a, err := s.changeAgent(ctx, a.Name, func(cur *Agent) (bool, error) {
		if cur.State == StateStopped || cur.State == StateDeleting {
			return false, nil
		}
		changed := cur.State != StateStopping || cur.StopUnlisted != unlisted
		cur.State, cur.StopUnlisted = StateStopping, unlisted
		if cur.StopRequestedAt == nil {
			cur.StopRequestedAt, changed = &now, true
		}
		return changed, nil
	})
	if err != nil {
		return a, err
	}
	if a.StopRequestedAt != nil && now.Equal(*a.StopRequestedAt) {
		detail := "agent=" + a.Name
		if unlisted {
			detail += " stopping-unlisted: the stop list is full; the credentials are deleted, tokens expire on their own"
			slog.Error("stop list full: the agent is stopped without a list entry", "agent", a.Name)
		}
		s.event(ctx, actor, "agent-stop", "", a.Owner, detail)
	}
	return s.advanceLocked(ctx, a)
}

// advanceLocked repeats S2 and S3 and, once the controller acknowledged the entry on every Gateway (S5), does S6. Call it
// holding s.mu, in a term.
func (s *KeyService) advanceLocked(ctx context.Context, a Agent) (Agent, error) {
	if a.State != StateStopping {
		return a, nil
	}
	now := s.now()
	// S2: every key of the agent is deleted (a key made while stopping is deleted here too).
	keys, err := s.AgentKeys(ctx, a.Name)
	if err != nil {
		return a, err
	}
	var errs []error
	for _, k := range keys {
		if _, err := s.changeKey(ctx, k.ID, func(*Key) (keyOp, error) { return keyDelete, nil }, "agent-stop-key", consoleActor); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return a, fmt.Errorf("%w: %w", ErrDeleteIncomplete, err)
	}
	// S3: grants (OAuth); none in this release.
	if s.RevokeGrants != nil {
		if err := s.RevokeGrants(ctx, a); err != nil {
			return a, err
		}
	}
	var gen int64
	l, err := s.changeStopList(ctx, func(l *controller.StopList) (bool, bool) {
		i := entryIndex(*l, a.Name)
		if i < 0 {
			return false, false
		}
		gen = l.Entries[i].Gen
		if l.Entries[i].KeysDeletedAt != nil {
			return false, false
		}
		l.Entries[i].KeysDeletedAt = &now
		if s.RevokeGrants != nil {
			l.Entries[i].GrantsRevokedAt = &now
		}
		return false, true
	})
	if err != nil {
		return a, err
	}
	_ = l
	if entryIndex(l, a.Name) < 0 && !a.StopUnlisted {
		return a, nil // listed no more (a Start in between): nothing to confirm
	}
	if a.StopUnlisted { // try to get a place in the list again; the stop is not confirmed without one
		return s.relist(ctx, a)
	}
	ack, have, err := s.StopAck(ctx)
	if err != nil {
		return a, err
	}
	if acknowledged(ack, have, gen) {
		var stopped Agent
		stopped, err = s.changeAgent(ctx, a.Name, func(cur *Agent) (bool, error) {
			if cur.State != StateStopping {
				return false, nil
			}
			cur.State, cur.StoppedAt = StateStopped, &now
			return true, nil
		})
		if err == nil && stopped.State == StateStopped {
			parts := []string{}
			for _, g := range slices.Sorted(mapKeys(ack.Gateways)) {
				parts = append(parts, fmt.Sprintf("%s=%s", g, ack.Gateways[g].At.UTC().Format(time.RFC3339)))
			}
			s.event(ctx, consoleActor, "agent-stopped", "", stopped.Owner, "agent="+stopped.Name+" acked "+strings.Join(parts, " "))
		}
		return stopped, err
	}
	if a.StopRequestedAt != nil && a.StopNotConfirmedAt == nil && now.Sub(*a.StopRequestedAt) > controller.StopAckLag {
		var late []string
		for g, v := range ack.Gateways {
			if v.AckedGen < gen {
				late = append(late, g)
			}
		}
		slices.Sort(late)
		why := "no answer from the controller"
		if ack.Degraded != "" {
			why = "the stop list was not rendered: " + ack.Degraded
		} else if len(late) > 0 {
			why = "not confirmed on " + strings.Join(late, ", ")
		}
		slog.Error("stop not confirmed", "agent", a.Name, "why", why)
		a, err = s.changeAgent(ctx, a.Name, func(cur *Agent) (bool, error) {
			if cur.StopNotConfirmedAt != nil {
				return false, nil
			}
			cur.StopNotConfirmedAt = &now
			return true, nil
		})
		if err == nil {
			s.event(ctx, consoleActor, "agent-stop-not-confirmed", "", a.Owner, "agent="+a.Name+" "+why)
		}
	}
	return a, err
}

// relist puts an unlisted agent on the list when there is room again.
func (s *KeyService) relist(ctx context.Context, a Agent) (Agent, error) {
	now := s.now()
	placed := false
	if _, err := s.changeStopList(ctx, func(l *controller.StopList) (bool, bool) {
		placed = false
		if entryIndex(*l, a.Name) >= 0 || len(l.Entries) >= controller.MaxStopEntries {
			return false, false
		}
		l.Entries = append(l.Entries, controller.StopEntry{Kind: "agent", Name: a.Name, Gen: l.Gen + 1, RequestedAt: now, KeysDeletedAt: &now})
		placed = true
		return true, false
	}); err != nil || !placed {
		return a, err
	}
	return s.changeAgent(ctx, a.Name, func(cur *Agent) (bool, error) { cur.StopUnlisted = false; return true, nil })
}

// StartAgent makes a stopped agent active again. Its credentials stay deleted: it gets new keys. Its stop entry is
// removed (a new list generation); the stop policy then lets the agent's new keys through.
func (s *KeyService) StartAgent(ctx context.Context, by Session, name string) (Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.agentsOn(); err != nil {
		return Agent{}, err
	}
	ctx, cancel, err := s.opCtx(ctx)
	if err != nil {
		return Agent{}, err
	}
	defer cancel()
	actor, err := adminEmail(by)
	if err != nil {
		return Agent{}, err
	}
	a, err := s.GetAgent(ctx, name)
	if err != nil {
		return Agent{}, err
	}
	if !(by.Role == RoleAdmin || canManage(by, a, actor) == nil) {
		return Agent{}, ErrNoAgent
	}
	if a.State == StateActive {
		return a, nil
	}
	if a.State != StateStopped {
		return a, ErrNotStopped
	}
	if blocked, err := s.blockedEmails(ctx); err != nil {
		return a, err
	} else if blocked[a.Owner] {
		return a, ErrOffboarded
	}
	if _, err := s.changeStopList(ctx, func(l *controller.StopList) (bool, bool) {
		i := entryIndex(*l, name)
		if i < 0 {
			return false, false
		}
		l.Entries = slices.Delete(l.Entries, i, i+1)
		return true, false
	}); err != nil {
		return a, err
	}
	a, err = s.changeAgent(ctx, name, func(cur *Agent) (bool, error) {
		cur.State, cur.StoppedAt, cur.StopRequestedAt, cur.StopNotConfirmedAt, cur.StopUnlisted = StateActive, nil, nil, nil, false
		return true, nil
	})
	if err == nil {
		s.event(ctx, actor, "agent-start", "", a.Owner, "agent="+a.Name)
	}
	return a, err
}

func mapKeys[K comparable, V any](m map[K]V) func(yield func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// AdvanceStops moves every stopping agent on (S2, S3, S6, the not-confirmed report), prunes entries that are done, and
// stops itself-mode agents whose owner has been gone for agents.orphanDays. The leader calls it every few seconds. It
// returns whether more work remains (an agent still stopping or an entry waiting to be pruned).
func (s *KeyService) AdvanceStops(ctx context.Context) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel, err := s.opCtx(ctx)
	if err != nil {
		return false, err
	}
	defer cancel()
	agents, err := s.AllAgents(ctx)
	if err != nil {
		return false, err
	}
	now := s.now()
	var errs []error
	more := false
	byName := map[string]Agent{}
	for _, a := range agents {
		if a.State == StateActive && a.Mode == ModeItself && a.OrphanedAt != nil && now.Sub(*a.OrphanedAt) >= time.Duration(s.orphanDays())*24*time.Hour {
			slog.Warn("an itself-mode agent whose owner left is stopped", "agent", a.Name, "since", a.OrphanedAt)
			s.event(ctx, consoleActor, "agent-orphan-stop", "", a.Owner, "agent="+a.Name)
			if a, err = s.stopLocked(ctx, consoleActor, a); err != nil {
				errs = append(errs, err)
			}
		} else if a.State == StateStopping {
			if a, err = s.advanceLocked(ctx, a); err != nil {
				errs = append(errs, err)
			}
		}
		byName[a.Name] = a
		more = more || a.State == StateStopping
	}
	// Entries whose agent is gone (deleted) or stopped for long enough are pruned. An entry of a stopping agent stays.
	ack, have, err := s.StopAck(ctx)
	if err != nil {
		errs = append(errs, err)
		return true, errors.Join(errs...)
	}
	pruned := 0
	if _, err := s.changeStopList(ctx, func(l *controller.StopList) (bool, bool) {
		pruned = 0
		keep := l.Entries[:0:0]
		for _, e := range l.Entries {
			a, ok := byName[e.Name]
			done := e.KeysDeletedAt != nil && acknowledged(ack, have, e.Gen) && now.Sub(*e.KeysDeletedAt) >= s.stopRetention() &&
				(e.GrantsRevokedAt == nil || now.Sub(*e.GrantsRevokedAt) >= s.stopRetention())
			switch {
			case !ok && done: // the agent was deleted: its keys are gone and the name is never used again
				pruned++
			case ok && a.State == StateStopped && done:
				pruned++
			default:
				keep = append(keep, e)
			}
		}
		l.Entries = keep
		return pruned > 0, false
	}); err != nil {
		errs = append(errs, err)
	}
	if l, _, err := s.stopList(ctx); err == nil {
		more = more || len(l.Entries) > 0 // entries wait for their retention
	} else {
		errs = append(errs, err)
	}
	return more, errors.Join(errs...)
}

// OffboardAgents is the agent part of offboarding a person (after their keys): their person-mode agents are stopped
// with the state machine, and the itself-mode agents they own start their orphan clock. Idempotent; the identity
// provider retries until it succeeds.
func (s *KeyService) OffboardAgents(ctx context.Context, email, trigger string) (stopped, orphaned int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel, err := s.opCtx(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer cancel()
	email, ok := identity.CanonicalEmail(email)
	if !ok {
		return 0, 0, ErrBadInput
	}
	return s.stopAgentsOf(ctx, email, trigger)
}

// stopAgentsOf stops the person-mode agents of an offboarded person and starts the clock for the itself-mode agents they
// own (they keep working for agents.orphanDays; an admin moves them, or they are stopped). Call it holding s.mu, in a
// term. It returns how many agents it stopped.
func (s *KeyService) stopAgentsOf(ctx context.Context, email, trigger string) (stopped, orphaned int, err error) {
	agents, err := s.AllAgents(ctx)
	if err != nil {
		return 0, 0, err
	}
	var errs []error
	now := s.now()
	for _, a := range agents {
		switch {
		case a.Mode == ModePerson && a.Owner == email:
			if a.State == StateActive || a.State == StateStopping {
				if _, err := s.stopLocked(ctx, trigger, a); err != nil {
					errs = append(errs, err)
				} else {
					stopped++
				}
			}
		case a.Mode == ModeItself && a.Owner == email && a.OrphanedAt == nil && a.State != StateDeleting:
			if _, err := s.changeAgent(ctx, a.Name, func(cur *Agent) (bool, error) {
				if cur.Owner != email || cur.OrphanedAt != nil {
					return false, nil
				}
				cur.OrphanedAt = &now
				return true, nil
			}); err != nil {
				errs = append(errs, err)
				continue
			}
			orphaned++
			s.event(ctx, trigger, "agent-orphaned", "", email, fmt.Sprintf("agent=%s stops in %d days unless an admin moves it", a.Name, s.orphanDays()))
		}
	}
	return stopped, orphaned, errors.Join(errs...)
}

// TestListOnly puts an agent on the stop list, or takes it off, without touching its keys or record. Only the acceptance
// test uses it (AMBIT_TEST_HOOKS): it shows that the gateway's stop policy alone refuses a key that is still valid.
func (s *KeyService) TestListOnly(ctx context.Context, name string, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel, err := s.opCtx(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	now := s.now()
	_, err = s.changeStopList(ctx, func(l *controller.StopList) (bool, bool) {
		i := entryIndex(*l, name)
		switch {
		case on && i < 0:
			l.Entries = append(l.Entries, controller.StopEntry{Kind: "agent", Name: name, Gen: l.Gen + 1, RequestedAt: now})
			return true, false
		case !on && i >= 0:
			l.Entries = slices.Delete(l.Entries, i, i+1)
			return true, false
		}
		return false, false
	})
	return err
}

// TestFillStopList replaces the filler entries (names filler-NNNN) with n new ones; the acceptance test checks that a list
// of more than 150 entries is split into several policies and still refuses.
func (s *KeyService) TestFillStopList(ctx context.Context, n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel, err := s.opCtx(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	now := s.now()
	_, err = s.changeStopList(ctx, func(l *controller.StopList) (bool, bool) {
		l.Entries = slices.DeleteFunc(l.Entries, func(e controller.StopEntry) bool { return strings.HasPrefix(e.Name, "filler-") })
		for i := 0; i < n; i++ {
			l.Entries = append(l.Entries, controller.StopEntry{Kind: "agent", Name: fmt.Sprintf("filler-%04d", i), Gen: l.Gen + 1, RequestedAt: now})
		}
		return true, false
	})
	return err
}
