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
