package console

import (
	"context"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/validate"
	"sigs.k8s.io/yaml"
)

// In GitOps mode the API server does not see a change until it is merged, so the console runs the
// same checks first (the ones `ambit validate` runs in the pull request).
var validator = sync.OnceValues(validate.New)

// InvalidChange is a change the cluster would refuse; the console answers 400 instead of opening a pull request.
type InvalidChange struct{ Problems []string }

func (e *InvalidChange) Error() string {
	return "the cluster would refuse this: " + strings.Join(e.Problems, "; ")
}

// PullRequester is the Git host; *GitHub implements it.
type PullRequester interface {
	OpenPullRequest(ctx context.Context, branch, title, body string, f FileChange) (PullRequest, error)
	OpenPullRequests(ctx context.Context) ([]PullRequest, error)
	PullRequestState(ctx context.Context, number int) (PullRequestState, error)
	OpenPullRequestFiles(ctx context.Context, branch, title, body string, files []FileChange) (PullRequest, error)
	PullRequestByBranch(ctx context.Context, branch string) (PullRequestState, bool, error)
	DeleteBranch(ctx context.Context, branch string) error
	// Contains reports whether revision includes commit (revision is commit or a descendant of it).
	Contains(ctx context.Context, commit, revision string) (bool, error)
}

// PullRequestState is where a pull request is: open, merged (with its merge commit) or closed unmerged.
type PullRequestState struct {
	PullRequest
	Merged      bool      `json:"merged"`
	Closed      bool      `json:"closed"`
	MergeCommit string    `json:"mergeCommit,omitempty"`
	MergedAt    time.Time `json:"mergedAt,omitzero"`
}

// GitOps turns console changes into pull requests against the configuration repository that Argo CD
// (or Flux) syncs. Each Ambit object lives at <Dir>/<plural>/<name>.yaml.
type GitOps struct {
	Repo       PullRequester
	Repository string // owner/name, shown in the console
	Branch     string
	Dir        string
}

// ObjectPath is where an object lives in the repository, e.g. ambit/accessrules/eng-time.yaml.
func (g *GitOps) ObjectPath(plural, name string) string {
	return path.Join(g.Dir, plural, name+".yaml")
}

// Manifest renders the object as it should appear in Git: no namespace (the Argo CD Application sets
// it), no status, no server-set metadata.
func Manifest(kind, name string, labels, annotations map[string]string, spec any) ([]byte, error) {
	md := map[string]any{"name": name}
	if len(labels) > 0 {
		md["labels"] = labels
	}
	if len(annotations) > 0 {
		md["annotations"] = annotations
	}
	return yaml.Marshal(map[string]any{"apiVersion": v1.GroupVersion.String(), "kind": kind, "metadata": md, "spec": spec})
}

// Propose opens a pull request that writes (manifest set) or deletes (manifest nil) one object.
func (g *GitOps) Propose(ctx context.Context, verb, kind, plural, name, by string, manifest []byte) (PullRequest, error) {
	if manifest != nil {
		v, err := validator()
		if err != nil {
			return PullRequest{}, err
		}
		var obj map[string]any
		if err := yaml.Unmarshal(manifest, &obj); err != nil {
			return PullRequest{}, err
		}
		if problems, err := v.Object(ctx, obj); err != nil || len(problems) > 0 {
			return PullRequest{}, &InvalidChange{Problems: problems}
		}
	}
	title := fmt.Sprintf("Ambit: %s %s %s", verb, humanKind(kind), name)
	var b strings.Builder
	fmt.Fprintf(&b, "Requested by %s in the Ambit console.\n\n", by)
	if manifest != nil {
		fmt.Fprintf(&b, "```yaml\n%s```\n\n", manifest)
	} else {
		fmt.Fprintf(&b, "Deletes `%s`.\n\n", g.ObjectPath(plural, name))
	}
	b.WriteString("Merging this pull request applies the change; Argo CD syncs it to the cluster. Review it like any other change.\n")
	branch := fmt.Sprintf("%s%s-%s-%s-%d", branchPrefix, verb, strings.ToLower(kind), name, time.Now().Unix())
	return g.Repo.OpenPullRequest(ctx, branch, title, b.String(), FileChange{Path: g.ObjectPath(plural, name), Content: manifest})
}

// ProposedObject is one manifest of a multi-object change.
type ProposedObject struct {
	Kind, Plural, Name string
	Manifest           []byte
}

// ProposeObjects opens one pull request, one commit, for several objects (e.g. an app and its server).
// The caller must use a branch unique to one change (Task 13 uses one per operation ID): an existing
// branch is reused as it is, and its content is not compared with objs.
func (g *GitOps) ProposeObjects(ctx context.Context, branch, title, by string, objs []ProposedObject) (PullRequest, error) {
	v, err := validator()
	if err != nil {
		return PullRequest{}, err
	}
	var files []FileChange
	var b strings.Builder
	fmt.Fprintf(&b, "Requested by %s in the Ambit console.\n\n", by)
	for _, o := range objs {
		var obj map[string]any
		if err := yaml.Unmarshal(o.Manifest, &obj); err != nil {
			return PullRequest{}, err
		}
		if problems, err := v.Object(ctx, obj); err != nil || len(problems) > 0 {
			return PullRequest{}, &InvalidChange{Problems: problems}
		}
		files = append(files, FileChange{Path: g.ObjectPath(o.Plural, o.Name), Content: o.Manifest})
		fmt.Fprintf(&b, "```yaml\n%s```\n\n", o.Manifest)
	}
	b.WriteString("Merging this pull request applies the change; Argo CD syncs it to the cluster. Review it like any other change.\n")
	return g.Repo.OpenPullRequestFiles(ctx, branch, title, b.String(), files)
}

func humanKind(kind string) string {
	switch kind {
	case "AccessRule":
		return "access rule"
	case "Server":
		return "MCP server"
	}
	return strings.ToLower(kind)
}
