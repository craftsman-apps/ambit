// Package guard is ambit-guard: an agentgateway ExtMcp processor (MCP guardrails) that
//   - hides and refuses tools whose definition changed since an admin approved it (journey A6), and
//   - masks personal data in tool results before a model sees them (journey U6).
//
// The gateway calls it with FailClosed, so if the guard is down, tool calls fail instead of passing
// unchecked definitions or unmasked data.
package guard

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/craftsman-apps/ambit/internal/controller"
	"github.com/craftsman-apps/ambit/internal/extmcp"
	"github.com/craftsman-apps/ambit/internal/identity"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// maxMessage is the largest gRPC message the guard accepts and sends. agentgateway buffers MCP bodies up
// to 32 MiB, and grpc-go's default receive limit is 4 MiB, which would refuse a larger tool result before
// it is checked (the gateway then fails the call: FailClosed).
const maxMessage = 32 << 20

// ServerOptions are the gRPC server options of ambit-guard.
func ServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{grpc.MaxRecvMsgSize(maxMessage), grpc.MaxSendMsgSize(maxMessage), grpc.UnaryInterceptor(recoverUnary)}
}

// Pins reports the approved fingerprints of a server's tools and the current ones the controller last
// read. pinned is false only for a per-person server without pins (no admin has read its tools): nothing is
// hidden. A shared server without pins, or one that cannot be read, is pinned with nothing approved.
type Pins interface {
	Pins(ctx context.Context, server string) (approved, current map[string]string, pinned bool)
}

// Masker replaces personal data in text.
type Masker interface {
	Mask(ctx context.Context, text string) (masked string, found int, err error)
}

// BatchMasker masks many texts in one go. A Masker that also has this (Presidio) is called once per
// result instead of once per string.
type BatchMasker interface {
	MaskAll(ctx context.Context, texts []string) (masked []string, found int, err error)
}

type Guard struct {
	extmcp.UnimplementedExtMcpServer
	Pins   Pins
	Masker Masker
	// Rules, when set, enforces argument limits from access rules (requirement 8). The caller's groups
	// come from the gateway's metadata ("groups", from the verified token), never from the client.
	Rules Rules
	// Compression, when set, compresses the text of tool results of servers that opted in (see compress.go).
	// It runs before masking, so the masker always has the last word.
	Compression CompressionSource
	// Records, when set, receives one record per compressed text (server, type, sizes, the caller's groups; no
	// content, no email). It feeds the Savings page. Delivery is best effort and never fails a call.
	Records RecordSink
	// Injection, when set, looks for prompt-injection text in a result after masking (injection.go).
	Injection *Injector
}

func pass() *extmcp.McpRequestResult {
	return &extmcp.McpRequestResult{Result: &extmcp.McpRequestResult_Pass{Pass: &extmcp.Pass{}}}
}

func deny(reason string) *extmcp.McpRequestResult {
	return &extmcp.McpRequestResult{Result: &extmcp.McpRequestResult_Error{Error: &extmcp.AuthorizationError{
		Code: extmcp.AuthorizationError_PERMISSION_DENIED, Reason: reason}}}
}

// split returns the server and its own tool name for a (possibly prefixed) client-visible name.
func split(name string, services []string) (server, tool string) {
	for _, s := range services {
		if t, ok := strings.CutPrefix(name, s+"_"); ok {
			return s, t
		}
	}
	if len(services) == 1 {
		return services[0], name
	}
	return "", name
}

