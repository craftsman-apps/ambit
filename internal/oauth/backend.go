package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/craftsman-apps/ambit/internal/identity"
)

// ServerChoice is one MCP server a person may let a platform use: what the person's AccessRules allow and the front
// door serves (the console computes the intersection; ambit-auth never decides it).
type ServerChoice struct {
	Name       string `json:"name"`
	Display    string `json:"display"`
	Connection string `json:"connection,omitempty"` // the Connection (a person's own sign-in to an app) the server needs, if any
}

// AgentInput creates the person-mode agent that a consent makes. The grant ID is made by ambit-auth.
type AgentInput struct {
	Owner       string   `json:"owner"`
	Platform    string   `json:"platform"`
	DisplayName string   `json:"displayName"`
	Scope       []string `json:"scope"`
	Connections []string `json:"connections"`
	GrantID     string   `json:"grantID"`
	Groups      []string `json:"groups"` // the groups of the person's sign-in: the console checks the scope against what these allow
}

// Backend is the console's internal API (docs/oauth.md, "Internal API"): agent records are written by the console only.
type Backend interface {
	// Servers: what the person may offer a platform. groups are the groups the identity provider gave at sign-in.
	Servers(ctx context.Context, email string, groups []string) ([]ServerChoice, error)
	CreateAgent(ctx context.Context, in AgentInput) (name string, err error)
	// Active: the agent exists, is not stopped or being deleted, this grant is one of its grants, and the scope of the token is still within
	// the agent's scope. A refresh is allowed only then.
	Active(ctx context.Context, agent, grant string, scope []string) (bool, error)
}

// ErrRefused: the console refused (for example the person has too many agents). The text is for the log; the person and
// the platform get a fixed sentence.
type ErrRefused struct{ Msg string }

func (e ErrRefused) Error() string { return e.Msg }

// ConsoleBackend calls the console with the pod's own service account token (a projected token with the console's
// audience, read again for every call because Kubernetes rotates it).
type ConsoleBackend struct {
	Base      string // for example http://ambit-console.ambit-system.svc:8081
	TokenFile string
	HTTP      *http.Client
}

func (c *ConsoleBackend) call(ctx context.Context, method, path string, body, out any) error {
	tok, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return fmt.Errorf("console token: %w", err)
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil { // a console that does not answer must not hold a sign-in or a refresh for long: 3 s to connect, 6 s in all
		hc = &http.Client{Timeout: 6 * time.Second, Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext}}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("console: %w", stripURL(err)) // the address holds the person's email
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusUnprocessableEntity:
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = "the console refused the request"
		}
		return ErrRefused{e.Error}
	case resp.StatusCode > 299:
		return fmt.Errorf("console: %s %s: HTTP %d", method, pathOnly(path), resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return errors.New("console: bad answer")
		}
	}
	return nil
}

func (c *ConsoleBackend) Servers(ctx context.Context, email string, groups []string) ([]ServerChoice, error) {
	var o struct {
		Servers []ServerChoice `json:"servers"`
	}
	err := c.call(ctx, "POST", "/internal/oauth/v1/servers", map[string]any{"email": email, "groups": groups}, &o)
	return o.Servers, err
}

func (c *ConsoleBackend) CreateAgent(ctx context.Context, in AgentInput) (string, error) {
	var o struct {
		Name string `json:"name"`
	}
	err := c.call(ctx, "POST", "/internal/oauth/v1/agents", in, &o)
	return o.Name, err
}

func (c *ConsoleBackend) Active(ctx context.Context, agent, grant string, scope []string) (bool, error) {
	var o struct {
		Active bool `json:"active"`
	}
	err := c.call(ctx, "POST", "/internal/oauth/v1/active", map[string]any{"agent": agent, "grant": grant, "scope": scope}, &o)
	return o.Active, err
}

// Deny is what the token hook asks: is this person offboarded, is this agent or grant stopped. Stale data is an error,
// and the hook then refuses (fail closed): refresh waits, access tokens in use are not affected.
type Deny interface {
	Offboarded(email string) (bool, error)
	Stopped(agent, grant string) (bool, error)
	// RotatedAt is the time of the last emergency rotation of the signing keys (zero: none). A refresh of a grant made before it is refused.
	RotatedAt() (time.Time, error)
}

