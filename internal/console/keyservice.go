package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/craftsman-apps/ambit/internal/identity"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	ErrKeysOff    = errors.New("keys are switched off")
	ErrNotLeader  = errors.New("this console is not the key writer; try again in a few seconds")
	ErrOffboarded = errors.New("this person is offboarded")
	ErrTooMany    = errors.New("a person may have at most 20 keys")
	ErrTooFast    = errors.New("at most 10 keys per hour")
	ErrBadInput   = errors.New("invalid key request")
	ErrNotFound   = errors.New("no such key")
)

const (
	maxKeysPerOwner = 20
	defaultPerHour  = 10
	consoleActor    = "ambit-console"
)

var keyIDPattern = regexp.MustCompile(`^[a-z2-7]{12}$`)

// termBackoff spaces the start-of-term repair retries (1 s → 30 s); tests shorten it.
var termBackoff = wait.Backoff{Duration: time.Second, Factor: 2, Cap: 30 * time.Second, Steps: math.MaxInt32}

// KeyService owns every change to personal keys, owner records and key events. Every write runs under mu and
// inside the current leadership term (opCtx), and re-reads what it depends on after taking the lock.
type KeyService struct {
	Client    client.Client
	Namespace string
	Settings  KeySettings
	Now       func() time.Time
	PerHour   int // creations per person per hour; 0 means 10

	mu      sync.Mutex
	term    context.Context // live while this console holds the key Lease; read and written only under mu
	created map[string][]time.Time
	seq     uint64 // orders events recorded within the same second
}

type RepairReport struct{ Deleted, Updated int }

func (s *KeyService) now() time.Time { return s.Now().UTC().Truncate(time.Second) }

// StartTerm begins a leadership term: under the lock it repairs every key (retrying until the listings succeed
// or lead ends; once capped, retries repeat every 30 s) and only then, if lead is still live, records changed
// settings and accepts writes until lead ends. Errors on single objects are logged and left to the hourly repair.
func (s *KeyService) StartTerm(lead context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := lead.Err(); err != nil { // a stale start must not touch a newer term
		return err
	}
	if s.term != nil && s.term.Err() != nil {
		s.term = nil
	}
	var last error
	err := termBackoff.DelayFunc().Until(lead, true, false, func(ctx context.Context) (bool, error) {
		_, objErrs, err := s.repairLocked(ctx)
		if last = err; err != nil {
			slog.Warn("key repair at the start of a term failed; retrying", "err", err)
			return false, nil
		}
		if objErrs != nil {
			slog.Error("key repair at the start of a term left some objects unrepaired; the hourly repair retries them", "err", objErrs)
		}
		return true, nil
	})
	if err == nil {
		err = lead.Err()
	}
	if err != nil {
		return errors.Join(err, last)
	}
	s.recordSettings(lead)
	s.term = lead
	return nil
}

// recordSettings records a settings-changed event when the settings differ from the last recorded ones.
// Call it holding s.mu.
func (s *KeyService) recordSettings(ctx context.Context) {
	cur := fmt.Sprintf("enabled=%t inactivityDays=%d maxDays=%d", s.Settings.Enabled, s.Settings.InactivityDays, s.Settings.MaxDays)
	ev, err := s.Events(ctx)
	if err != nil {
		slog.Error("key settings: events unreadable; recording the settings again", "err", err)
	}
	for _, e := range ev { // newest first
		if e.Action == "settings-changed" {
			if e.Detail == cur {
				return
			}
			break
		}
	}
	s.event(ctx, consoleActor, "settings-changed", "", "", cur)
}

// Writable reports whether this console may change keys now. It takes the lock; never call it while holding it.
func (s *KeyService) Writable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.term != nil && s.term.Err() == nil
}

// EndTerm ends the current term; the leader calls it when the Lease is lost.
func (s *KeyService) EndTerm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.term = nil
}

// EndTermForTest ends the current term as if the Lease were lost.
func (s *KeyService) EndTermForTest() { s.EndTerm() }