// CheckRequest refuses a call to a tool that is not approved in its current form, or whose arguments
// break a limit of the caller's access rules.
func (g *Guard) CheckRequest(ctx context.Context, req *extmcp.McpRequest) (*extmcp.McpRequestResult, error) {
	if req.Method != "tools/call" || req.McpRequest == nil {
		return pass(), nil
	}
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	dec := json.NewDecoder(bytes.NewReader(req.McpRequest))
	dec.UseNumber() // exact numbers for argument limits
	if err := dec.Decode(&p); err != nil {
		return deny("the tool call could not be read, so it was not checked"), nil
	}
	// For tools/call the gateway sends the one target server and the tool's own (unprefixed) name.
	server, tool := split(p.Name, req.ServiceNames)
	if len(req.ServiceNames) == 1 {
		server, tool = req.ServiceNames[0], p.Name
	}
	if approved, current, pinned := g.Pins.Pins(ctx, server); pinned {
		if fp, ok := approved[tool]; !ok || current[tool] != fp {
			slog.Warn("guard: refused unapproved tool", "server", server, "tool", tool)
			if len(approved) == 0 {
				return deny(fmt.Sprintf("%s has no approved tool definitions (its tools have not been read yet, or the Server could not be read), so tool %s is refused", server, tool)), nil
			}
			return deny(fmt.Sprintf("tool %s on %s changed since it was approved; an admin must approve it again", tool, server)), nil
		}
	}
	if g.Rules != nil {
		grants, err := g.Rules.Grants(ctx, server)
		if err != nil {
			slog.Error("guard: cannot read access rules; call refused", "server", server, "err", err)
			return deny("the access rules could not be read, so the call was not checked"), nil
		}
		if Limited(grants) {
			groups, ok := metadataStrings(req.MetadataContext, "groups")
			if !ok {
				slog.Error("guard: no groups from the gateway; call refused", "server", server, "tool", tool)
				return deny("the caller's groups did not reach the guard, so argument limits could not be checked"), nil
			}
			email, present := metadataEmail(req.MetadataContext)
			if !present && slices.ContainsFunc(grants, func(x Grant) bool { return len(x.Users) > 0 && len(x.Arguments) > 0 }) {
				// Without it a listed user's limited grant would not apply, and the call would pass unchecked.
				slog.Error("guard: no email from the gateway; call refused", "server", server, "tool", tool)
				return deny("the caller's email did not reach the guard, so argument limits could not be checked"), nil
			}
			if reason := CheckArguments(grants, groups, email, tool, p.Arguments); reason != "" {
				slog.Info("guard: refused arguments", "server", server, "tool", tool, "reason", reason)
				return deny(reason), nil
			}
		}
	}
	// The call ID links this call's compression records to the gateway's access-log record of it (#203): agentgateway
	// keeps the metadata for the call and exposes it to CEL as mcpGuardrails.callId, in the guard policy's response
	// metadata and in the access log. The guard makes it here, every time; it never reads a callId from the request.
	r := pass()
	r.Metadata, _ = structpb.NewStruct(map[string]any{"callId": newCallID(), "tool": tool})
	return r, nil
}

// newCallID is "c_" and 128 random bits in hex.
func newCallID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand)
	return "c_" + hex.EncodeToString(b[:])
}

// metadataStrings reads a list of strings the gateway put in the metadata context: a list, or a JSON
// array or single value in a string (as a claim may arrive). ok is false when the key is missing or null.
func metadataStrings(ctx *structpb.Struct, key string) (out []string, ok bool) {
	v, found := ctx.GetFields()[key]
	if !found {
		return nil, false
	}
	switch x := v.GetKind().(type) {
	case *structpb.Value_ListValue:
		for _, e := range x.ListValue.Values {
			out = append(out, e.GetStringValue())
		}
		return out, true
	case *structpb.Value_StringValue:
		if json.Unmarshal([]byte(x.StringValue), &out) == nil {
			return out, true
		}
		return []string{x.StringValue}, true
	}
	return nil, false
}

// metadataEmail is the caller's canonical email from the gateway's metadata, or "" when it has none (an
// emailless JWT sends ""). present is false when the gateway sent no email at all (a policy older than users in
// access rules).
func metadataEmail(ctx *structpb.Struct) (email string, present bool) {
	v, present := ctx.GetFields()["email"]
	email, _ = identity.CanonicalEmail(v.GetStringValue())
	return email, present
}

func (g *Guard) CheckResponse(ctx context.Context, resp *extmcp.McpResponse) (*extmcp.McpResponseResult, error) {
	switch resp.Method {
	case "tools/list":
		return g.filterList(ctx, resp)
	case "tools/call", "resources/read", "prompts/get":
		return g.maskResult(ctx, resp)
	}
	return &extmcp.McpResponseResult{Result: &extmcp.McpResponseResult_Pass{Pass: &extmcp.Pass{}}}, nil
}

func passResp() *extmcp.McpResponseResult {
	return &extmcp.McpResponseResult{Result: &extmcp.McpResponseResult_Pass{Pass: &extmcp.Pass{}}}
}

// mutated encodes without HTML escaping, so masks read "<NATIONAL_ID>" rather than "\u003cNATIONAL_ID\u003e".
func mutated(v any) (*extmcp.McpResponseResult, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return &extmcp.McpResponseResult{Result: &extmcp.McpResponseResult_Mutated{Mutated: bytes.TrimSpace(buf.Bytes())}}, nil
}

