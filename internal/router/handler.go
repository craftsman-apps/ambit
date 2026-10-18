package router

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/craftsman-apps/ambit/internal/identity"
	"github.com/craftsman-apps/ambit/internal/routerconfig"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	filterv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	extproc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/protobuf/types/known/structpb"
)

// Classifier answers a category for a text (any text: the caller checks it against the router's categories).
type Classifier interface {
	Classify(ctx context.Context, r routerconfig.Router, key, text string) (answer string, tokens int, err error)
}

// Handler is the ExtProc server. It never changes the body and never answers with an error: a failure is a reason
// and no label (spec, "Per request").
type Handler struct {
	extproc.UnimplementedExternalProcessorServer
	Store       *Store
	Classifier  Classifier
	Concurrency int // classifications at once per replica (default 16)
	Metrics     *Metrics

	once sync.Once
	sem  chan struct{}

	logMu  sync.Mutex
	logged map[string]time.Time // router -> last "classifier call failed" warning
}

// failureLogEvery limits the warning about a failing classifier to one per router per interval.
const failureLogEvery = time.Minute

// logFailure makes a broken classifier diagnosable (wrong URL, revoked key) without flooding the log. Classify's
// errors carry the HTTP status or the endpoint, never the key.
func (h *Handler) logFailure(router string, err error) {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	if h.logged == nil {
		h.logged = map[string]time.Time{}
	}
	if time.Since(h.logged[router]) < failureLogEvery {
		return
	}
	h.logged[router] = time.Now()
	slog.Warn("classifier call failed", "router", router, "err", err.Error())
}

// Attrs are the values the gateway sends per request (the policy's requestAttributes).
type Attrs struct {
	Groups []string
	Email  string // canonical, or "" when the caller has none
	Config string // the configuration hash the gateway's policy names
}

// Result: Route "" means no label; Reason "" means the request is not for a router of type LLM.
type Result struct{ Route, Reason, ConfigHash string }

