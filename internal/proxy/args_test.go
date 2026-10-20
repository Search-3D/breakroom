package proxy

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Cursor rejects the whole tool list if any outputSchema holds the
// "anything" schema `true`, which jsonschema infers for envelope.Data.
func TestOutputSchemaDataIsObject(t *testing.T) {
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	if _, err := newMCPServer(&session{client: NewClient()}).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	res, err := cs.ListTools(ctx, nil)
	if err != nil || len(res.Tools) == 0 {
		t.Fatalf("ListTools = %v, %v", res, err)
	}
	for _, tool := range res.Tools {
		b, _ := json.Marshal(tool.OutputSchema)
		var s struct {
			Type       any
			Properties map[string]json.RawMessage
		}
		_ = json.Unmarshal(b, &s)
		if s.Type != "object" {
			t.Errorf("%s: outputSchema.type = %v, want object", tool.Name, s.Type)
		}
		if got := string(s.Properties["data"]); got != `{"type":"object"}` {
			t.Errorf("%s: outputSchema.properties.data = %s", tool.Name, got)
		}
	}
}

// Every tool except register_agent advertises caller_id in its input
// schema; register_agent mints the id and must not take it.
func TestInputSchemaCallerID(t *testing.T) {
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	if _, err := newMCPServer(&session{client: NewClient()}).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	res, err := cs.ListTools(ctx, nil)
	if err != nil || len(res.Tools) == 0 {
		t.Fatalf("ListTools = %v, %v", res, err)
	}
	for _, tool := range res.Tools {
		b, _ := json.Marshal(tool.InputSchema)
		var s struct {
			Properties map[string]json.RawMessage
		}
		_ = json.Unmarshal(b, &s)
		raw, has := s.Properties["caller_id"]
		if tool.Name == "register_agent" {
			if has {
				t.Errorf("register_agent: input schema has caller_id, want none")
			}
			continue
		}
		if !has {
			t.Errorf("%s: input schema has no caller_id", tool.Name)
			continue
		}
		var p struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &p)
		if p.Type != "string" {
			t.Errorf("%s: caller_id type = %q, want string", tool.Name, p.Type)
		}
	}
}

func TestDecodeArgs(t *testing.T) {
	params := map[string]bool{"items": true}
	for _, c := range []struct{ name, in, want string }{
		{"stringified array decoded", `{"items":"[{\"a\":1}]","proof":"p"}`, `{"items":[{"a":1}],"proof":"p"}`},
		{"real array and plain string untouched", `{"items":[{"a":1}],"proof":"[1]"}`, `{"items":[{"a":1}],"proof":"[1]"}`},
		{"undecodable left for validation", `{"items":"[{broken","proof":"p"}`, `{"items":"[{broken","proof":"p"}`},
		{"plain string", `{"items":"not json"}`, `{"items":"not json"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := string(decodeArgs([]byte(c.in), params)); got != c.want {
				t.Errorf("decodeArgs(%s) = %s, want %s", c.in, got, c.want)
			}
		})
	}
}