// filterList drops every tool whose definition, as the server sends it now, is not the approved one.
// It checks the live response, so a changed description is hidden on the very next tools/list.
func (g *Guard) filterList(ctx context.Context, resp *extmcp.McpResponse) (*extmcp.McpResponseResult, error) {
	var result map[string]json.RawMessage
	if err := json.Unmarshal(resp.McpResponse, &result); err != nil {
		return passResp(), nil
	}
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(result["tools"], &tools); err != nil {
		return passResp(), nil
	}
	keep := tools[:0:0]
	for _, t := range tools {
		var name, desc string
		_ = json.Unmarshal(t["name"], &name)
		_ = json.Unmarshal(t["description"], &desc)
		server, tool := split(name, resp.ServiceNames)
		approved, _, pinned := g.Pins.Pins(ctx, server)
		// A legacy approval (made before 0.5.0) is accepted until the controller moves it to the new fingerprint, which
		// it does on its next read of the server's tools if the legacy fingerprint still matches (issue 299).
		if fp := approved[tool]; pinned && fp != controller.Fingerprint(tool, t) && fp != controller.LegacyFingerprint(tool, desc, t["inputSchema"]) {
			slog.Warn("guard: hid changed or unapproved tool", "server", server, "tool", tool)
			continue
		}
		keep = append(keep, t)
	}
	if len(keep) == len(tools) {
		return passResp(), nil
	}
	result["tools"], _ = json.Marshal(keep)
	return mutated(result)
}

// maskRoots are the parts of a result that hold content, per method: items are MCP content items (their type and
// mimeType are structure, not content), keys means object keys are masked too.
var maskRoots = map[string][]struct {
	key        string
	item, keys bool
}{
	// _meta is the result's own metadata (next to the content); its keys stay as they are (issue 374).
	"tools/call":     {{"content", true, false}, {"structuredContent", false, true}, {"_meta", false, false}},
	"resources/read": {{"contents", true, false}, {"_meta", false, false}},
	"prompts/get":    {{"messages", true, false}, {"description", false, false}, {"_meta", false, false}},
}

