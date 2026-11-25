package console

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	keyLabel         = "ambitproject.dev/key"
	keyLLMLabel      = "ambitproject.dev/key-llm"
	keyMCPLabel      = "ambitproject.dev/key-mcp"
	keyOwnerLabel    = "ambitproject.dev/key-owner"
	ownerRecordLabel = "ambitproject.dev/key-owner-record"
	// ownerKindLabel marks a service account's owner record, so the accounts can be listed without reading every
	// person's record. Listings also require ownerRecordLabel, which admission protects.
	ownerKindLabel = "ambitproject.dev/key-owner-kind"
	annPrefix      = "ambitproject.dev/key-"
	// agentLabel marks an agent's record (a ConfigMap) and, with the agent's name as its value, each key of the agent.
	agentLabel = "ambitproject.dev/agent"
)

type Scope string

const (
	ScopeLLM  Scope = "llm"
	ScopeMCP  Scope = "mcp"
	ScopeBoth Scope = "both"
)

type KeySettings struct {
	Enabled                 bool
	InactivityDays, MaxDays int
}

type Key struct {
	ID, Name, Prefix, Email                    string
	Groups                                     []string
	Scope                                      Scope
	Created, Expires, SignedIn, Ceiling, Until time.Time
	Hash                                       string
	UID, ResourceVersion                       string
	// IssuedBy is the admin who created a service account key (the only person who saw it); "" for personal keys.
	IssuedBy string
	// Exposed is when the exposure rule lowered the key's end date (its issuer was offboarded); zero when never.
	Exposed time.Time
	// Agent is set on an agent's key: what the gateway reads from the key's metadata (docs/agents.md).
	Agent *KeyAgent
}

// KeyAgent is an agent key's metadata. The gateway reads these fields as apiKey.agent, apiKey.agentMode, and so on;
// only the console writes them (key Secrets are fenced by admission). Experimental in 0.6.x.
type KeyAgent struct {
	Name         string   // the agent's name, as in the record
	Mode         string   // person or itself
	OwnerAtIssue string   // the owner when the key was issued; never changed, so Audit shows who was accountable then
	Platform     string   // the platform the agent's record declares (never a proof)
	Scope        []string // "server" and "server/tool" entries, at most 50; empty: the key may use no tool
	Rev          int      // the record's revision this scope was copied from
	SourceRanges []string // at most 10 CIDRs; the key is accepted only from these addresses
}

// KindServiceAccount is the owner record kind of a service account; a person's record has no kind.
const KindServiceAccount = "service-account"

type Owner struct {
	Email      string    `json:"email"`
	Groups     []string  `json:"groups"`
	SignedIn   time.Time `json:"signedIn"`
	Offboarded bool      `json:"offboarded"`
	// Service accounts only. Kind never changes once written; Deleted marks a tombstone, which keeps the name taken.
	Kind string `json:"kind,omitempty"`
	// Agent names the itself-mode agent this account is, "" for an ordinary account: the account's keys are then made
	// only through the agent (they carry its scope).
	Agent                string    `json:"agent,omitempty"`
	Description          string    `json:"description,omitempty"`
	CreatedBy            string    `json:"createdBy,omitempty"`
	Created              time.Time `json:"created,omitzero"`
	Deleted              bool      `json:"deleted,omitempty"`
	UID, ResourceVersion string    `json:"-"`
}

// service reports whether the record is a service account's.
func (o Owner) service() bool { return o.Kind == KindServiceAccount }

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// NewSecretKey returns a new key (shown once), its display prefix and its SHA-256 hash, the only form stored.
func NewSecretKey() (plain, prefix, hash string, err error) {
	b := make([]byte, 43)
	max := big.NewInt(int64(len(base62)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", "", "", err
		}
		b[i] = base62[n.Int64()]
	}
	plain = "ambit_" + string(b)
	sum := sha256.Sum256([]byte(plain))
	return plain, plain[:10], "sha256:" + hex.EncodeToString(sum[:]), nil
}

func newKeyID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))[:12], nil
}

func ownerHash(email string) string {
	sum := sha256.Sum256([]byte(email))
	return hex.EncodeToString(sum[:])[:32]
}

func until(ceiling, signedIn time.Time, inactivityDays int) time.Time {
	u := signedIn.UTC().AddDate(0, 0, inactivityDays)
	if ceiling.Before(u) {
		u = ceiling
	}
	return u.UTC().Truncate(time.Second)
}

type keyEntry struct {
	KeyHash  string `json:"keyHash"`
	Metadata struct {
		KeyID  string   `json:"keyId"`
		Email  string   `json:"email"`
		Groups []string `json:"groups"`
		Until  string   `json:"until"`
		// Agent keys only (all omitted otherwise).
		Agent             string   `json:"agent,omitempty"`
		AgentMode         string   `json:"agentMode,omitempty"`
		AgentOwnerAtIssue string   `json:"agentOwnerAtIssue,omitempty"`
		PlatformDeclared  string   `json:"platformDeclared,omitempty"`
		AgentScope        []string `json:"agentScope,omitempty"` // an empty list is omitted: the gateway then lets the key use no tool
		AgentRev          int      `json:"agentRev,omitempty"`
		SourceRanges      []string `json:"sourceRanges,omitempty"`
	} `json:"metadata"`
}

