package console

import (
	"net/http"
	"time"

	"math"
	"sort"
	"strconv"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
)

// The Savings page: what compression and model routing saved, in tokens and dollars, per day, group, server and
// router. Every figure is an estimate (docs/savings.md). aggregateSavings holds all the rules and has no I/O, so it
// is tested without ClickHouse (the same pattern as aggregateUsage).

// bytesPerToken converts bytes removed from a tool result into tokens not read by a model. The values are the middle
// of what the offline token check measured on its fixtures (JSON 2.2 to 3.6 bytes per token). They are not a proven
// bound and the tokenizer is not the one of the models Ambit routes to: the page shows a range of plus and minus
// tokenRange around the point value. Replace them with a measurement from real clients (docs/concepts.md).
var bytesPerToken = map[string]float64{"json": 2.9, "table": 3.0, "yaml": 3.4, "logs": 2.4, "test": 2.7}

const (
	defaultBytesPerToken = 3.0
	tokenRange           = 0.25
)

// SavingsLLMRow is one row of the LLM daily summary. Group "" is the all-groups row: it counts each request once.
type SavingsLLMRow struct {
	Day, Group, Router, Model string
	Requests                  uint64
	In, Out                   int64
}

// SavingsCompressRow is one row of the compression daily summary. Group "" is the all-groups row.
type SavingsCompressRow struct {
	Day, Group, Server, Type string
	Results, BytesIn         uint64
	BytesOut, Omitted        uint64
}

// SavingsClassifierRow is one row of the classifier daily summary: the classifier calls of a router of type LLM, with
// their tokens. Group "" is the all-groups row.
type SavingsClassifierRow struct {
	Day, Group, Router string
	Calls              uint64
	In, Out            int64
}

// RouterCandidate is a Model a router can pick: the Model object's name and the model name clients send for it
// (Model is empty when the Model object no longer exists).
type RouterCandidate struct{ Object, Model string }

// RouterInfo is a ModelRouter as it is now: the name clients send and every Model it can pick (default and routes).
type RouterInfo struct {
	Name       string
	Candidates []RouterCandidate
	// Classifier is the model name clients send for the router's classifier Model ("" for type Rules, or when that
	// Model no longer exists).
	Classifier string
}

// SavingsSettings is the admin's setting for compression dollars when a group has no priced LLM traffic.
type SavingsSettings struct {
	ReferenceInputPrice *float64 // US dollars per million input tokens
}

type CompressionFigures struct {
	Results     uint64   `json:"results"`
	BytesIn     uint64   `json:"bytesIn"`
	BytesOut    uint64   `json:"bytesOut"`
	TokensSaved int64    `json:"tokensSaved"`
	TokensLow   int64    `json:"tokensLow"`
	TokensHigh  int64    `json:"tokensHigh"`
	Dollars     *float64 `json:"dollars"` // nil: no price (see priceSource)
	DollarsLow  *float64 `json:"dollarsLow"`
	DollarsHigh *float64 `json:"dollarsHigh"`
	// PriceSource: "llm" (the scope's own average input price), "reference" (the admin setting) or "none".
	PriceSource string `json:"priceSource"`
}

type RoutingFigures struct {
	Requests uint64   `json:"requests"`
	Dollars  *float64 `json:"dollars"` // nil: no priced row
}

type SavingsDay struct {
	Day                string   `json:"day"`
	CompressionDollars *float64 `json:"compressionDollars"`
	RoutingDollars     *float64 `json:"routingDollars"`
	CompressionTokens  int64    `json:"compressionTokens"`
}

type SavingsGroup struct {
	Group       string             `json:"group"`
	Compression CompressionFigures `json:"compression"`
	Routing     RoutingFigures     `json:"routing"`
}

type SavingsServer struct {
	Server      string `json:"server"`
	Type        string `json:"type"`
	Results     uint64 `json:"results"`
	BytesIn     uint64 `json:"bytesIn"`
	BytesOut    uint64 `json:"bytesOut"`
	TokensSaved int64  `json:"tokensSaved"`
	Omitted     uint64 `json:"omitted"`
}

