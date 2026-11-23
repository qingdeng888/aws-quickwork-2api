package proxy

import (
	"strings"
	"testing"
)

var bridgeTestAllowed = map[string]bool{"read": true, "bash": true}

func TestParseClientToolCalls(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		allowed   map[string]bool
		wantClean string
		wantNames []string
		wantArgs  []string
	}{
		{
			name:      "single tagged call",
			in:        `<client_tool_call>{"name":"read","arguments":{"file_path":"a"}}</client_tool_call>`,
			wantClean: "",
			wantNames: []string{"read"},
			wantArgs:  []string{`{"file_path":"a"}`},
		},
		{
			name:      "two calls with surrounding text",
			in:        `hi <client_tool_call>{"name":"read","arguments":{}}</client_tool_call> <client_tool_call>{"name":"bash","arguments":{"command":"ls"}}</client_tool_call>`,
			wantClean: "hi",
			wantNames: []string{"read", "bash"},
			wantArgs:  []string{`{}`, `{"command":"ls"}`},
		},
		{
			name:      "arguments encoded as a JSON string",
			in:        `<tool_call>{"name":"bash","arguments":"{\"command\":\"ls\"}"}</tool_call>`,
			wantClean: "",
			wantNames: []string{"bash"},
			wantArgs:  []string{`{"command":"ls"}`},
		},
		{
			name:      "arguments that are not JSON become an empty object",
			in:        `<tool_call>{"name":"bash","arguments":"not json"}</tool_call>`,
			wantClean: "",
			wantNames: []string{"bash"},
			wantArgs:  []string{`{}`},
		},
		{
			name:      "namespace prefixed tool name resolves",
			in:        `<client_tool_call>{"name":"functions.read","arguments":{}}</client_tool_call>`,
			wantClean: "",
			wantNames: []string{"read"},
			wantArgs:  []string{`{}`},
		},
		{
			name:      "unclosed tag is left untouched",
			in:        `<client_tool_call>{"name":"read","arguments":{}}`,
			wantClean: `<client_tool_call>{"name":"read","arguments":{}}`,
		},
		{
			name:      "unknown tool is left untouched",
			in:        `<client_tool_call>{"name":"rm_rf","arguments":{}}</client_tool_call>`,
			wantClean: `<client_tool_call>{"name":"rm_rf","arguments":{}}</client_tool_call>`,
		},
		{
			name:      "call inside a markdown fence",
			in:        "```json\n{\"name\":\"read\",\"arguments\":{\"file_path\":\"b\"}}\n```",
			wantClean: "",
			wantNames: []string{"read"},
			wantArgs:  []string{`{"file_path":"b"}`},
		},
		{
			name:      "ordinary code fence is preserved",
			in:        "```go\nfmt.Println(1)\n```",
			wantClean: "```go\nfmt.Println(1)\n```",
		},
		{
			name:      "bare openai tool_calls wrapper",
			in:        `{"tool_calls":[{"function":{"name":"bash","arguments":"{}"}}]}`,
			wantClean: "",
			wantNames: []string{"bash"},
			wantArgs:  []string{`{}`},
		},
		{
			name:      "no tools declared means no parsing",
			in:        `<client_tool_call>{"name":"read","arguments":{}}</client_tool_call>`,
			allowed:   map[string]bool{},
			wantClean: `<client_tool_call>{"name":"read","arguments":{}}</client_tool_call>`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			allowed := tc.allowed
			if allowed == nil {
				allowed = bridgeTestAllowed
			}
			clean, replies := parseClientToolCalls(tc.in, allowed)
			if clean != tc.wantClean {
				t.Errorf("clean = %q, want %q", clean, tc.wantClean)
			}
			if len(replies) != len(tc.wantNames) {
				t.Fatalf("got %d replies, want %d: %+v", len(replies), len(tc.wantNames), replies)
			}
			for i, reply := range replies {
				if reply.Name != tc.wantNames[i] || reply.Arguments != tc.wantArgs[i] {
					t.Errorf("reply %d = %s %s, want %s %s",
						i, reply.Name, reply.Arguments, tc.wantNames[i], tc.wantArgs[i])
				}
			}
		})
	}
}

func TestToolCallIDsUnique(t *testing.T) {
	const n = 20
	in := strings.Repeat(`<client_tool_call>{"name":"read","arguments":{}}</client_tool_call>`, n)
	_, replies := parseClientToolCalls(in, bridgeTestAllowed)
	if len(replies) != n {
		t.Fatalf("got %d replies, want %d", len(replies), n)
	}
	seen := make(map[string]bool, n)
	for _, reply := range replies {
		if !strings.HasPrefix(reply.ID, "call_") {
			t.Fatalf("unexpected tool call id %q", reply.ID)
		}
		if seen[reply.ID] {
			t.Fatalf("duplicate tool call id %q", reply.ID)
		}
		seen[reply.ID] = true
	}
}

func TestLimitToolReplies(t *testing.T) {
	replies := []ToolCallReply{{Name: "read"}, {Name: "bash"}}
	if got := limitToolReplies(replies, "auto", "", false); len(got) != 1 {
		t.Errorf("parallel_tool_calls=false kept %d replies, want 1", len(got))
	}
	if got := limitToolReplies(replies, "function", "bash", true); len(got) != 1 || got[0].Name != "bash" {
		t.Errorf("tool_choice function=bash = %+v", got)
	}
	if got := limitToolReplies(replies, "auto", "", true); len(got) != 2 {
		t.Errorf("parallel kept %d replies, want 2", len(got))
	}
}

func TestParseToolChoice(t *testing.T) {
	if mode, _ := parseToolChoice("Required"); mode != "required" {
		t.Errorf("string mode = %q, want required", mode)
	}
	mode, name := parseToolChoice(map[string]any{
		"type":     "function",
		"function": map[string]any{"name": "read"},
	})
	if mode != "function" || name != "read" {
		t.Errorf("object choice = %q %q, want function read", mode, name)
	}
	if mode, _ := parseToolChoice(nil); mode != "auto" {
		t.Errorf("nil choice mode = %q, want auto", mode)
	}
}

func TestNativeToolReplies(t *testing.T) {
	calls := []ToolCallState{
		{Name: "read", Arguments: "not json"},
		{Name: "secret", Arguments: "{}"},
		{Name: "bash", ID: "call_x", Arguments: `{"command":"ls"}`},
	}
	got := nativeToolReplies(calls, bridgeTestAllowed)
	if len(got) != 2 {
		t.Fatalf("got %d replies, want 2: %+v", len(got), got)
	}
	if got[0].Name != "read" || got[0].Arguments != "{}" || got[0].ID == "" {
		t.Errorf("invalid arguments were not normalized: %+v", got[0])
	}
	if got[1].ID != "call_x" || got[1].Arguments != `{"command":"ls"}` {
		t.Errorf("upstream id/arguments not preserved: %+v", got[1])
	}
}

func TestRequestedToolNames(t *testing.T) {
	req := &ChatRequest{
		Tools:     []Tool{{Function: ToolFunction{Name: "read"}}},
		Functions: []ToolFunction{{Name: "bash"}},
	}
	names := requestedToolNames(req)
	if !names["read"] || !names["bash"] || len(names) != 2 {
		t.Errorf("requested names = %v", names)
	}
}
