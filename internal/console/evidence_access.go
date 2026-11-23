package console

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/controller"
	"github.com/craftsman-apps/ambit/internal/identity"
)

// The access review export: which groups and people the rules allowed to use which models, MCP servers and tools at
// the end of a date D (from the controller's access snapshots), which keys were alive then, and who was seen using each
// group in the 90 days before. Group membership itself comes from the identity provider: Ambit keeps no directory.

// peopleSeenDays is how far before the end of D people-seen.csv looks.
const peopleSeenDays = 90

const snapshotWhere = `LogAttributes['ambit.kind'] = '` + controller.SnapshotKind + `'`

// The snapshot in force at {end}: the newest record before it.
const snapshotInForceSQL = `SELECT Timestamp, LogAttributes['ambit.rulesVersion'], LogAttributes['ambit.rulesHash'], LogAttributes['ambit.snapshot.accessMode'], Body
FROM otel_logs
WHERE ` + snapshotWhere + ` AND Timestamp < {end:DateTime64(9)}
ORDER BY Timestamp DESC
LIMIT 1`

// The snapshots of [start, end): every rules version of the day, with its time.
const snapshotsOfDaySQL = `SELECT Timestamp, LogAttributes['ambit.rulesVersion'], LogAttributes['ambit.rulesHash'], LogAttributes['ambit.snapshot.accessMode']
FROM otel_logs
WHERE ` + snapshotWhere + ` AND Timestamp >= {start:DateTime64(9)} AND Timestamp < {end:DateTime64(9)}
ORDER BY Timestamp`

const firstSnapshotSQL = `SELECT count(), min(Timestamp) FROM otel_logs WHERE ` + snapshotWhere

// People seen with each group: gateway records of the 90 days before {end} that carried the group (jwt.groups).
var peopleSeenSQL = withClassify(`{classify}
SELECT arrayJoin(c_groups) AS grp, c_person, min(Timestamp), max(Timestamp), count()
FROM otel_logs
WHERE LogAttributes['ambit.kind'] = '' AND Timestamp >= {start:DateTime64(9)} AND Timestamp < {end:DateTime64(9)} AND c_person != ''
GROUP BY grp, c_person
ORDER BY grp, c_person`)

// keyDeleteActions are the key history actions that delete a key.
var keyDeleteActions = []string{"revoke", "revoke-self", "offboard-delete", "sa-delete-key", "cleanup-delete", "repair-delete"}

type snapshotInfo struct {
	Time         time.Time `json:"time"`
	RulesVersion string    `json:"rulesVersion"`
	RulesHash    string    `json:"rulesHash"`
	AccessMode   string    `json:"accessMode"`
}

// firstSnapshot is the time of the oldest access snapshot kept; nil when there is none.
func (c *Catalog) firstSnapshot(ctx context.Context) (*time.Time, error) {
	var n uint64
	var t time.Time
	if err := c.Audit.DB.QueryRowContext(exportContext(ctx, nil), firstSnapshotSQL).Scan(&n, &t); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	t = t.UTC()
	return &t, nil
}

// argumentLimits is a short readable form of a server's argument limits: "repo oneOf [web, api]; per_page max 100".
func argumentLimits(rules []v1.ArgumentRule) string {
	var out []string
	for _, a := range rules {
		if len(a.OneOf) > 0 {
			out = append(out, a.Name+" oneOf ["+strings.Join(a.OneOf, ", ")+"]")
		}
		if len(a.NotContains) > 0 {
			out = append(out, a.Name+" notContains ["+strings.Join(a.NotContains, ", ")+"]")
		}
		if a.Max != "" {
			out = append(out, a.Name+" max "+a.Max)
		}
	}
	return strings.Join(out, "; ")
}

// grantRows are the rows of grants.csv for the rules of a snapshot body: one per (rule, who, what).
func grantRows(body string) ([][5]string, error) {
	var rules []struct {
		Name string            `json:"name"`
		Spec v1.AccessRuleSpec `json:"spec"`
	}
	if err := json.Unmarshal([]byte(body), &rules); err != nil {
		return nil, fmt.Errorf("the access snapshot's body is not the AccessRule list: %w", err)
	}
	var out [][5]string // rule, who_type, who, what_type, what; argument limits follow per row below
	for _, r := range rules {
		var who [][2]string
		for _, g := range r.Spec.Groups {
			who = append(who, [2]string{"group", g})
		}
		for _, u := range r.Spec.Users {
			kind := "user"
			if identity.IsService(u) {
				kind = "service account"
			}
			who = append(who, [2]string{kind, u})
		}
		if r.Spec.AllSignedIn {
			who = append(who, [2]string{"all-signed-in", "*"})
		}
		var what [][3]string // type, name, argument limits
		for _, m := range r.Spec.Models {
			what = append(what, [3]string{"model", m, ""})
		}
		for _, s := range r.Spec.Servers {
			limits := argumentLimits(s.Arguments)
			if len(s.Tools) == 0 {
				what = append(what, [3]string{"server", s.Name, limits})
			}
			for _, t := range s.Tools {
				what = append(what, [3]string{"tool", s.Name + "/" + t, limits})
			}
		}
		for _, w := range who {
			for _, x := range what {
				out = append(out, [5]string{r.Name, w[0], w[1], x[0], x[1] + "\x00" + x[2]})
			}
		}
	}
	return out, nil
}

