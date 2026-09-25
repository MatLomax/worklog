package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MatLomax/worklog/internal/store"
)

// TestToolsListDefaultMatchesSnapshot pins the default-config tools/list to
// the snapshot captured from the hard-coded tool list it replaced. The one
// intended difference is the status FILTER on task-list and work-find, which
// became an open, pattern-checked string (so a status saved before a config
// change stays queryable); the test patches exactly those two properties into
// the snapshot and requires every other byte to match.
func TestToolsListDefaultMatchesSnapshot(t *testing.T) {
	golden, err := os.ReadFile("testdata/tools_list_default.json")
	if err != nil {
		t.Fatal(err)
	}
	var snap struct {
		Tools []toolDef `json:"tools"`
	}
	if err := json.Unmarshal(golden, &snap); err != nil {
		t.Fatal(err)
	}
	// Decoding into toolDef and re-encoding must be lossless, so the patched
	// comparison below is byte-faithful to the snapshot.
	if rt, _ := json.Marshal(map[string]any{"tools": snap.Tools}); !bytes.Equal(rt, golden) {
		t.Fatal("snapshot does not round-trip through toolDef")
	}
	filter := map[string]string{
		"task-list": "filter to one status",
		"work-find": "restrict matched tasks to this status",
	}
	for _, td := range snap.Tools {
		if desc, ok := filter[td.Name]; ok {
			td.InputSchema["properties"].(map[string]any)["status"] = map[string]any{
				"type":        "string",
				"description": desc + " (configured: pending, in_progress, blocked, done, dropped; a status saved before a config change is also accepted)",
				"pattern":     "^[a-z][a-z0-9]*(_[a-z0-9]+)*$",
			}
			delete(filter, td.Name)
		}
	}
	if len(filter) != 0 {
		t.Fatalf("snapshot lacks tools %v", filter)
	}
	want, _ := json.Marshal(map[string]any{"tools": snap.Tools})

	s := &Server{dbPath: filepath.Join(t.TempDir(), store.DirName, store.FileName), out: bufio.NewWriter(io.Discard)}
	res, rerr := s.dispatch(rpcRequest{Method: "tools/list"})
	if rerr != nil {
		t.Fatal(rerr)
	}
	got, _ := json.Marshal(res)
	if !bytes.Equal(got, want) {
		t.Fatalf("default tools/list drifted from snapshot\n got: %s\nwant: %s", got, want)
	}
}

const customConfig = `{
  // review is worked like in_progress; wontfix closes like done.
  "statuses": [
    {"name": "pending", "kind": "open", "default": true},
    {"name": "review", "kind": "active"},
    {"name": "blocked", "kind": "blocked"},
    {"name": "done", "kind": "closed"},
    {"name": "wontfix", "kind": "closed"},
  ],
}`

// projectDB returns a database path under a fresh project whose config.jsonc
// holds cfg. The database itself is not created.
func projectDB(t *testing.T, cfg string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), store.DirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, store.ConfigFileName), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, store.FileName)
}

// listTools answers tools/list through the real dispatch, keyed by tool name.
func listTools(t *testing.T, s *Server) map[string]toolDef {
	t.Helper()
	res, rerr := s.dispatch(rpcRequest{Method: "tools/list"})
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	out := map[string]toolDef{}
	for _, td := range res.(map[string]any)["tools"].([]toolDef) {
		out[td.Name] = td
	}
	return out
}

func prop(td toolDef, name string) map[string]any {
	return td.InputSchema["properties"].(map[string]any)[name].(map[string]any)
}

