package console

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/craftsman-apps/ambit/internal/controller"
	"github.com/craftsman-apps/ambit/internal/identity"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Agents (docs/agents.md), EXPERIMENTAL in 0.6.x: a hosted assistant that calls MCP tools with its own key. An agent
// is a small record (a ConfigMap only the console writes, no secrets) plus keys (key Secrets whose metadata carries
// the agent's name, mode, owner at issue, declared platform, scope and source ranges, as the gateway reads them).
// Person mode: acts for its owner, who creates it for themselves. Itself mode: a service account with an owner, a
// platform and a scope, created by admins. Every write runs under the KeyService lock and term, like keys.

// AgentSettings are the agents.* Helm values.
type AgentSettings struct {
	Enabled                            bool
	MaxTotal, MaxPerPerson, KeyMaxDays int
}

const (
	ModePerson = "person"
	ModeItself = "itself"

	StateActive = "active"
	// StatePending: an itself-mode record whose service account is not bound yet; never used, deleted by the repair
	// after pendingTTL. StateDeleting: a delete that is not finished; the repair finishes it. A stop passes through
	// StateStopping to StateStopped (docs/agents.md). Only an active agent gets keys, and the keys of any other agent
	// are deleted by the repair.
	StatePending  = "pending"
	StateDeleting = "deleting"
	StateStopping = "stopping"
	StateStopped  = "stopped"
	pendingTTL    = 10 * time.Minute
	agentSigAnn   = "ambitproject.dev/agent-sig"

	agentRecordPrefix = "ambit-agent-"
	agentOwnerLabel   = "ambitproject.dev/agent-owner" // the owner's hash, to list one person's agents
	maxAgentScope     = 50
	maxScopeEntry     = 200
	maxSourceRanges   = 10
	maxAgentKeys      = 2  // live keys per person-mode agent (an itself-mode agent has the service account's two)
	maxConsent        = 20 // connections a person may allow an agent to use
	maxDisplayName    = 100
	// A record with 50 scope entries of 200 characters is about 11 KiB; 16 KiB leaves room, and etcd's limit is 1 MiB.
	maxAgentRecord = 16 << 10
)

