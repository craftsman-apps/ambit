package console

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GitHub opens pull requests in the configuration repository as a GitHub App. The App's key signs a
// 10-minute JWT, which is exchanged for an installation token limited to this one repository.
type GitHub struct {
	API        string // https://api.github.com, or https://<host>/api/v3 for GitHub Enterprise Server
	Repository string // owner/name
	Branch     string // base branch Argo CD watches
	AppID      string
	Key        *rsa.PrivateKey
	HTTP       *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// PullRequest is what the console shows after proposing a change.
type PullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Title  string `json:"title"`
	Branch string `json:"branch"`
	Author string `json:"author,omitempty"`
}

// ParseAppKey reads the App's PEM private key (PKCS#1, as GitHub issues it, or PKCS#8).
func ParseAppKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	b, _ := pem.Decode(pemBytes)
	if b == nil {
		return nil, errors.New("GitHub App key: no PEM block")
	}
	if k, err := x509.ParsePKCS1PrivateKey(b.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(b.Bytes)
	if err != nil {
		return nil, fmt.Errorf("GitHub App key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("GitHub App key: not an RSA key")
	}
	return rk, nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// appJWT authenticates as the App itself (RS256), valid for 9 minutes, backdated 60 s for clock drift.
func (g *GitHub) appJWT(now time.Time) (string, error) {
	header := b64([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iss": g.AppID, "iat": now.Add(-60 * time.Second).Unix(), "exp": now.Add(9 * time.Minute).Unix()})
	signing := header + "." + b64(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, g.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + b64(sig), nil
}

// installationToken returns a cached token, refreshed 5 minutes before GitHub's one-hour expiry.
func (g *GitHub) installationToken(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.token != "" && time.Until(g.expires) > 5*time.Minute {
		return g.token, nil
	}
	jwt, err := g.appJWT(time.Now())
	if err != nil {
		return "", err
	}
	var inst struct {
		ID int64 `json:"id"`
	}
	if err := g.call(ctx, "Bearer "+jwt, http.MethodGet, "/repos/"+g.Repository+"/installation", nil, &inst); err != nil {
		return "", fmt.Errorf("the GitHub App is not installed on %s: %w", g.Repository, err)
	}
	name := g.Repository[strings.Index(g.Repository, "/")+1:]
	body := map[string]any{"repositories": []string{name}, "permissions": map[string]string{"contents": "write", "pull_requests": "write"}}
	var tok struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := g.call(ctx, "Bearer "+jwt, http.MethodPost, "/app/installations/"+strconv.FormatInt(inst.ID, 10)+"/access_tokens", body, &tok); err != nil {
		return "", fmt.Errorf("GitHub installation token: %w", err)
	}
	g.token, g.expires = tok.Token, tok.ExpiresAt
	return g.token, nil
}

// apiError keeps GitHub's status so callers can tell "not found" from real failures.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("GitHub returned %d: %s", e.Status, e.Message) }

func isNotFound(err error) bool {
	var e *apiError
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func (g *GitHub) call(ctx context.Context, auth, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(g.API, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := g.HTTP
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		var m struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &m)
		return &apiError{Status: resp.StatusCode, Message: m.Message}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (g *GitHub) api(ctx context.Context, method, path string, in, out any) error {
	tok, err := g.installationToken(ctx)
	if err != nil {
		return err
	}
	return g.call(ctx, "Bearer "+tok, method, path, in, out)
}

// FileChange is one file to write (Content set) or delete (Content nil) in the pull request.
type FileChange struct {
	Path    string
	Content []byte
}

// OpenPullRequest commits the change on a new branch off the base branch and opens a pull request.
// ponytail: one file per pull request, which is all the console proposes today; use the Git data
// API (trees) when a change needs several files in one commit.
func (g *GitHub) OpenPullRequest(ctx context.Context, branch, title, body string, f FileChange) (PullRequest, error) {
	repo := "/repos/" + g.Repository
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := g.api(ctx, http.MethodGet, repo+"/git/ref/heads/"+url.PathEscape(g.Branch), nil, &ref); err != nil {
		return PullRequest{}, fmt.Errorf("read branch %s: %w", g.Branch, err)
	}
	if err := g.api(ctx, http.MethodPost, repo+"/git/refs", map[string]string{"ref": "refs/heads/" + branch, "sha": ref.Object.SHA}, nil); err != nil {
		return PullRequest{}, fmt.Errorf("create branch %s: %w", branch, err)
	}
	var existing struct {
		SHA string `json:"sha"`
	}
	contents := repo + "/contents/" + escapePath(f.Path)
	err := g.api(ctx, http.MethodGet, contents+"?ref="+url.QueryEscape(g.Branch), nil, &existing)
	if err != nil && !isNotFound(err) {
		return PullRequest{}, fmt.Errorf("read %s: %w", f.Path, err)
	}
	change := map[string]any{"message": title, "branch": branch}
	if existing.SHA != "" {
		change["sha"] = existing.SHA
	}
	if f.Content == nil {
		if existing.SHA == "" {
			return PullRequest{}, fmt.Errorf("%s is not in %s on %s; the object was not created through this repository", f.Path, g.Repository, g.Branch)
		}
		err = g.api(ctx, http.MethodDelete, contents, change, nil)
	} else {
		change["content"] = base64.StdEncoding.EncodeToString(f.Content)
		err = g.api(ctx, http.MethodPut, contents, change, nil)
	}
	if err != nil {
		return PullRequest{}, fmt.Errorf("commit %s: %w", f.Path, err)
	}
	var pr struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := g.api(ctx, http.MethodPost, repo+"/pulls", map[string]string{"title": title, "head": branch, "base": g.Branch, "body": body}, &pr); err != nil {
		return PullRequest{}, fmt.Errorf("open pull request: %w", err)
	}
	return PullRequest{Number: pr.Number, URL: pr.HTMLURL, Title: title, Branch: branch}, nil
}

// OpenPullRequests lists the console's open pull requests (branches named ambit/...), newest first.
func (g *GitHub) OpenPullRequests(ctx context.Context) ([]PullRequest, error) {
	var prs []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		Title   string `json:"title"`
		Head    struct {
			Ref string `json:"ref"`
		} `json:"head"`
		Body string `json:"body"`
	}
	if err := g.api(ctx, http.MethodGet, "/repos/"+g.Repository+"/pulls?state=open&per_page=100&base="+url.QueryEscape(g.Branch), nil, &prs); err != nil {
		return nil, err
	}
	out := []PullRequest{}
	for _, p := range prs {
		if strings.HasPrefix(p.Head.Ref, branchPrefix) {
			out = append(out, PullRequest{Number: p.Number, URL: p.HTMLURL, Title: p.Title, Branch: p.Head.Ref, Author: requestedBy(p.Body)})
		}
	}
	return out, nil
}

const branchPrefix = "ambit/"

// requestedBy reads the "Requested by" line the console writes into every pull request body.
func requestedBy(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "Requested by "); ok && len(strings.Fields(v)) > 0 {
			return strings.Fields(v)[0]
		}
	}
	return ""
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}