func TestToolsListAndCallsFollowConfig(t *testing.T) {
	path := projectDB(t, customConfig)
	s := &Server{dbPath: path, out: bufio.NewWriter(io.Discard)}
	t.Cleanup(s.shutdown)

	// Before the database exists, tools/list already reflects the config.
	check := func(tools map[string]toolDef) {
		t.Helper()
		want := []string{"pending", "review", "blocked", "done", "wontfix"}
		for _, name := range []string{"task-create", "task-update"} {
			if got := prop(tools[name], "status")["enum"]; !reflect.DeepEqual(got, want) {
				t.Errorf("%s status enum = %v, want %v", name, got, want)
			}
		}
		if d := prop(tools["task-create"], "status")["description"]; d != "initial status (default pending)" {
			t.Errorf("task-create status description = %q", d)
		}
		if d := prop(tools["task-list"], "include_closed")["description"]; d != "include done/wontfix tasks" {
			t.Errorf("include_closed description = %q", d)
		}
		if d := tools["task-next"].Description; !strings.Contains(d, "(pending/review with no active blockers)") {
			t.Errorf("task-next description = %q", d)
		}
		if d := tools["task-update"].Description; !strings.Contains(d, "Setting status to done/wontfix stamps") {
			t.Errorf("task-update description = %q", d)
		}
		if d := tools["task-add-blocker"].Description; !strings.Contains(d, "until each blocker is done or wontfix.") {
			t.Errorf("task-add-blocker description = %q", d)
		}
		for _, name := range []string{"task-list", "work-find"} {
			f := prop(tools[name], "status")
			if _, ok := f["enum"]; ok {
				t.Errorf("%s status filter is a closed enum; leftover statuses would be rejected", name)
			}
			if f["pattern"] != "^[a-z][a-z0-9]*(_[a-z0-9]+)*$" {
				t.Errorf("%s status filter pattern = %v", name, f["pattern"])
			}
			if d, _ := f["description"].(string); !strings.Contains(d, "configured: pending, review, blocked, done, wontfix;") {
				t.Errorf("%s status filter description = %q", name, d)
			}
		}
	}
	before := listTools(t, s)
	check(before)

	// init runs with a config that still had in_progress, and a task is saved
	// holding it; the project config has since dropped in_progress.
	st, err := store.OpenWithConfig(path, store.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(store.CreateTaskInput{Title: "Legacy work", Status: "in_progress"}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	out, isErr := call(t, s, "task-create", map[string]any{"title": "Needs eyes", "status": "review"})
	if isErr || !strings.Contains(out, `"status": "review"`) {
		t.Fatalf("create with configured status review: isErr=%v out=%s", isErr, out)
	}
	if out, isErr := call(t, s, "task-create", map[string]any{"title": "Nope", "status": "in_progress"}); !isErr {
		t.Fatalf("create with unconfigured status in_progress succeeded: %s", out)
	}
	if out, isErr := call(t, s, "task-update", map[string]any{"slug": "needs-eyes", "status": "in_progress"}); !isErr {
		t.Fatalf("update to unconfigured status in_progress succeeded: %s", out)
	}

	// Filters accept the leftover status.
	out, isErr = call(t, s, "task-list", map[string]any{"status": "in_progress"})
	if isErr || !strings.Contains(out, "legacy-work") || strings.Contains(out, "needs-eyes") {
		t.Fatalf("task-list filtered by leftover status: isErr=%v out=%s", isErr, out)
	}
	out, isErr = call(t, s, "work-find", map[string]any{"status": "in_progress"})
	if isErr || !strings.Contains(out, "legacy-work") {
		t.Fatalf("work-find filtered by leftover status: isErr=%v out=%s", isErr, out)
	}
	if out, isErr := call(t, s, "task-list", map[string]any{"status": "Not A Status"}); !isErr {
		t.Fatalf("malformed status filter accepted: %s", out)
	}

	// The attached store and tools/list agree.
	if after := listTools(t, s); !reflect.DeepEqual(after, before) {
		t.Fatal("tools/list changed after the database attached")
	}
	if got := s.st.Config().Names(); !reflect.DeepEqual(got, []string{"pending", "review", "blocked", "done", "wontfix"}) {
		t.Fatalf("attached store config = %v", got)
	}
}

// writeConfig replaces the project's config.jsonc beside path.
func writeConfig(t *testing.T, path, cfg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), store.ConfigFileName), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

func statusEnum(t *testing.T, s *Server) any {
	t.Helper()
	return prop(listTools(t, s)["task-create"], "status")["enum"]
}

// TestStoreAttachesWithServerConfig pins that the database is opened with the
// configuration the server loaded (and advertised in tools/list), not one
// re-read independently: the file is edited between the server's read and the
// attach, and the attached store must still hold what the server loaded until
// the next sync swaps the edit in.
func TestStoreAttachesWithServerConfig(t *testing.T) {
	path := projectDB(t, customConfig)
	st, err := store.OpenWithConfig(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	s := &Server{dbPath: path, out: bufio.NewWriter(io.Discard)}
	t.Cleanup(s.shutdown)
	listTools(t, s) // the server loads customConfig

	writeConfig(t, path, `{"statuses": [{"name": "todo", "kind": "open", "default": true}, {"name": "fin", "kind": "closed"}]}`)
	if _, err := s.store(); err != nil {
		t.Fatal(err)
	}
	if got, want := s.st.Config().Names(), []string{"pending", "review", "blocked", "done", "wontfix"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("attached store config = %v, want the server's loaded %v", got, want)
	}

	if out, isErr := call(t, s, "task-create", map[string]any{"title": "Swapped", "status": "todo"}); isErr {
		t.Fatalf("create after the next sync: %s", out)
	}
	if got := s.st.Config().Names(); !reflect.DeepEqual(got, []string{"todo", "fin"}) {
		t.Fatalf("store config after sync = %v", got)
	}
}

// TestSyncConfigNoReloadWhileUnchanged pins the change check: while the file's
// bytes are unchanged, syncConfig must not re-apply it to the store (here
// detected by a configuration set on the store directly surviving the sync);
// once the bytes change it does.
func TestSyncConfigNoReloadWhileUnchanged(t *testing.T) {
	path := projectDB(t, customConfig)
	st, err := store.OpenWithConfig(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	s := &Server{dbPath: path, out: bufio.NewWriter(io.Discard), stderr: io.Discard}
	t.Cleanup(s.shutdown)
	if out, isErr := call(t, s, "task-list", nil); isErr {
		t.Fatalf("task-list: %s", out)
	}

	marker := &store.Config{Statuses: []store.StatusDef{
		{Name: "marker", Kind: store.KindOpen, Default: true},
		{Name: "closed_marker", Kind: store.KindClosed},
	}}
	if err := s.st.SetConfig(marker); err != nil {
		t.Fatal(err)
	}
	s.syncConfig()
	if out, isErr := call(t, s, "task-list", nil); isErr {
		t.Fatalf("task-list: %s", out)
	}
	if got := s.st.Config().Names(); !reflect.DeepEqual(got, []string{"marker", "closed_marker"}) {
		t.Fatalf("store config = %v after syncs of an unchanged file; it was reloaded", got)
	}

	writeConfig(t, path, customConfig+"\n// edited\n")
	s.syncConfig()
	if got := s.st.Config().Names(); !reflect.DeepEqual(got, []string{"pending", "review", "blocked", "done", "wontfix"}) {
		t.Fatalf("store config = %v after the file changed, want it reloaded", got)
	}
}

// TestTaskNextEmptyMessageNamesNoStatus pins the empty task-next result: it
// must not name a status (such as the default "done") that a project's config
// may not have.
func TestTaskNextEmptyMessageNamesNoStatus(t *testing.T) {
	s := newServer(t)
	out, isErr := call(t, s, "task-next", nil)
	if isErr {
		t.Fatalf("task-next: %s", out)
	}
	if want := "No actionable task — every unclosed task is on hold or waiting on a blocker, or there are none.\n"; out != want {
		t.Fatalf("task-next = %q, want %q", out, want)
	}
}

// Configs used by the reload tests. None of them has the built-in default
// status "pending", so a task saved as pending proves the defaults were
// applied.
const (
	todoFinConfig    = `{"statuses": [{"name": "todo", "kind": "open", "default": true}, {"name": "fin", "kind": "closed"}]}`
	queuedConfig     = `{"statuses": [{"name": "queued", "kind": "open", "default": true}, {"name": "shipped", "kind": "closed"}]}`
	backlogConfig    = `{"statuses": [{"name": "backlog", "kind": "open", "default": true}, {"name": "later", "kind": "active"}, {"name": "gone", "kind": "closed"}]}`
	noClosedConfig   = `{"statuses": [{"name": "pending", "kind": "open", "default": true}]}`
	notePrefix       = "note: worklog config can't be used, so this call used the last valid statuses: "
	refusedFragment  = "worklog config can't be used, so tool calls are refused: "
	retryHintMessage = "(fix or restore the file and retry; if it is being saved, just retry)"
)

// removedNote is the note on a tool result while the config file at cfgFile,
// once present, is missing and the last valid statuses stay in force.
func removedNote(cfgFile string) string {
	return "note: worklog config " + cfgFile + " was removed, so this call used the last valid statuses; they stay in force until the server restarts (recreate the file to change them)"
}

// removedRefusal is the refusal of a server that has never had a usable
// configuration once the config file at cfgFile has been removed.
func removedRefusal(cfgFile string) string {
	return "worklog config " + cfgFile + " was removed before the server loaded a usable one, so tool calls are refused: recreate the file and retry, or restart the server to use the default statuses (if it is being saved, just retry)"
}

// cfgFileOf is the config file beside the database at path.
func cfgFileOf(path string) string { return filepath.Join(filepath.Dir(path), store.ConfigFileName) }

// initDB creates the database at path (with the default statuses, as init
// would have before the project's config was written).
func initDB(t *testing.T, path string) {
	t.Helper()
	st, err := store.OpenWithConfig(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
}

// callBlocks invokes a tool through the real tools/call dispatch and returns
// its content text blocks and whether it reported an error.
func callBlocks(t *testing.T, s *Server, name string, args map[string]any) ([]string, bool) {
	t.Helper()
	argJSON, _ := json.Marshal(args)
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": json.RawMessage(argJSON)})
	res, rerr := s.toolsCall(params)
	if rerr != nil {
		t.Fatalf("tools/call %s: rpc error %v", name, rerr)
	}
	m := res.(map[string]any)
	isErr, _ := m["isError"].(bool)
	var blocks []string
	for _, c := range m["content"].([]map[string]any) {
		blocks = append(blocks, c["text"].(string))
	}
	return blocks, isErr
}

// create creates a task without a status and returns the status saved and
// the config note appended to the result ("" when there is none).
func create(t *testing.T, s *Server, title string) (status, note string) {
	t.Helper()
	blocks, isErr := callBlocks(t, s, "task-create", map[string]any{"title": title})
	if isErr {
		t.Fatalf("task-create %q: %q", title, blocks)
	}
	switch {
	case len(blocks) == 2 && strings.HasPrefix(blocks[1], "note: "):
		note = blocks[1]
	case len(blocks) != 1:
		t.Fatalf("task-create %q returned %d blocks: %q", title, len(blocks), blocks)
	}
	var task struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(blocks[0]), &task); err != nil {
		t.Fatalf("task-create output is not a task: %v\n%s", err, blocks[0])
	}
	return task.Status, note
}

// wantCreate creates a task and requires the given status and note state.
func wantCreate(t *testing.T, s *Server, title, status string, noted bool) string {
	t.Helper()
	got, note := create(t, s, title)
	if got != status {
		t.Fatalf("%s: saved status %q, want %q", title, got, status)
	}
	if noted != (note != "") {
		t.Fatalf("%s: note = %q, want a note: %v", title, note, noted)
	}
	return note
}

// newFileServer returns a server over a project whose config.jsonc holds cfg
// and whose database exists, reading the real file, with stderr in log.
func newFileServer(t *testing.T, cfg string, log io.Writer) (*Server, string) {
	t.Helper()
	path := projectDB(t, cfg)
	initDB(t, path)
	s := &Server{dbPath: path, out: bufio.NewWriter(io.Discard), stderr: log}
	t.Cleanup(s.shutdown)
	return s, cfgFileOf(path)
}

// TestBadConfigAtStartupRefusesCalls: a server whose first read finds an
// invalid file has never had a usable configuration, so tool calls are refused
// with the file's error and a hint to retry; tools/list lists the default
// statuses; fixing the file recovers without a restart.
func TestBadConfigAtStartupRefusesCalls(t *testing.T) {
	path := projectDB(t, noClosedConfig)
	initDB(t, path)
	s := &Server{dbPath: path, out: bufio.NewWriter(io.Discard), stderr: io.Discard}
	t.Cleanup(s.shutdown)

	got := listTools(t, s)
	want := map[string]toolDef{}
	for _, td := range toolsFor(store.DefaultConfig()) {
		want[td.Name] = td
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("tools/list under a bad config is not the default list")
	}

	blocks, isErr := callBlocks(t, s, "task-list", nil)
	if want := refusedFragment + cfgFileOf(path) + ":1:14: statuses: no status of kind closed; at least one is required " + retryHintMessage; !isErr || len(blocks) != 1 || blocks[0] != want {
		t.Fatalf("tool call under a bad config: isErr=%v out=%q, want the refusal %q", isErr, blocks, want)
	}
	out := blocks[0]
	if strings.Contains(out, "worklog init") {
		t.Fatalf("bad config reported as a missing database: %s", out)
	}
	if s.st != nil {
		t.Fatal("store attached despite an invalid config")
	}

	writeConfig(t, path, todoFinConfig)
	wantCreate(t, s, "Fixed", "todo", false)
}

// TestBadConfigAtStartupThenRemovedStaysRefused: a server that has never had
// a usable configuration does not take a later missing file for the
// defaults, since that may be the gap of an editor saving the fix. Its
// refusal follows the file's state: an invalid file says to fix it; a removed
// one says to recreate it or restart for the defaults, and nothing about an
// invalid file; a file written again, still invalid, says to fix it again.
func TestBadConfigAtStartupThenRemovedStaysRefused(t *testing.T) {
	path := projectDB(t, noClosedConfig)
	initDB(t, path)
	cfgFile := cfgFileOf(path)
	s := &Server{dbPath: path, out: bufio.NewWriter(io.Discard), stderr: io.Discard}
	t.Cleanup(s.shutdown)
	invalid := refusedFragment + cfgFile + ":1:14: statuses: no status of kind closed; at least one is required " + retryHintMessage
	refusal := func(what, want string) string {
		t.Helper()
		blocks, isErr := callBlocks(t, s, "task-create", map[string]any{"title": "Gap"})
		if !isErr || len(blocks) != 1 || blocks[0] != want {
			t.Fatalf("call %s: isErr=%v out=%q, want %q", what, isErr, blocks, want)
		}
		return blocks[0]
	}
	refusal("under a bad config", invalid)
	if err := os.Remove(cfgFile); err != nil {
		t.Fatal(err)
	}
	out := refusal("after the never-valid file was removed", removedRefusal(cfgFile))
	for _, stale := range []string{"is invalid", "can't be used", "fix the file", "fix or restore"} {
		if strings.Contains(out, stale) {
			t.Fatalf("refusal for a removed file says %q: %s", stale, out)
		}
	}
	writeConfig(t, path, noClosedConfig)
	refusal("after an invalid file was written again", invalid)
	writeConfig(t, path, queuedConfig)
	wantCreate(t, s, "Saved", "queued", false)
}

// TestConfigReloadsBetweenCalls checks that a running server follows the
// config file: an edit takes effect on the next tool call and in tools/list;
// a file gone bad keeps the last valid statuses (with a note on each result);
// fixing it applies the fix; deleting it keeps the last valid statuses (with a
// note on each result) until a valid file is written again.
func TestConfigReloadsBetweenCalls(t *testing.T) {
	const pendingDone = `{"statuses": [{"name": "pending", "kind": "open", "default": true}, {"name": "done", "kind": "closed"}]}`
	s, cfgFile := newFileServer(t, todoFinConfig, io.Discard)
	path := s.dbPath

	if out, isErr := call(t, s, "task-create", map[string]any{"title": "One", "status": "todo"}); isErr {
		t.Fatalf("create with todo under todo/fin: %s", out)
	}

	writeConfig(t, path, pendingDone)
	if out, isErr := call(t, s, "task-create", map[string]any{"title": "Two", "status": "todo"}); !isErr {
		t.Fatalf("create with todo succeeded after the config dropped it: %s", out)
	}
	if out, isErr := call(t, s, "task-create", map[string]any{"title": "Three", "status": "pending"}); isErr {
		t.Fatalf("create with pending after the config added it: %s", out)
	}
	if got := statusEnum(t, s); !reflect.DeepEqual(got, []string{"pending", "done"}) {
		t.Fatalf("tools/list enum after reload = %v", got)
	}

	writeConfig(t, path, `{"statuses": [{"name": "pending", "kind": "open"}]}`)
	blocks, isErr := callBlocks(t, s, "task-create", map[string]any{"title": "Four", "status": "pending"})
	if isErr || len(blocks) != 2 || !strings.Contains(blocks[0], `"status": "pending"`) {
		t.Fatalf("create under a bad config with the last valid statuses: isErr=%v out=%q", isErr, blocks)
	}
	if want := notePrefix + cfgFile + ":1:14: statuses: no status of kind closed; at least one is required (fix the file to apply your changes)"; blocks[1] != want {
		t.Fatalf("note = %q, want %q", blocks[1], want)
	}
	// A call that fails on its own carries the note too, after its error.
	blocks, isErr = callBlocks(t, s, "task-get", map[string]any{"slug": "no-such-task"})
	if !isErr || len(blocks) != 2 || !strings.HasPrefix(blocks[1], notePrefix) {
		t.Fatalf("failing call under a bad config: isErr=%v out=%q", isErr, blocks)
	}
	if got := statusEnum(t, s); !reflect.DeepEqual(got, []string{"pending", "done"}) {
		t.Fatalf("tools/list under a bad config = %v, want the last good list", got)
	}

	writeConfig(t, path, todoFinConfig)
	if out, isErr := call(t, s, "task-create", map[string]any{"title": "Five", "status": "todo"}); isErr || strings.Contains(out, "note: ") {
		t.Fatalf("create after the config was fixed: isErr=%v out=%s", isErr, out)
	}
	if out, isErr := call(t, s, "task-create", map[string]any{"title": "Six", "status": "pending"}); !isErr {
		t.Fatalf("create with pending succeeded after the fix dropped it: %s", out)
	}
	if got := statusEnum(t, s); !reflect.DeepEqual(got, []string{"todo", "fin"}) {
		t.Fatalf("tools/list enum after the fix = %v", got)
	}

	// Deleting the file keeps the last valid statuses until a restart, and
	// every result says so, a failing call's after its error.
	if err := os.Remove(cfgFile); err != nil {
		t.Fatal(err)
	}
	blocks, isErr = callBlocks(t, s, "task-create", map[string]any{"title": "Seven", "status": "in_progress"})
	if !isErr || len(blocks) != 2 || blocks[1] != removedNote(cfgFile) {
		t.Fatalf("create with a default status after the config was removed: isErr=%v out=%q", isErr, blocks)
	}
	if note := wantCreate(t, s, "Eight", "todo", true); note != removedNote(cfgFile) {
		t.Fatalf("note after removal = %q, want %q", note, removedNote(cfgFile))
	}
	if got := statusEnum(t, s); !reflect.DeepEqual(got, []string{"todo", "fin"}) {
		t.Fatalf("tools/list enum after removal = %v", got)
	}

	// Writing a valid file again applies it, and the note is gone.
	writeConfig(t, path, queuedConfig)
	wantCreate(t, s, "Nine", "queued", false)
}

// TestConfigRemovedThenRewritten models an editor that saves by removing the
// file and writing a new one: a call in the gap keeps the configuration in
// force (a task created without a status gets its default, never the
// built-in "pending") and notes the removal, and the new file is adopted,
// without a note, once written.
func TestConfigRemovedThenRewritten(t *testing.T) {
	s, cfgFile := newFileServer(t, todoFinConfig, io.Discard)
	wantCreate(t, s, "Before", "todo", false)
	if err := os.Remove(cfgFile); err != nil {
		t.Fatal(err)
	}
	if note := wantCreate(t, s, "Gap", "todo", true); note != removedNote(cfgFile) {
		t.Fatalf("note in the gap = %q, want %q", note, removedNote(cfgFile))
	}
	if got := s.st.Config().Names(); !reflect.DeepEqual(got, []string{"todo", "fin"}) {
		t.Fatalf("store config in the gap = %v", got)
	}
	writeConfig(t, s.dbPath, queuedConfig)
	wantCreate(t, s, "After", "queued", false)
}

// TestConfigTruncatedThenRewritten models an editor that saves in place
// (truncate, then write): a call that finds the file empty keeps the
// configuration in force and notes the invalid file on its result, and the
// new content is adopted, without a note, once written.
func TestConfigTruncatedThenRewritten(t *testing.T) {
	s, cfgFile := newFileServer(t, todoFinConfig, io.Discard)
	wantCreate(t, s, "Before", "todo", false)
	if err := os.Truncate(cfgFile, 0); err != nil {
		t.Fatal(err)
	}
	note := wantCreate(t, s, "Empty", "todo", true)
	if !strings.HasPrefix(note, notePrefix+cfgFile+": config file has no content") {
		t.Fatalf("note = %q", note)
	}
	writeConfig(t, s.dbPath, queuedConfig)
	wantCreate(t, s, "After", "queued", false)
}

// TestConfigPartialWriteKeepsLastValid: a call that finds the file half
// written keeps the configuration in force and notes the syntax error.
func TestConfigPartialWriteKeepsLastValid(t *testing.T) {
	s, cfgFile := newFileServer(t, todoFinConfig, io.Discard)
	wantCreate(t, s, "Before", "todo", false)
	writeConfig(t, s.dbPath, queuedConfig[:len(queuedConfig)/2])
	note := wantCreate(t, s, "Partial", "todo", true)
	if !strings.HasPrefix(note, notePrefix+cfgFile+":1:") {
		t.Fatalf("note = %q, want the positioned syntax error", note)
	}
	writeConfig(t, s.dbPath, queuedConfig)
	wantCreate(t, s, "After", "queued", false)
}

// TestConfigMissingAtStartupThenCreated: with no config file the defaults
// apply; a file created later is adopted, and a problem with it after that
// keeps the configuration in force.
func TestConfigMissingAtStartupThenCreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), store.DirName)
	path := filepath.Join(dir, store.FileName)
	initDB(t, path)
	s := &Server{dbPath: path, out: bufio.NewWriter(io.Discard), stderr: io.Discard}
	t.Cleanup(s.shutdown)
	wantCreate(t, s, "Defaults", "pending", false)
	// An invalid file appearing keeps the defaults in force, with a note.
	if err := os.WriteFile(cfgFileOf(path), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	wantCreate(t, s, "Empty", "pending", true)
	writeConfig(t, path, queuedConfig)
	wantCreate(t, s, "Created", "queued", false)
	if got := statusEnum(t, s); !reflect.DeepEqual(got, []string{"queued", "shipped"}) {
		t.Fatalf("tools/list enum = %v", got)
	}
}

// TestSyncConfigParsesTheBytesItSnapshots pins that the configuration comes
// from the very bytes that set the change snapshot. The read hook serves a
// valid config while the file on disk is empty: parsing anything else would
// pin an error against a snapshot of the valid file, and since that snapshot
// then matches every later read, the server would never recover. A steady
// file costs one read per sync.
func TestSyncConfigParsesTheBytesItSnapshots(t *testing.T) {
	path := projectDB(t, "")
	var reads int
	s := &Server{dbPath: path, out: bufio.NewWriter(io.Discard), stderr: io.Discard,
		readFile: func(string) ([]byte, error) { reads++; return []byte(customConfig), nil }}
	t.Cleanup(s.shutdown)

	want := []string{"pending", "review", "blocked", "done", "wontfix"}
	for i := 0; i < 2; i++ {
		if got := statusEnum(t, s); !reflect.DeepEqual(got, want) {
			t.Fatalf("sync %d: enum = %v, want the snapshot's %v", i, got, want)
		}
	}
	if reads != 2 {
		t.Fatalf("config read %d times over 2 syncs, want 2", reads)
	}
	if !bytes.Equal(s.cfgSnap.data, []byte(customConfig)) {
		t.Fatalf("snapshot = %q", s.cfgSnap.data)
	}
}

// TestSyncConfigLogsEachProblemStateOnce pins the logging rule: every problem
// state (existence plus bytes or read error) is logged once, when a read first
// finds it, and not again while it persists. Two different invalid contents
// are two states, each logged even when their error text is the same; a
// missing file that was present before is logged once; a valid file logs
// nothing.
func TestSyncConfigLogsEachProblemStateOnce(t *testing.T) {
	path := projectDB(t, todoFinConfig)
	initDB(t, path)
	cfgFile := cfgFileOf(path)
	var log bytes.Buffer
	var readErr error
	s := &Server{dbPath: path, out: bufio.NewWriter(io.Discard), stderr: &log,
		readFile: func(name string) ([]byte, error) {
			if readErr != nil {
				return nil, readErr
			}
			return os.ReadFile(name)
		}}
	t.Cleanup(s.shutdown)
	var want []string
	step := func(change func(), logged string) {
		t.Helper()
		change()
		for i := 0; i < 3; i++ {
			listTools(t, s)
			call(t, s, "task-list", nil)
		}
		if logged != "" {
			want = append(want, "worklog: config: "+logged)
		}
		got := strings.Split(strings.TrimSuffix(log.String(), "\n"), "\n")
		if log.Len() == 0 {
			got = nil
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("log = %q, want %q", got, want)
		}
	}
	file := func(content string) func() {
		return func() { readErr = nil; writeConfig(t, path, content) }
	}
	noClosed := cfgFile + ":1:14: statuses: no status of kind closed; at least one is required"
	missing := cfgFile + " is missing; still using the last valid statuses (deleting the file takes effect when the server restarts)"
	step(file(todoFinConfig), "")
	step(file(noClosedConfig), noClosed)
	step(file(noClosedConfig+"\n// another edit, the same mistake\n"), noClosed)
	step(func() { readErr = errors.New("permission denied") }, "permission denied")
	step(func() { readErr = errors.New("input/output error") }, "input/output error")
	step(func() { readErr = errors.New("permission denied") }, "permission denied")
	step(file(todoFinConfig), "")
	step(func() {
		readErr = nil
		if err := os.Remove(cfgFile); err != nil {
			t.Fatal(err)
		}
	}, missing)
	step(file(noClosedConfig), noClosed)
	step(file(queuedConfig), "")
}

// TestConfigSavesUnderLoadNeverApplyDefaults is a stress test with a real
// writer: a goroutine keeps rewriting the config file, alternating an
// editor's two save styles (remove then write; truncate then write), always
// with complete, valid content drawn from configs that lack "pending", while
// the server handles task creates without a status. Every saved status must be
// the default of one of the written configs: a mid-save read must never put
// the built-in defaults in force.
func TestConfigSavesUnderLoadNeverApplyDefaults(t *testing.T) {
	configs := []string{todoFinConfig, queuedConfig, backlogConfig}
	allowed := map[string]bool{"todo": true, "queued": true, "backlog": true}
	s, cfgFile := newFileServer(t, todoFinConfig, io.Discard)
	wantCreate(t, s, "Before", "todo", false)

	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			content := []byte(configs[i%len(configs)])
			if i%2 == 0 {
				if err := retrySharing(func() error { return os.Remove(cfgFile) }); err != nil {
					done <- err
					return
				}
			}
			if err := retrySharing(func() error { return os.WriteFile(cfgFile, content, 0o644) }); err != nil {
				done <- err
				return
			}
		}
	}()

	n := 500
	if testing.Short() {
		n = 100
	}
	seen := map[string]int{}
	for i := 0; i < n; i++ {
		got, _ := create(t, s, fmt.Sprintf("Load %d", i))
		seen[got]++
	}
	close(stop)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	t.Logf("saved statuses over %d creates: %v", n, seen)
	for status, count := range seen {
		if !allowed[status] {
			t.Errorf("%d of %d creates saved status %q, which no written config has as its default", count, n, status)
		}
	}
	if len(seen) < 2 {
		t.Errorf("statuses saved = %v; the writer's edits were never picked up, so the test exercised nothing", seen)
	}
}

