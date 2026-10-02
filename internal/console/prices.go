package console

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
)

//go:embed prices/litellm-prices.json
var builtinPriceData []byte

// LiteLLMLicence is LiteLLM's licence at the commit the built-in list comes from (MIT), shipped with it.
//
//go:embed prices/LICENSE
var LiteLLMLicence string

type priceEntry struct {
	Provider string  `json:"litellm_provider"`
	In       float64 `json:"input_cost_per_token"`
	Out      float64 `json:"output_cost_per_token"`
}

// PriceList is LiteLLM's model price list: the copy built into this release, or one read from a mirror.
type PriceList struct {
	Source, Commit, Date string
	entries              map[string]priceEntry
}

// Suggestion is a price for one Model, in dollars per million tokens, and where it came from.
type Suggestion struct {
	Input  string `json:"inputPerMillionTokens"`
	Output string `json:"outputPerMillionTokens"`
	Key    string `json:"key"`
	Source string `json:"source"`
	Commit string `json:"commit"`
	Date   string `json:"date"`
}

// litellmProvider maps Ambit's provider to LiteLLM's key prefix and its accepted litellm_provider values.
var litellmProvider = map[string]struct {
	prefix    string
	providers []string
}{
	"OpenAI": {"openai", []string{"openai"}}, "Anthropic": {"anthropic", []string{"anthropic"}},
	"Gemini": {"gemini", []string{"gemini"}}, "VertexAI": {"vertex_ai", []string{"vertex_ai"}},
	"Azure": {"azure", []string{"azure"}}, "Bedrock": {"bedrock", []string{"bedrock", "bedrock_converse"}},
	"Openrouter": {"openrouter", []string{"openrouter"}},
}

// ParseLiteLLM reads LiteLLM's file (or the trimmed built-in copy). Every entry it keeps must have a
// provider and non-negative numeric prices; anything else in an entry that has prices makes the whole
// file invalid, so a damaged mirror never half-replaces the list.
func ParseLiteLLM(data []byte, source, commit, date string) (*PriceList, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("price list is not a JSON object: %w", err)
	}
	l := &PriceList{Source: source, Commit: commit, Date: date, entries: map[string]priceEntry{}}
	for key, msg := range raw {
		if key == "_ambit" {
			var meta struct{ Commit, Date string }
			if json.Unmarshal(msg, &meta) == nil {
				l.Commit, l.Date = meta.Commit, meta.Date
			}
			continue
		}
		var probe map[string]any
		if json.Unmarshal(msg, &probe) != nil {
			continue // not an entry (e.g. LiteLLM's "sample_spec" text)
		}
		in, hasIn := probe["input_cost_per_token"]
		out, hasOut := probe["output_cost_per_token"]
		// A price that is present must be a non-negative number, in every row: one bad value rejects the file.
		for _, v := range []struct {
			v   any
			has bool
		}{{in, hasIn}, {out, hasOut}} {
			if f, ok := v.v.(float64); v.has && (!ok || f < 0) {
				return nil, fmt.Errorf("price list entry %q has an invalid price", key)
			}
		}
		if !hasIn || !hasOut {
			continue // e.g. embedding models with no output price: kept out of suggestions
		}
		p, _ := probe["litellm_provider"].(string)
		if p == "" {
			continue // without a provider an entry cannot be matched; LiteLLM has a few such rows
		}
		fi, fo := in.(float64), out.(float64)
		l.entries[key] = priceEntry{Provider: p, In: fi, Out: fo}
	}
	if len(l.entries) == 0 {
		return nil, errors.New("price list has no prices")
	}
	return l, nil
}

// perMillion turns LiteLLM's dollars per token into dollars per million tokens, as a short decimal.
func perMillion(perToken float64) string {
	r := new(big.Rat).SetFloat64(perToken)
	r.Mul(r, big.NewRat(1_000_000, 1))
	s := strings.TrimRight(strings.TrimRight(r.FloatString(6), "0"), ".")
	if s == "" {
		return "0"
	}
	return s
}

// Suggest finds the Model's entry: "<prefix>/<model>", then "<model>", with a matching provider. A Model with
// its own server (baseURL) gets none: a public price would be wrong for it.
func (l *PriceList) Suggest(m v1.Model) (Suggestion, bool) {
	lp, ok := litellmProvider[m.Spec.Provider]
	if !ok || m.Spec.BaseURL != "" {
		return Suggestion{}, false
	}
	for _, key := range []string{lp.prefix + "/" + m.Spec.Model, m.Spec.Model} {
		e, found := l.entries[key]
		if !found {
			continue
		}
		for _, p := range lp.providers {
			if e.Provider == p || (p == "vertex_ai" && strings.HasPrefix(e.Provider, "vertex_ai-")) {
				return Suggestion{Input: perMillion(e.In), Output: perMillion(e.Out), Key: key, Source: l.Source, Commit: l.Commit, Date: l.Date}, true
			}
		}
		return Suggestion{}, false
	}
	return Suggestion{}, false
}

// BuiltinPrices is the copy built into this release (hack/prices.py).
func BuiltinPrices() *PriceList {
	l, err := ParseLiteLLM(builtinPriceData, "built in", "", "")
	if err != nil {
		panic("built-in price list: " + err.Error()) // generated and tested at build time
	}
	return l
}

// PriceSource keeps the current list: the built-in copy, replaced by the operator's mirror (prices.sourceURL)
// whenever a read of it succeeds. Only HTTPS, no redirects, a time limit, at most 10 MiB.
type PriceSource struct {
	URL     string
	HTTP    *http.Client // nil: a client with the console's CA bundle (SSL_CERT_DIR) and HTTPS_PROXY
	Timeout time.Duration
	mu      sync.Mutex
	list    *PriceList
	lastErr string
}

const maxPriceList = 10 << 20

func (p *PriceSource) Current() (*PriceList, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.list == nil {
		p.list = BuiltinPrices()
	}
	return p.list, p.lastErr
}

func (p *PriceSource) refresh(ctx context.Context) {
	l, err := p.fetch(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.lastErr = err.Error()
		slog.Warn("price mirror", "url", p.URL, "err", err)
		if p.list == nil {
			p.list = BuiltinPrices()
		}
		return
	}
	p.list, p.lastErr = l, ""
}

func (p *PriceSource) fetch(ctx context.Context) (*PriceList, error) {
	u, err := url.Parse(p.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("prices.sourceURL must be an https:// address")
	}
	hc := http.Client{Transport: http.DefaultTransport}
	if p.HTTP != nil {
		hc = *p.HTTP
	}
	hc.Timeout = p.Timeout
	if hc.Timeout == 0 {
		hc.Timeout = 20 * time.Second
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("redirects are not followed") }
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		return nil, fmt.Errorf("compressed price lists (%s) are refused", enc)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPriceList+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPriceList {
		return nil, errors.New("price list is larger than 10 MiB")
	}
	return ParseLiteLLM(data, p.URL, "", time.Now().UTC().Format("2006-01-02"))
}

// Run reads the mirror at start and once a day. Without a URL the built-in list is used.
func (p *PriceSource) Run(ctx context.Context) {
	if p.URL == "" {
		return
	}
	for {
		p.refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(24 * time.Hour):
		}
	}
}
