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

func TestEveryAdvertisedToolHasAHandler(t *testing.T) {
	if len(handlers) != len(toolList) {
		t.Fatalf("handlers=%d toolList=%d — every advertised tool must map 1:1 to a handler", len(handlers), len(toolList))
	}
	for _, td := range toolList {
		if handlers[td.Name] == nil {
			t.Errorf("tool %q is advertised but has no handler", td.Name)
		}
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

func TestBlockerToolsManageDepsOnExistingTasks(t *testing.T) {
	s := newServer(t)
	for _, title := range []string{"Ship it", "Build it", "Design it"} {
		if _, isErr := call(t, s, "task-create", map[string]any{"title": title}); isErr {
			t.Fatalf("task-create %q errored", title)
		}
	}

	// Add two blockers to a task that was created without any.
	out, isErr := call(t, s, "task-add-blocker", map[string]any{
		"slug": "ship-it", "blocked_by": []string{"build-it", "design-it"},
	})
	if isErr {
		t.Fatalf("task-add-blocker errored: %s", out)
	}
	if !strings.Contains(out, "build-it") || !strings.Contains(out, "design-it") {
		t.Fatalf("added blockers not echoed: %s", out)
	}

	// A blocked task is not offered as the next actionable one.
	if next, _ := call(t, s, "task-next", nil); strings.Contains(next, "ship-it") {
		t.Fatalf("blocked task returned as next: %s", next)
	}

	// Removing one leaves the other in place (incremental, not a full replace).
	out, isErr = call(t, s, "task-remove-blocker", map[string]any{
		"slug": "ship-it", "blocked_by": []string{"build-it"},
	})
	if isErr {
		t.Fatalf("task-remove-blocker errored: %s", out)
	}
	if strings.Contains(out, "build-it") || !strings.Contains(out, "design-it") {
		t.Fatalf("remove left the wrong blocker set: %s", out)
	}

	// A nonexistent blocker slug is an error, not a silent no-op.
	if _, isErr := call(t, s, "task-add-blocker", map[string]any{
		"slug": "ship-it", "blocked_by": []string{"does-not-exist"},
	}); !isErr {
		t.Fatal("adding a nonexistent blocker should error")
	}
}

func TestUnknownToolIsError(t *testing.T) {
	s := newServer(t)
	out, isErr := call(t, s, "no-such-tool", nil)
	if !isErr || !strings.Contains(out, "unknown tool") {
		t.Fatalf("expected unknown-tool error, got isErr=%v out=%q", isErr, out)
	}
}