// attributesKey is where agentgateway puts requestAttributes (ext_proc.rs:31, seen in the proof task).
const attributesKey = "envoy.filters.http.ext_proc"

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func (h *Handler) decide(ctx context.Context, start time.Time, snap *Snapshot, a Attrs, body []byte) Result {
	var req struct {
		Model    string    `json:"model"`
		Messages []message `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return Result{} // the gateway answers a bad request itself
	}
	if snap == nil || a.Config == "" || a.Config != snap.Config.Hash {
		return Result{Reason: "stale"} // never label under a configuration the gateway's policy does not name
	}
	r, ok := snap.Config.Find(req.Model)
	if !ok {
		return Result{}
	}
	if !r.AllSignedIn && !slices.ContainsFunc(a.Groups, func(g string) bool { return slices.Contains(r.Groups, g) }) &&
		(a.Email == "" || !slices.Contains(r.Users, a.Email)) {
		return Result{Reason: "denied"}
	}
	text, ok := lastUserText(req.Messages)
	if !ok {
		return Result{Reason: "error"}
	}
	if l := snap.limits[r.Name]; l != nil && !l.Allow() {
		return Result{Reason: "rate-limited"}
	}
	h.once.Do(func() {
		n := h.Concurrency
		if n <= 0 {
			n = 16
		}
		h.sem = make(chan struct{}, n)
	})
	select {
	case h.sem <- struct{}{}:
	default:
		return Result{Reason: "busy"}
	}
	timeout := time.Duration(r.TimeoutMillis) * time.Millisecond
	cctx, cancel := context.WithDeadline(ctx, start.Add(timeout))
	type out struct {
		answer string
		err    error
	}
	ch := make(chan out, 1)
	go func() {
		defer func() { <-h.sem }() // the slot is free only when the classifier call has returned
		defer cancel()
		var key string
		if r.LLM != nil {
			key = snap.keys[r.LLM.CredentialKey]
		}
		x, n, err := h.Classifier.Classify(cctx, r, key, cut(text, r.MaxBytes))
		if r.LLM != nil {
			h.Metrics.Tokens(r.Name, r.LLM.Model, n)
		}
		ch <- out{x, err}
	}()
	var o out
	select {
	case o = <-ch:
	case <-time.After(time.Until(start.Add(timeout + 50*time.Millisecond))): // hard deadline, even if the call ignores ctx
		return Result{Reason: "timeout"}
	}
	switch {
	case errors.Is(o.err, context.DeadlineExceeded):
		return Result{Reason: "timeout"}
	case o.err != nil:
		h.logFailure(r.Name, o.err)
		return Result{Reason: "error"}
	}
	name := strings.ToLower(strings.TrimSpace(o.answer))
	for _, c := range r.Categories {
		if c.Name == name {
			return Result{Route: name, Reason: "classified", ConfigHash: snap.Config.Hash}
		}
	}
	return Result{Reason: "unknown-category"}
}

// lastUserText is the last user message when its content is a string (the same text the rule lines see).
func lastUserText(ms []message) (string, bool) {
	for i := len(ms) - 1; i >= 0; i-- {
		if ms[i].Role == "user" {
			var s string
			if json.Unmarshal(ms[i].Content, &s) != nil {
				return "", false
			}
			return s, true
		}
	}
	return "", false
}

// cut shortens s to at most n bytes on a UTF-8 boundary.
func cut(s string, n int64) string {
	if n <= 0 || int64(len(s)) <= n {
		return s
	}
	i := int(n)
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}

func attrsOf(st *structpb.Struct) Attrs {
	var a Attrs
	if st == nil {
		return a
	}
	f := st.GetFields()
	for _, v := range f["ambit.groups"].GetListValue().GetValues() {
		a.Groups = append(a.Groups, v.GetStringValue())
	}
	a.Email, _ = identity.CanonicalEmail(f["ambit.email"].GetStringValue())
	a.Config = f["ambit.config"].GetStringValue()
	return a
}

func routerName(snap *Snapshot, body []byte) string {
	var req struct {
		Model string `json:"model"`
	}
	if snap == nil || json.Unmarshal(body, &req) != nil {
		return ""
	}
	r, _ := snap.Config.Find(req.Model)
	return r.Name
}

func (h *Handler) Process(s extproc.ExternalProcessor_ProcessServer) error {
	var a Attrs
	for {
		req, err := s.Recv()
		if err != nil {
			return nil
		}
		var resp *extproc.ProcessingResponse
		switch x := req.Request.(type) {
		case *extproc.ProcessingRequest_RequestHeaders:
			a = attrsOf(req.GetAttributes()[attributesKey])
			resp = &extproc.ProcessingResponse{
				Response:     &extproc.ProcessingResponse_RequestHeaders{RequestHeaders: &extproc.HeadersResponse{}},
				ModeOverride: &filterv3.ProcessingMode{RequestBodyMode: filterv3.ProcessingMode_BUFFERED},
			}
		case *extproc.ProcessingRequest_RequestBody:
			start := time.Now()
			snap := h.Store.Snapshot()
			b := x.RequestBody.GetBody()
			res := h.decide(s.Context(), start, snap, a, b)
			var set []*corev3.HeaderValueOption
			add := func(k, v string) {
				set = append(set, &corev3.HeaderValueOption{Header: &corev3.HeaderValue{Key: k, RawValue: []byte(v)}})
			}
			if res.Reason != "" {
				add("x-ambit-route-reason", res.Reason)
				h.Metrics.Reason(routerName(snap, b), res.Reason)
			}
			if res.Route != "" {
				add("x-ambit-route", res.Route)
				add("x-ambit-config", res.ConfigHash)
			}
			common := &extproc.CommonResponse{}
			if len(set) > 0 {
				common.HeaderMutation = &extproc.HeaderMutation{SetHeaders: set}
			}
			resp = &extproc.ProcessingResponse{Response: &extproc.ProcessingResponse_RequestBody{RequestBody: &extproc.BodyResponse{Response: common}}}
		case *extproc.ProcessingRequest_RequestTrailers:
			resp = &extproc.ProcessingResponse{Response: &extproc.ProcessingResponse_RequestTrailers{RequestTrailers: &extproc.TrailersResponse{}}}
		default:
			continue // the policy skips the response phases
		}
		if err := s.Send(resp); err != nil {
			return nil
		}
	}
}