// opCtx returns a context cancelled by either the request or the end of the term. Call it holding s.mu.
func (s *KeyService) opCtx(req context.Context) (context.Context, context.CancelFunc, error) {
	if s.term == nil || s.term.Err() != nil {
		return nil, nil, ErrNotLeader
	}
	ctx, cancel := context.WithCancel(req)
	stop := context.AfterFunc(s.term, cancel)
	return ctx, func() { stop(); cancel() }, nil
}

func (s *KeyService) Create(ctx context.Context, sess Session, name string, scope Scope, days int) (string, Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.Settings.Enabled {
		return "", Key{}, ErrKeysOff
	}
	ctx, cancel, err := s.opCtx(ctx)
	if err != nil {
		return "", Key{}, err
	}
	defer cancel()
	email, ok := identity.CanonicalEmail(sess.Email)
	name = strings.TrimSpace(name)
	if !ok || name == "" || len(name) > 100 || (scope != ScopeLLM && scope != ScopeMCP && scope != ScopeBoth) ||
		!slices.Contains([]int{30, 90, 365}, days) || days > s.Settings.MaxDays || sess.SignedIn == 0 {
		return "", Key{}, ErrBadInput
	}
	if blocked, err := s.blockedEmails(ctx); err != nil {
		return "", Key{}, err
	} else if blocked[email] {
		return "", Key{}, ErrOffboarded
	}
	now := s.now()
	recent := s.recent(email, now)
	if len(recent) >= s.perHour() {
		return "", Key{}, ErrTooFast
	}
	keys, err := s.list(ctx, email)
	if err != nil {
		return "", Key{}, err
	}
	live := 0
	for _, k := range keys {
		if k.Until.After(now) {
			live++
		}
	}
	if live >= maxKeysPerOwner {
		return "", Key{}, ErrTooMany
	}
	signed := time.Unix(sess.SignedIn, 0).UTC()
	o, _, err := s.changeOwner(ctx, email, false, func(o *Owner, exists bool) (bool, error) { // owner record first
		if o.Offboarded {
			return false, ErrOffboarded
		}
		if !exists || signed.After(o.SignedIn) {
			o.Groups, o.SignedIn = sess.Groups, signed
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return "", Key{}, err
	}
	plain, prefix, hash, err := NewSecretKey()
	if err != nil {
		return "", Key{}, err
	}
	id, err := newKeyID()
	if err != nil {
		return "", Key{}, err
	}
	ceiling := now.AddDate(0, 0, days)
	k := Key{ID: id, Name: name, Prefix: prefix, Email: email, Groups: o.Groups, Scope: scope, Created: now,
		Expires: ceiling, SignedIn: o.SignedIn, Ceiling: ceiling, Until: until(ceiling, o.SignedIn, s.Settings.InactivityDays), Hash: hash}
	sec := k.Secret(s.Namespace)
	if err := s.Client.Create(ctx, sec); err != nil {
		return "", Key{}, err
	}
	k.UID, k.ResourceVersion = string(sec.UID), sec.ResourceVersion
	s.created[email] = append(recent, now)
	s.event(ctx, email, "create", k.ID, email, "")
	return plain, k, nil
}

func (s *KeyService) perHour() int {
	if s.PerHour > 0 {
		return s.PerHour
	}
	return defaultPerHour
}

// recent returns the person's creation times within the last hour. Call it holding s.mu.
func (s *KeyService) recent(email string, now time.Time) []time.Time {
	if s.created == nil {
		s.created = map[string][]time.Time{}
	}
	keep := s.created[email][:0]
	for _, t := range s.created[email] {
		if now.Sub(t) < time.Hour {
			keep = append(keep, t)
		}
	}
	s.created[email] = keep
	return keep
}

// List returns one person's keys, oldest first.
// Reads take no lock: they only query the API server and must not wait behind a repair.
func (s *KeyService) List(ctx context.Context, email string) ([]Key, error) {
	email, ok := identity.CanonicalEmail(email)
	if !ok {
		return nil, ErrBadInput
	}
	return s.list(ctx, email)
}

// ListAll returns every readable key, oldest first.
func (s *KeyService) ListAll(ctx context.Context) ([]Key, error) {
	secs, err := s.keySecrets(ctx, client.MatchingLabels{keyLabel: "true"})
	if err != nil {
		return nil, err
	}
	return decodeKeys(secs, ""), nil
}

func (s *KeyService) list(ctx context.Context, email string) ([]Key, error) {
	secs, err := s.keySecrets(ctx, client.MatchingLabels{keyLabel: "true", keyOwnerLabel: ownerHash(email)})
	if err != nil {
		return nil, err
	}
	return decodeKeys(secs, email), nil
}

// keySecrets is the only Secret List in this file; every caller passes a selector with keyLabel.
func (s *KeyService) keySecrets(ctx context.Context, sel client.MatchingLabels) ([]corev1.Secret, error) {
	var l corev1.SecretList
	if err := s.Client.List(ctx, &l, client.InNamespace(s.Namespace), sel); err != nil {
		return nil, err
	}
	return l.Items, nil
}

// decodeKeys skips unreadable Secrets (the repair deletes them) and, when email is set, keys of anyone else.
func decodeKeys(secs []corev1.Secret, email string) []Key {
	var keys []Key
	for i := range secs {
		k, err := KeyFromSecret(&secs[i])
		if err != nil || (email != "" && k.Email != email) {
			continue
		}
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b Key) int {
		if c := a.Created.Compare(b.Created); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return keys
}

func (s *KeyService) owner(ctx context.Context, email string) (Owner, error) {
	var sec corev1.Secret
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: "ambit-key-owner-" + ownerHash(email)}, &sec); err != nil {
		return Owner{}, err
	}
	o, err := OwnerFromSecret(&sec)
	if err != nil { // the version is kept so OffboardKeys can overwrite the record
		return Owner{Email: email, UID: string(sec.UID), ResourceVersion: sec.ResourceVersion}, fmt.Errorf("%w: %v", errBadOwner, err)
	}
	return o, nil
}

var errBadOwner = errors.New("owner record unreadable")

// changeOwner re-reads the owner record, lets rule change it (exists is false when there is none yet) and writes
// it when rule says so, version-checked, retrying on conflict. With replaceBad, an undecodable record is handed
// to rule as a fresh record that overwrites it; otherwise it is an error.
func (s *KeyService) changeOwner(ctx context.Context, email string, replaceBad bool, rule func(o *Owner, exists bool) (bool, error)) (Owner, bool, error) {
	var out Owner
	var wrote bool
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		o, err := s.owner(ctx, email)
		exists := err == nil
		if apierrors.IsNotFound(err) {
			o = Owner{Email: email}
		} else if replaceBad && errors.Is(err, errBadOwner) {
			exists = true // o is {email} with the bad record's version
		} else if err != nil {
			return err
		}
		write, err := rule(&o, exists)
		out, wrote = o, false
		if err != nil || !write {
			return err
		}
		sec := o.Secret(s.Namespace)
		if exists {
			err = s.Client.Update(ctx, sec)
		} else {
			sec.UID, sec.ResourceVersion = "", ""
			err = s.Client.Create(ctx, sec)
		}
		if err == nil {
			out.UID, out.ResourceVersion, wrote = string(sec.UID), sec.ResourceVersion, true
		}
		return err
	})
	return out, wrote, err
}

