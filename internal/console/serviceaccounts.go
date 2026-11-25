package console

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/craftsman-apps/ambit/internal/identity"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Service accounts (docs/service-accounts.md): non-human identities <name>@svc.ambit.internal, owned by one group,
// created by admins. An account is an owner record of kind service-account; its keys are ordinary key Secrets with
// the issuing admin's email in an annotation. Every write runs under the KeyService lock and term, like personal keys.

// ServiceSettings are the serviceAccounts.* Helm values.
type ServiceSettings struct {
	Enabled               bool
	MaxDays, ExposureDays int
}

var (
	ErrServicesOff        = errors.New("service accounts are switched off")
	ErrNameTaken          = errors.New("this name was used before; service account names are never reused")
	ErrTooManyAccounts    = fmt.Errorf("at most %d service accounts", maxAccounts)
	ErrTooManyAccountKeys = fmt.Errorf("a service account may have at most %d keys; revoke one first", maxAccountKeys)
	ErrNoAccount          = errors.New("no such service account")
	// ErrDeleteIncomplete: the account is deleted (a tombstone), some key deletes failed; the hourly repair also
	// deletes them. The client retries.
	ErrDeleteIncomplete = errors.New("the service account is deleted, but some of its keys are not yet; try again")
)

const (
	maxAccounts    = 200
	maxAccountKeys = 2
	maxDescription = 200
	maxGroupLen    = 128
)

var (
	accountNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$`)
	groupPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@/-]*$`) // the Budget and AccessRule group pattern
	// roleGroups mean console roles (session.go roleFor); a service account never gets a session, so they are refused.
	roleGroups = []string{"ambit-admins", "ambit-approvers"}
)

// AccountIdentity is the identity of the service account named name.
func AccountIdentity(name string) string { return name + identity.ServiceDomain }

// servicesOn reports whether service accounts may be created now. Call it holding s.mu.
func (s *KeyService) servicesOn() error {
	if !s.Settings.Enabled {
		return ErrKeysOff
	}
	if !s.Services.Enabled {
		return ErrServicesOff
	}
	return nil
}

// adminEmail is the acting admin's canonical email (the bootstrap admin's too).
func adminEmail(by Session) (string, error) {
	e, ok := identity.CanonicalEmail(by.Email)
	if !ok || identity.IsService(e) {
		return "", ErrBadInput
	}
	return e, nil
}

// CreateAccount creates the service account name in group. Names are never reused: any object at the record's
// name, readable or not, keeps the name taken.
func (s *KeyService) CreateAccount(ctx context.Context, by Session, name, group, description string) (Owner, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createAccountLocked(ctx, by, name, group, description, "")
}

// createAccountLocked is CreateAccount; call it holding s.mu. An itself-mode agent creates its account with it
// (agent names the agent; the account's keys are then made only through it).
func (s *KeyService) createAccountLocked(ctx context.Context, by Session, name, group, description, agent string) (Owner, error) {
	if err := s.servicesOn(); err != nil {
		return Owner{}, err
	}
	ctx, cancel, err := s.opCtx(ctx)
	if err != nil {
		return Owner{}, err
	}
	defer cancel()
	actor, err := adminEmail(by)
	if err != nil {
		return Owner{}, err
	}
	group, description = strings.TrimSpace(group), strings.TrimSpace(description)
	if !accountNamePattern.MatchString(name) || !groupPattern.MatchString(group) || len(group) > maxGroupLen ||
		slices.Contains(roleGroups, group) || utf8.RuneCountInString(description) > maxDescription || !utf8.ValidString(description) {
		return Owner{}, ErrBadInput
	}
	id := AccountIdentity(name)
	var cur corev1.Secret
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: "ambit-key-owner-" + ownerHash(id)}, &cur); err == nil {
		return Owner{}, ErrNameTaken
	} else if !apierrors.IsNotFound(err) {
		return Owner{}, err
	}
	live, err := s.accounts(ctx)
	if err != nil {
		return Owner{}, err
	}
	if len(live) >= maxAccounts {
		return Owner{}, ErrTooManyAccounts
	}
	o, _, err := s.changeOwnerKind(ctx, KindServiceAccount, id, nil, func(o *Owner, exists bool) (bool, error) {
		if exists {
			return false, ErrNameTaken
		}
		*o = Owner{Email: id, Kind: KindServiceAccount, Groups: []string{group}, Description: description, CreatedBy: actor, Created: s.now(), Agent: agent}
		return true, nil
	})
	if apierrors.IsAlreadyExists(err) { // created meanwhile by something else: the name is taken
		return Owner{}, ErrNameTaken
	}
	if err != nil {
		return Owner{}, err
	}
	s.event(ctx, actor, "sa-create", "", id, "group="+group)
	return o, nil
}