type SavingsRouter struct {
	Router         string   `json:"router"`
	BaselineModels []string `json:"baselineModels"`
	Requests       uint64   `json:"requests"`
	Dollars        *float64 `json:"dollars"`
	UnpricedModels []string `json:"unpricedModels"`
	Missing        bool     `json:"missing"` // the router no longer exists: its rows are not priced
	// The classifier calls of a router of type LLM and their cost, which Dollars already has subtracted (nil: no
	// call counted, or the classifier has no price).
	ClassifierRequests uint64   `json:"classifierRequests"`
	ClassifierDollars  *float64 `json:"classifierDollars"`
}

type SavingsTotals struct {
	Compression CompressionFigures `json:"compression"`
	Routing     RoutingFigures     `json:"routing"`
}

// Savings is the response of GET /api/savings. Every money and token figure is an estimate.
type Savings struct {
	Estimated bool               `json:"estimated"`
	Settings  SavingsSettingsOut `json:"settings"`
	Totals    SavingsTotals      `json:"totals"`
	Days      []SavingsDay       `json:"days"`
	Groups    []SavingsGroup     `json:"groups"`
	Servers   []SavingsServer    `json:"servers"`
	Routers   []SavingsRouter    `json:"routers"`
	Notes     []string           `json:"notes"`
}

type SavingsSettingsOut struct {
	ReferenceInputPrice *float64 `json:"referenceInputPrice"`
}

func parsePrice(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }

func cost(p v1.ModelPrice, in, out int64) float64 {
	return (float64(in)*parsePrice(p.InputPerMillionTokens) + float64(out)*parsePrice(p.OutputPerMillionTokens)) / 1e6
}

func tokensFor(typ string, bytesIn, bytesOut uint64) float64 {
	if bytesOut >= bytesIn {
		return 0
	}
	bpt, ok := bytesPerToken[typ]
	if !ok {
		bpt = defaultBytesPerToken
	}
	return float64(bytesIn-bytesOut) / bpt
}

func add(p **float64, v float64) {
	if *p == nil {
		*p = new(float64)
	}
	**p += v
}

func fptr(v float64) *float64 { return &v }

// inputPrices is the average input price (US dollars per million tokens) of each scope ("" = everyone, else a
// group), weighted by input tokens over the priced models of its LLM requests.
func inputPrices(llm []SavingsLLMRow, prices map[string]v1.ModelPrice) map[string]float64 {
	spend, tokens := map[string]float64{}, map[string]float64{}
	for _, r := range llm {
		p, ok := prices[r.Model]
		if !ok || r.In <= 0 {
			continue
		}
		spend[r.Group] += float64(r.In) * parsePrice(p.InputPerMillionTokens)
		tokens[r.Group] += float64(r.In)
	}
	out := map[string]float64{}
	for g, t := range tokens {
		out[g] = spend[g] / t
	}
	return out
}

// compressionFigures turns tokens into the figures of a scope. price is dollars per million input tokens.
func compressionFigures(f *CompressionFigures, tokens float64, price *float64) {
	f.TokensSaved = int64(math.Round(tokens))
	f.TokensLow = int64(math.Round(tokens * (1 - tokenRange)))
	f.TokensHigh = int64(math.Round(tokens * (1 + tokenRange)))
	if price == nil {
		f.PriceSource = "none"
		return
	}
	f.Dollars = fptr(tokens * *price / 1e6)
	f.DollarsLow = fptr(tokens * (1 - tokenRange) * *price / 1e6)
	f.DollarsHigh = fptr(tokens * (1 + tokenRange) * *price / 1e6)
}

