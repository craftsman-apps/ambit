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

// freeSlot is the lowest slot no entry has, or -1 when the list is full.
func freeSlot(l controller.StopList) int {
	used := make([]bool, controller.MaxStopEntries)
	for _, e := range l.Entries {
		if e.Slot >= 0 && e.Slot < len(used) {
			used[e.Slot] = true
		}
	}
	if i := slices.Index(used, false); i >= 0 && len(l.Entries) < controller.MaxStopEntries {
		return i
	}
	return -1
}

// addEntry puts name on the list (S1) if it is not there; listed is false when the list is full. Call it holding s.mu.
func (s *KeyService) addEntry(ctx context.Context, name string) (listed bool, err error) {
	now := s.now()
	_, err = s.changeStopList(ctx, func(l *controller.StopList) (bool, bool) {
		listed = true
		if entryIndex(*l, name) >= 0 {
			return false, false
		}
		slot := freeSlot(*l)
		if slot < 0 {
			listed = false
			return false, false
		}
		l.Entries = append(l.Entries, controller.StopEntry{Kind: "agent", Name: name, Gen: l.Gen + 1, Slot: slot, RequestedAt: now})
		return true, false
	})
	return listed, err
}

// StopAck reads the controller's acknowledgment; ok is false when there is none or it does not parse.
func (s *KeyService) StopAck(ctx context.Context) (controller.StopAck, bool, error) {
	var cm corev1.ConfigMap
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: controller.StopsAckConfigMap}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return controller.StopAck{}, false, nil
		}
		return controller.StopAck{}, false, err
	}
	a, ok := controller.ParseStopAck(&cm)
	return a, ok, nil
}