func (k Key) Secret(ns string) *corev1.Secret {
	var e keyEntry
	e.KeyHash = k.Hash
	e.Metadata.KeyID, e.Metadata.Email, e.Metadata.Groups = k.ID, k.Email, nonNil(k.Groups)
	e.Metadata.Until = k.Until.UTC().Format(time.RFC3339)
	if a := k.Agent; a != nil {
		e.Metadata.Agent, e.Metadata.AgentMode, e.Metadata.AgentOwnerAtIssue = a.Name, a.Mode, a.OwnerAtIssue
		e.Metadata.PlatformDeclared, e.Metadata.AgentScope, e.Metadata.AgentRev, e.Metadata.SourceRanges = a.Platform, a.Scope, a.Rev, a.SourceRanges
	}
	b, _ := json.Marshal(e)
	labels := map[string]string{keyLabel: "true", keyOwnerLabel: ownerHash(k.Email)}
	if k.Agent != nil {
		labels[agentLabel] = k.Agent.Name
	}
	if k.Scope == ScopeLLM || k.Scope == ScopeBoth {
		labels[keyLLMLabel] = "true"
	}
	if k.Scope == ScopeMCP || k.Scope == ScopeBoth {
		labels[keyMCPLabel] = "true"
	}
	ts := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	ann := map[string]string{
		annPrefix + "name": k.Name, annPrefix + "prefix": k.Prefix, annPrefix + "owner-email": k.Email,
		annPrefix + "created": ts(k.Created), annPrefix + "expires": ts(k.Expires), annPrefix + "signed-in": ts(k.SignedIn),
		annPrefix + "ceiling": ts(k.Ceiling), annPrefix + "scope": string(k.Scope)}
	if k.IssuedBy != "" {
		ann[annPrefix+"issued-by"] = k.IssuedBy
	}
	if !k.Exposed.IsZero() {
		ann[annPrefix+"exposed"] = ts(k.Exposed)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ambit-key-" + k.ID, Namespace: ns, Labels: labels, UID: typesUID(k.UID), ResourceVersion: k.ResourceVersion,
			Annotations: ann},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"key": b},
	}
}

func KeyFromSecret(s *corev1.Secret) (Key, error) {
	if len(s.Data) != 1 || len(s.Data["key"]) == 0 {
		return Key{}, fmt.Errorf("key Secret %s must hold exactly one entry, key", s.Name)
	}
	var e keyEntry
	if err := json.Unmarshal(s.Data["key"], &e); err != nil || e.Metadata.KeyID == "" {
		return Key{}, fmt.Errorf("key Secret %s: unreadable entry", s.Name)
	}
	a := s.Annotations
	p := func(name string) (time.Time, error) { return time.Parse(time.RFC3339, a[annPrefix+name]) }
	k := Key{ID: e.Metadata.KeyID, Email: e.Metadata.Email, Groups: e.Metadata.Groups, Hash: e.KeyHash,
		Name: a[annPrefix+"name"], Prefix: a[annPrefix+"prefix"], Scope: Scope(a[annPrefix+"scope"]),
		UID: string(s.UID), ResourceVersion: s.ResourceVersion, IssuedBy: a[annPrefix+"issued-by"]}
	if m := e.Metadata; m.Agent != "" {
		k.Agent = &KeyAgent{Name: m.Agent, Mode: m.AgentMode, OwnerAtIssue: m.AgentOwnerAtIssue, Platform: m.PlatformDeclared,
			Scope: m.AgentScope, Rev: m.AgentRev, SourceRanges: m.SourceRanges}
	}
	if v := a[annPrefix+"exposed"]; v != "" {
		k.Exposed, _ = time.Parse(time.RFC3339, v) // unreadable: treated as never exposed; the ceiling holds the date
	}
	var errs []error
	var err error
	if k.Until, err = time.Parse(time.RFC3339, e.Metadata.Until); err != nil {
		errs = append(errs, err)
	}
	for name, dst := range map[string]*time.Time{"created": &k.Created, "expires": &k.Expires, "signed-in": &k.SignedIn, "ceiling": &k.Ceiling} {
		if *dst, err = p(name); err != nil {
			errs = append(errs, err)
		}
	}
	if k.Scope != ScopeLLM && k.Scope != ScopeMCP && k.Scope != ScopeBoth {
		errs = append(errs, fmt.Errorf("scope %q", k.Scope))
	}
	if err := errors.Join(errs...); err != nil {
		return Key{}, fmt.Errorf("key Secret %s: %w", s.Name, err)
	}
	return k, nil
}

func (o Owner) Secret(ns string) *corev1.Secret {
	b, _ := json.Marshal(o)
	labels := map[string]string{ownerRecordLabel: "true"}
	if o.service() {
		labels[ownerKindLabel] = KindServiceAccount
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ambit-key-owner-" + ownerHash(o.Email), Namespace: ns,
			Labels: labels, UID: typesUID(o.UID), ResourceVersion: o.ResourceVersion},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"owner.json": b},
	}
}

func OwnerFromSecret(s *corev1.Secret) (Owner, error) {
	var o Owner
	if err := json.Unmarshal(s.Data["owner.json"], &o); err != nil || o.Email == "" {
		return Owner{}, fmt.Errorf("owner record %s unreadable", s.Name)
	}
	o.UID, o.ResourceVersion = string(s.UID), s.ResourceVersion
	return o, nil
}

func typesUID(s string) types.UID {
	return types.UID(s)
}