// keyAlive is one row of keys.csv.
type keyAlive struct {
	id, owner, ownerType, groups string
	created, until, revoked      time.Time
}

// keysAliveAt lists the keys alive at end: the stored keys created before it whose end is after it, and the keys
// deleted since, from the key history (create and delete events).
func (c *Catalog) keysAliveAt(ctx context.Context, end time.Time) ([]keyAlive, error) {
	ownerType := func(email string) string {
		if identity.IsService(email) {
			return "service account"
		}
		return "person"
	}
	var out []keyAlive
	stored := map[string]bool{}
	keys, err := c.Keys.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		stored[k.ID] = true
		if k.Created.After(end) || !k.Until.After(end) {
			continue
		}
		out = append(out, keyAlive{id: k.ID, owner: k.Email, ownerType: ownerType(k.Email), groups: strings.Join(k.Groups, ", "), created: k.Created, until: k.Until})
	}
	events, err := c.Keys.EventsBetween(ctx, time.Time{}, c.Evidence.now().Add(time.Minute), append([]string{"create"}, keyDeleteActions...)...)
	if err != nil {
		return nil, err
	}
	deleted := map[string]time.Time{} // the first delete event of each key (events are oldest first)
	for _, e := range events {
		if _, seen := deleted[e.KeyID]; e.Action != "create" && !seen {
			deleted[e.KeyID] = e.At
		}
	}
	for _, e := range events { // a deleted key was alive at end when it was created before and deleted after
		if d, ok := deleted[e.KeyID]; e.Action == "create" && !stored[e.KeyID] && !e.At.After(end) && ok && d.After(end) {
			out = append(out, keyAlive{id: e.KeyID, owner: e.Email, ownerType: ownerType(e.Email), created: e.At, revoked: d})
		}
	}
	slices.SortFunc(out, func(a, b keyAlive) int { return cmp.Or(a.created.Compare(b.created), strings.Compare(a.id, b.id)) })
	return out, nil
}