// routingSaving is the baseline cost minus the routed cost of one summary row. The baseline is the most expensive
// cost among the router's priced candidates, for the row's tokens; the routed Model is always a candidate, so the
// saving is never negative. ok is false when the routed Model has no price.
func routingSaving(r SavingsLLMRow, info RouterInfo, prices map[string]v1.ModelPrice) (saving float64, ok bool) {
	routed, priced := prices[r.Model]
	if !priced {
		return 0, false
	}
	routedCost := cost(routed, r.In, r.Out)
	base := routedCost
	for _, c := range info.Candidates {
		if p, ok := prices[c.Model]; ok && c.Model != "" {
			base = math.Max(base, cost(p, r.In, r.Out))
		}
	}
	return base - routedCost, true
}

func aggregateSavings(llm []SavingsLLMRow, comp []SavingsCompressRow, prices map[string]v1.ModelPrice, routers []RouterInfo, settings SavingsSettings, classifier ...SavingsClassifierRow) Savings {
	out := Savings{Estimated: true, Settings: SavingsSettingsOut{ReferenceInputPrice: settings.ReferenceInputPrice},
		Days: []SavingsDay{}, Groups: []SavingsGroup{}, Servers: []SavingsServer{}, Routers: []SavingsRouter{}, Notes: []string{}}
	avg := inputPrices(llm, prices)
	priceOf := func(scope string) *float64 {
		if p, ok := avg[scope]; ok {
			return &p
		}
		return settings.ReferenceInputPrice
	}
	priceSource := func(scope string) string {
		if _, ok := avg[scope]; ok {
			return "llm"
		}
		if settings.ReferenceInputPrice != nil {
			return "reference"
		}
		return "none"
	}

	// Compression: tokens per scope, per day (all-groups rows only), per server and type (all-groups rows only).
	type acc struct {
		f      CompressionFigures
		tokens float64
	}
	scopes := map[string]*acc{"": {}}
	days := map[string]*SavingsDay{}
	dayTokens := map[string]float64{}
	type srvKey struct{ server, typ string }
	servers := map[srvKey]*SavingsServer{}
	for _, r := range comp {
		a := scopes[r.Group]
		if a == nil {
			a = &acc{}
			scopes[r.Group] = a
		}
		t := tokensFor(r.Type, r.BytesIn, r.BytesOut)
		a.tokens += t
		a.f.Results += r.Results
		a.f.BytesIn += r.BytesIn
		a.f.BytesOut += r.BytesOut
		if r.Group != "" { // totals, days and servers read only the all-groups row: a user in two groups is counted once
			continue
		}
		d := days[r.Day]
		if d == nil {
			d = &SavingsDay{Day: r.Day}
			days[r.Day] = d
		}
		dayTokens[r.Day] += t
		k := srvKey{r.Server, r.Type}
		s := servers[k]
		if s == nil {
			s = &SavingsServer{Server: r.Server, Type: r.Type}
			servers[k] = s
		}
		s.Results += r.Results
		s.BytesIn += r.BytesIn
		s.BytesOut += r.BytesOut
		s.Omitted += r.Omitted
		s.TokensSaved += int64(math.Round(t))
	}
	for g, a := range scopes {
		compressionFigures(&a.f, a.tokens, priceOf(g))
		a.f.PriceSource = priceSource(g)
		if a.f.Dollars == nil {
			a.f.PriceSource = "none"
		}
	}
	out.Totals.Compression = scopes[""].f
	if out.Totals.Compression.Dollars == nil && out.Totals.Compression.TokensSaved > 0 {
		out.Notes = append(out.Notes, "No price: set a price on a Model or the reference price (console.savings.referenceInputPricePerMillion). Compression is shown in tokens only.")
	}

	// Routing: one saving per summary row (day, group, router, model).
	byName := map[string]RouterInfo{}
	for _, ri := range routers {
		byName[ri.Name] = ri
	}
	routing := map[string]*RoutingFigures{"": {}}
	routerOut := map[string]*SavingsRouter{}
	routerDollars := map[string]*SavingsRouter{}
	unpriced := map[string]map[string]bool{}
	for _, r := range llm {
		if r.Router == "" {
			continue
		}
		g := routing[r.Group]
		if g == nil {
			g = &RoutingFigures{}
			routing[r.Group] = g
		}
		g.Requests += r.Requests
		info, exists := byName[r.Router]
		sr := routerOut[r.Router]
		if r.Group == "" && sr == nil {
			sr = &SavingsRouter{Router: r.Router, BaselineModels: []string{}, UnpricedModels: []string{}, Missing: !exists}
			routerOut[r.Router] = sr
			routerDollars[r.Router] = sr
			unpriced[r.Router] = map[string]bool{}
		}
		if r.Group == "" {
			sr.Requests += r.Requests
		}
		if !exists {
			continue // the router was deleted: no baseline, so no dollar figure is guessed
		}
		s, ok := routingSaving(r, info, prices)
		if !ok {
			if r.Group == "" {
				unpriced[r.Router][r.Model] = true
			}
			continue
		}
		add(&g.Dollars, s)
		if r.Group == "" {
			add(&sr.Dollars, s)
			d := days[r.Day]
			if d == nil {
				d = &SavingsDay{Day: r.Day}
				days[r.Day] = d
			}
			add(&d.RoutingDollars, s)
		}
	}
	// Routing is net of the classifier (#216): the cost of each classifier call is subtracted from the same group,
	// router, day and total. The baseline stays the dearest candidate.
	for _, r := range classifier {
		g := routing[r.Group]
		if g == nil {
			g = &RoutingFigures{}
			routing[r.Group] = g
		}
		info, exists := byName[r.Router]
		sr := routerOut[r.Router]
		if r.Group == "" && sr == nil { // every classifier call comes with its LLM row; this only guards against a gap
			sr = &SavingsRouter{Router: r.Router, BaselineModels: []string{}, UnpricedModels: []string{}, Missing: !exists}
			routerOut[r.Router] = sr
			unpriced[r.Router] = map[string]bool{}
		}
		if r.Group == "" {
			sr.ClassifierRequests += r.Calls
		}
		if !exists {
			continue // a deleted router is not priced at all
		}
		p, priced := prices[info.Classifier]
		if !priced {
			if r.Group == "" {
				if info.Classifier == "" {
					out.Notes = append(out.Notes, "Router "+r.Router+" has classifier calls but no classifier Model now: its routing figure is gross, not net.")
				} else {
					out.Notes = append(out.Notes, "The classifier "+info.Classifier+" of router "+r.Router+" has no price: its routing figure is gross, not net.")
				}
			}
			continue
		}
		c := cost(p, r.In, r.Out)
		add(&g.Dollars, -c)
		if r.Group == "" {
			add(&sr.ClassifierDollars, c)
			add(&sr.Dollars, -c)
			d := days[r.Day]
			if d == nil {
				d = &SavingsDay{Day: r.Day}
				days[r.Day] = d
			}
			add(&d.RoutingDollars, -c)
		}
	}
	out.Totals.Routing = *routing[""]
	for name, sr := range routerOut {
		for m := range unpriced[name] {
			sr.UnpricedModels = append(sr.UnpricedModels, m)
		}
		if info, ok := byName[name]; ok {
			for _, c := range info.Candidates {
				if _, priced := prices[c.Model]; priced && c.Model != "" {
					sr.BaselineModels = append(sr.BaselineModels, c.Model)
				} else if c.Model != "" && !hasString(sr.UnpricedModels, c.Model) {
					out.Notes = append(out.Notes, c.Model+" has no price, so it is not a baseline for router "+name+".")
				}
				if c.Model == "" {
					out.Notes = append(out.Notes, "Router "+name+" lists the Model "+c.Object+", which no longer exists.")
				}
			}
		} else {
			out.Notes = append(out.Notes, "Router "+name+" no longer exists: its requests are not priced.")
		}
		sort.Strings(sr.UnpricedModels)
		sort.Strings(sr.BaselineModels)
		sr.BaselineModels = uniqStr(sr.BaselineModels)
		out.Routers = append(out.Routers, *sr)
	}
	sort.Slice(out.Routers, func(i, j int) bool { return out.Routers[i].Router < out.Routers[j].Router })
	for _, sr := range out.Routers {
		for _, m := range sr.UnpricedModels {
			out.Notes = append(out.Notes, m+" has no price or no longer exists, so its requests through router "+sr.Router+" are not counted in the dollar figure.")
		}
	}

	// Days, servers, groups.
	for name, d := range days {
		d.CompressionTokens = int64(math.Round(dayTokens[name]))
		if p := priceOf(""); p != nil && dayTokens[name] > 0 {
			d.CompressionDollars = fptr(dayTokens[name] * *p / 1e6)
		}
		out.Days = append(out.Days, *d)
	}
	sort.Slice(out.Days, func(i, j int) bool { return out.Days[i].Day < out.Days[j].Day })
	for _, s := range servers {
		out.Servers = append(out.Servers, *s)
	}
	sort.Slice(out.Servers, func(i, j int) bool {
		if out.Servers[i].Server != out.Servers[j].Server {
			return out.Servers[i].Server < out.Servers[j].Server
		}
		return out.Servers[i].Type < out.Servers[j].Type
	})
	names := map[string]bool{}
	for g := range scopes {
		if g != "" {
			names[g] = true
		}
	}
	for g := range routing {
		if g != "" {
			names[g] = true
		}
	}
	for g := range names {
		sg := SavingsGroup{Group: g}
		if a := scopes[g]; a != nil {
			sg.Compression = a.f
		} else {
			sg.Compression.PriceSource = "none"
		}
		if r := routing[g]; r != nil {
			sg.Routing = *r
		}
		out.Groups = append(out.Groups, sg)
	}
	sort.Slice(out.Groups, func(i, j int) bool {
		a, b := groupDollars(out.Groups[i]), groupDollars(out.Groups[j])
		if a != b {
			return a > b
		}
		return out.Groups[i].Group < out.Groups[j].Group
	})
	out.Notes = uniqStr(out.Notes)
	return out
}

