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
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/craftsman-apps/ambit/internal/controller"
	"github.com/craftsman-apps/ambit/internal/extmcp"
)

// Pins reports the approved fingerprints of a server's tools and the current ones the controller last
// read. pinned is false for servers without pins (unknown tools, e.g. per-user servers): nothing is hidden.
type Pins interface {
	Pins(ctx context.Context, server string) (approved, current map[string]string, pinned bool)
}

// Masker replaces personal data in text.
type Masker interface {
	Mask(ctx context.Context, text string) (masked string, found int, err error)
}

type Guard struct {
	extmcp.UnimplementedExtMcpServer
	Pins   Pins
	Masker Masker
}

func pass() *extmcp.McpRequestResult { return &extmcp.McpRequestResult{Result: &extmcp.McpRequestResult_Pass{Pass: &extmcp.Pass{}}} }

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

// CheckRequest refuses a call to a tool that is not approved in its current form.
func (g *Guard) CheckRequest(ctx context.Context, req *extmcp.McpRequest) (*extmcp.McpRequestResult, error) {
	if req.Method != "tools/call" || req.McpRequest == nil {
		return pass(), nil
	}
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(req.McpRequest, &p); err != nil {
		return pass(), nil // the gateway and the server validate the request itself
	}
	server, tool := split(p.Name, req.ServiceNames)
	approved, current, pinned := g.Pins.Pins(ctx, server)
	if !pinned {
		return pass(), nil
	}
	if fp, ok := approved[tool]; !ok || current[tool] != fp {
		slog.Warn("guard: refused unapproved tool", "server", server, "tool", tool)
		return deny(fmt.Sprintf("tool %s on %s changed since it was approved; an admin must approve it again", tool, server)), nil
	}
	return pass(), nil
}

func (g *Guard) CheckResponse(ctx context.Context, resp *extmcp.McpResponse) (*extmcp.McpResponseResult, error) {
	switch resp.Method {
	case "tools/list":
		return g.filterList(ctx, resp)
	case "tools/call":
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
		if pinned && approved[tool] != controller.Fingerprint(tool, desc, t["inputSchema"]) {
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

// maskResult masks personal data in every text the tool returned. If masking fails the result is
// withheld: better a failed call than an unmasked national ID number in a model's context.
func (g *Guard) maskResult(ctx context.Context, resp *extmcp.McpResponse) (*extmcp.McpResponseResult, error) {
	var result map[string]any
	if err := json.Unmarshal(resp.McpResponse, &result); err != nil {
		return passResp(), nil
	}
	total := 0
	var maskErr error
	var walk func(v any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			m, n, err := g.Masker.Mask(ctx, x)
			if err != nil {
				maskErr = err
				return x
			}
			total += n
			return m
		case []any:
			for i := range x {
				x[i] = walk(x[i])
			}
		case map[string]any:
			for k, e := range x {
				if k == "type" || k == "mimeType" || k == "uri" { // structure, not content
					continue
				}
				x[k] = walk(e)
			}
		}
		return v
	}
	for _, k := range []string{"content", "structuredContent"} {
		if v, ok := result[k]; ok {
			result[k] = walk(v)
		}
	}
	if maskErr != nil {
		slog.Error("guard: masking failed; result withheld", "server", strings.Join(resp.ServiceNames, ","), "err", maskErr)
		return &extmcp.McpResponseResult{Result: &extmcp.McpResponseResult_Error{Error: &extmcp.AuthorizationError{
			Code: extmcp.AuthorizationError_UNKNOWN, Reason: "the tool result could not be checked for personal data, so it was withheld"}}}, nil
	}
	if total == 0 {
		return passResp(), nil
	}
	slog.Info("guard: masked personal data", "server", strings.Join(resp.ServiceNames, ","), "count", total)
	return mutated(result)
}