// putOwner stores o as the owner record (create or update).
func (s *KeyService) putOwner(ctx context.Context, o Owner) error {
	_, _, err := s.changeOwner(ctx, o.Email, false, func(cur *Owner, _ bool) (bool, error) {
		uid, rv := cur.UID, cur.ResourceVersion
		*cur = o
		cur.UID, cur.ResourceVersion = uid, rv
		return true, nil
	})
	return err
}

// putKey stores k (create or update over the current version).
func (s *KeyService) putKey(ctx context.Context, k Key) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var cur corev1.Secret
		err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: "ambit-key-" + k.ID}, &cur)
		sec := k.Secret(s.Namespace)
		if apierrors.IsNotFound(err) {
			sec.UID, sec.ResourceVersion = "", ""
			return s.Client.Create(ctx, sec)
		} else if err != nil {
			return err
		}
		sec.UID, sec.ResourceVersion = cur.UID, cur.ResourceVersion
		return s.Client.Update(ctx, sec)
	})
}

func (s *KeyService) getKey(ctx context.Context, id string) (Key, error) {
	var sec corev1.Secret
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: "ambit-key-" + id}, &sec); err != nil {
		return Key{}, err
	}
	if sec.Labels[keyLabel] != "true" {
		return Key{}, ErrNotFound
	}
	return KeyFromSecret(&sec)
}

