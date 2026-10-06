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
	t           *testing.T
	key         *rsa.PublicKey
	mu          sync.Mutex
	calls       []string
	files       map[string]string // path -> content on main
	fileCommits map[string]string // path -> "put:<content>" or "delete", on the pull request branch
	tokens      int
	prs         []map[string]string

	// Git data API (several files in one commit)
	commits            int
	pulls              int
	lastTree           []string
	branches           map[string]bool
	prsByBranch        map[string]PullRequestState
	failNextPullCreate bool
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
	case r.Method == "GET" && strings.Contains(r.URL.Path, "/git/ref/heads/"):
		if f.branches[strings.SplitN(r.URL.Path, "/git/ref/heads/", 2)[1]] {
			reply(200, map[string]any{"object": map[string]string{"sha": "commit1"}})
			return
		}
		reply(404, map[string]string{"message": "Not Found"})
	case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/git/refs/heads/"):
		name := strings.SplitN(r.URL.Path, "/git/refs/heads/", 2)[1]
		if !f.branches[name] {
			reply(422, map[string]string{"message": "Reference does not exist"})
			return
		}
		delete(f.branches, name)
		w.WriteHeader(204)
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/git/commits/base123"):
		reply(200, map[string]any{"tree": map[string]string{"sha": "tree0"}})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/git/trees"):
		if in["base_tree"] != "tree0" {
			f.t.Errorf("tree without base_tree: %v", in)
		}
		f.lastTree = nil
		entries, _ := in["tree"].([]any)
		for _, e := range entries {
			f.lastTree = append(f.lastTree, e.(map[string]any)["path"].(string))
		}
		reply(201, map[string]string{"sha": "tree1"})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/git/commits"):
		if in["tree"] != "tree1" || len(in["parents"].([]any)) != 1 || in["parents"].([]any)[0] != "base123" {
			f.t.Errorf("commit: %v", in)
		}
		f.commits++
		reply(201, map[string]string{"sha": "commit1"})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/git/refs"):
		ref := in["ref"].(string)
		if (in["sha"] != "base123" && in["sha"] != "commit1") || !strings.HasPrefix(ref, "refs/heads/ambit/") {
			f.t.Errorf("branch: %v", in)
		}
		f.branches[strings.TrimPrefix(ref, "refs/heads/")] = true
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
				f.fileCommits[p] = "put:" + string(c)
			} else {
				f.fileCommits[p] = "delete"
			}
			reply(200, map[string]any{})
		}
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls"):
		if f.failNextPullCreate {
			f.failNextPullCreate = false
			reply(500, map[string]string{"message": "boom"})
			return
		}
		n := len(f.prs) + 7
		f.pulls++
		f.prsByBranch[in["head"].(string)] = PullRequestState{PullRequest: PullRequest{Number: n, Branch: in["head"].(string)}}
		f.prs = append(f.prs, map[string]string{"title": in["title"].(string), "head": in["head"].(string), "body": in["body"].(string)})
		reply(201, map[string]any{"number": n, "html_url": "https://github.com/craftsman-apps/ambit-config-test/pull/" + itoa(n)})
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls") && r.URL.Query().Get("head") != "":
		head := strings.TrimPrefix(r.URL.Query().Get("head"), "craftsman-apps:")
		if st, ok := f.prsByBranch[head]; ok {
			reply(200, []map[string]any{{"number": st.Number}})
			return
		}
		reply(200, []map[string]any{})
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
	case r.Method == "GET" && strings.Contains(r.URL.Path, "/pulls/") && f.prByNumber(r.URL.Path) != nil:
		st := f.prByNumber(r.URL.Path)
		state := "open"
		if st.Closed || st.Merged {
			state = "closed"
		}
		reply(200, map[string]any{"number": st.Number, "state": state, "merged": st.Merged,
			"head": map[string]any{"ref": st.Branch, "repo": map[string]string{"full_name": "craftsman-apps/ambit-config-test"}}})
	default:
		reply(404, map[string]string{"message": "Not Found: " + r.Method + " " + r.URL.Path})
	}
}

