package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MatLomax/worklog/internal/store"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns whatever
// it printed. Output is expected to be small (a single line), well within the
// pipe buffer, so a plain read after closing the writer suffices.
func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	runErr := fn()
	w.Close()
	os.Stdout = orig
	out, _ := io.ReadAll(r)
	r.Close()
	if runErr != nil {
		t.Fatalf("command returned error: %v", runErr)
	}
	return string(out)
}

// sessionStartPayload mirrors the JSON cmdSessionStart emits.
type sessionStartPayload struct {
	SystemMessage      string `json:"systemMessage"`
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// TestSessionStartCommand exercises the load-bearing contract of
// `worklog session-start`: silence (exit 0) until a project database exists,
// then a single valid JSON object carrying the warm-load in additionalContext
// (for the model) and the next task in systemMessage (for the user).
func TestSessionStartCommand(t *testing.T) {
	dir := t.TempDir()
	dbFile := filepath.Join(dir, store.DirName, store.FileName)
	t.Setenv("WORKLOG_DB", dbFile) // checked before $CLAUDE_PROJECT_DIR and cwd

	// No database yet: nothing printed, no error — safe to wire globally.
	if out := captureStdout(t, func() error { return cmdSessionStart(nil) }); out != "" {
		t.Fatalf("no-db: got %q, want empty", out)
	}

	// A task whose title needs JSON escaping and spans two lines. The output must
	// be one valid JSON object; systemMessage a single escaped line ending in the
	// priority tag with no slug; additionalContext the model briefing naming it.
	title := "Fix \"parser\" & <tag>\r\nsecond line"
	st, err := store.Open(dbFile)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := st.CreateTask(store.CreateTaskInput{Title: title}); err != nil {
		t.Fatalf("create: %v", err)
	}
	st.Close()

	out := captureStdout(t, func() error { return cmdSessionStart(nil) })
	if strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Fatalf("output is not a single JSON line: %q", out)
	}
	var p sessionStartPayload
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &p); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, out)
	}

	// systemMessage: user-visible, single line, no slug, priority as (P3).
	if !strings.HasPrefix(p.SystemMessage, "worklog next task: ") {
		t.Fatalf("systemMessage %q missing expected prefix", p.SystemMessage)
	}
	if !strings.Contains(p.SystemMessage, `Fix "parser" & <tag>`) {
		t.Fatalf("systemMessage %q dropped/garbled the escaped title", p.SystemMessage)
	}
	if strings.Contains(p.SystemMessage, "second line") {
		t.Fatalf("systemMessage %q leaked the title's second line; want one line only", p.SystemMessage)
	}
	if strings.ContainsAny(p.SystemMessage, "\r\n") {
		t.Fatalf("systemMessage %q contains a CR/LF; the notice must be one line", p.SystemMessage)
	}
	if !strings.HasSuffix(p.SystemMessage, " (P3)") {
		t.Fatalf("systemMessage %q should end with the (P3) priority tag", p.SystemMessage)
	}
	if strings.Contains(p.SystemMessage, "fix-parser") {
		t.Fatalf("systemMessage %q should not contain the task slug", p.SystemMessage)
	}

	// additionalContext: model-facing warm-load, tagged as SessionStart.
	if p.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Fatalf("hookEventName = %q, want SessionStart", p.HookSpecificOutput.HookEventName)
	}
	if !strings.Contains(p.HookSpecificOutput.AdditionalContext, "Next up:") ||
		!strings.Contains(p.HookSpecificOutput.AdditionalContext, `Fix "parser"`) {
		t.Fatalf("additionalContext missing the warm-load briefing:\n%s", p.HookSpecificOutput.AdditionalContext)
	}
}

