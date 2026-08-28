package mcp

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MatLomax/worklog/internal/store"
)

func newServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &Server{st: st, out: bufio.NewWriter(io.Discard)}
}

// call invokes a tool through the real tools/call dispatch and returns the
// content text blocks joined, plus whether the tool reported an error.
func call(t *testing.T, s *Server, name string, args map[string]any) (string, bool) {
	t.Helper()
	argJSON, _ := json.Marshal(args)
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": json.RawMessage(argJSON)})
	res, rerr := s.toolsCall(params)
	if rerr != nil {
		t.Fatalf("tools/call %s: rpc error %v", name, rerr)
	}
	m := res.(map[string]any)
	isErr, _ := m["isError"].(bool)
	var sb strings.Builder
	for _, c := range m["content"].([]map[string]any) {
		sb.WriteString(c["text"].(string))
		sb.WriteString("\n")
	}
	return sb.String(), isErr
}

func TestInitializeStartsSession(t *testing.T) {
	s := newServer(t)
	params, _ := json.Marshal(map[string]any{
		"protocolVersion": "2025-06-18",
		"clientInfo":      map[string]any{"name": "test-agent", "version": "1.0"},
	})
	res := s.initialize(params).(map[string]any)
	if res["serverInfo"].(map[string]any)["name"] != "worklog" {
		t.Fatal("serverInfo.name != worklog")
	}
	if s.st.CurrentSessionID() == 0 {
		t.Fatal("initialize did not start a session")
	}
}

func TestToolsListAdvertisesAll(t *testing.T) {
	s := newServer(t)
	res, rerr := s.dispatch(rpcRequest{Method: "tools/list"})
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	tools := res.(map[string]any)["tools"].([]toolDef)
	if len(tools) != len(toolList) || len(tools) == 0 {
		t.Fatalf("advertised %d tools", len(tools))
	}
}

func TestToolCallRoundTrip(t *testing.T) {
	s := newServer(t)

	out, isErr := call(t, s, "task-create", map[string]any{
		"title": "Wire up the thing", "priority": 2,
	})
	if isErr {
		t.Fatalf("task-create errored: %s", out)
	}
	if !strings.Contains(out, "wire-up-the-thing") {
		t.Fatalf("created slug missing: %s", out)
	}

	// Record a decision and a github link, then confirm task-get surfaces both.
	if _, isErr := call(t, s, "task-decide", map[string]any{
		"slug": "wire-up-the-thing", "decision": "Use a channel, not a mutex",
	}); isErr {
		t.Fatal("task-decide errored")
	}
	if _, isErr := call(t, s, "task-link", map[string]any{
		"slug": "wire-up-the-thing", "url": "https://github.com/org/repo/issues/9",
	}); isErr {
		t.Fatal("task-link errored")
	}
	get, isErr := call(t, s, "task-get", map[string]any{"slug": "wire-up-the-thing"})
	if isErr {
		t.Fatalf("task-get errored: %s", get)
	}
	if !strings.Contains(get, "## Decisions") || !strings.Contains(get, "channel, not a mutex") {
		t.Fatalf("decision not in task-get: %s", get)
	}
	if !strings.Contains(get, "issues/9") {
		t.Fatalf("link not in task-get: %s", get)
	}

	// work-find should locate the decision.
	found, _ := call(t, s, "work-find", map[string]any{"query": "channel"})
	if !strings.Contains(found, "channel, not a mutex") {
		t.Fatalf("work-find missed decision: %s", found)
	}
}

func TestAttachOnlyUntilInit(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".worklog", "tasks.db")
	s := &Server{dbPath: path, out: bufio.NewWriter(io.Discard)}

	// No database yet: tools are inert and point at init, and nothing is created.
	out, isErr := call(t, s, "task-list", nil)
	if !isErr || !strings.Contains(out, "worklog init") {
		t.Fatalf("expected inert init message, got isErr=%v out=%q", isErr, out)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("serve created a database — it must be attach-only")
	}

	// init creates the database; the running server then attaches on next call.
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("init open: %v", err)
	}
	st.Close()

	out, isErr = call(t, s, "task-create", map[string]any{"title": "Now it works"})
	if isErr {
		t.Fatalf("task-create after init errored: %s", out)
	}
	if !strings.Contains(out, "now-it-works") {
		t.Fatalf("expected created task, got %q", out)
	}
	s.shutdown()
}

func TestUnknownToolIsError(t *testing.T) {
	s := newServer(t)
	out, isErr := call(t, s, "no-such-tool", nil)
	if !isErr || !strings.Contains(out, "unknown tool") {
		t.Fatalf("expected unknown-tool error, got isErr=%v out=%q", isErr, out)
	}
}