// retrySharing runs a file operation of the config writer, retrying it
// briefly while it fails: on Windows, removing or rewriting a file the server
// has open for reading fails with a sharing violation until the read closes,
// and an editor saving there retries the same way.
func retrySharing(op func() error) error {
	var err error
	for range 200 {
		if err = op(); err == nil {
			return nil
		}
		time.Sleep(time.Millisecond)
	}
	return err
}

// defaultsNotePrefix starts the note on a tool result while the config file
// can't be used and the default statuses, put in force by a missing file, are
// in force.
const defaultsNotePrefix = "note: worklog config can't be used, so this call used the default statuses: "

// newMissingServer returns a server over a project with a database and no
// config file, whose reads fail with *readErr while it is non-nil, logging to
// log.
func newMissingServer(t *testing.T, readErr *error, log io.Writer) (*Server, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), store.DirName)
	path := filepath.Join(dir, store.FileName)
	initDB(t, path)
	s := &Server{dbPath: path, out: bufio.NewWriter(io.Discard), stderr: log,
		readFile: func(name string) ([]byte, error) {
			if *readErr != nil {
				return nil, *readErr
			}
			return os.ReadFile(name)
		}}
	t.Cleanup(s.shutdown)
	return s, cfgFileOf(path)
}

// permissionDenied is the error a read of the config file returns when its
// directory lacks search permission, whether or not the file exists.
func permissionDenied(cfgFile string) error {
	return &fs.PathError{Op: "open", Path: cfgFile, Err: fs.ErrPermission}
}

