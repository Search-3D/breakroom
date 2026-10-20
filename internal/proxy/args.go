package proxy

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// structuredParams maps tool name -> its array/object parameter names.
type structuredParams map[string]map[string]bool

// callerKey carries the per-call caller_id (see proxy.go) through middleware.
type callerKey struct{}

// withCaller stores the caller's agent id in ctx.
func withCaller(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, callerKey{}, id)
}

// callerFromCtx returns the caller_id argument the client passed, or "".
func callerFromCtx(ctx context.Context) string {
	id, _ := ctx.Value(callerKey{}).(string)
	return id
}

// extractCallerID moves caller_id out of the tool arguments and into ctx,
// before schema validation, so tool handlers never see it.
func extractCallerID() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			r, ok := req.(*mcp.CallToolRequest)
			if !ok || r.Params == nil || len(r.Params.Arguments) == 0 {
				return next(ctx, method, req)
			}
			var args map[string]json.RawMessage
			if json.Unmarshal(r.Params.Arguments, &args) != nil {
				return next(ctx, method, req)
			}
			raw, present := args["caller_id"]
			if !present {
				return next(ctx, method, req)
			}
			var id string
			if json.Unmarshal(raw, &id) != nil {
				return next(ctx, method, req)
			}
			delete(args, "caller_id")
			out, err := json.Marshal(args)
			if err != nil {
				return next(ctx, method, req)
			}
			r.Params.Arguments = out
			return next(withCaller(ctx, id), method, req)
		}
	}
}

// addTool registers a tool and records which parameters take arrays or
// objects, for decodeStringArgs.
func addTool[In, Out any](server *mcp.Server, sp structuredParams, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	if s, err := jsonschema.For[In](nil); err == nil {
		for name, p := range s.Properties {
			if slices.ContainsFunc(append(p.Types, p.Type), func(t string) bool { return t == "array" || t == "object" }) {
				if sp[t.Name] == nil {
					sp[t.Name] = map[string]bool{}
				}
				sp[t.Name][name] = true
			}
		}
		// caller_id names the acting agent on every tool except
		// register_agent, so one MCP process can serve several chats. It
		// must be declared: the inferred schemas reject undeclared
		// properties, and clients need it advertised to send it.
		if t.Name != "register_agent" {
			if s.Properties == nil {
				s.Properties = map[string]*jsonschema.Schema{}
			}
			s.Properties["caller_id"] = &jsonschema.Schema{Type: "string", Description: "your agent id from register_agent (e.g. otter-417); required when several agents share this process"}
			t.InputSchema = s
		}
	}
	// envelope.Data is `any`, which infers the schema `true`. Cursor rejects
	// that and drops every tool; data is always an object. *envelope also
	// infers ["null","object"], but MCP requires outputSchema type "object".
	if s, err := jsonschema.For[Out](nil); err == nil && s.Properties["data"] != nil {
		s.Type, s.Types = "object", nil
		s.Properties["data"] = &jsonschema.Schema{Type: "object"}
		t.OutputSchema = s
	}
	mcp.AddTool(server, t, h)
}

// decodeStringArgs accepts clients (like Muse's bridge) that send array or
// object arguments as JSON strings, by decoding them before validation.
func decodeStringArgs(sp structuredParams) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if r, ok := req.(*mcp.CallToolRequest); ok && r.Params != nil {
				r.Params.Arguments = decodeArgs(r.Params.Arguments, sp[r.Params.Name])
			}
			return next(ctx, method, req)
		}
	}
}

// decodeArgs replaces string values of structured params with their decoded
// JSON, leaving anything that doesn't decode for validation to reject.
func decodeArgs(raw json.RawMessage, params map[string]bool) json.RawMessage {
	if len(params) == 0 {
		return raw
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(raw, &args) != nil {
		return raw
	}
	changed := false
	for k, v := range args {
		var s string
		if !params[k] || json.Unmarshal(v, &s) != nil {
			continue
		}
		s = strings.TrimSpace(s)
		if (strings.HasPrefix(s, "[") || strings.HasPrefix(s, "{")) && json.Valid([]byte(s)) {
			args[k], changed = json.RawMessage(s), true
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(args)
	if err != nil {
		return raw
	}
	return out
}