// TestCommandsFollowStatusConfig checks that `session-start` and `context`
// pick up the project's config.jsonc: a configured active status gets its own
// section, ordered active statuses first (in config order), then open ones.
// An invalid config fails `context` naming the file; `session-start` reports
// it in its payload instead of failing the hook.
func TestCommandsFollowStatusConfig(t *testing.T) {
	dir := t.TempDir()
	wlDir := filepath.Join(dir, store.DirName)
	dbFile := filepath.Join(wlDir, store.FileName)
	cfgFile := filepath.Join(wlDir, store.ConfigFileName)
	t.Setenv("WORKLOG_DB", dbFile)
	if err := os.MkdirAll(wlDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{
  "statuses": [
    {"name": "pending", "kind": "open", "default": true},
    {"name": "in_progress", "kind": "active"},
    {"name": "review", "kind": "active"},
    {"name": "blocked", "kind": "blocked"},
    {"name": "done", "kind": "closed"},
  ],
}`
	if err := os.WriteFile(cfgFile, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(dbFile)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, in := range []store.CreateTaskInput{
		{Title: "Queued task"},
		{Title: "Awaiting review", Status: "review"},
		{Title: "Being built", Status: "in_progress"},
	} {
		if _, err := st.CreateTask(in); err != nil {
			t.Fatalf("create %q: %v", in.Title, err)
		}
	}
	st.Close()

	wantOrder := func(label, text string) {
		t.Helper()
		last := -1
		for _, h := range []string{"## In progress\n", "## Review\n", "## Pending\n"} {
			i := strings.Index(text, h)
			if i < 0 {
				t.Fatalf("%s: missing section %q:\n%s", label, h, text)
			}
			if i < last {
				t.Fatalf("%s: section %q out of order:\n%s", label, h, text)
			}
			last = i
		}
		if !strings.Contains(text[strings.Index(text, "## Review\n"):], "Awaiting review") {
			t.Fatalf("%s: review task not listed under Review:\n%s", label, text)
		}
	}

	ctxOut := captureStdout(t, func() error { return cmdContext(nil) })
	wantOrder("context", ctxOut)

	var p sessionStartPayload
	ssOut := captureStdout(t, func() error { return cmdSessionStart(nil) })
	if err := json.Unmarshal([]byte(strings.TrimSpace(ssOut)), &p); err != nil {
		t.Fatalf("session-start output is not valid JSON: %v\n%s", err, ssOut)
	}
	wantOrder("session-start", p.HookSpecificOutput.AdditionalContext)

	// An invalid config (no default status) fails `context`, naming the file.
	// `session-start` still succeeds with the normal payload shape, carrying
	// the error to the user (systemMessage) and the model (additionalContext).
	bad := `{"statuses": [{"name": "pending", "kind": "open"}, {"name": "done", "kind": "closed"}]}`
	if err := os.WriteFile(cfgFile, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	err = cmdContext(nil)
	if err == nil {
		t.Fatal("context: succeeded under an invalid config")
	}
	if !strings.Contains(err.Error(), cfgFile) {
		t.Fatalf("context: error %q does not name %s", err, cfgFile)
	}

	ssOut = captureStdout(t, func() error { return cmdSessionStart(nil) })
	if strings.Count(strings.TrimSpace(ssOut), "\n") != 0 {
		t.Fatalf("session-start under a bad config: output is not a single JSON line: %q", ssOut)
	}
	p = sessionStartPayload{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(ssOut)), &p); err != nil {
		t.Fatalf("session-start under a bad config: output is not valid JSON: %v\n%s", err, ssOut)
	}
	if want := "worklog: invalid config " + cfgFile + ":1:14: statuses: no status is marked"; !strings.HasPrefix(p.SystemMessage, want) {
		t.Fatalf("systemMessage = %q, want prefix %q", p.SystemMessage, want)
	}
	if !strings.Contains(p.SystemMessage, "default") || strings.ContainsAny(p.SystemMessage, "\r\n") {
		t.Fatalf("systemMessage = %q, want the one-line validation error", p.SystemMessage)
	}
	if p.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Fatalf("hookEventName = %q, want SessionStart", p.HookSpecificOutput.HookEventName)
	}
	ac := p.HookSpecificOutput.AdditionalContext
	if want := p.SystemMessage + "\n" + badConfigAdvice; ac != want {
		t.Fatalf("additionalContext = %q, want %q", ac, want)
	}
	for _, sub := range []string{"Fix the config file", "briefing of open work is unavailable", "keep using the statuses already in force", "never loaded a usable config, are refused"} {
		if !strings.Contains(ac, sub) {
			t.Fatalf("additionalContext lacks %q:\n%s", sub, ac)
		}
	}
	if strings.Contains(ac, "every worklog tool call is refused") {
		t.Fatalf("additionalContext claims every call is refused, which a server with a usable config does not do:\n%s", ac)
	}
}

// captureStderr runs fn with os.Stderr redirected to a pipe and returns what it
// wrote along with fn's error.
func captureStderr(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	runErr := fn()
	w.Close()
	os.Stderr = orig
	out, _ := io.ReadAll(r)
	r.Close()
	return string(out), runErr
}

const badStatusConfig = `{"statuses": [{"name": "pending", "kind": "open"}]}`

// TestSessionEndIgnoresBadConfig checks that `session-end` (the Stop hook,
// whose failures are swallowed) still closes the open session when the
// project's config is invalid.
func TestSessionEndIgnoresBadConfig(t *testing.T) {
	wlDir := filepath.Join(t.TempDir(), store.DirName)
	dbFile := filepath.Join(wlDir, store.FileName)
	t.Setenv("WORKLOG_DB", dbFile)
	st, err := store.OpenWithConfig(dbFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	se, err := st.StartSession("agent")
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := os.WriteFile(filepath.Join(wlDir, store.ConfigFileName), []byte(badStatusConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := cmdSessionEnd([]string{"-summary", "wrapped up"}); err != nil {
		t.Fatalf("session-end under a bad config: %v", err)
	}
	st, err = store.OpenWithConfig(dbFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := st.GetSession(se.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.EndedAt == "" || got.Summary != "wrapped up" {
		t.Fatalf("session not closed under a bad config: %+v", got)
	}
}

// TestInitWithBadConfig checks that `init` creates the database even when the
// project's config is invalid, warning on stderr with the file named.
func TestInitWithBadConfig(t *testing.T) {
	dir := t.TempDir()
	wlDir := filepath.Join(dir, store.DirName)
	cfgFile := filepath.Join(wlDir, store.ConfigFileName)
	if err := os.MkdirAll(wlDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgFile, []byte(badStatusConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout string
	stderr, err := captureStderr(t, func() error {
		stdout = captureStdout(t, func() error { return cmdInit([]string{"-dir", dir}) })
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	dbFile := filepath.Join(wlDir, store.FileName)
	if !strings.Contains(stdout, "initialized "+dbFile) {
		t.Fatalf("init stdout = %q", stdout)
	}
	if _, err := os.Stat(dbFile); err != nil {
		t.Fatalf("init did not create the database: %v", err)
	}
	if !strings.Contains(stderr, "warning") || !strings.Contains(stderr, cfgFile) {
		t.Fatalf("init stderr = %q, want a warning naming %s", stderr, cfgFile)
	}

	// A valid config produces no warning.
	if err := os.Remove(cfgFile); err != nil {
		t.Fatal(err)
	}
	stderr, _ = captureStderr(t, func() error {
		captureStdout(t, func() error { return cmdInit([]string{"-dir", dir}) })
		return nil
	})
	if stderr != "" {
		t.Fatalf("init under a valid config wrote to stderr: %q", stderr)
	}
}

// TestEmptyConfigIsReportedByCommands: a config file that exists but holds no
// JSON value (here only a comment) is an error for `init` (a warning) and
// `session-start` (the error payload), never the default statuses.
func TestEmptyConfigIsReportedByCommands(t *testing.T) {
	dir := t.TempDir()
	wlDir := filepath.Join(dir, store.DirName)
	cfgFile := filepath.Join(wlDir, store.ConfigFileName)
	dbFile := filepath.Join(wlDir, store.FileName)
	t.Setenv("WORKLOG_DB", dbFile)
	if err := os.MkdirAll(wlDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgFile, []byte("// statuses go here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const noContent = "config file has no content"

	stderr, err := captureStderr(t, func() error {
		captureStdout(t, func() error { return cmdInit([]string{"-dir", dir}) })
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "warning") || !strings.Contains(stderr, cfgFile+": "+noContent) {
		t.Fatalf("init stderr = %q, want a warning that %s has no content", stderr, cfgFile)
	}

	out := captureStdout(t, func() error { return cmdSessionStart(nil) })
	var p sessionStartPayload
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &p); err != nil {
		t.Fatalf("session-start output is not valid JSON: %v\n%s", err, out)
	}
	if want := "worklog: invalid config " + cfgFile + ": " + noContent; !strings.HasPrefix(p.SystemMessage, want) {
		t.Fatalf("systemMessage = %q, want prefix %q", p.SystemMessage, want)
	}
	if !strings.Contains(p.SystemMessage, "or {} for the default statuses") {
		t.Fatalf("systemMessage = %q, want the remedy", p.SystemMessage)
	}
}