// deleteKey deletes k only if it is still the version that was read, then records the event.
func (s *KeyService) deleteKey(ctx context.Context, k Key, action, actor string) error {
	uid, rv := typesUID(k.UID), k.ResourceVersion
	if err := s.Client.Delete(ctx, k.Secret(s.Namespace), client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil {
		return err
	}
	s.event(ctx, actor, action, k.ID, k.Email, "")
	return nil
}

type keyOp int

const (
	keyKeep keyOp = iota
	keyUpdate
	keyDelete
)

// changeKey re-reads key id, lets rule decide (keep, update in place, or delete as delAction) and writes with
// version checks, retrying on conflict. A key that is gone returns a NotFound error.
func (s *KeyService) changeKey(ctx context.Context, id string, rule func(k *Key) (keyOp, error), delAction, actor string) (keyOp, error) {
	var op keyOp
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		op = keyKeep
		k, err := s.getKey(ctx, id)
		if err != nil {
			return err
		}
		if op, err = rule(&k); err != nil {
			return err
		}
		switch op {
		case keyUpdate:
			return s.Client.Update(ctx, k.Secret(s.Namespace))
		case keyDelete:
			return s.deleteKey(ctx, k, delAction, actor)
		}
		return nil
	})
	return op, err
}

// applyOwner brings keys (all of o's) in line with the owner record and the settings: keys past until are
// deleted, ceilings above maxDays lowered, groups and sign-in copied. A refresh — or a repair of a key older than
// the owner record (an interrupted refresh) — recomputes until; otherwise until is never extended.
func (s *KeyService) applyOwner(ctx context.Context, o Owner, keys []Key, refresh bool) (updated, deleted int, err error) {
	now := s.now()
	var errs []error
	for _, k := range keys {
		op, err := s.changeKey(ctx, k.ID, func(k *Key) (keyOp, error) {
			if !k.Until.After(now) {
				return keyDelete, nil
			}
			return s.recompute(k, o, refresh), nil
		}, "cleanup-delete", consoleActor)
		if apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			errs = append(errs, err)
			continue
		}
		switch op {
		case keyDelete:
			deleted++
		case keyUpdate:
			updated++
			if !refresh {
				s.event(ctx, consoleActor, "repair", k.ID, o.Email, "")
			}
		}
	}
	return updated, deleted, errors.Join(errs...)
}

func (s *KeyService) recompute(k *Key, o Owner, full bool) keyOp {
	before := *k
	if c := k.Created.AddDate(0, 0, s.Settings.MaxDays); c.Before(k.Ceiling) {
		k.Ceiling = c // lowered for good: a later raise of maxDays never raises it back
	}
	full = full || o.SignedIn.After(k.SignedIn)
	if !o.SignedIn.Before(k.SignedIn) {
		k.Groups, k.SignedIn = o.Groups, o.SignedIn
	}
	u := until(k.Ceiling, k.SignedIn, s.Settings.InactivityDays)
	if !full && u.After(k.Until) {
		u = k.Until
	}
	k.Until = u
	if slices.Equal(before.Groups, k.Groups) && before.SignedIn.Equal(k.SignedIn) && before.Ceiling.Equal(k.Ceiling) && before.Until.Equal(k.Until) {
		return keyKeep
	}
	return keyUpdate
}

