package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func price(in, out string) v1.ModelPrice {
	return v1.ModelPrice{InputPerMillionTokens: in, OutputPerMillionTokens: out}
}

var testPrices = map[string]v1.ModelPrice{
	"claude-sonnet": price("3.00", "15.00"),
	"gemini-pro":    price("1.25", "10.00"),
	"gemma":         price("0.10", "0.20"),
}

func near(t *testing.T, what string, got *float64, want float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > 1e-6 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// T3: tokens saved = (bytes in - bytes out) / bytes per token of the type, with a range of plus and minus 25%.
func TestCompressionTokensAndRange(t *testing.T) {
	comp := []SavingsCompressRow{
		{Day: "2026-10-30", Group: "", Server: "github", Type: "json", Results: 40, BytesIn: 1_000_000, BytesOut: 400_000},
		{Day: "2026-10-30", Group: "", Server: "kubectl", Type: "yaml", Results: 10, BytesIn: 340_000, BytesOut: 140_000},
	}
	s := aggregateSavings(nil, comp, nil, nil, SavingsSettings{})
	c := s.Totals.Compression
	// json: 600,000 / 2.9 = 206,897; yaml: 200,000 / 3.4 = 58,824
	if c.TokensSaved != 265_720 || c.TokensLow != 199_290 || c.TokensHigh != 332_150 {
		t.Errorf("tokens %d (%d to %d)", c.TokensSaved, c.TokensLow, c.TokensHigh)
	}
	if c.Dollars != nil || c.PriceSource != "none" {
		t.Errorf("no price must give tokens only: %+v", c)
	}
	if !s.Estimated || len(s.Notes) == 0 || !strings.Contains(s.Notes[0], "No price") {
		t.Errorf("want the no-price note: %v", s.Notes)
	}
	if len(s.Servers) != 2 || s.Servers[0].Server != "github" || s.Servers[0].TokensSaved != 206_897 {
		t.Errorf("servers %+v", s.Servers)
	}
}

// T4: the dollar price is the group's own average input price, else the reference price, else none.
func TestCompressionDollarOrder(t *testing.T) {
	comp := []SavingsCompressRow{}
	for _, g := range []string{"", "eng", "support", "ops"} {
		comp = append(comp, SavingsCompressRow{Day: "2026-10-30", Group: g, Server: "github", Type: "json", Results: 1, BytesIn: 2_900_000, BytesOut: 0})
	}
	// eng: 1,000,000 input tokens on claude-sonnet ($3) and 3,000,000 on gemma ($0.10): (3 + 0.3) / 4 = $0.825 a million.
	// support has only an unpriced model; ops has no LLM traffic.
	llm := []SavingsLLMRow{
		{Day: "2026-10-30", Group: "eng", Model: "claude-sonnet", In: 1_000_000, Out: 100_000, Requests: 10},
		{Day: "2026-10-30", Group: "eng", Model: "gemma", In: 3_000_000, Out: 100_000, Requests: 10},
		{Day: "2026-10-30", Group: "support", Model: "local-llama", In: 5_000_000, Requests: 5},
		{Day: "2026-10-30", Group: "", Model: "claude-sonnet", In: 1_000_000, Out: 100_000, Requests: 10},
		{Day: "2026-10-30", Group: "", Model: "gemma", In: 3_000_000, Out: 100_000, Requests: 10},
	}
	ref := 2.0
	s := aggregateSavings(llm, comp, testPrices, nil, SavingsSettings{ReferenceInputPrice: &ref})
	by := map[string]SavingsGroup{}
	for _, g := range s.Groups {
		by[g.Group] = g
	}
	// 1,000,000 tokens saved per row (2,900,000 bytes / 2.9)
	near(t, "eng", by["eng"].Compression.Dollars, 0.825)
	if by["eng"].Compression.PriceSource != "llm" {
		t.Errorf("eng source %q", by["eng"].Compression.PriceSource)
	}
	near(t, "support (reference)", by["support"].Compression.Dollars, 2.0)
	near(t, "ops (reference)", by["ops"].Compression.Dollars, 2.0)
	if by["ops"].Compression.PriceSource != "reference" {
		t.Errorf("ops source %q", by["ops"].Compression.PriceSource)
	}
	near(t, "total", s.Totals.Compression.Dollars, 0.825)
	// without the reference price a group with no priced traffic gets tokens only
	s = aggregateSavings(llm, comp, testPrices, nil, SavingsSettings{})
	for _, g := range s.Groups {
		if g.Group == "ops" && (g.Compression.Dollars != nil || g.Compression.PriceSource != "none" || g.Compression.TokensSaved != 1_000_000) {
			t.Errorf("ops: %+v", g.Compression)
		}
	}
}

// T2 (unit part): a person in two groups counts in both group rows, but totals, days and servers read only the all-groups row.
func TestTotalsReadOnlyTheAllGroupsRow(t *testing.T) {
	row := func(g string) SavingsCompressRow {
		return SavingsCompressRow{Day: "2026-10-30", Group: g, Server: "github", Type: "json", Results: 10, BytesIn: 29_000, BytesOut: 0}
	}
	s := aggregateSavings(nil, []SavingsCompressRow{row(""), row("eng"), row("ml-research")}, nil, nil, SavingsSettings{})
	if s.Totals.Compression.Results != 10 || s.Servers[0].Results != 10 || s.Totals.Compression.TokensSaved != 10_000 || s.Days[0].CompressionTokens != 10_000 {
		t.Errorf("a person in two groups was counted twice: totals %+v servers %+v", s.Totals.Compression, s.Servers)
	}
	var sum uint64
	for _, g := range s.Groups {
		sum += g.Compression.Results
	}
	if sum != 20 || len(s.Groups) != 2 {
		t.Errorf("group rows must overlap: %+v", s.Groups)
	}
}

func router(name string, objs ...string) RouterInfo {
	r := RouterInfo{Name: name}
	wire := map[string]string{"sonnet": "claude-sonnet", "pro": "gemini-pro", "small": "gemma", "local": "local-llama"}
	for _, o := range objs {
		r.Candidates = append(r.Candidates, RouterCandidate{Object: o, Model: wire[o]})
	}
	return r
}

// T6: the baseline is the most expensive priced candidate for the row's tokens; never negative; named gaps.
func TestRoutingBaseline(t *testing.T) {
	llm := []SavingsLLMRow{
		// 1,000,000 in and 200,000 out answered by gemma: routed $0.14; baseline claude-sonnet $3 + $3 = $6.
		{Day: "2026-10-30", Group: "", Router: "auto", Model: "gemma", In: 1_000_000, Out: 200_000, Requests: 100},
		// answered by the dearest model itself (fall-through for a user who may not use others): saving 0
		{Day: "2026-10-30", Group: "", Router: "auto", Model: "claude-sonnet", In: 1_000_000, Out: 200_000, Requests: 10},
		// a direct request (router "") adds nothing
		{Day: "2026-10-30", Group: "", Router: "", Model: "gemma", In: 9_000_000, Out: 9_000_000, Requests: 900},
	}
	s := aggregateSavings(llm, nil, testPrices, []RouterInfo{router("auto", "small", "pro", "sonnet", "local")}, SavingsSettings{})
	near(t, "routing", s.Totals.Routing.Dollars, 5.86)
	if s.Totals.Routing.Requests != 110 {
		t.Errorf("requests %d (direct requests must not count)", s.Totals.Routing.Requests)
	}
	if len(s.Routers) != 1 || strings.Join(s.Routers[0].BaselineModels, ",") != "claude-sonnet,gemini-pro,gemma" {
		t.Errorf("router %+v", s.Routers)
	}
	if !strings.Contains(strings.Join(s.Notes, "|"), "local-llama has no price, so it is not a baseline") {
		t.Errorf("unpriced candidate not named: %v", s.Notes)
	}
	if s.Totals.Routing.Dollars != nil && *s.Totals.Routing.Dollars < 0 {
		t.Error("negative saving")
	}
}

// The routed Model is not in the router's candidate list any more (the router was edited after the request): the
// baseline is still the dearest priced candidate, so the saving is positive and the answering model is priced.
func TestRoutingRoutedModelNotACandidate(t *testing.T) {
	llm := []SavingsLLMRow{{Day: "2026-10-30", Router: "auto", Model: "gemma", In: 1_000_000, Out: 200_000, Requests: 5}}
	s := aggregateSavings(llm, nil, testPrices, []RouterInfo{router("auto", "sonnet", "pro")}, SavingsSettings{})
	near(t, "routing", s.Totals.Routing.Dollars, 5.86)
}

// T6: a routed Model without a price is excluded and named; a deleted router and a deleted Model give an unpriced row.
func TestRoutingUnpricedAndDeleted(t *testing.T) {
	llm := []SavingsLLMRow{
		{Day: "2026-10-30", Router: "auto", Model: "local-llama", In: 1_000_000, Out: 1000, Requests: 7},
		{Day: "2026-10-30", Router: "old-router", Model: "gemma", In: 1_000_000, Out: 1000, Requests: 3},
		{Day: "2026-10-30", Router: "auto", Model: "gemma", In: 1_000_000, Out: 0, Requests: 1},
	}
	r := router("auto", "small", "sonnet")
	r.Candidates = append(r.Candidates, RouterCandidate{Object: "deleted-model"}) // Model object gone
	s := aggregateSavings(llm, nil, testPrices, []RouterInfo{r}, SavingsSettings{})
	by := map[string]SavingsRouter{}
	for _, x := range s.Routers {
		by[x.Router] = x
	}
	if a := by["auto"]; a.Requests != 8 || strings.Join(a.UnpricedModels, ",") != "local-llama" {
		t.Errorf("auto: %+v", a)
	}
	near(t, "auto dollars (only the priced row)", by["auto"].Dollars, 2.9) // sonnet $3 - gemma $0.10
	if o := by["old-router"]; !o.Missing || o.Dollars != nil || o.Requests != 3 {
		t.Errorf("old-router: %+v", o)
	}
	notes := strings.Join(s.Notes, "|")
	for _, want := range []string{"local-llama has no price or no longer exists", "Router old-router no longer exists", "deleted-model, which no longer exists"} {
		if !strings.Contains(notes, want) {
			t.Errorf("missing note %q in %v", want, s.Notes)
		}
	}
}

func TestEmptyPeriod(t *testing.T) {
	s := aggregateSavings(nil, nil, nil, nil, SavingsSettings{})
	b, _ := json.Marshal(s)
	for _, k := range []string{`"days":[]`, `"groups":[]`, `"servers":[]`, `"routers":[]`, `"estimated":true`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("%s missing in %s", k, b)
		}
	}
}

