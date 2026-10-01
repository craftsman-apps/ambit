package console

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub is the part of GitHub's REST API the console uses. Files live in a map per branch.
type fakeGitHub struct {
	t       *testing.T
	key     *rsa.PublicKey
	mu      sync.Mutex
	calls   []string
	files   map[string]string // path -> content on main
	commits map[string]string // path -> "put:<content>" or "delete", on the pull request branch
	tokens  int
	prs     []map[string]string
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	body, _ := io.ReadAll(r.Body)
	in := map[string]any{}
	_ = json.Unmarshal(body, &in)
	reply := func(code int, v any) { w.WriteHeader(code); json.NewEncoder(w).Encode(v) }
	switch {
	case strings.HasSuffix(r.URL.Path, "/installation") || strings.HasSuffix(r.URL.Path, "/access_tokens"):
		f.checkAppJWT(auth)
		if strings.HasSuffix(r.URL.Path, "/installation") {
			reply(200, map[string]any{"id": 42})
			return
		}
		if repos, _ := in["repositories"].([]any); len(repos) != 1 || repos[0] != "ambit-config-test" {
			f.t.Errorf("token not limited to one repository: %v", in)
		}
		f.tokens++
		reply(201, map[string]any{"token": "ghs_test", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
	case auth != "ghs_test":
		reply(401, map[string]string{"message": "Bad credentials"})
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/git/ref/heads/main"):
		reply(200, map[string]any{"object": map[string]string{"sha": "base123"}})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/git/refs"):
		if in["sha"] != "base123" || !strings.HasPrefix(in["ref"].(string), "refs/heads/ambit/") {
			f.t.Errorf("branch: %v", in)
		}
		reply(201, map[string]any{})
	case strings.Contains(r.URL.Path, "/contents/"):
		p := r.URL.Path[strings.Index(r.URL.Path, "/contents/")+len("/contents/"):]
		switch r.Method {
		case "GET":
			if _, ok := f.files[p]; !ok {
				reply(404, map[string]string{"message": "Not Found"})
				return
			}
			reply(200, map[string]string{"sha": "file-" + p})
		case "PUT", "DELETE":
			if _, ok := f.files[p]; ok && in["sha"] != "file-"+p {
				f.t.Errorf("%s %s without the current sha: %v", r.Method, p, in)
			}
			if r.Method == "PUT" {
				c, _ := base64.StdEncoding.DecodeString(in["content"].(string))
				f.commits[p] = "put:" + string(c)
			} else {
				f.commits[p] = "delete"
			}
			reply(200, map[string]any{})
		}
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls"):
		n := len(f.prs) + 7
		f.prs = append(f.prs, map[string]string{"title": in["title"].(string), "head": in["head"].(string), "body": in["body"].(string)})
		reply(201, map[string]any{"number": n, "html_url": "https://github.com/craftsman-apps/ambit-config-test/pull/" + itoa(n)})
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls"):
		reply(200, []map[string]any{
			{"number": 9, "html_url": "u9", "title": "Ambit: update access rule eng-time", "head": map[string]string{"ref": "ambit/update-accessrule-eng-time-1"}, "body": "Requested by priya@acme.test in the Ambit console."},
			{"number": 8, "html_url": "u8", "title": "Bump chart", "head": map[string]string{"ref": "renovate/chart"}, "body": ""},
		})
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls/9"):
		reply(200, map[string]any{"number": 9, "html_url": "u9", "title": "Ambit: update access rule eng-time", "state": "closed", "merged": true,
			"merge_commit_sha": "abc1234def", "merged_at": "2026-10-01T06:00:00Z", "head": map[string]any{"ref": "ambit/update-accessrule-eng-time-1", "repo": map[string]string{"full_name": "craftsman-apps/ambit-config-test"}},
			"body": "Requested by priya@acme.test in the Ambit console."})
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls/11"): // from a fork, with a console-like branch
		reply(200, map[string]any{"number": 11, "state": "open", "head": map[string]any{"ref": "ambit/update-accessrule-eng-time-1", "repo": map[string]string{"full_name": "someone/ambit-config-test"}}})
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls/10"):
		reply(200, map[string]any{"number": 10, "state": "closed", "merged": false, "merge_commit_sha": "tmp999", "head": map[string]string{"ref": "ambit/x"}})
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/compare/abc1234def...later"):
		reply(200, map[string]string{"status": "ahead"})
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/compare/abc1234def...older"):
		reply(200, map[string]string{"status": "behind"})
	default:
		reply(404, map[string]string{"message": "Not Found: " + r.Method + " " + r.URL.Path})
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// checkAppJWT verifies what GitHub verifies: RS256 over header.claims with the App's key, iss = App ID.
func (f *fakeGitHub) checkAppJWT(jwt string) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		f.t.Fatalf("not a JWT: %q", jwt)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(f.key, crypto.SHA256, sum[:], sig); err != nil {
		f.t.Fatalf("App JWT signature: %v", err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Iss      string `json:"iss"`
		Iat, Exp int64
	}
	_ = json.Unmarshal(raw, &c)
	if c.Iss != "123456" || c.Exp-c.Iat > 600 || c.Iat > time.Now().Unix() {
		f.t.Fatalf("App JWT claims: %+v (GitHub allows at most 10 minutes)", c)
	}
}

func githubFixture(t *testing.T, files map[string]string) (*GitHub, *fakeGitHub) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeGitHub{t: t, key: &key.PublicKey, files: files, commits: map[string]string{}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return &GitHub{API: srv.URL, Repository: "craftsman-apps/ambit-config-test", Branch: "main", AppID: "123456", Key: key}, fake
}

func TestPullRequestWritesOneFileOnANewBranch(t *testing.T) {
	gh, fake := githubFixture(t, map[string]string{"ambit/accessrules/eng-time.yaml": "old"})
	g := &GitOps{Repo: gh, Branch: "main", Dir: "ambit"}
	manifest, _ := Manifest("AccessRule", "eng-time", nil, map[string]any{"groups": []string{"eng"}, "models": []string{"gemma"}})
	pr, err := g.Propose(t.Context(), "update", "AccessRule", "accessrules", "eng-time", "priya@acme.test", manifest)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 7 || !strings.HasSuffix(pr.URL, "/pull/7") || pr.Title != "Ambit: update access rule eng-time" {
		t.Fatalf("pr %+v", pr)
	}
	got := fake.commits["ambit/accessrules/eng-time.yaml"]
	if !strings.HasPrefix(got, "put:apiVersion: ambitproject.dev/v1alpha1\nkind: AccessRule\nmetadata:\n  name: eng-time\n") || strings.Contains(got, "namespace") {
		t.Fatalf("committed %q", got)
	}
	if !strings.Contains(fake.prs[0]["body"], "Requested by priya@acme.test in the Ambit console.") {
		t.Fatalf("body %q", fake.prs[0]["body"])
	}
	// A second change reuses the installation token.
	if _, err := g.Propose(t.Context(), "create", "AccessRule", "accessrules", "ml-time", "priya@acme.test", manifest); err != nil {
		t.Fatal(err)
	}
	if fake.tokens != 1 {
		t.Fatalf("installation tokens requested: %d, want 1 (cached)", fake.tokens)
	}
}

func TestDeleteNeedsTheFileInGit(t *testing.T) {
	gh, fake := githubFixture(t, map[string]string{"ambit/accessrules/eng-time.yaml": "old"})
	g := &GitOps{Repo: gh, Branch: "main", Dir: "ambit"}
	if _, err := g.Propose(t.Context(), "delete", "AccessRule", "accessrules", "eng-time", "priya@acme.test", nil); err != nil {
		t.Fatal(err)
	}
	if fake.commits["ambit/accessrules/eng-time.yaml"] != "delete" {
		t.Fatalf("commits %v", fake.commits)
	}
	_, err := g.Propose(t.Context(), "delete", "AccessRule", "accessrules", "made-by-kubectl", "priya@acme.test", nil)
	if err == nil || !strings.Contains(err.Error(), "not in craftsman-apps/ambit-config-test") {
		t.Fatalf("want a clear error for an object that is not in Git, got %v", err)
	}
}

func TestOpenPullRequestsListsOnlyTheConsolesOwn(t *testing.T) {
	gh, _ := githubFixture(t, nil)
	prs, err := gh.OpenPullRequests(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 || prs[0].Number != 9 || prs[0].Author != "priya@acme.test" {
		t.Fatalf("%+v", prs)
	}
}

func TestParseAppKeyRejectsGarbage(t *testing.T) {
	if _, err := ParseAppKey([]byte("not a key")); err == nil {
		t.Fatal("want an error")
	}
}

func TestPullRequestStateAndContains(t *testing.T) {
	gh, _ := githubFixture(t, nil)
	st, err := gh.PullRequestState(t.Context(), 9)
	if err != nil || !st.Merged || st.Closed || st.MergeCommit != "abc1234def" || st.Branch != "ambit/update-accessrule-eng-time-1" || st.Author != "priya@acme.test" {
		t.Fatalf("merged pull request: %+v %v", st, err)
	}
	// A closed, unmerged pull request has a test-merge commit; it must not count as merged.
	if st, err := gh.PullRequestState(t.Context(), 10); err != nil || st.Merged || !st.Closed || st.MergeCommit != "" {
		t.Fatalf("closed pull request: %+v %v", st, err)
	}
	if st, err := gh.PullRequestState(t.Context(), 11); err != nil || st.Branch != "" {
		t.Fatalf("a fork's pull request must not look like the console's: %+v %v", st, err)
	}
	if in, err := gh.Contains(t.Context(), "abc1234def", "later"); err != nil || !in {
		t.Errorf("later revision: %v %v", in, err)
	}
	if in, err := gh.Contains(t.Context(), "abc1234def", "older"); err != nil || in {
		t.Errorf("older revision: %v %v", in, err)
	}
}