// account reads a live service account's record; anything else is ErrNoAccount.
func (s *KeyService) account(ctx context.Context, name string) (Owner, error) {
	if !accountNamePattern.MatchString(name) {
		return Owner{}, ErrNoAccount
	}
	o, err := s.owner(ctx, AccountIdentity(name))
	if apierrors.IsNotFound(err) || errors.Is(err, errBadOwner) || (err == nil && (!o.service() || o.Deleted)) {
		return Owner{}, ErrNoAccount
	}
	return o, err
}

// CreateAccountKey creates a key for the service account name: its one group, until = the end date (no inactivity
// rule), issued by the acting admin. Checked under the lock: the admin is not on the offboarding block list (SCIM
// writes the list before it takes the lock to expose the admin's keys, so a key is either refused or exposed).
func (s *KeyService) CreateAccountKey(ctx context.Context, by Session, name, keyName string, scope Scope, days int) (string, Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createAccountKeyLocked(ctx, by, name, keyName, scope, days, nil)
}

// createAccountKeyLocked is CreateAccountKey; with ranges set (even empty) the key is that itself-mode agent's:
// MCP only, with the agent's metadata read from its record now. Call it holding s.mu.
func (s *KeyService) createAccountKeyLocked(ctx context.Context, by Session, name, keyName string, scope Scope, days int, agent *agentKeyRequest) (string, Key, error) {
	if err := s.servicesOn(); err != nil {
		return "", Key{}, err
	}
	ctx, cancel, err := s.opCtx(ctx)
	if err != nil {
		return "", Key{}, err
	}
	defer cancel()
	actor, err := adminEmail(by)
	if err != nil {
		return "", Key{}, err
	}
	keyName = strings.TrimSpace(keyName)
	if keyName == "" || len(keyName) > 100 || (scope != ScopeLLM && scope != ScopeMCP && scope != ScopeBoth) ||
		!slices.Contains([]int{30, 90, 365}, days) || days > s.Services.MaxDays {
		return "", Key{}, ErrBadInput
	}
	if blocked, err := s.blockedEmails(ctx); err != nil {
		return "", Key{}, err
	} else if blocked[actor] {
		return "", Key{}, ErrOffboarded
	}
	o, err := s.account(ctx, name)
	if err != nil {
		return "", Key{}, err
	}
	var meta *KeyAgent
	if agent == nil && o.Agent != "" {
		// An itself-mode agent's keys carry its scope: a key without it would be the account's, with no limit.
		return "", Key{}, fmt.Errorf("%w: this service account is an agent; create its keys on the agent", ErrBadInput)
	}
	if agent != nil {
		if scope != ScopeMCP || days > s.Agents.KeyMaxDays || days > s.Settings.MaxDays || o.Agent != name {
			return "", Key{}, ErrBadInput
		}
		if meta, err = s.agentKeyMeta(ctx, name, ModeItself, "", o.UID); err != nil {
			return "", Key{}, err
		}
		meta.SourceRanges = agent.ranges
	}
	now := s.now()
	recent := s.recent(actor, now) // counted with the admin's personal keys
	if len(recent) >= s.perHour() {
		return "", Key{}, ErrTooFast
	}
	keys, err := s.list(ctx, o.Email)
	if err != nil {
		return "", Key{}, err
	}
	live := 0
	for _, k := range keys {
		if k.Until.After(now) {
			live++
		}
	}
	if live >= maxAccountKeys {
		return "", Key{}, ErrTooManyAccountKeys
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
	k := Key{ID: id, Name: keyName, Prefix: prefix, Email: o.Email, Groups: o.Groups, Scope: scope, Created: now,
		Expires: ceiling, Ceiling: ceiling, Until: ceiling, Hash: hash, IssuedBy: actor, Agent: meta}
	sec := k.Secret(s.Namespace)
	if err := s.Client.Create(ctx, sec); err != nil {
		return "", Key{}, err
	}
	k.UID, k.ResourceVersion = string(sec.UID), sec.ResourceVersion
	s.created[actor] = append(recent, now)
	s.event(ctx, actor, "create", k.ID, o.Email, "")
	return plain, k, nil
}

// DeleteAccount deletes every key of the service account name, then marks its record deleted (a tombstone that
// keeps the name taken). Idempotent; it works while service accounts are switched off. When a key delete fails the
// record is still marked deleted (the repair then deletes the rest) and the answer is ErrDeleteIncomplete.
func (s *KeyService) DeleteAccount(ctx context.Context, by Session, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	actor, err := adminEmail(by)
	if err != nil {
		return err
	}
	// An itself-mode agent is this account: it is deleted as an agent, in the order that can be finished.
	if o, err := s.owner(ctx, AccountIdentity(name)); err == nil && o.Agent != "" {
		return s.deleteAgentLocked(ctx, actor, o.Agent)
	} else if errors.Is(err, errBadOwner) {
		slog.Warn("service account record unreadable; deleting it as an account", "account", name)
	}
	return s.deleteAccountLocked(ctx, actor, name)
}

// deleteAccountLocked is DeleteAccount; call it holding s.mu.
func (s *KeyService) deleteAccountLocked(ctx context.Context, actor, name string) error {
	ctx, cancel, err := s.opCtx(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	if !accountNamePattern.MatchString(name) {
		return ErrNoAccount
	}
	id := AccountIdentity(name)
	o, err := s.owner(ctx, id)
	switch {
	case apierrors.IsNotFound(err):
		return ErrNoAccount
	case errors.Is(err, errBadOwner): // unreadable or planted: keys go (fail closed), the record stays and keeps the name
	case err != nil:
		return err
	case !o.service():
		return ErrNoAccount
	}
	keys, lerr := s.list(ctx, id)
	if lerr != nil {
		return lerr
	}
	var errs []error
	for _, k := range keys {
		if _, err := s.changeKey(ctx, k.ID, func(*Key) (keyOp, error) { return keyDelete, nil }, "sa-delete-key", actor); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	if err == nil && !o.Deleted {
		if _, wrote, err := s.changeOwnerKind(ctx, KindServiceAccount, id, nil, func(o *Owner, exists bool) (bool, error) {
			if !exists || o.Deleted {
				return false, nil
			}
			o.Deleted = true
			return true, nil
		}); err != nil {
			return err
		} else if wrote {
			s.event(ctx, actor, "sa-delete", "", id, "")
		}
	}
	if len(errs) > 0 {
		slog.Error("service account deleted with keys left; the hourly repair deletes them", "account", id, "err", errors.Join(errs...))
		return ErrDeleteIncomplete
	}
	return nil
}

// ExposeIssuedBy applies the exposure rule (decision SA-5): every service account key issued by email gets its
// end date lowered to at most now + exposureDays. Idempotent (an end date is never raised); SCIM calls it when the
// person is offboarded, after their own keys.
func (s *KeyService) ExposeIssuedBy(ctx context.Context, email, trigger string) (int, error) {
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
	secs, err := s.keySecrets(ctx, client.MatchingLabels{keyLabel: "true"})
	if err != nil {
		return 0, err
	}
	now := s.now()
	stop := now.AddDate(0, 0, s.Services.ExposureDays)
	n := 0
	var errs []error
	for _, k := range decodeKeys(secs, "") {
		if issuer, _ := identity.CanonicalEmail(k.IssuedBy); !identity.IsService(k.Email) || issuer != email {
			continue
		}
		op, err := s.changeKey(ctx, k.ID, func(k *Key) (keyOp, error) {
			if !stop.Before(k.Ceiling) && !stop.Before(k.Until) {
				return keyKeep, nil
			}
			if stop.Before(k.Ceiling) {
				k.Ceiling = stop
			}
			if stop.Before(k.Until) {
				k.Until = stop
			}
			if k.Exposed.IsZero() {
				k.Exposed = now
			}
			return keyUpdate, nil
		}, "", consoleActor)
		if apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			errs = append(errs, err)
			continue
		}
		if op == keyUpdate {
			n++
			s.event(ctx, trigger, "exposed", k.ID, k.Email, "issuedBy="+email+" stops="+stop.Format(time.RFC3339))
		}
	}
	return n, errors.Join(errs...)
}

// Account is a service account with its keys, for the console.
type Account struct {
	Owner Owner
	Keys  []Key
}

// Accounts returns the live service accounts (no tombstones), by name, with their keys. Reads take no lock.
func (s *KeyService) Accounts(ctx context.Context) ([]Account, error) {
	owners, err := s.accounts(ctx)
	if err != nil {
		return nil, err
	}
	keys, err := s.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Account, 0, len(owners))
	for _, o := range owners {
		a := Account{Owner: o}
		for _, k := range keys {
			if k.Email == o.Email {
				a.Keys = append(a.Keys, k)
			}
		}
		out = append(out, a)
	}
	return out, nil
}

// accounts lists the live service account records, by email. Records that are unreadable, of another kind or
// for another email are left out (and logged).
func (s *KeyService) accounts(ctx context.Context) ([]Owner, error) {
	var l corev1.SecretList
	if err := s.Client.List(ctx, &l, client.InNamespace(s.Namespace), client.MatchingLabels{ownerRecordLabel: "true", ownerKindLabel: KindServiceAccount}); err != nil {
		return nil, err
	}
	var out []Owner
	for i := range l.Items {
		o, err := OwnerFromSecret(&l.Items[i])
		if err != nil || !o.service() || !identity.IsService(o.Email) || l.Items[i].Name != "ambit-key-owner-"+ownerHash(o.Email) {
			slog.Warn("service account record left out", "record", l.Items[i].Name, "err", err)
			continue
		}
		if !o.Deleted {
			out = append(out, o)
		}
	}
	slices.SortFunc(out, func(a, b Owner) int { return strings.Compare(a.Email, b.Email) })
	return out, nil
}