// Refresh copies a verified sign-in's groups onto the owner record, then onto the owner's keys. An older
// sign-in never overwrites a newer one; keys already past until are deleted, never extended; offboarded people
// are left alone.
func (s *KeyService) Refresh(ctx context.Context, sess Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel, err := s.opCtx(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	email, ok := identity.CanonicalEmail(sess.Email)
	if !ok {
		return nil
	}
	if blocked, err := s.blockedEmails(ctx); err != nil {
		return err
	} else if blocked[email] {
		return nil
	}
	signed := time.Unix(sess.SignedIn, 0).UTC()
	o, wrote, err := s.changeOwner(ctx, email, false, func(o *Owner, exists bool) (bool, error) {
		if !exists || o.Offboarded || !signed.After(o.SignedIn) {
			return false, nil // no keys yet, offboarded, or not newer: nothing to refresh
		}
		o.Groups, o.SignedIn = sess.Groups, signed
		return true, nil
	})
	if err != nil || !wrote {
		return err
	}
	keys, err := s.list(ctx, email)
	if err != nil {
		return err
	}
	n, _, err := s.applyOwner(ctx, o, keys, true)
	s.event(ctx, email, "refresh", "", email, "keys="+strconv.Itoa(n))
	return err
}

// OffboardKeys marks the owner record offboarded (creating it if needed), then deletes every key the person has.
// It is idempotent; the identity provider retries it until it succeeds.
func (s *KeyService) OffboardKeys(ctx context.Context, email, trigger string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel, err := s.opCtx(ctx)
	if err != nil {
		return 0, err
	}
	defer cancel()
	email, ok := identity.CanonicalEmail(email)
	if !ok {
		return 0, ErrBadInput
	}
	_, wrote, err := s.changeOwner(ctx, email, true, func(o *Owner, exists bool) (bool, error) {
		if exists && o.Offboarded {
			return false, nil
		}
		o.Offboarded = true
		return true, nil
	})
	if err != nil {
		return 0, err
	}
	if wrote {
		s.event(ctx, trigger, "offboard", "", email, "")
	}
	keys, err := s.list(ctx, email)
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, k := range keys {
		_, err := s.changeKey(ctx, k.ID, func(*Key) (keyOp, error) { return keyDelete, nil }, "offboard-delete", trigger)
		if err == nil {
			n++
		} else if !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	return n, errors.Join(errs...)
}

// Reactivate clears the owner record's offboarded flag so the person may create keys again.
func (s *KeyService) Reactivate(ctx context.Context, email string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel, err := s.opCtx(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	email, ok := identity.CanonicalEmail(email)
	if !ok {
		return ErrBadInput
	}
	_, wrote, err := s.changeOwner(ctx, email, false, func(o *Owner, exists bool) (bool, error) {
		if !exists || !o.Offboarded {
			return false, nil
		}
		o.Offboarded = false
		return true, nil
	})
	if wrote {
		s.event(ctx, consoleActor, "reactivate", "", email, "")
	}
	return err
}

// Revoke deletes a key. Without admin, only the key's owner may; anyone else gets ErrNotFound.
func (s *KeyService) Revoke(ctx context.Context, by Session, id string, admin bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel, err := s.opCtx(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	if !keyIDPattern.MatchString(id) {
		return ErrNotFound
	}
	actor, ok := identity.CanonicalEmail(by.Email)
	if !ok {
		if !admin {
			return ErrNotFound
		}
		actor = by.Email
	}
	_, err = s.changeKey(ctx, id, func(k *Key) (keyOp, error) {
		if !admin && k.Email != actor {
			return keyKeep, ErrNotFound
		}
		return keyDelete, nil
	}, "revoke", actor)
	if apierrors.IsNotFound(err) {
		return ErrNotFound
	}
	return err
}

// ExpireForTest sets a key's until (raising its ceiling if needed). Only the acceptance-test hook calls it.
func (s *KeyService) ExpireForTest(ctx context.Context, id string, u time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel, err := s.opCtx(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	if !keyIDPattern.MatchString(id) {
		return ErrNotFound
	}
	u = u.UTC().Truncate(time.Second)
	_, err = s.changeKey(ctx, id, func(k *Key) (keyOp, error) {
		k.Until = u
		if u.After(k.Ceiling) {
			k.Ceiling = u
		}
		return keyUpdate, nil
	}, "", consoleActor)
	if apierrors.IsNotFound(err) {
		return ErrNotFound
	}
	return err
}

// Repair re-applies the current rules to every key (the hourly run).
func (s *KeyService) Repair(ctx context.Context) (RepairReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel, err := s.opCtx(ctx)
	if err != nil {
		return RepairReport{}, err
	}
	defer cancel()
	rep, objErrs, err := s.repairLocked(ctx)
	return rep, errors.Join(err, objErrs)
}

// repairLocked deletes unreadable keys, keys without an owner record, keys of offboarded people and keys past
// until; brings the rest in line with their owner records; and deletes events past retention. err reports a
// failed listing (nothing was checked); objErrs the objects that could not be repaired. Call it holding s.mu.
func (s *KeyService) repairLocked(ctx context.Context) (rep RepairReport, objErrs, err error) {
	blocked, err := s.blockedEmails(ctx)
	if err != nil {
		return rep, nil, err
	}
	secs, err := s.keySecrets(ctx, client.MatchingLabels{keyLabel: "true"})
	if err != nil {
		return rep, nil, err
	}
	var errs []error
	byOwner := map[string][]Key{}
	for i := range secs {
		k, err := KeyFromSecret(&secs[i])
		if err == nil {
			byOwner[k.Email] = append(byOwner[k.Email], k)
			continue
		}
		if gone, err := s.deleteUnreadable(ctx, secs[i].Name); err != nil {
			errs = append(errs, err)
		} else if gone {
			rep.Deleted++
		}
	}
	emails := make([]string, 0, len(byOwner))
	for e := range byOwner {
		emails = append(emails, e)
	}
	slices.Sort(emails)
	for _, email := range emails {
		keys := byOwner[email]
		o, err := s.owner(ctx, email)
		if err != nil && !apierrors.IsNotFound(err) && !errors.Is(err, errBadOwner) {
			errs = append(errs, err)
			continue
		}
		if err == nil && !o.Offboarded && !blocked[email] {
			u, d, err := s.applyOwner(ctx, o, keys, false)
			rep.Updated += u
			rep.Deleted += d
			if err != nil {
				errs = append(errs, err)
			}
			continue
		}
		for _, k := range keys { // no readable owner record, or offboarded: fail closed
			_, err := s.changeKey(ctx, k.ID, func(*Key) (keyOp, error) { return keyDelete, nil }, "repair-delete", consoleActor)
			if err == nil {
				rep.Deleted++
			} else if !apierrors.IsNotFound(err) {
				errs = append(errs, err)
			}
		}
	}
	if err := s.cleanupEvents(ctx); err != nil {
		errs = append(errs, err)
	}
	return rep, errors.Join(errs...), nil
}

// deleteUnreadable deletes a key-labelled Secret that still cannot be decoded after a re-read.
func (s *KeyService) deleteUnreadable(ctx context.Context, name string) (bool, error) {
	gone := false
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var cur corev1.Secret
		if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: name}, &cur); err != nil {
			return client.IgnoreNotFound(err)
		}
		if _, err := KeyFromSecret(&cur); err == nil || cur.Labels[keyLabel] != "true" {
			return nil // repaired or relabelled meanwhile
		}
		uid, rv := cur.UID, cur.ResourceVersion
		if err := s.Client.Delete(ctx, &cur, client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil {
			return err
		}
		gone = true
		return nil
	})
	if gone {
		s.event(ctx, consoleActor, "repair-delete", strings.TrimPrefix(name, "ambit-key-"), "", "unreadable")
	}
	return gone, err
}

// blockedEmails reads the offboarding records (canonical emails). A missing ConfigMap blocks nobody.
func (s *KeyService) blockedEmails(ctx context.Context) (map[string]bool, error) {
	var cm corev1.ConfigMap
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: OffboardedConfigMap}, &cm); apierrors.IsNotFound(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var emails []string
	if raw := cm.Data["emails"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &emails); err != nil {
			return nil, err // fail closed: an unreadable block list must not let blocked people create keys
		}
	}
	out := map[string]bool{}
	for _, e := range emails {
		if c, ok := identity.CanonicalEmail(e); ok {
			out[c] = true
		}
	}
	return out, nil
}