func groupDollars(g SavingsGroup) float64 {
	var t float64
	if g.Compression.Dollars != nil {
		t += *g.Compression.Dollars
	}
	if g.Routing.Dollars != nil {
		t += *g.Routing.Dollars
	}
	return t
}

func hasString(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func uniqStr(l []string) []string {
	out := l[:0:0]
	for _, x := range l {
		if !hasString(out, x) {
			out = append(out, x)
		}
	}
	return out
}

// savings answers GET /api/savings. The period is in whole UTC days, because the summaries are per day.
func (a *Audit) savings(w http.ResponseWriter, r *http.Request, _ Session) {
	from, to, err := period(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if ok, serr := a.summaryStatus(); !ok {
		msg := "the savings summaries are not created yet"
		if serr != nil {
			msg += ": " + serr.Error()
		}
		writeErr(w, http.StatusServiceUnavailable, msg+". Usage and Audit are not affected.")
		return
	}
	llm, comp, since, err := a.SummaryRows(r.Context(), from, to.Add(-time.Nanosecond))
	if err != nil {
		storeErr(w, err)
		return
	}
	cls, classifierSince, err := a.ClassifierRows(r.Context(), from, to.Add(-time.Nanosecond))
	if err != nil {
		storeErr(w, err)
		return
	}
	var prices map[string]v1.ModelPrice
	if a.Prices != nil {
		prices = a.Prices(r.Context())
	}
	var routers []RouterInfo
	if a.Routers != nil {
		routers = a.Routers(r.Context())
	}
	res := aggregateSavings(llm, comp, prices, routers, SavingsSettings{ReferenceInputPrice: a.ReferenceInputPrice}, cls...)
	writeJSON(w, http.StatusOK, map[string]any{"from": from, "to": to, "since": since, "classifierSince": classifierSince, "savings": res})
}