// maskResult masks personal data in every text of a result: strings, numbers in decimal form (a masked number becomes the
// placeholder text), uri values, and the object keys of structuredContent. If masking fails the result is
// withheld: better a failed call than an unmasked national ID number in a model's context.
func (g *Guard) maskResult(ctx context.Context, resp *extmcp.McpResponse) (*extmcp.McpResponseResult, error) {
	what := "result"
	if resp.Method == "tools/call" {
		what = "tool result"
	}
	withhold := func(reason string) (*extmcp.McpResponseResult, error) {
		return &extmcp.McpResponseResult{Result: &extmcp.McpResponseResult_Error{Error: &extmcp.AuthorizationError{
			Code: extmcp.AuthorizationError_UNKNOWN, Reason: "the " + what + " " + reason}}}, nil
	}
	// Numbers are exact when the result may hold a number of 16 digits or more (decodeResult, issue 360).
	result, err := decodeResult(resp.McpResponse)
	if err != nil {
		// A result must be an object; anything else (a list, a string, broken JSON) could hold text the walk below
		// would never see.
		slog.Error("guard: result is not a JSON object; result withheld", "server", strings.Join(resp.ServiceNames, ","), "method", resp.Method)
		return withhold("is not a JSON object, so it could not be checked for personal data and was withheld")
	}
	// The decoded form is the only copy we need. Release the raw message now: a large result is held
	// several times over while it is masked and encoded again, and the guard has a small memory limit.
	resp.McpResponse = nil
	// The records of compressed texts are written only when the result is sent: a result that is withheld later (masking
	// failed, the injection guard refused it) saved nobody anything.
	var pendingRecords []func()
	compressed := resp.Method == "tools/call" && g.compressContent(ctx, resp.ServiceNames, resp.MetadataContext, result, &pendingRecords)
	var texts, keys []string
	var set []func(string)
	var renames []func(string) bool // false: the masked key is taken
	// number masks a number from its text; a masked number becomes the placeholder text, an unchanged one stays as it is.
	number := func(text string, put func(any)) {
		texts, set = append(texts, text), append(set, func(s string) {
			if s != text {
				put(s)
			}
		})
	}
	var walk func(v any, put func(any), item, withKeys bool)
	walk = func(v any, put func(any), item, withKeys bool) {
		switch x := v.(type) {
		case string:
			texts, set = append(texts, x), append(set, func(s string) { put(s) })
		case float64: // exact for integers up to 2^53, which holds 16-digit card numbers
			number(strconv.FormatFloat(x, 'f', -1, 64), put)
		case json.Number: // the digits the server wrote (issue 360)
			number(numberText(string(x)), put)
		case []any:
			for i := range x {
				walk(x[i], func(s any) { x[i] = s }, item, withKeys)
			}
		case map[string]any:
			for k, e := range x {
				if item && (k == "type" || k == "mimeType") { // structure of a content item
					continue
				}
				walk(e, func(s any) { x[k] = s }, item, withKeys)
				if withKeys {
					keys, renames = append(keys, k), append(renames, func(m string) bool {
						if m == k {
							return true
						}
						if _, taken := x[m]; taken {
							return false
						}
						x[m] = x[k]
						delete(x, k)
						return true
					})
				}
			}
		}
	}
	for _, r := range maskRoots[resp.Method] {
		if v, ok := result[r.key]; ok {
			walk(v, func(s any) { result[r.key] = s }, r.item, r.keys)
		}
	}
	all := append(texts, keys...)
	var hidden []string
	if g.Injection != nil {
		// Hidden characters come out before masking: they can split an email address or a rule's words in two.
		var over bool
		if all, hidden, over = g.Injection.StripHidden(all); over {
			g.reportInjection(resp, InjectionResult{Rules: hidden, Action: ActionRejected})
			return withhold("has more hidden characters than the prompt-injection check can remove, so it was withheld")
		}
	}
	masked, total, maskErr := g.maskTexts(ctx, all)
	if maskErr != nil {
		slog.Error("guard: masking failed; result withheld", "server", strings.Join(resp.ServiceNames, ","), "err", maskErr)
		return withhold("could not be checked for personal data, so it was withheld")
	}
	changed := total > 0 || compressed || len(hidden) > 0
	var inj InjectionResult
	if g.Injection != nil {
		// After masking: a webhook never receives personal data.
		inj = g.Injection.Check(ctx, masked, hidden)
		if inj.Withhold != "" {
			g.reportInjection(resp, inj)
			return withhold(inj.Withhold)
		}
		if inj.Changed {
			masked, changed = inj.Texts, true
		}
	}
	sent := func() { // the result goes to the client
		for _, f := range pendingRecords {
			f()
		}
		g.reportInjection(resp, inj)
	}
	if !changed {
		sent() // also the findings of the audit mode, and a failed webhook
		return passResp(), nil
	}
	for i := range texts {
		set[i](masked[i]) // the unmasked text is dropped here
	}
	for i, rename := range renames {
		if !rename(masked[len(texts)+i]) {
			slog.Error("guard: two keys mask to the same text; result withheld", "server", strings.Join(resp.ServiceNames, ","))
			if inj.Action != "" {
				inj.Action = ActionRejected // the client gets no masked result
			}
			g.reportInjection(resp, inj)
			return withhold("has two keys that mask to the same text, so it was withheld")
		}
	}
	sent()
	if total > 0 {
		slog.Info("guard: masked personal data", "server", strings.Join(resp.ServiceNames, ","), "method", resp.Method, "count", total)
	}
	return mutated(result)
}

// reportInjection counts and records the findings of the injection guard: the rule names, never the matched text.
func (g *Guard) reportInjection(resp *extmcp.McpResponse, r InjectionResult) {
	if g.Injection == nil {
		return
	}
	server := strings.Join(resp.ServiceNames, ",")
	if r.HookError != nil {
		injectionFindings.WithLabelValues(server, ActionError).Inc()
		slog.Error("guard: the prompt-injection webhook failed", "server", server, "err", r.HookError)
	}
	for _, rule := range r.Rules {
		injectionFindings.WithLabelValues(server, r.Action).Inc()
		g.recordInjection(resp.MetadataContext, InjectionRecord{Server: server, Mode: g.Injection.mode, Action: r.Action, Rule: rule})
	}
	if len(r.Rules) > 0 {
		slog.Warn("guard: prompt injection found in a result", "server", server, "method", resp.Method, "mode", g.Injection.mode, "action", r.Action, "rules", strings.Join(r.Rules, ","))
	}
}