func (c *Catalog) prepareAccessExport(ctx context.Context, q url.Values) (*exportJob, error) {
	if c.Audit == nil {
		return nil, refuse(http.StatusNotFound, "the audit store is off (evidence.enabled: false): access snapshots are kept there, so there is no access review to export")
	}
	if q.Has("from") || q.Has("to") || q.Has("email") {
		return nil, refuse(http.StatusBadRequest, "the access export takes date=YYYY-MM-DD (the end of that UTC day), not from, to or email")
	}
	day, err := time.Parse("2006-01-02", q.Get("date"))
	if err != nil {
		return nil, refuse(http.StatusBadRequest, "date: use 2026-12-31")
	}
	end := day.AddDate(0, 0, 1)
	first, err := c.firstSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	var snap snapshotInfo
	var body string
	err = c.Audit.DB.QueryRowContext(exportContext(ctx, clickhouse.Parameters{"end": chTime(end)}), snapshotInForceSQL).
		Scan(&snap.Time, &snap.RulesVersion, &snap.RulesHash, &snap.AccessMode, &body)
	switch {
	case errors.Is(err, sql.ErrNoRows) && first == nil:
		return nil, refuse(http.StatusNotFound, "No snapshot for this date: the controller has not recorded the access rules yet (evidence.enabled, Ambit 0.5.0 or later).")
	case errors.Is(err, sql.ErrNoRows):
		return nil, refuse(http.StatusNotFound, "No snapshot for this date. Snapshots exist from %s (the upgrade to 0.5.0).", first.Format("2006-01-02"))
	case err != nil:
		return nil, err
	}
	snap.Time = snap.Time.UTC()
	grants, err := grantRows(body)
	if err != nil {
		return nil, err
	}
	var ofDay []snapshotInfo
	rows, err := c.Audit.DB.QueryContext(exportContext(ctx, clickhouse.Parameters{"start": chTime(day), "end": chTime(end)}), snapshotsOfDaySQL)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var s snapshotInfo
		if err := rows.Scan(&s.Time, &s.RulesVersion, &s.RulesHash, &s.AccessMode); err != nil {
			rows.Close()
			return nil, err
		}
		s.Time = s.Time.UTC()
		ofDay = append(ofDay, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	src, err := c.auditSource(ctx)
	if err != nil {
		return nil, err
	}
	var keys []keyAlive
	keyHistory := map[string]any{"retention": eventRetention.String(), "oldestEventKept": nil}
	if c.Keys != nil {
		if keys, err = c.keysAliveAt(ctx, end); err != nil {
			return nil, fmt.Errorf("read the keys and the key history: %w", err)
		}
		oldest, err := c.Keys.OldestEvent(ctx)
		if err != nil {
			return nil, fmt.Errorf("read the key history: %w", err)
		}
		if !oldest.IsZero() {
			keyHistory["oldestEventKept"] = oldest.UTC()
		}
	}
	date := day.Format("2006-01-02")
	seenFrom := end.AddDate(0, 0, -peopleSeenDays)
	seenParams := clickhouse.Parameters{"start": chTime(seenFrom), "end": chTime(end)}
	job := &exportJob{
		export:     "access",
		parameters: map[string]string{"date": date, "end": end.Format(time.RFC3339), "peopleSeenFrom": seenFrom.Format(time.RFC3339)},
		sources: map[string]any{"snapshot": snap, "snapshotsOfTheDay": ofDay, "snapshotsSince": first, "accessMode": snap.AccessMode,
			"auditRetention": src.Retention, "auditRetentionFrom": src.RetentionFrom, "oldestRecordKept": src.Oldest,
			"periodStartsInsideRetention": src.insideRetention(seenFrom), "keyHistory": keyHistory},
		queries: map[string]string{"grants.csv": snapshotInForceSQL, "people-seen.csv": peopleSeenSQL,
			"keys.csv": "key Secrets (label ambitproject.dev/key) and the key history (create and delete events)"},
		notes:    []string{noteUTC, noteSigned, notePersonal, noteHashes},
		filename: "ambit-access-" + date + ".zip",
		period:   date,
	}
	job.notes = append(job.notes,
		fmt.Sprintf("grants.csv is the AccessRules in force at the end of %s: the newest access snapshot before it (sources.snapshot, recorded %s). A snapshot is written when a change is active in the gateway, when only an AccessRule's text changes, at controller start, and daily at 00:05 UTC (the daily record repeats the state; it is not a measurement at midnight).", date, snap.Time.Format(time.RFC3339)),
		"rules_version 'unknown': the snapshot has no rules version. Gateway records carry the rules version that judged each call (ambit.rulesVersion), so a call can be tied to these rules.",
		"Ambit keeps no group directory: the identity provider decides who is in each group. people-seen.csv shows observed use (people whose calls carried the group in the 90 days before the end of the date), not who may use the group's access: a member who never made a call is not in it. A complete access review needs a dated member list of each granted group from your identity provider (docs/guides/access-review.md).",
		"keys.csv lists the keys alive at the end of the date: stored keys created before it whose end (until) is after it, and keys deleted since (revoked_at) from the key history, which is kept 400 days (sources.keyHistory). Groups of deleted keys are not kept, so they are empty. Key values and hashes are never exported.")
	if len(ofDay) > 1 {
		job.notes = append(job.notes, fmt.Sprintf("The rules changed during %s: sources.snapshotsOfTheDay lists each of the %d snapshots of the day with its time; grants.csv is the last one.", date, len(ofDay)))
	}
	audit := snap.AccessMode == "audit"
	for _, s := range ofDay {
		audit = audit || s.AccessMode == "audit"
	}
	if audit {
		job.notes = append(job.notes, "Audit-only mode (access.mode: audit) was in force on this date (sources.snapshotsOfTheDay): the rules were not enforced, so \"may use\" was not \"was limited to\" during that time.")
	}
	if !src.insideRetention(seenFrom) {
		job.notes = append(job.notes, "The 90 days of people-seen.csv start before the oldest audit record kept (sources.oldestRecordKept): people seen only before it are missing.")
	}
	job.files = []exportFile{
		{name: "grants.csv", header: []string{"snapshot_time", "rules_version", "rules_hash", "rule", "who_type", "who", "what_type", "what", "argument_limits", "access_mode"},
			rows: func(_ context.Context, w *csvWriter) error {
				for _, g := range grants {
					what, limits, _ := strings.Cut(g[4], "\x00")
					if err := w.Row(csvTime(snap.Time), cmp.Or(snap.RulesVersion, "unknown"), snap.RulesHash, g[0], g[1], g[2], g[3], what, limits, snap.AccessMode); err != nil {
						return err
					}
				}
				return nil
			}},
		{name: "keys.csv", header: []string{"key_id", "owner", "owner_type", "groups", "created", "until", "revoked_at"}, rows: func(_ context.Context, w *csvWriter) error {
			for _, k := range keys {
				if err := w.Row(k.id, k.owner, k.ownerType, k.groups, csvTime(k.created), csvTime(k.until), csvTime(k.revoked)); err != nil {
					return err
				}
			}
			return nil
		}},
		{name: "people-seen.csv", header: []string{"group", "email", "first_seen", "last_seen", "calls"}, rows: func(ctx context.Context, w *csvWriter) error {
			rows, err := c.Audit.DB.QueryContext(exportContext(ctx, seenParams), peopleSeenSQL)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var group, person string
				var firstSeen, lastSeen time.Time
				var calls uint64
				if err := rows.Scan(&group, &person, &firstSeen, &lastSeen, &calls); err != nil {
					return err
				}
				if err := w.Row(group, person, csvTime(firstSeen), csvTime(lastSeen), calls); err != nil {
					return err
				}
			}
			return rows.Err()
		}},
	}
	return job, nil
}