// ConfigDeny reads the offboarding list (ConfigMap ambit-offboarded, key emails: a JSON list) and the stop list
// (ConfigMap ambit-agent-stops, key list.json) through get, and keeps the last good answer for at most maxAge.
type ConfigDeny struct {
	Get    func(ctx context.Context, name string) (data map[string]string, err error)
	MaxAge time.Duration
	Now    func() time.Time

	mu      sync.RWMutex
	off     map[string]bool
	stopped map[string]bool
	rotated time.Time
	at      time.Time
}

const (
	OffboardedConfigMap = "ambit-offboarded"
	StopsConfigMap      = "ambit-agent-stops"
	// RotationConfigMap is in rotate.go.
)

func (d *ConfigDeny) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Refresh reads both lists. A missing ConfigMap is an empty list (get returns nil data and no error).
func (d *ConfigDeny) Refresh(ctx context.Context) error {
	off := map[string]bool{}
	od, err := d.Get(ctx, OffboardedConfigMap)
	if err != nil {
		return err
	}
	if s := od["emails"]; s != "" {
		var l []string
		if err := json.Unmarshal([]byte(s), &l); err != nil {
			return errors.New("the offboarding list is unreadable")
		}
		for _, e := range l {
			off[asciiEmail(e)] = true
		}
	}
	st := map[string]bool{}
	sd, err := d.Get(ctx, StopsConfigMap)
	if err != nil {
		return err
	}
	if s := sd["list.json"]; s != "" {
		var l struct {
			Entries []struct{ Kind, Name string } `json:"entries"`
		}
		if err := json.Unmarshal([]byte(s), &l); err != nil {
			return errors.New("the stop list is unreadable")
		}
		for _, e := range l.Entries {
			// An entry that cannot be understood is an error, not an empty list: the last good list stays and goes stale.
			if k := strings.ToLower(e.Kind); (k != "agent" && k != "grant") || e.Name == "" {
				return errors.New("the stop list has an entry that cannot be read")
			}
			st[strings.ToLower(e.Kind)+":"+e.Name] = true
		}
	}
	var rotated time.Time
	rd, err := d.Get(ctx, RotationConfigMap)
	if err != nil {
		return err
	}
	if v := rd["since"]; v != "" {
		if rotated, err = time.Parse(time.RFC3339, v); err != nil {
			return errors.New("the time of the last key rotation is unreadable")
		}
	}
	d.mu.Lock()
	d.off, d.stopped, d.rotated, d.at = off, st, rotated, d.now()
	d.mu.Unlock()
	return nil
}

// Fresh says whether the last good read is recent enough to trust.
func (d *ConfigDeny) Fresh() error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.at.IsZero() || d.now().Sub(d.at) > d.MaxAge {
		return errors.New("the offboarding list and the stop list could not be read recently")
	}
	return nil
}

func (d *ConfigDeny) Offboarded(email string) (bool, error) {
	if err := d.Fresh(); err != nil {
		return false, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.off[asciiEmail(email)], nil
}

func (d *ConfigDeny) Stopped(agent, grant string) (bool, error) {
	if err := d.Fresh(); err != nil {
		return false, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return (agent != "" && d.stopped["agent:"+agent]) || (grant != "" && d.stopped["grant:"+grant]), nil
}

func (d *ConfigDeny) RotatedAt() (time.Time, error) {
	if err := d.Fresh(); err != nil {
		return time.Time{}, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.rotated, nil
}

// Run refreshes every interval until ctx ends.
func (d *ConfigDeny) Run(ctx context.Context, interval time.Duration, logf func(string, error)) {
	for {
		if err := d.Refresh(ctx); err != nil {
			logf("reading the offboarding and stop lists", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// asciiEmail is the address as the rest of Ambit writes it (identity.CanonicalEmail); an unusable one is only trimmed and
// lower-cased A-Z, so that it still matches the same text in the list.
func asciiEmail(s string) string {
	if c, ok := identity.CanonicalEmail(s); ok {
		return c
	}
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return r
	}, strings.TrimSpace(s))
}