// TestReadErrorAtStartupThenMissingAppliesDefaults: a failed read finds
// nothing, so a file that a failed first read could not see, and that turns
// out to be absent once reads succeed, never existed: the defaults apply with
// no refusal and nothing about a removal, in results or the log.
func TestReadErrorAtStartupThenMissingAppliesDefaults(t *testing.T) {
	var log bytes.Buffer
	var readErr error
	s, cfgFile := newMissingServer(t, &readErr, &log)
	readErr = permissionDenied(cfgFile)
	blocks, isErr := callBlocks(t, s, "task-create", map[string]any{"title": "Refused"})
	if want := refusedFragment + readErr.Error() + " " + retryHintMessage; !isErr || len(blocks) != 1 || blocks[0] != want {
		t.Fatalf("call under an unreadable config: isErr=%v out=%q, want %q", isErr, blocks, want)
	}
	readErr = nil
	wantCreate(t, s, "Defaults", "pending", false)
	if want := "worklog: config: " + permissionDenied(cfgFile).Error() + "\n"; log.String() != want {
		t.Fatalf("log = %q, want only %q", log.String(), want)
	}
}

// TestTransientReadErrorWithDefaultsInForce: with the defaults in force (no
// file at startup), a read error keeps them, noting it in the defaults'
// wording; once reads succeed and the file is still missing, the note is gone
// and nothing says the file was removed.
func TestTransientReadErrorWithDefaultsInForce(t *testing.T) {
	var log bytes.Buffer
	var readErr error
	s, cfgFile := newMissingServer(t, &readErr, &log)
	wantCreate(t, s, "Before", "pending", false)
	readErr = permissionDenied(cfgFile)
	if note := wantCreate(t, s, "During", "pending", true); note != defaultsNotePrefix+readErr.Error()+" (fix the file to apply your changes)" {
		t.Fatalf("note = %q", note)
	}
	readErr = nil
	wantCreate(t, s, "After", "pending", false)
	if want := "worklog: config: " + permissionDenied(cfgFile).Error() + "\n"; log.String() != want {
		t.Fatalf("log = %q, want only %q", log.String(), want)
	}
}