func (f *fakeGitHub) prByNumber(path string) *PullRequestState {
	for _, st := range f.prsByBranch {
		if strings.HasSuffix(path, "/pulls/"+itoa(st.Number)) {
			return &st
		}
	}
	return nil
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
	fake := &fakeGitHub{t: t, key: &key.PublicKey, files: files, branches: map[string]bool{}, fileCommits: map[string]string{}, prsByBranch: map[string]PullRequestState{}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return &GitHub{API: srv.URL, Repository: "craftsman-apps/ambit-config-test", Branch: "main", AppID: "123456", Key: key}, fake
}

func TestPullRequestWritesOneFileOnANewBranch(t *testing.T) {
	gh, fake := githubFixture(t, map[string]string{"ambit/accessrules/eng-time.yaml": "old"})
	g := &GitOps{Repo: gh, Branch: "main", Dir: "ambit"}
	manifest, _ := Manifest("AccessRule", "eng-time", nil, nil, map[string]any{"groups": []string{"eng"}, "models": []string{"gemma"}})
	pr, err := g.Propose(t.Context(), "update", "AccessRule", "accessrules", "eng-time", "priya@acme.test", manifest)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 7 || !strings.HasSuffix(pr.URL, "/pull/7") || pr.Title != "Ambit: update access rule eng-time" {
		t.Fatalf("pr %+v", pr)
	}
	got := fake.fileCommits["ambit/accessrules/eng-time.yaml"]
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
	if fake.fileCommits["ambit/accessrules/eng-time.yaml"] != "delete" {
		t.Fatalf("commits %v", fake.fileCommits)
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

func newFakeGitHub(t *testing.T) (*GitHub, *fakeGitHub) { return githubFixture(t, nil) }

func TestOpenPullRequestFilesMakesOneCommit(t *testing.T) {
	gh, fake := newFakeGitHub(t)
	pr, err := gh.OpenPullRequestFiles(t.Context(), "ambit/add-github-op1abc", "Ambit: add app github", "body",
		[]FileChange{{Path: "ambit/connections/github.yaml", Content: []byte("a")}, {Path: "ambit/servers/github.yaml", Content: []byte("b")}})
	if err != nil || pr.Branch != "ambit/add-github-op1abc" {
		t.Fatalf("pr %+v err %v", pr, err)
	}
	if fake.commits != 1 || len(fake.lastTree) != 2 {
		t.Fatalf("commits %d tree %v", fake.commits, fake.lastTree)
	}
}

func TestOpenPullRequestFilesRetryReusesBranchAndPR(t *testing.T) {
	gh, fake := newFakeGitHub(t)
	files := []FileChange{{Path: "ambit/connections/github.yaml", Content: []byte("a")}}
	fake.failNextPullCreate = true // the first attempt dies after the branch exists
	if _, err := gh.OpenPullRequestFiles(t.Context(), "ambit/add-github-op1abc", "t", "b", files); err == nil {
		t.Fatal("expected the injected failure")
	}
	pr, err := gh.OpenPullRequestFiles(t.Context(), "ambit/add-github-op1abc", "t", "b", files)
	if err != nil || pr.Number == 0 || fake.commits != 1 {
		t.Fatalf("retry: pr %+v err %v commits %d", pr, err, fake.commits)
	}
	again, _ := gh.OpenPullRequestFiles(t.Context(), "ambit/add-github-op1abc", "t", "b", files)
	if again.Number != pr.Number || fake.pulls != 1 {
		t.Fatalf("second retry opened another pull request: %+v", again)
	}
}

func TestOpenPullRequestFilesClosedPullRequestIsAnError(t *testing.T) {
	gh, fake := newFakeGitHub(t)
	fake.prsByBranch["ambit/add-x-1"] = PullRequestState{PullRequest: PullRequest{Number: 5, Branch: "ambit/add-x-1"}, Closed: true}
	if _, err := gh.OpenPullRequestFiles(t.Context(), "ambit/add-x-1", "t", "b", []FileChange{{Path: "a", Content: []byte("a")}}); err == nil || fake.commits != 0 {
		t.Fatalf("want an error and no commit, got %v, commits %d", err, fake.commits)
	}
}

func TestDeleteBranchIgnoresMissing(t *testing.T) {
	gh, fake := newFakeGitHub(t)
	if err := gh.DeleteBranch(t.Context(), "ambit/none"); err != nil {
		t.Fatal(err)
	}
	fake.branches["ambit/there"] = true
	if err := gh.DeleteBranch(t.Context(), "ambit/there"); err != nil || fake.branches["ambit/there"] {
		t.Fatalf("branch not deleted: %v", err)
	}
}

func TestPullRequestByBranchFindsTimedOutPR(t *testing.T) {
	gh, fake := newFakeGitHub(t)
	fake.prsByBranch["ambit/add-github-op1"] = PullRequestState{PullRequest: PullRequest{Number: 7, Branch: "ambit/add-github-op1"}}
	st, ok, err := gh.PullRequestByBranch(t.Context(), "ambit/add-github-op1")
	if err != nil || !ok || st.Number != 7 {
		t.Fatalf("st %+v ok %v err %v", st, ok, err)
	}
	if _, ok, _ := gh.PullRequestByBranch(t.Context(), "ambit/none"); ok {
		t.Fatal("found a pull request that does not exist")
	}
}