// acknowledged: the answer is for this list (its UID), renders, and every Gateway the controller knows is ready right now
// at generation gen or later. No answer: not acknowledged. A controller that found no route to attach to answers with no
// Gateways: nothing can be reached, so the stop is complete.
func acknowledged(a controller.StopAck, have bool, listUID string, gen int64) bool {
	if !have || a.Degraded != "" || a.ListUID != listUID || a.Gen < gen {
		return false
	}
	for _, g := range a.Gateways {
		if !g.Ready || g.AckedGen < gen {
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

// stopLocked records the stop first (state stopping: no key can be issued any more, and the loop repeats every step from
// the record), then runs S1 to S3. Idempotent. Call it holding s.mu, in a term.
func (s *KeyService) stopLocked(ctx context.Context, actor string, a Agent) (Agent, error) {
	switch a.State {
	case StateDeleting, StatePending:
		return a, ErrAgentNotActive
	case StateStopped:
		return a, nil
	}
	now := s.now()
	s.stopWork.Store(true)
	first := a.State != StateStopping
	a, err := s.changeAgent(ctx, a.Name, func(cur *Agent) (bool, error) {
		if cur.State == StateStopped || cur.State == StateDeleting {
			return false, nil
		}
		changed := cur.State != StateStopping
		cur.State = StateStopping
		if cur.StopRequestedAt == nil {
			cur.StopRequestedAt, changed = &now, true
		}
		return changed, nil
	})
	if err != nil {
		return a, err
	}
	if first {
		s.event(ctx, actor, "agent-stop", "", a.Owner, "agent="+a.Name)
	}
	return s.advanceLocked(ctx, a)
}

// deleteAgentKeySecrets deletes every credential Secret that names the agent, decodable by the console or not (the
// gateway reads the Secret's own data), with a UID precondition, and checks that none is left.
func (s *KeyService) deleteAgentKeySecrets(ctx context.Context, name, actor string) error {
	secs, err := s.keySecrets(ctx, client.MatchingLabels{keyLabel: "true", agentLabel: name})
	if err != nil {
		return err
	}
	var errs []error
	for i := range secs {
		sec := &secs[i]
		uid := sec.UID
		if err := s.Client.Delete(ctx, sec, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
			continue
		}
		s.event(ctx, actor, "agent-stop-key", strings.TrimPrefix(sec.Name, "ambit-key-"), sec.Annotations[annPrefix+"owner-email"], "agent="+name)
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%w: %w", ErrDeleteIncomplete, err)
	}
	left, err := s.keySecrets(ctx, client.MatchingLabels{keyLabel: "true", agentLabel: name})
	if err != nil {
		return err
	}
	if len(left) > 0 { // deleted but still there (a finalizer): the credential may still work
		return fmt.Errorf("%w: %d key Secrets of the agent are still there", ErrDeleteIncomplete, len(left))
	}
	return nil
}

// advanceLocked repeats S1, S2 and S3 and, once the controller acknowledged the entry on every Gateway (S5), does S6.
// Call it holding s.mu, in a term.
func (s *KeyService) advanceLocked(ctx context.Context, a Agent) (Agent, error) {
	if a.State != StateStopping {
		return a, nil
	}
	now := s.now()
	// S1 again: the entry is on the list (a full list is not an error: the credentials go all the same).
	listed, err := s.addEntry(ctx, a.Name)
	if err != nil {
		return a, err
	}
	if !listed != a.StopUnlisted {
		if a, err = s.changeAgent(ctx, a.Name, func(cur *Agent) (bool, error) { cur.StopUnlisted = !listed; return true, nil }); err != nil {
			return a, err
		}
		if !listed {
			slog.Error("stop list full: the agent is stopped without a list entry until there is room", "agent", a.Name)
			s.event(ctx, consoleActor, "agent-stop-unlisted", "", a.Owner, "agent="+a.Name+" the stop list is full; the credentials are deleted, tokens expire on their own")
		}
	}
	// S2: every key of the agent (also one made while it is stopping, and one the console cannot decode).
	if err := s.deleteAgentKeySecrets(ctx, a.Name, consoleActor); err != nil {
		return a, err
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
	ack, have, err := s.StopAck(ctx)
	if err != nil {
		return a, err
	}
	_, listCM, err := s.stopList(ctx)
	if err != nil {
		return a, err
	}
	switch {
	case entryIndex(l, a.Name) < 0: // not listed (the list is full): not confirmed until there is room
	case acknowledged(ack, have, string(listCM.UID), gen):
		return s.finishStop(ctx, a, ack, now)
	}
	return s.notConfirmed(ctx, a, ack, have, gen, now)
}

// finishStop is S6: the agent is stopped. The times each Gateway acknowledged are kept in the record, because the stop
// list entry is pruned later.
func (s *KeyService) finishStop(ctx context.Context, a Agent, ack controller.StopAck, now time.Time) (Agent, error) {
	acks := map[string]time.Time{}
	for g, v := range ack.Gateways {
		acks[g] = v.At.UTC()
	}
	stopped, err := s.changeAgent(ctx, a.Name, func(cur *Agent) (bool, error) {
		if cur.State != StateStopping {
			return false, nil
		}
		cur.State, cur.StoppedAt, cur.StopAcks = StateStopped, &now, acks
		return true, nil
	})
	if err != nil {
		return stopped, err
	}
	if stopped.State == StateStopped {
		parts := []string{}
		for _, g := range slices.Sorted(mapKeys(acks)) {
			parts = append(parts, g+"="+acks[g].Format(time.RFC3339))
		}
		s.event(ctx, consoleActor, "agent-stopped", "", stopped.Owner, "agent="+stopped.Name+" acked "+strings.Join(parts, " "))
		_, _ = s.changeStopList(ctx, func(l *controller.StopList) (bool, bool) {
			i := entryIndex(*l, a.Name)
			if i < 0 || l.Entries[i].AckedAt != nil {
				return false, false
			}
			l.Entries[i].AckedAt = &now
			return false, true
		})
	}
	return stopped, nil
}

// notConfirmed reports a stop that no Gateway confirmed 60 s after the request: once, with the reason.
func (s *KeyService) notConfirmed(ctx context.Context, a Agent, ack controller.StopAck, have bool, gen int64, now time.Time) (Agent, error) {
	if a.StopRequestedAt == nil || a.StopNotConfirmedAt != nil || now.Sub(*a.StopRequestedAt) <= controller.StopAckLag {
		return a, nil
	}
	var late []string
	for g, v := range ack.Gateways {
		if !v.Ready || v.AckedGen < gen {
			late = append(late, g)
		}
	}
	slices.Sort(late)
	why := "no answer from the controller"
	switch {
	case a.StopUnlisted:
		why = "the stop list is full"
	case ack.Degraded != "":
		why = "the stop list was not rendered: " + ack.Degraded
	case len(late) > 0:
		why = "not confirmed on " + strings.Join(late, ", ")
	case have:
		why = "the answer of the controller is for another list or generation"
	}
	slog.Error("stop not confirmed", "agent", a.Name, "why", why)
	a, err := s.changeAgent(ctx, a.Name, func(cur *Agent) (bool, error) {
		if cur.StopNotConfirmedAt != nil {
			return false, nil
		}
		cur.StopNotConfirmedAt = &now
		return true, nil
	})
	if err == nil {
		s.event(ctx, consoleActor, "agent-stop-not-confirmed", "", a.Owner, "agent="+a.Name+" "+why)
	}
	return a, err
}

// ErrStartTooSoon: the agent's keys were deleted only a moment ago, and the gateways may still know them.
var ErrStartTooSoon = errors.New("the stop is too recent to start the agent again: the gateways may still know its old keys; try again in a minute")

// startWait is how long after S2 a stopped agent cannot be started: enough for the proxies to drop deleted keys; with
// OAuth (RevokeGrants) the whole retention, because an access token already issued stays valid until it expires.
func (s *KeyService) startWait() time.Duration {
	if s.RevokeGrants != nil {
		return s.stopRetention()
	}
	return time.Minute
}

// StartAgent makes a stopped agent active again. Its credentials stay deleted: it gets new keys. Its stop entry is
// removed (a new list generation) once the old credentials cannot work any more; the stop policy then lets the agent's new
// keys through.
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
	l, _, err := s.stopList(ctx)
	if err != nil {
		return a, err
	}
	if i := entryIndex(l, name); i >= 0 {
		e := l.Entries[i]
		since := e.KeysDeletedAt
		if e.GrantsRevokedAt != nil && (since == nil || e.GrantsRevokedAt.After(*since)) {
			since = e.GrantsRevokedAt
		}
		if since == nil || s.now().Sub(*since) < s.startWait() {
			return a, ErrStartTooSoon
		}
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
		cur.State, cur.StoppedAt, cur.StopRequestedAt, cur.StopNotConfirmedAt, cur.StopUnlisted, cur.StopAcks = StateActive, nil, nil, nil, false, nil
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

// AdvanceStops moves every stopping agent on (S1 to S3, S6, the not-confirmed report), finishes the credentials of
// deleted agents that are still on the list, prunes entries that are done, and stops itself-mode agents whose owner has
// been gone for agents.orphanDays. The leader calls it every few seconds. It returns whether more work remains.
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
		var err error
		if a.State == StateActive && a.Mode == ModeItself && a.OrphanedAt != nil && now.Sub(*a.OrphanedAt) >= time.Duration(s.orphanDays())*24*time.Hour {
			slog.Warn("an itself-mode agent whose owner left is stopped", "agent", a.Name, "since", a.OrphanedAt)
			s.event(ctx, consoleActor, "agent-orphan-stop", "", a.Owner, "agent="+a.Name)
			a, err = s.stopLocked(ctx, consoleActor, a)
		} else if a.State == StateStopping {
			a, err = s.advanceLocked(ctx, a)
		}
		if err != nil {
			errs = append(errs, err)
		}
		byName[a.Name] = a
		more = more || a.State == StateStopping
	}
	ack, have, err := s.StopAck(ctx)
	if err != nil {
		errs = append(errs, err)
		return true, errors.Join(errs...)
	}
	l, listCM, err := s.stopList(ctx)
	if err != nil {
		errs = append(errs, err)
		return true, errors.Join(errs...)
	}
	// An entry of an agent that was deleted before its credentials were: finish them from the entry.
	for _, e := range l.Entries {
		if _, ok := byName[e.Name]; !ok && e.Kind == "agent" && e.KeysDeletedAt == nil && !strings.HasPrefix(e.Name, "filler-") {
			if err := s.deleteAgentKeySecrets(ctx, e.Name, consoleActor); err != nil {
				errs = append(errs, err)
				continue
			}
			if _, err := s.changeStopList(ctx, func(l *controller.StopList) (bool, bool) {
				if i := entryIndex(*l, e.Name); i >= 0 && l.Entries[i].KeysDeletedAt == nil {
					l.Entries[i].KeysDeletedAt = &now
					return false, true
				}
				return false, false
			}); err != nil {
				errs = append(errs, err)
			}
		}
	}
	// Entries whose agent is gone (deleted) or stopped for long enough are pruned: the credentials are gone, every Gateway
	// acknowledged the entry, and the retention has passed since the later of S2, S3 and the acknowledgment.
	pruned := 0
	if _, err := s.changeStopList(ctx, func(l *controller.StopList) (bool, bool) {
		pruned = 0
		keep := l.Entries[:0:0]
		for _, e := range l.Entries {
			a, ok := byName[e.Name]
			since := e.KeysDeletedAt
			for _, t := range []*time.Time{e.GrantsRevokedAt, e.AckedAt} {
				if t != nil && since != nil && t.After(*since) {
					since = t
				}
			}
			done := since != nil && acknowledged(ack, have, string(listCM.UID), e.Gen) && now.Sub(*since) >= s.stopRetention() &&
				(e.AckedAt != nil || !ok)
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
func (s *KeyService) OffboardAgents(ctx context.Context, email, trigger string) (requested, stopped, orphaned int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel, err := s.opCtx(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	defer cancel()
	email, ok := identity.CanonicalEmail(email)
	if !ok {
		return 0, 0, 0, ErrBadInput
	}
	return s.stopAgentsOf(ctx, email, trigger)
}

// stopAgentsOf stops the person-mode agents of an offboarded person and starts the clock for the itself-mode agents they
// own (they keep working for agents.orphanDays; an admin moves them, or they are stopped). Call it holding s.mu, in a
// term. It returns how many agents it stopped.
func (s *KeyService) stopAgentsOf(ctx context.Context, email, trigger string) (requested, stopped, orphaned int, err error) {
	agents, err := s.AllAgents(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	var errs []error
	now := s.now()
	for _, a := range agents {
		switch {
		case a.Mode == ModePerson && a.Owner == email:
			if a.State == StateActive || a.State == StateStopping {
				// requested: the stop goes on (and is confirmed by the loop) even when a step reports an error now
				requested++
				got, err := s.stopLocked(ctx, trigger, a)
				if err != nil {
					errs = append(errs, err)
				}
				if got.State == StateStopped {
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
	return requested, stopped, orphaned, errors.Join(errs...)
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
			l.Entries = append(l.Entries, controller.StopEntry{Kind: "agent", Name: name, Gen: l.Gen + 1, Slot: freeSlot(*l), RequestedAt: now})
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
			l.Entries = append(l.Entries, controller.StopEntry{Kind: "agent", Name: fmt.Sprintf("filler-%04d", i), Gen: l.Gen + 1, Slot: freeSlot(*l), RequestedAt: now})
		}
		return true, false
	})
	return err
}