// TestConfigNoteWordingFollowsSource pins the note's wording by where the
// configuration in force came from. With the defaults in force (no file at
// startup), an invalid file is noted as leaving the default statuses in force,
// and removing it is no problem at all (a missing file means the defaults,
// which are in force), so nothing suggests a restart. With a config loaded
// from a file in force, an invalid file is noted as leaving the last valid
// statuses in force, and its removal as lasting until a restart.
func TestConfigNoteWordingFollowsSource(t *testing.T) {
	var log bytes.Buffer
	var readErr error
	s, cfgFile := newMissingServer(t, &readErr, &log)
	path := s.dbPath
	wantCreate(t, s, "Defaults", "pending", false)

	writeConfig(t, path, noClosedConfig)
	noClosed := cfgFile + ":1:14: statuses: no status of kind closed; at least one is required"
	if note := wantCreate(t, s, "Invalid over defaults", "pending", true); note != defaultsNotePrefix+noClosed+" (fix the file to apply your changes)" {
		t.Fatalf("note with the defaults in force = %q", note)
	}
	if err := os.Remove(cfgFile); err != nil {
		t.Fatal(err)
	}
	wantCreate(t, s, "Removed over defaults", "pending", false)
	if want := "worklog: config: " + noClosed + "\n"; log.String() != want {
		t.Fatalf("log = %q, want only %q", log.String(), want)
	}

	writeConfig(t, path, todoFinConfig)
	wantCreate(t, s, "Loaded", "todo", false)
	writeConfig(t, path, noClosedConfig)
	if note := wantCreate(t, s, "Invalid over loaded", "todo", true); note != notePrefix+noClosed+" (fix the file to apply your changes)" {
		t.Fatalf("note with a loaded config in force = %q", note)
	}
	if err := os.Remove(cfgFile); err != nil {
		t.Fatal(err)
	}
	if note := wantCreate(t, s, "Removed over loaded", "todo", true); note != removedNote(cfgFile) {
		t.Fatalf("note after removal with a loaded config in force = %q", note)
	}
}