func (g *Guard) maskTexts(ctx context.Context, texts []string) ([]string, int, error) {
	if b, ok := g.Masker.(BatchMasker); ok {
		return b.MaskAll(ctx, texts)
	}
	out, total := make([]string, len(texts)), 0
	for i, t := range texts {
		m, n, err := g.Masker.Mask(ctx, t)
		if err != nil {
			return nil, 0, err
		}
		out[i], total = m, total+n
	}
	return out, total, nil
}

// compressContent compresses content[].text of a result in place, for a server that opted in, and reports
// whether any text changed. Results with isError, non-text content and structuredContent (it must keep the
// tool's output schema) are not touched. A problem in a compressor never fails the call: the original text stays.
func (g *Guard) compressContent(ctx context.Context, services []string, meta *structpb.Struct, result map[string]any, pending *[]func()) bool {
	if g.Compression == nil || len(services) != 1 {
		return false
	}
	server := services[0]
	cfg := g.Compression.Compression(ctx, server)
	if cfg.Mode != ModeLossless && cfg.Mode != ModeShorten {
		return false
	}
	if isErr, _ := result["isError"].(bool); isErr {
		return false
	}
	items, _ := result["content"].([]any)
	presidio := g.hasPresidio()
	changed := false
	for part, it := range items {
		m, _ := it.(map[string]any)
		text, _ := m["text"].(string)
		if m == nil || m["type"] != "text" {
			continue
		}
		switch {
		case len(text) < cfg.MinBytes:
			compressResults.WithLabelValues(server, "unknown", outSmall).Inc()
			continue
		case len(text) > maxCompressBytes:
			compressResults.WithLabelValues(server, "unknown", outTooLarge).Inc()
			continue
		}
		start := time.Now()
		out, typ, omitted, outcome := compressText(ctx, text, cfg.Mode, presidio)
		compressSeconds.WithLabelValues(typ).Observe(time.Since(start).Seconds())
		compressResults.WithLabelValues(server, typ, outcome).Inc()
		if outcome != outCompressed {
			continue
		}
		compressBytesIn.WithLabelValues(server, typ).Add(float64(len(text)))
		compressBytesOut.WithLabelValues(server, typ).Add(float64(len(out)))
		compressOmitted.WithLabelValues(server, typ).Add(float64(omitted))
		slog.Info("guard: compressed tool result", "server", server, "type", typ, "mode", cfg.Mode, "bytes_in", len(text), "bytes_out", len(out), "omitted", omitted)
		rec := CompressionRecord{Server: server, Type: typ, Mode: cfg.Mode, Part: part, BytesIn: len(text), BytesOut: len(out), Omitted: omitted}
		*pending = append(*pending, func() { g.record(meta, rec) })
		m["text"], changed = out, true
	}
	return changed
}

// record hands one compressed text to the record sink, with the caller and the call from the gateway's metadata.
// Without the caller's groups there is no row to count it in, so it is dropped (and counted) rather than filed under
// a made-up group. recordEmail and keyId are absent with protection.compressionAudit.caller false.
func (g *Guard) record(meta *structpb.Struct, r CompressionRecord) {
	if g.Records == nil {
		return
	}
	groups, ok := metadataStrings(meta, "groups")
	if !ok {
		recordDropped.WithLabelValues(dropNoGroups).Inc()
		slog.Warn("guard: no groups from the gateway; compression record dropped", "server", r.Server)
		return
	}
	f := meta.GetFields()
	r.Groups, r.Email, r.KeyID = groups, f["recordEmail"].GetStringValue(), f["keyId"].GetStringValue()
	r.CallID, r.Tool = f["callId"].GetStringValue(), f["tool"].GetStringValue()
	g.Records.Record(r)
}

// recordInjection hands one finding to the record sink (when it takes injection records), with the caller and the call
// from the gateway's metadata. Unlike a compression record it is kept without groups: it is evidence, not a statistic.
func (g *Guard) recordInjection(meta *structpb.Struct, r InjectionRecord) {
	sink, ok := g.Records.(InjectionSink)
	if !ok {
		return
	}
	f := meta.GetFields()
	r.Groups, _ = metadataStrings(meta, "groups")
	r.Email, r.KeyID = f["recordEmail"].GetStringValue(), f["keyId"].GetStringValue()
	r.CallID, r.Tool = f["callId"].GetStringValue(), f["tool"].GetStringValue()
	sink.RecordInjection(r)
}

func (g *Guard) hasPresidio() bool {
	switch g.Masker.(type) {
	case BuiltinThenPresidio, *BuiltinThenPresidio:
		return true
	}
	return false
}
