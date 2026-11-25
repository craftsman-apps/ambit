package console

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/craftsman-apps/ambit/internal/controller"
)

// The agent-stop export (GET /api/evidence/agent-stop/{agent}.zip): what happened after a stop was requested, as
// measured. It states times and counts, never "zero calls": the calls of the agent after the request that the gateways
// accepted, with their times, and the first call refused by the stop policy (the policy refuses a call that still
// carries a valid key; a call with a key that is already deleted is logged as an invalid key without the agent, so it
// cannot be attributed).

const agentStopCallsSQL = `SELECT Timestamp, LogAttributes['ambit.ingress'], LogAttributes['ambit.identity'], LogAttributes['ambit.key_id'],
       LogAttributes['mcp.method.name'], ` + "{status}" + `
FROM otel_logs
WHERE LogAttributes['ambit.kind'] = '' AND LogAttributes['ambit.agent'] = {agent:String}
  AND Timestamp >= {from:DateTime64(9)} AND Timestamp < {to:DateTime64(9)}
ORDER BY Timestamp`

func (c *Catalog) prepareAgentStopExport(ctx context.Context, name string) (*exportJob, error) {
	if c.Keys == nil {
		return nil, refuse(http.StatusNotFound, "keys and agents are switched off")
	}
	a, err := c.Keys.GetAgent(ctx, name)
	if err != nil {
		return nil, refuse(http.StatusNotFound, "no such agent")
	}
	if a.StopRequestedAt == nil {
		return nil, refuse(http.StatusConflict, "this agent has no stop request")
	}
	l, _, err := c.Keys.stopList(ctx)
	if err != nil {
		return nil, err
	}
	ack, haveAck, err := c.Keys.StopAck(ctx)
	if err != nil {
		return nil, err
	}
	var entry *controller.StopEntry
	if i := entryIndex(l, name); i >= 0 {
		entry = &l.Entries[i]
	}
	from := a.StopRequestedAt.UTC()
	to := c.Evidence.now().Add(time.Minute)
	if a.StoppedAt != nil {
		to = a.StoppedAt.Add(30 * time.Minute) // the retention window: records of later calls are not part of this stop
	}
	params := clickhouse.Parameters{"agent": name, "from": chTime(from), "to": chTime(to)}
	query := strings.ReplaceAll(agentStopCallsSQL, "{status}", status)
	job := &exportJob{
		export:     "agent-stop",
		parameters: map[string]string{"agent": name, "from": from.Format(time.RFC3339Nano), "to": to.Format(time.RFC3339Nano)},
		sources:    map[string]any{"state": a.State, "stopListGeneration": l.Gen, "acknowledgmentKnown": haveAck},
		queries:    map[string]string{"calls-after-request.csv": query},
		filename:   "agent-stop-" + name + ".zip",
		period:     from.Format("2006-01-02"),
		notes: []string{
			noteUTC, noteSigned,
			"timeline.csv: the steps of the stop, from the agent's record, the stop list and the controller's acknowledgment. S1 request, S2 keys deleted, S3 grants revoked (empty without OAuth), S5 the time each Gateway accepted the stop policy, S6 stopped.",
			"calls-after-request.csv: every gateway record that carries this agent's name from the request on. A call is accepted when its status is below 400. Ambit does not promise that no call is accepted after a stop request: this file counts the ones that were.",
			"A call made with a key that was already deleted is logged as an invalid key without the agent's name, so it is not in this file. A call refused by the stop policy while the key still existed is (status 403).",
			"summary.csv: the measured counts and times. The time to the first refusal is the first refused call of this file; without one it is empty.",
		},
	}
	type call struct {
		at                                     time.Time
		ingress, identity, keyID, method, stat string
	}
	var calls []call
	secs := func(t time.Time) string { return strconv.FormatFloat(t.Sub(from).Seconds(), 'f', 3, 64) }
	job.files = []exportFile{
		{name: "timeline.csv", header: []string{"step", "what", "time", "gateway", "seconds_from_request"}, rows: func(_ context.Context, w *csvWriter) error {
			row := func(step, what string, t *time.Time, gw string) error {
				if t == nil {
					return w.Row(step, what, "", gw, "")
				}
				return w.Row(step, what, csvTime(*t), gw, secs(*t))
			}
			if err := row("S1", "stop requested", a.StopRequestedAt, ""); err != nil {
				return err
			}
			var keys, grants *time.Time
			if entry != nil {
				keys, grants = entry.KeysDeletedAt, entry.GrantsRevokedAt
			}
			if err := row("S2", "keys deleted", keys, ""); err != nil {
				return err
			}
			if err := row("S3", "grants revoked", grants, ""); err != nil {
				return err
			}
			for _, g := range sortedKeys(ack.Gateways) {
				v := ack.Gateways[g]
				var at *time.Time
				if entry != nil && v.AckedGen >= entry.Gen && !v.At.IsZero() {
					at = &v.At
				}
				if err := row("S5", "stop policy accepted by the Gateway (generation "+fmt.Sprint(v.AckedGen)+")", at, g); err != nil {
					return err
				}
			}
			if err := row("S6", "reported stopped", a.StoppedAt, ""); err != nil {
				return err
			}
			return row("", "reported as not confirmed after 60 s", a.StopNotConfirmedAt, "")
		}},
		{name: "calls-after-request.csv", header: []string{"time", "seconds_from_request", "ingress", "identity", "key_id", "method", "status", "outcome"}, rows: func(ctx context.Context, w *csvWriter) error {
			rows, err := c.Audit.DB.QueryContext(exportContext(ctx, params), query)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var k call
				if err := rows.Scan(&k.at, &k.ingress, &k.identity, &k.keyID, &k.method, &k.stat); err != nil {
					return err
				}
				calls = append(calls, k)
				n, _ := strconv.Atoi(k.stat)
				outcome := "accepted"
				if n >= 400 {
					outcome = "refused"
				}
				if err := w.Row(csvTime(k.at), secs(k.at), k.ingress, k.identity, k.keyID, k.method, k.stat, outcome); err != nil {
					return err
				}
			}
			return rows.Err()
		}},
		{name: "summary.csv", header: []string{"measure", "ingress", "value"}, rows: func(_ context.Context, w *csvWriter) error {
			acc, ref := map[string]int{}, map[string]*call{}
			var lastAcc *call
			for i := range calls {
				k := &calls[i]
				if n, _ := strconv.Atoi(k.stat); n >= 400 {
					if ref[k.ingress] == nil {
						ref[k.ingress] = k
					}
					continue
				}
				acc[k.ingress]++
				lastAcc = k
			}
			for _, g := range []string{"private", "public"} {
				if err := w.Row("accepted_calls_after_request", g, acc[g]); err != nil {
					return err
				}
				first := ""
				if r := ref[g]; r != nil {
					first = secs(r.at)
				}
				if err := w.Row("first_refused_call_seconds_from_request", g, first); err != nil {
					return err
				}
			}
			last := ""
			if lastAcc != nil {
				last = secs(lastAcc.at)
			}
			return w.Row("last_accepted_call_seconds_from_request", "", last)
		}},
	}
	return job, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// evidenceAgentStop answers GET /api/evidence/agent-stop/{agent}.zip.
func (c *Catalog) evidenceAgentStop(w http.ResponseWriter, r *http.Request, s Session) {
	name, ok := strings.CutSuffix(r.PathValue("file"), ".zip")
	if !ok || !accountNamePattern.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "use /api/evidence/agent-stop/<agent>.zip")
		return
	}
	if c.Audit == nil {
		writeErr(w, http.StatusNotFound, "the audit store is not installed (evidence.enabled), so there are no records to export")
		return
	}
	if !c.Evidence.running.CompareAndSwap(false, true) {
		writeErr(w, http.StatusConflict, "another evidence export is running; try again when it has finished")
		return
	}
	defer c.Evidence.running.Store(false)
	ctx, cancel := context.WithTimeout(r.Context(), exportLimit)
	defer cancel()
	job, err := c.prepareAgentStopExport(ctx, name)
	if err != nil {
		if ee, ok := err.(*exportErr); ok {
			writeErr(w, ee.status, ee.msg)
		} else {
			storeErr(w, err)
		}
		return
	}
	c.streamExport(ctx, w, s, job)
}