// TestEmptyObjectConfigMeansDefaults: a valid file without "statuses", such as
// {}, means the defaults, exactly as a missing file does: an invalid file
// after it is noted in the defaults' wording, and deleting it is no problem
// (nothing is noted or logged, and nothing suggests a restart). A file that
// lists the default statuses explicitly is a loaded file like any other.
func TestEmptyObjectConfigMeansDefaults(t *testing.T) {
	var log bytes.Buffer
	s, cfgFile := newFileServer(t, todoFinConfig, &log)
	path := s.dbPath
	wantCreate(t, s, "Loaded", "todo", false)

	writeConfig(t, path, "// defaults\n{}\n")
	wantCreate(t, s, "Empty object", "pending", false)
	writeConfig(t, path, noClosedConfig)
	noClosed := cfgFile + ":1:14: statuses: no status of kind closed; at least one is required"
	if note := wantCreate(t, s, "Invalid over {}", "pending", true); note != defaultsNotePrefix+noClosed+" (fix the file to apply your changes)" {
		t.Fatalf("note with {} in force = %q", note)
	}
	writeConfig(t, path, "{}")
	wantCreate(t, s, "Back to {}", "pending", false)
	log.Reset()
	if err := os.Remove(cfgFile); err != nil {
		t.Fatal(err)
	}
	wantCreate(t, s, "Removed {}", "pending", false)
	if log.Len() != 0 {
		t.Fatalf("removing {} logged %q", log.String())
	}

	// The same statuses, listed in the file, are the file's: removing that
	// file keeps them until a restart, and says so.
	writeConfig(t, path, `{"statuses": [{"name": "pending", "kind": "open", "default": true}, {"name": "in_progress", "kind": "active"}, {"name": "blocked", "kind": "blocked"}, {"name": "done", "kind": "closed"}, {"name": "dropped", "kind": "closed"}]}`)
	wantCreate(t, s, "Explicit defaults", "pending", false)
	if err := os.Remove(cfgFile); err != nil {
		t.Fatal(err)
	}
	if note := wantCreate(t, s, "Removed explicit", "pending", true); note != removedNote(cfgFile) {
		t.Fatalf("note after removing an explicit list = %q", note)
	}
}