// Large byte counts stay exact (UInt64 sizes, tokens in int64).
func TestVeryLargeByteCounts(t *testing.T) {
	s := aggregateSavings(nil, []SavingsCompressRow{{Day: "2026-10-30", Type: "json", Results: 1, BytesIn: 5 << 50, BytesOut: 1 << 50}}, nil, nil, SavingsSettings{})
	if s.Totals.Compression.BytesIn != 5<<50 || s.Totals.Compression.TokensSaved != int64(math.Round(float64(4<<50)/2.9)) {
		t.Errorf("%+v", s.Totals.Compression)
	}
}

// T12: retention conversion.
func TestSummaryDays(t *testing.T) {
	for in, want := range map[string]int{"26280h": 1095, "24h": 1, "25h": 2, "8760h": 365, "720h30m": 31, "168h": 7} {
		if got, err := SummaryDays(in); err != nil || got != want {
			t.Errorf("%s = %d %v, want %d", in, got, err, want)
		}
	}
	if got, err := SummaryDays("2562047h"); err != nil || got != 106752 { // the largest whole number of hours in a Go duration
		t.Errorf("largest: %d %v", got, err)
	}
	for _, bad := range []string{"23h", "0", "1h", "", "3 years", "-24h"} {
		if _, err := SummaryDays(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

type fakeDB struct {
	stmts []string
	fail  func(q string) error
}

func (f *fakeDB) ExecContext(_ context.Context, q string, _ ...any) (sql.Result, error) {
	f.stmts = append(f.stmts, q)
	if f.fail != nil {
		return nil, f.fail(q)
	}
	return nil, nil
}

// T12: the same statements at every start (idempotent: IF NOT EXISTS), the TTL from the retention, and a changed
// retention changes only the TTL statements.
func TestEnsureSummaries(t *testing.T) {
	a, b := &fakeDB{}, &fakeDB{}
	if err := EnsureSummaries(context.Background(), a, 1095); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSummaries(context.Background(), b, 365); err != nil {
		t.Fatal(err)
	}
	if len(a.stmts) != 6 {
		t.Fatalf("%d statements", len(a.stmts))
	}
	for _, q := range a.stmts[:4] {
		if !strings.Contains(q, "IF NOT EXISTS") {
			t.Errorf("not idempotent: %.60s", q)
		}
	}
	if !strings.Contains(a.stmts[0], "TTL day + INTERVAL 1095 DAY") || !strings.Contains(b.stmts[0], "TTL day + INTERVAL 365 DAY") {
		t.Error("TTL not taken from the retention")
	}
	if !strings.Contains(a.stmts[4], "MODIFY TTL day + INTERVAL 1095 DAY") || !strings.Contains(a.stmts[4], "materialize_ttl_after_modify = 0") || !strings.Contains(b.stmts[5], "INTERVAL 365 DAY") {
		t.Errorf("TTL statements: %q / %q", a.stmts[4], b.stmts[5])
	}
	for _, q := range a.stmts {
		if strings.Contains(q, "{days}") {
			t.Errorf("placeholder left: %.80s", q)
		}
	}
}

// T12: a failure (otel_logs does not exist yet) is kept for the page; Usage and Audit are not involved.
func TestSummaryFailureShownOnThePage(t *testing.T) {
	db := &fakeDB{fail: func(string) error { return context.DeadlineExceeded }}
	a := &Audit{}
	ctx, cancel := context.WithCancel(context.Background())
	a.sumMu.Lock()
	a.sum = summaryState{err: EnsureSummaries(ctx, db, 30)}
	a.sumMu.Unlock()
	cancel()
	rec := httptest.NewRecorder()
	a.savings(rec, httptest.NewRequest("GET", "/api/savings", nil), Session{Role: RoleAdmin})
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not created yet") || !strings.Contains(rec.Body.String(), "Usage and Audit are not affected") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

// T12: RunSummaries retries until it works, then stops.
func TestRunSummariesRetries(t *testing.T) {
	n := 0
	db := &fakeDB{fail: func(q string) error {
		if n++; n <= 2 {
			return context.DeadlineExceeded
		}
		return nil
	}}
	a := &Audit{DB: nil}
	done := make(chan struct{})
	go func() { a.runSummaries(t.Context(), db, 30, time.Millisecond); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("did not stop after success")
	}
	if ok, err := a.summaryStatus(); !ok || err != nil {
		t.Errorf("status %v %v", ok, err)
	}
}

// T8: /api/savings is admin only, like /api/usage: viewer and approver get 403, an admin reaches the handler.
func TestSavingsAdminOnly(t *testing.T) {
	auth := &Auth{codec: Codec{Key: []byte("0123456789abcdef0123456789abcdef")}, ttl: time.Hour, Sessions: NewSessionStore()}
	h := NewServer(auth, testSPA, LoginOptions{}, &Catalog{Client: fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build(), Namespace: testNS, Audit: &Audit{}})
	for _, role := range []string{RoleViewer, RoleApprover} {
		if rec := do(t, h, "GET", "/api/savings", user(auth, "x@acme.test", role), nil); rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d", role, rec.Code)
		}
	}
	// The summaries are not created: a clear 503 that names the cause, not an empty page.
	rec := do(t, h, "GET", "/api/savings", user(auth, "priya@acme.test", RoleAdmin), nil)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not created yet") {
		t.Errorf("admin: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", "/api/savings?from=2026-04-01&to=2026-03-01", user(auth, "priya@acme.test", RoleAdmin), nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad period: %d", rec.Code)
	}
}
