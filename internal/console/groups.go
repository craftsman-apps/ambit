package console

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	groupsConfigMap = "ambit-console-groups" // no ambitproject.dev/key* label: the ambit-keys-writer policy selects those
	maxKnownGroups  = 1000
	groupDay        = "2006-01-02"
)

// GroupStore remembers the group names seen at sign-in (names only), so the access rule form can suggest them.
// Groups come from the identity provider's "groups" claim; Ambit does not create them.
type GroupStore struct {
	Client    client.Client
	Namespace string
	Now       func() time.Time // tests; nil is time.Now
}

type knownGroup struct {
	Name     string `json:"name"`
	LastSeen string `json:"lastSeen"` // UTC date, YYYY-MM-DD
}

// RecordAsync records the groups in the background; sign-in never waits for it. Errors are logged only.
func (g *GroupStore) RecordAsync(ctx context.Context, groups []string) {
	if len(groups) == 0 {
		return
	}
	ctx = context.WithoutCancel(ctx)
	go func() {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := g.Record(ctx, groups); err != nil {
			slog.Error("record sign-in groups", "err", err)
		}
	}()
}

// Record adds new names and refreshes last-seen dates that are older than today (UTC). It writes only when
// something changed, and retries an update conflict up to 3 times.
func (g *GroupStore) Record(ctx context.Context, groups []string) error {
	now := time.Now()
	if g.Now != nil {
		now = g.Now()
	}
	today := now.UTC().Format(groupDay)
	var err error
	for range 3 {
		if err = g.recordOnce(ctx, groups, today); !apierrors.IsConflict(err) && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return err
}

func (g *GroupStore) recordOnce(ctx context.Context, groups []string, today string) error {
	key := client.ObjectKey{Namespace: g.Namespace, Name: groupsConfigMap}
	var cm corev1.ConfigMap
	create := false
	if err := g.Client.Get(ctx, key, &cm); apierrors.IsNotFound(err) {
		cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
		create = true
	} else if err != nil {
		return err
	}
	known := parseKnownGroups(cm.Data["groups"])
	idx := map[string]int{}
	for i, k := range known {
		idx[k.Name] = i
	}
	changed := false
	for _, n := range groups {
		if len(n) > 256 || !groupRE.MatchString(n) {
			continue
		}
		if i, ok := idx[n]; !ok {
			idx[n] = len(known)
			known = append(known, knownGroup{Name: n, LastSeen: today})
			changed = true
		} else if known[i].LastSeen < today {
			known[i].LastSeen = today
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if len(known) > maxKnownGroups { // drop the oldest last-seen
		sort.Slice(known, func(i, j int) bool {
			if known[i].LastSeen != known[j].LastSeen {
				return known[i].LastSeen > known[j].LastSeen
			}
			return known[i].Name < known[j].Name
		})
		known = known[:maxKnownGroups]
	}
	slices.SortFunc(known, func(a, b knownGroup) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		}
		return 0
	})
	raw, _ := json.Marshal(known)
	cm.Data = map[string]string{"groups": string(raw)}
	if create {
		return g.Client.Create(ctx, &cm)
	}
	return g.Client.Update(ctx, &cm)
}

func parseKnownGroups(raw string) []knownGroup {
	var out []knownGroup
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			slog.Warn("ambit-console-groups is not valid JSON; starting over", "err", err)
		}
	}
	return out
}

// Names returns the recorded group names.
func (g *GroupStore) Names(ctx context.Context) ([]string, error) {
	var cm corev1.ConfigMap
	if err := g.Client.Get(ctx, client.ObjectKey{Namespace: g.Namespace, Name: groupsConfigMap}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, k := range parseKnownGroups(cm.Data["groups"]) {
		out = append(out, k.Name)
	}
	return out, nil
}

// GroupView is one known group and where it was seen: "rules", "sign-in" or "both".
type GroupView struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

func (c *Catalog) listGroups(w http.ResponseWriter, r *http.Request, _ Session) {
	var rs v1.AccessRuleList
	if err := c.Client.List(r.Context(), &rs, client.InNamespace(c.Namespace)); err != nil {
		serverErr(w, err)
		return
	}
	src := map[string]string{}
	for _, ar := range rs.Items {
		for _, n := range ar.Spec.Groups {
			src[n] = "rules"
		}
	}
	if c.Groups != nil {
		seen, err := c.Groups.Names(r.Context())
		if err != nil {
			serverErr(w, err)
			return
		}
		for _, n := range seen {
			if src[n] == "rules" {
				src[n] = "both"
			} else {
				src[n] = "sign-in"
			}
		}
	}
	out := []GroupView{}
	for n, s := range src {
		out = append(out, GroupView{Name: n, Source: s})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}