var (
	ErrAgentsOff      = errors.New("agents are switched off")
	ErrAgentLimit     = errors.New("agent limit reached")
	ErrNoAgent        = errors.New("no such agent")
	ErrNotAllowed     = errors.New("you may not do this")
	ErrAgentNotActive = errors.New("this agent is not active")
	// ErrAgentsNotReady: the controller has not published that the MCP policy in force carries the agent-scope clause
	// (an older controller, or a change the gateway has not applied): no agent key is issued before it.
	ErrAgentsNotReady   = errors.New("agents are not ready: the gateway has not applied the agent scope policy yet; try again in a minute")
	ErrTooManyAgentKeys = fmt.Errorf("an agent may have at most %d keys; revoke one first", maxAgentKeys)

	platformPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}$`)
	serverPattern   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`) // a Server object's name
	toolPattern     = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)              // AccessRule tools (CRD pattern)
	connPattern     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`) // a Connection object's name
)

// Agent is an agent's record.
type Agent struct {
	Name        string `json:"name"` // never reused; the name in a key's metadata and in the audit
	DisplayName string `json:"displayName"`
	// Owner is the person it acts for (person mode), or the accountable person ("alice@acme.com") or group
	// ("group:eng") of an itself-mode agent.
	Owner string `json:"owner"`
	Mode  string `json:"mode"`
	// Group is the service account's group (itself mode). AccountUID is the UID of the service account's owner record
	// the agent is bound to: a record never acts on an account it was not bound to.
	Group      string `json:"group,omitempty"`
	AccountUID string `json:"accountUID,omitempty"`
	// Platform is what the agent says it is ("chatgpt", "grok", ...): declared, never a proof; no policy uses it.
	Platform string `json:"platform"`
	// Grants: the OAuth grants of this agent (person mode; made at the consent page, docs/oauth.md). A refresh of a grant that is
	// not listed here, or of an agent that is not active, is refused.
	Grants []string `json:"grants,omitempty"`
	// Scope: "server" and "server/tool" entries, at most 50. Empty: the agent may use no tool.
	Scope []string `json:"scope"`
	// ConnectionConsent: the Connections (apps) whose person-mode owner token the agent may use. Empty: none.
	ConnectionConsent []string  `json:"connectionConsent"`
	State             string    `json:"state"`
	Rev               int       `json:"rev"` // changes with the scope and the platform; keys carry the revision they were written from
	Created           time.Time `json:"created"`
	CreatedBy         string    `json:"createdBy"`

	UID, ResourceVersion string `json:"-"`
}

// recordSig is the console's signature of a record (HMAC with a key only the console holds): a record that something
// else wrote, or changed, before the admission policy existed or since, is not an agent.
func (s *KeyService) recordSig(name, data string) string {
	m := hmac.New(sha256.New, s.RecordKey)
	m.Write([]byte(name + "\n" + data))
	return hex.EncodeToString(m.Sum(nil))
}

// AgentRecordKey derives the record signing key from the console's session key.
func AgentRecordKey(sessionKey []byte) []byte {
	m := hmac.New(sha256.New, sessionKey)
	m.Write([]byte("ambit agent records v1"))
	return m.Sum(nil)
}

func (s *KeyService) record(a Agent) *corev1.ConfigMap {
	b, _ := json.Marshal(a)
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: agentRecordPrefix + a.Name, Namespace: s.Namespace,
		Labels:      map[string]string{agentLabel: "true", agentOwnerLabel: ownerHash(a.Owner)},
		Annotations: map[string]string{agentSigAnn: s.recordSig(a.Name, string(b))},
		UID:         typesUID(a.UID), ResourceVersion: a.ResourceVersion},
		Data: map[string]string{"agent.json": string(b)}}
}

func (s *KeyService) agentFromRecord(cm *corev1.ConfigMap) (Agent, error) {
	var a Agent
	if cm.Labels[agentLabel] != "true" {
		return a, fmt.Errorf("record %s has no agent label", cm.Name)
	}
	if len(s.RecordKey) == 0 || !hmac.Equal([]byte(cm.Annotations[agentSigAnn]), []byte(s.recordSig(strings.TrimPrefix(cm.Name, agentRecordPrefix), cm.Data["agent.json"]))) {
		return a, fmt.Errorf("agent record %s was not written by the console", cm.Name)
	}
	if err := json.Unmarshal([]byte(cm.Data["agent.json"]), &a); err != nil || a.Name == "" || cm.Name != agentRecordPrefix+a.Name {
		return Agent{}, fmt.Errorf("agent record %s is unreadable", cm.Name)
	}
	if (a.Mode != ModePerson && a.Mode != ModeItself) || a.Owner == "" ||
		!slices.Contains([]string{StateActive, StatePending, StateDeleting, StateStopping, StateStopped}, a.State) {
		return Agent{}, fmt.Errorf("agent record %s has an unknown mode or state", cm.Name)
	}
	a.UID, a.ResourceVersion = string(cm.UID), cm.ResourceVersion
	return a, nil
}

// AgentInput is a new agent.
type AgentInput struct {
	Mode        string // "" or person: for the caller; itself: admins only
	DisplayName string
	Platform    string
	Scope       []string
	// Consent: the Connections the agent may use (person mode only).
	Consent []string
	// Grant: the OAuth grant this agent is made for (person mode, from the consent page); "" for an agent made by hand.
	Grant string
	// Itself mode only: the service account's name, its group, and its accountable owner (a person, or "group:<name>";
	// default the admin who creates it).
	Name, Group, Owner string
}

// AgentPatch changes an agent; a nil field stays as it is.
type AgentPatch struct {
	DisplayName, Platform *string
	Scope, Consent        *[]string
}

var grantPattern = regexp.MustCompile(`^g-[0-9a-z]{1,13}-[A-Za-z0-9_-]{6,24}$`)

func (s *KeyService) agentsOn() error {
	if !s.Settings.Enabled {
		return ErrKeysOff
	}
	if !s.Agents.Enabled {
		return ErrAgentsOff
	}
	return nil
}

// cleanDisplayName trims and checks a display name: 1 to 100 characters, no control characters.
func cleanDisplayName(n string) (string, error) {
	n = strings.TrimSpace(n)
	if n == "" || !utf8.ValidString(n) || utf8.RuneCountInString(n) > maxDisplayName || strings.IndexFunc(n, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("%w: the name must be 1 to %d characters", ErrBadInput, maxDisplayName)
	}
	return n, nil
}

func cleanPlatform(p string) (string, error) {
	if p = strings.TrimSpace(p); p == "" {
		return "other", nil
	}
	if !platformPattern.MatchString(p) {
		return "", fmt.Errorf("%w: platform must be lower-case letters, digits, . _ - (at most 40 characters)", ErrBadInput)
	}
	return p, nil
}

// cleanScope checks "server" and "server/tool" entries: at most 50, each at most 200 characters, no duplicates.
// The result is sorted, so the same scope always gives the same key metadata.
func cleanScope(in []string) ([]string, error) {
	if len(in) > maxAgentScope {
		return nil, fmt.Errorf("%w: a scope has at most %d entries", ErrBadInput, maxAgentScope)
	}
	out := make([]string, 0, len(in))
	for _, e := range in {
		server, tool, hasTool := strings.Cut(e, "/")
		if len(e) > maxScopeEntry || !serverPattern.MatchString(server) || (hasTool && !toolPattern.MatchString(tool)) {
			return nil, fmt.Errorf("%w: scope entry %q must be a server name, or server/tool", ErrBadInput, e)
		}
		out = append(out, e)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

func cleanConsent(in []string) ([]string, error) {
	if len(in) > maxConsent {
		return nil, fmt.Errorf("%w: at most %d connections", ErrBadInput, maxConsent)
	}
	out := make([]string, 0, len(in))
	for _, c := range in {
		if !connPattern.MatchString(c) {
			return nil, fmt.Errorf("%w: %q is not a connection name", ErrBadInput, c)
		}
		out = append(out, c)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// cleanRanges checks source ranges: at most 10 CIDRs, written in their canonical form.
func cleanRanges(in []string) ([]string, error) {
	if len(in) > maxSourceRanges {
		return nil, fmt.Errorf("%w: at most %d source ranges", ErrBadInput, maxSourceRanges)
	}
	out := make([]string, 0, len(in))
	for _, r := range in {
		p, err := netip.ParsePrefix(strings.TrimSpace(r))
		if err != nil || p.Addr().Zone() != "" {
			return nil, fmt.Errorf("%w: %q is not a CIDR range such as 203.0.113.0/24", ErrBadInput, r)
		}
		out = append(out, p.Masked().String())
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// agentSlug is the readable part of a person-mode agent's name: letters and digits of the display name, at most 24.
func agentSlug(display string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(display) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 24 {
			break
		}
	}
	if s := strings.Trim(b.String(), "-"); s != "" {
		return s
	}
	return "agent"
}

// newAgentName makes a person-mode agent's name: the slug and six random letters, so that a name is not reused
// after a delete (the record is removed, and the chance of the same six characters is about 1 in a billion).
func newAgentName(display string) (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return agentSlug(display) + "-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))[:6], nil
}

// ownerOf validates an itself-mode owner: a person's email, or "group:<name>".
func cleanItselfOwner(o string) (string, error) {
	if o = strings.TrimSpace(o); len(o) > 254 {
		return "", fmt.Errorf("%w: the owner is too long", ErrBadInput)
	}
	if g, ok := strings.CutPrefix(o, "group:"); ok {
		if !groupPattern.MatchString(g) || len(g) > maxGroupLen {
			return "", fmt.Errorf("%w: the owner group is not valid", ErrBadInput)
		}
		return "group:" + g, nil
	}
	e, ok := identity.CanonicalEmail(o)
	if !ok || identity.IsService(e) {
		return "", fmt.Errorf("%w: the owner must be a person's email address or group:<name>", ErrBadInput)
	}
	return e, nil
}

func (s *KeyService) agentRecords(ctx context.Context, sel client.MatchingLabels) ([]Agent, error) {
	var l corev1.ConfigMapList
	if err := s.Client.List(ctx, &l, client.InNamespace(s.Namespace), sel); err != nil {
		return nil, err
	}
	out := make([]Agent, 0, len(l.Items))
	for i := range l.Items {
		a, err := s.agentFromRecord(&l.Items[i])
		if err != nil {
			slog.Warn("agent record left out", "record", l.Items[i].Name, "err", err)
			continue
		}
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b Agent) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// AllAgents returns every readable agent record, by name. Reads take no lock.
func (s *KeyService) AllAgents(ctx context.Context) ([]Agent, error) {
	return s.agentRecords(ctx, client.MatchingLabels{agentLabel: "true"})
}

// AgentOwners maps every agent's name to its owner as the records say now.
func (s *KeyService) AgentOwners(ctx context.Context) (map[string]string, error) {
	as, err := s.AllAgents(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(as))
	for _, a := range as {
		out[a.Name] = a.Owner
	}
	return out, nil
}

// AgentsOf returns the agents a person owns (both modes), by name.
func (s *KeyService) AgentsOf(ctx context.Context, email string) ([]Agent, error) {
	email, ok := identity.CanonicalEmail(email)
	if !ok {
		return nil, ErrBadInput
	}
	as, err := s.agentRecords(ctx, client.MatchingLabels{agentLabel: "true", agentOwnerLabel: ownerHash(email)})
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(as, func(a Agent) bool { return a.Owner != email }), nil
}

// GetAgent reads one record. Reads take no lock.
func (s *KeyService) GetAgent(ctx context.Context, name string) (Agent, error) {
	if !accountNamePattern.MatchString(name) {
		return Agent{}, ErrNoAgent
	}
	var cm corev1.ConfigMap
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: agentRecordPrefix + name}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return Agent{}, ErrNoAgent
		}
		return Agent{}, err
	}
	a, err := s.agentFromRecord(&cm)
	if err != nil {
		slog.Warn("agent record unreadable", "record", cm.Name, "err", err)
		return Agent{}, ErrNoAgent
	}
	return a, nil
}

// AgentKeys returns an agent's keys, oldest first. Reads take no lock.
func (s *KeyService) AgentKeys(ctx context.Context, name string) ([]Key, error) {
	secs, err := s.keySecrets(ctx, client.MatchingLabels{keyLabel: "true", agentLabel: name})
	if err != nil {
		return nil, err
	}
	var keys []Key
	for _, k := range decodeKeys(secs, "") {
		if k.Agent != nil && k.Agent.Name == name {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

// canManage: a person-mode agent is managed by its owner; an itself-mode agent by admins. Anyone else gets ErrNoAgent,
// so that names of other people's agents are not confirmed.
func canManage(by Session, a Agent, actor string) error {
	if (a.Mode == ModePerson && a.Owner == actor) || (a.Mode == ModeItself && by.Role == RoleAdmin) {
		return nil
	}
	return ErrNoAgent
}

// CreateAgent creates an agent. A person creates person-mode agents for themselves; only admins create itself-mode
// agents (a service account with the agent's owner, platform and scope).
func (s *KeyService) CreateAgent(ctx context.Context, by Session, in AgentInput) (Agent, error) {
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
	var a Agent
	if a.DisplayName, err = cleanDisplayName(in.DisplayName); err != nil {
		return Agent{}, err
	}
	if a.Platform, err = cleanPlatform(in.Platform); err != nil {
		return Agent{}, err
	}
	if a.Scope, err = cleanScope(in.Scope); err != nil {
		return Agent{}, err
	}
	if a.ConnectionConsent, err = cleanConsent(in.Consent); err != nil {
		return Agent{}, err
	}
	a.State, a.Rev, a.Created, a.CreatedBy = StateActive, 1, s.now(), actor
	if in.Grant != "" {
		if !grantPattern.MatchString(in.Grant) || (in.Mode != "" && in.Mode != ModePerson) {
			return Agent{}, fmt.Errorf("%w: a grant belongs to a person-mode agent and has the form g-<time>-<random>", ErrBadInput)
		}
		a.Grants = []string{in.Grant}
	}
	switch in.Mode {
	case "", ModePerson:
		a.Mode, a.Owner = ModePerson, actor
		if by.SignedIn == 0 {
			return Agent{}, ErrSignInAgain
		}
	case ModeItself:
		if by.Role != RoleAdmin {
			return Agent{}, fmt.Errorf("%w: only admins create itself-mode agents", ErrNotAllowed)
		}
		if len(a.ConnectionConsent) > 0 {
			return Agent{}, fmt.Errorf("%w: an itself-mode agent gets no app connections in 0.6.0", ErrBadInput)
		}
		a.Mode, a.Name, a.Group = ModeItself, in.Name, strings.TrimSpace(in.Group)
		if !accountNamePattern.MatchString(a.Name) || !groupPattern.MatchString(a.Group) || len(a.Group) > maxGroupLen || slices.Contains(roleGroups, a.Group) {
			return Agent{}, fmt.Errorf("%w: give a name (lower-case letters, digits, -) and a group", ErrBadInput)
		}
		if a.Owner = actor; strings.TrimSpace(in.Owner) != "" {
			if a.Owner, err = cleanItselfOwner(in.Owner); err != nil {
				return Agent{}, err
			}
		}
	default:
		return Agent{}, fmt.Errorf("%w: mode is person or itself", ErrBadInput)
	}
	if blocked, err := s.blockedEmails(ctx); err != nil {
		return Agent{}, err
	} else if blocked[actor] || blocked[a.Owner] {
		return Agent{}, ErrOffboarded
	}
	all, err := s.agentRecords(ctx, client.MatchingLabels{agentLabel: "true"})
	if err != nil {
		return Agent{}, err
	}
	if len(all) >= s.Agents.MaxTotal {
		return Agent{}, ErrAgentLimit
	}
	if a.Mode == ModePerson {
		n := 0
		for _, o := range all {
			if o.Mode == ModePerson && o.Owner == a.Owner {
				n++
			}
		}
		if n >= s.Agents.MaxPerPerson {
			return Agent{}, ErrAgentLimit
		}
	}
	if len(a.Owner) > 254 {
		return Agent{}, fmt.Errorf("%w: the owner is too long", ErrBadInput)
	}
	if a.Mode == ModeItself {
		a.State = StatePending // active only after the service account is created and bound
	}
	for try := 0; ; try++ {
		if a.Mode == ModePerson {
			if a.Name, err = newAgentName(a.DisplayName); err != nil {
				return Agent{}, err
			}
		}
		cm := s.record(a)
		if len(cm.Data["agent.json"]) > maxAgentRecord {
			return Agent{}, fmt.Errorf("%w: the agent record is too large", ErrBadInput)
		}
		err = s.Client.Create(ctx, cm)
		if apierrors.IsAlreadyExists(err) && a.Mode == ModePerson && try < 3 {
			continue
		}
		break
	}
	if apierrors.IsAlreadyExists(err) {
		return Agent{}, ErrNameTaken
	}
	if err != nil {
		return Agent{}, err
	}
	if a.Mode == ModeItself {
		// The record is written first, so a name taken by a service account is found before anything else happens; it
		// is bound to the account's owner record (its UID) before it becomes active. A record that stays pending is
		// deleted by the repair, and never acts on an account it is not bound to.
		o, err := s.createAccountLocked(ctx, by, a.Name, a.Group, "agent: "+a.DisplayName, a.Name)
		if err != nil {
			if derr := s.Client.Delete(ctx, s.record(a)); derr != nil && !apierrors.IsNotFound(derr) {
				slog.Error("pending agent record left; the repair deletes it", "agent", a.Name, "err", derr)
			}
			return Agent{}, err
		}
		if _, err := s.changeAgent(ctx, a.Name, func(cur *Agent) (bool, error) {
			cur.State, cur.AccountUID = StateActive, o.UID
			return true, nil
		}); err != nil {
			return Agent{}, err
		}
	}
	s.event(ctx, actor, "agent-create", "", a.Owner, "agent="+a.Name+" mode="+a.Mode+" platform="+a.Platform)
	return s.GetAgent(ctx, a.Name)
}

// changeAgent re-reads the record, lets rule change it (false: write nothing) and writes it version-checked,
// retrying on conflict. Call it holding s.mu.
func (s *KeyService) changeAgent(ctx context.Context, name string, rule func(a *Agent) (bool, error)) (Agent, error) {
	var out Agent
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		a, err := s.GetAgent(ctx, name)
		if err != nil {
			return err
		}
		write, err := rule(&a)
		out = a
		if err != nil || !write {
			return err
		}
		cm := s.record(a)
		if len(cm.Data["agent.json"]) > maxAgentRecord {
			return fmt.Errorf("%w: the agent record is too large", ErrBadInput)
		}
		if err := s.Client.Update(ctx, cm); err != nil {
			return err
		}
		out.UID, out.ResourceVersion = string(cm.UID), cm.ResourceVersion
		return nil
	})
	return out, err
}

// UpdateAgent changes an agent's name, platform, scope or connection consent. A new scope or platform raises the
// record's revision and is copied to the agent's keys, which the gateway then enforces; a failed copy returns an
// error, and the same request repeated finishes it (the hourly repair does too).
func (s *KeyService) UpdateAgent(ctx context.Context, by Session, name string, p AgentPatch) (Agent, error) {
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
	var display, platform string
	var scope, consent []string
	if p.DisplayName != nil {
		if display, err = cleanDisplayName(*p.DisplayName); err != nil {
			return Agent{}, err
		}
	}
	if p.Platform != nil {
		if platform, err = cleanPlatform(*p.Platform); err != nil {
			return Agent{}, err
		}
	}
	if p.Scope != nil {
		if scope, err = cleanScope(*p.Scope); err != nil {
			return Agent{}, err
		}
	}
	if p.Consent != nil {
		if consent, err = cleanConsent(*p.Consent); err != nil {
			return Agent{}, err
		}
	}
	if p.Scope != nil {
		// A scope that takes something away is enforced before it is saved: the keys are narrowed first. If that fails,
		// nothing is saved, and the old scope stays in the record and in every key that was not narrowed.
		cur, err := s.GetAgent(ctx, name)
		if err != nil {
			return Agent{}, err
		}
		if err := canManage(by, cur, actor); err != nil {
			return Agent{}, err
		}
		if cur.State != StateActive {
			return Agent{}, ErrAgentNotActive
		}
		if removesFrom(cur.Scope, scope) {
			if err := s.setKeyScope(ctx, cur.Name, keptScope(cur.Scope, scope), cur.Rev+1); err != nil {
				return Agent{}, fmt.Errorf("%w: the keys could not be narrowed, so nothing was changed; send the request again", ErrKeysBehind)
			}
		}
	}
	var changed []string
	a, err := s.changeAgent(ctx, name, func(a *Agent) (bool, error) {
		changed = nil
		if err := canManage(by, *a, actor); err != nil {
			return false, err
		}
		if a.State != StateActive {
			return false, ErrAgentNotActive
		}
		if p.Consent != nil && a.Mode == ModeItself && len(consent) > 0 {
			return false, fmt.Errorf("%w: an itself-mode agent gets no app connections in 0.6.0", ErrBadInput)
		}
		if p.DisplayName != nil && display != a.DisplayName {
			a.DisplayName = display
			changed = append(changed, "name")
		}
		bump := false
		if p.Platform != nil && platform != a.Platform {
			a.Platform, bump = platform, true
			changed = append(changed, "platform")
		}
		if p.Scope != nil && !slices.Equal(scope, a.Scope) {
			a.Scope, bump = scope, true
			changed = append(changed, "scope")
		}
		if p.Consent != nil && !slices.Equal(consent, a.ConnectionConsent) {
			a.ConnectionConsent = consent
			changed = append(changed, "consent")
		}
		if bump {
			a.Rev++
		}
		return len(changed) > 0, nil
	})
	if err != nil {
		return Agent{}, err
	}
	if len(changed) > 0 {
		s.event(ctx, actor, "agent-update", "", a.Owner, "agent="+a.Name+" changed="+strings.Join(changed, ","))
	}
	if err := s.syncAgentKeys(ctx, a); err != nil {
		return a, fmt.Errorf("%w: the change is saved but some keys of the agent still have the old scope; send the request again", ErrKeysBehind)
	}
	return a, nil
}

// scopeCovers: the scope list allows entry e (the same entry, or e is a tool of a server the list names whole).
func scopeCovers(list []string, e string) bool {
	if slices.Contains(list, e) {
		return true
	}
	server, _, isTool := strings.Cut(e, "/")
	return isTool && slices.Contains(list, server)
}

// removesFrom: old allows something that next does not.
func removesFrom(old, next []string) bool {
	return slices.ContainsFunc(old, func(e string) bool { return !scopeCovers(next, e) })
}

// keptScope is the part of next that old already allowed: what a key may keep while a change is saved.
func keptScope(old, next []string) []string {
	out := []string{}
	for _, e := range next {
		if scopeCovers(old, e) {
			out = append(out, e)
		}
	}
	return out
}

// setKeyScope sets the scope and revision of every key of the agent. Call it holding s.mu.
func (s *KeyService) setKeyScope(ctx context.Context, name string, scope []string, rev int) error {
	keys, err := s.AgentKeys(ctx, name)
	if err != nil {
		return err
	}
	var errs []error
	for _, k := range keys {
		if _, err := s.changeKey(ctx, k.ID, func(cur *Key) (keyOp, error) {
			if cur.Agent == nil || (slices.Equal(cur.Agent.Scope, scope) && cur.Agent.Rev == rev) {
				return keyKeep, nil
			}
			cur.Agent.Scope, cur.Agent.Rev = scope, rev
			return keyUpdate, nil
		}, "", consoleActor); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ErrKeysBehind: the record changed and some key Secrets did not follow yet.
var ErrKeysBehind = errors.New("agent keys not up to date")

// syncAgentKeys copies the record's scope, platform and revision to the agent's keys that differ. Call it holding
// s.mu. The record is the truth; the keys follow it (also in the other direction of a narrowing: a key may only
// ever be rewritten to the record's scope).
func (s *KeyService) syncAgentKeys(ctx context.Context, a Agent) error {
	keys, err := s.AgentKeys(ctx, a.Name)
	if err != nil {
		return err
	}
	var errs []error
	for _, k := range keys {
		if !keyBehind(k, a) {
			continue
		}
		_, err := s.changeKey(ctx, k.ID, func(cur *Key) (keyOp, error) {
			if cur.Agent == nil || !keyBehind(*cur, a) {
				return keyKeep, nil
			}
			cur.Agent.Scope, cur.Agent.Platform, cur.Agent.Rev = a.Scope, a.Platform, a.Rev
			return keyUpdate, nil
		}, "", consoleActor)
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func keyBehind(k Key, a Agent) bool {
	return k.Agent != nil && (k.Agent.Rev != a.Rev || k.Agent.Platform != a.Platform || !slices.Equal(k.Agent.Scope, a.Scope))
}

// MoveAgent changes an itself-mode agent's accountable owner (admins). Keys already issued keep the owner they
// were issued with (agentOwnerAtIssue); Audit adds the owner now when it reads.
func (s *KeyService) MoveAgent(ctx context.Context, by Session, name, owner string) (Agent, error) {
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
	if by.Role != RoleAdmin {
		return Agent{}, fmt.Errorf("%w: only admins move agents", ErrNotAllowed)
	}
	if owner, err = cleanItselfOwner(owner); err != nil {
		return Agent{}, err
	}
	if blocked, err := s.blockedEmails(ctx); err != nil {
		return Agent{}, err
	} else if blocked[owner] {
		return Agent{}, ErrOffboarded
	}
	var before string
	a, err := s.changeAgent(ctx, name, func(a *Agent) (bool, error) {
		if a.Mode != ModeItself {
			return false, fmt.Errorf("%w: only an itself-mode agent has an owner you can move", ErrBadInput)
		}
		before = a.Owner
		a.Owner = owner
		return a.Owner != before, nil
	})
	if err == nil && before != a.Owner {
		s.event(ctx, actor, "agent-move", "", a.Owner, "agent="+a.Name+" from="+before)
	}
	return a, err
}

// DeleteAgent deletes an agent: it first records state deleting (no new keys; a delete that stops half way is
// finished by the repair), then deletes the agent's keys, its service account (itself mode, which keeps its name
// taken) and its record. The owner (person mode) or an admin may. Idempotent.
func (s *KeyService) DeleteAgent(ctx context.Context, by Session, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	actor, err := adminEmail(by)
	if err != nil {
		return err
	}
	a, err := s.GetAgent(ctx, name)
	if err != nil {
		return err
	}
	if a.Mode == ModePerson && by.Role == RoleAdmin {
		// admins may delete anyone's person-mode agent (an offboarded person's, for example)
	} else if err := canManage(by, a, actor); err != nil {
		return err
	}
	return s.deleteAgentLocked(ctx, actor, name)
}

// deleteAgentLocked is the delete flow of DeleteAgent and of the Service accounts page. Call it holding s.mu.
func (s *KeyService) deleteAgentLocked(ctx context.Context, actor, name string) error {
	ctx, cancel, err := s.opCtx(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	a, err := s.GetAgent(ctx, name)
	if err != nil {
		return err
	}
	if a.State != StateDeleting {
		if a, err = s.changeAgent(ctx, name, func(cur *Agent) (bool, error) {
			if cur.State == StateDeleting {
				return false, nil
			}
			cur.State = StateDeleting
			return true, nil
		}); err != nil {
			return err
		}
	}
	return s.finishDelete(ctx, a, actor)
}

// finishDelete deletes the keys of an agent in state deleting, its service account when the record is bound to it
// (never another account of the same name), and the record. Call it holding s.mu, inside a term.
func (s *KeyService) finishDelete(ctx context.Context, a Agent, actor string) error {
	keys, err := s.AgentKeys(ctx, a.Name)
	if err != nil {
		return err
	}
	var errs []error
	for _, k := range keys {
		if _, err := s.changeKey(ctx, k.ID, func(*Key) (keyOp, error) { return keyDelete, nil }, "agent-delete-key", actor); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%w: %w", ErrDeleteIncomplete, err)
	}
	if a.Mode == ModeItself && a.AccountUID != "" {
		switch o, err := s.owner(ctx, AccountIdentity(a.Name)); {
		case apierrors.IsNotFound(err):
		case err == nil && o.UID == a.AccountUID && o.Agent == a.Name:
			if err := s.deleteAccountLocked(ctx, actor, a.Name); err != nil && !errors.Is(err, ErrNoAccount) {
				return err
			}
		default:
			slog.Warn("agent record is not bound to the service account of its name; the account is left alone", "agent", a.Name, "err", err)
		}
	}
	uid, rv := typesUID(a.UID), a.ResourceVersion
	if err := s.Client.Delete(ctx, s.record(a), client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	s.event(ctx, actor, "agent-delete", "", a.Owner, "agent="+a.Name)
	return nil
}

// agentScopeReady reports whether the controller published that the MCP policy in force has the agent-scope clause.
func (s *KeyService) agentScopeReady(ctx context.Context) bool {
	var cm corev1.ConfigMap
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: controller.AccessModeConfigMap}, &cm); err != nil {
		return false
	}
	return cm.Data[controller.AgentScopeKey] == "1"
}

// agentKeyMeta reads an agent's record under the lock and returns the metadata a new key of it carries. The agent must
// be active, of the given mode and, in person mode, owned by owner; in itself mode it must be bound to the service
// account with the owner record UID accountUID. Fail closed: no key before the gateway enforces the scope.
func (s *KeyService) agentKeyMeta(ctx context.Context, name, mode, owner, accountUID string) (*KeyAgent, error) {
	if !s.agentScopeReady(ctx) {
		return nil, ErrAgentsNotReady
	}
	a, err := s.GetAgent(ctx, name)
	if err != nil {
		return nil, err
	}
	if a.Mode != mode || (mode == ModePerson && a.Owner != owner) || (mode == ModeItself && (a.AccountUID == "" || a.AccountUID != accountUID)) {
		return nil, ErrNoAgent
	}
	if a.State != StateActive {
		return nil, ErrAgentNotActive
	}
	return &KeyAgent{Name: a.Name, Mode: a.Mode, OwnerAtIssue: a.Owner, Platform: a.Platform, Scope: a.Scope, Rev: a.Rev}, nil
}

// repairAgentRecords finishes interrupted deletes, deletes itself-mode records that never got bound (pending for more
// than pendingTTL), and deletes agent-labelled ConfigMaps that are not records the console wrote (planted, or signed
// with another key). Call it holding s.mu, inside a term.
func (s *KeyService) repairAgentRecords(ctx context.Context) error {
	var l corev1.ConfigMapList
	if err := s.Client.List(ctx, &l, client.InNamespace(s.Namespace), client.MatchingLabels{agentLabel: "true"}); err != nil {
		return err
	}
	var errs []error
	now := s.now()
	for i := range l.Items {
		cm := &l.Items[i]
		a, err := s.agentFromRecord(cm)
		switch {
		case err != nil:
			slog.Warn("deleting an agent-labelled object that is not an agent record", "record", cm.Name, "err", err)
			if derr := s.Client.Delete(ctx, cm); derr != nil && !apierrors.IsNotFound(derr) {
				errs = append(errs, derr)
			}
		case a.State == StateDeleting:
			if err := s.finishDelete(ctx, a, consoleActor); err != nil {
				errs = append(errs, err)
			}
		case a.State == StatePending && now.Sub(a.Created) > pendingTTL:
			if err := s.Client.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// checkAgentKeyRoom refuses a new key of a person-mode agent that already has its live keys.
func (s *KeyService) checkAgentKeyRoom(keys []Key, name string, now time.Time) error {
	n := 0
	for _, k := range keys {
		if k.Agent != nil && k.Agent.Name == name && k.Until.After(now) {
			n++
		}
	}
	if n >= maxAgentKeys {
		return ErrTooManyAgentKeys
	}
	return nil
}

// CreateAgentKey creates a key for an agent (MCP only, shown once). Person mode: the key acts as the owner, and only the
// owner creates it. Itself mode: the key is the service account's, created by an admin. sourceRanges, at most 10
// CIDRs, limit where the key is accepted from.
func (s *KeyService) CreateAgentKey(ctx context.Context, by Session, name, keyName string, days int, sourceRanges []string) (string, Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.agentsOn(); err != nil {
		return "", Key{}, err
	}
	actor, err := adminEmail(by)
	if err != nil {
		return "", Key{}, err
	}
	ranges, err := cleanRanges(sourceRanges)
	if err != nil {
		return "", Key{}, err
	}
	a, err := s.GetAgent(ctx, name)
	if err != nil {
		return "", Key{}, err
	}
	if err := canManage(by, a, actor); err != nil {
		return "", Key{}, err
	}
	if a.Mode == ModeItself {
		return s.createAccountKeyLocked(ctx, by, name, keyName, ScopeMCP, days, &agentKeyRequest{ranges: ranges})
	}
	return s.createLocked(ctx, by, keyName, ScopeMCP, days, &agentKeyRequest{name: name, ranges: ranges})
}

// agentKeyRequest asks key creation for an agent's key: the agent's name and the key's source ranges.
type agentKeyRequest struct {
	name   string
	ranges []string
}
