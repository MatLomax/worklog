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

func TestUpdateRenamesSlug(t *testing.T) {
	s := newServer(t)
	for _, title := range []string{"Ship it", "Build it"} {
		if _, isErr := call(t, s, "task-create", map[string]any{"title": title}); isErr {
			t.Fatalf("task-create %q errored", title)
		}
	}

	// A messy new_slug is normalized like a created slug, and the detail reflects it.
	out, isErr := call(t, s, "task-update", map[string]any{"slug": "ship-it", "new_slug": "Deploy It!"})
	if isErr {
		t.Fatalf("rename errored: %s", out)
	}
	if !strings.Contains(out, "deploy-it") {
		t.Fatalf("normalized new slug not echoed: %s", out)
	}
	// The old slug no longer resolves; the new one does.
	if _, isErr := call(t, s, "task-get", map[string]any{"slug": "ship-it"}); !isErr {
		t.Fatal("old slug still resolves after rename")
	}
	if _, isErr := call(t, s, "task-get", map[string]any{"slug": "deploy-it"}); isErr {
		t.Fatal("new slug does not resolve after rename")
	}

	// Renaming onto an existing slug is an error, not a silent auto-suffix.
	if _, isErr := call(t, s, "task-update", map[string]any{"slug": "deploy-it", "new_slug": "build-it"}); !isErr {
		t.Fatal("renaming onto an existing slug should error")
	}

	// Renaming a task to its own slug is a no-op, not a collision error.
	if _, isErr := call(t, s, "task-update", map[string]any{"slug": "deploy-it", "new_slug": "deploy-it"}); isErr {
		t.Fatal("renaming a task to its own slug should be a no-op")
	}

	// A new_slug with no slug-able characters is an error, not a silent
	// rename to the "task" fallback.
	for _, bad := range []string{"", "  ", "!!!"} {
		if _, isErr := call(t, s, "task-update", map[string]any{"slug": "deploy-it", "new_slug": bad}); !isErr {
			t.Fatalf("new_slug %q should error, not silently rename", bad)
		}
	}
	// The failed renames left the slug untouched.
	if _, isErr := call(t, s, "task-get", map[string]any{"slug": "deploy-it"}); isErr {
		t.Fatal("slug changed despite a rejected new_slug")
	}
}

func TestRenameRewritesBodyReferences(t *testing.T) {
	s := newServer(t)
	if _, isErr := call(t, s, "task-create", map[string]any{"title": "Alpha"}); isErr {
		t.Fatal("task-create Alpha errored")
	}
	if _, isErr := call(t, s, "task-create", map[string]any{"title": "Alpha two"}); isErr {
		t.Fatal("task-create Alpha two errored")
	}
	// Beta references alpha two delimited forms, a near-miss token, and bare prose.
	betaBody := "Depends on [[alpha]] and the `alpha` module. See also [[alpha-two]]. The alpha release."
	if _, isErr := call(t, s, "task-create", map[string]any{"title": "Beta", "body": betaBody}); isErr {
		t.Fatal("task-create Beta errored")
	}

	if _, isErr := call(t, s, "task-update", map[string]any{"slug": "alpha", "new_slug": "Gamma Ray"}); isErr {
		t.Fatal("rename errored")
	}

	body, isErr := call(t, s, "task-get", map[string]any{"slug": "beta"})
	if isErr {
		t.Fatalf("task-get beta errored: %s", body)
	}
	if !strings.Contains(body, "[[gamma-ray]]") {
		t.Fatalf("wikilink reference not rewritten: %s", body)
	}
	if !strings.Contains(body, "`gamma-ray`") {
		t.Fatalf("code-span reference not rewritten: %s", body)
	}
	if strings.Contains(body, "[[alpha]]") || strings.Contains(body, "`alpha`") {
		t.Fatalf("an old delimited reference survived: %s", body)
	}
	// The near-miss token and the bare word must be untouched.
	if !strings.Contains(body, "[[alpha-two]]") {
		t.Fatalf("near-miss token [[alpha-two]] was wrongly rewritten: %s", body)
	}
	if !strings.Contains(body, "The alpha release") {
		t.Fatalf("bare-prose 'alpha' was wrongly rewritten: %s", body)
	}
	// The rewrite is journaled on the affected task (a ref_rewrite entry appears
	// in beta's journal), and the rename itself on the renamed task — the exact
	// writes that were silently failing before the journal kind CHECK was dropped.
	if !strings.Contains(body, "ref_rewrite") {
		t.Fatalf("body rewrite was not journaled on beta: %s", body)
	}
	renamed, isErr := call(t, s, "task-get", map[string]any{"slug": "gamma-ray"})
	if isErr {
		t.Fatalf("task-get gamma-ray errored: %s", renamed)
	}
	if !strings.Contains(renamed, "slug_change") {
		t.Fatalf("rename was not journaled on the renamed task: %s", renamed)
	}

	// A self-reference in the renamed task's own body is rewritten too.
	if _, isErr := call(t, s, "task-create", map[string]any{"title": "Selfie", "body": "Blocks [[selfie]]."}); isErr {
		t.Fatal("task-create Selfie errored")
	}
	if _, isErr := call(t, s, "task-update", map[string]any{"slug": "selfie", "new_slug": "mirror"}); isErr {
		t.Fatal("self rename errored")
	}
	own, isErr := call(t, s, "task-get", map[string]any{"slug": "mirror"})
	if isErr {
		t.Fatalf("task-get mirror errored: %s", own)
	}
	if !strings.Contains(own, "[[mirror]]") || strings.Contains(own, "[[selfie]]") {
		t.Fatalf("self-reference not rewritten in own body: %s", own)
	}
}

func TestUnknownToolIsError(t *testing.T) {
	s := newServer(t)
	out, isErr := call(t, s, "no-such-tool", nil)
	if !isErr || !strings.Contains(out, "unknown tool") {
		t.Fatalf("expected unknown-tool error, got isErr=%v out=%q", isErr, out)
	}
}
