// Command worklog is a per-project, SQLite-backed task and decision log for AI
// coding agents, exposed to them over MCP so work is remembered across
// sessions. It ships as a single static binary for Linux, Windows, and macOS.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/MatLomax/worklog/internal/mcp"
	"github.com/MatLomax/worklog/internal/store"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	mcp.Version = version
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = cmdServe(args)
	case "init":
		err = cmdInit(args)
	case "context":
		err = cmdContext(args)
	case "session-start":
		err = cmdSessionStart(args)
	case "session-end":
		err = cmdSessionEnd(args)
	case "version", "--version", "-v":
		fmt.Println("worklog", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintln(os.Stderr, "worklog: unknown command:", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "worklog:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `worklog — per-project task & decision log for coding agents

usage:
  worklog serve   [--db PATH]        run the MCP server over stdio
  worklog init    [--dir DIR]        create .worklog/tasks.db for a project
  worklog context [--db PATH] [-n N] print the "where was I" briefing (plain text; for Codex / debugging)
  worklog session-start [--db PATH] [-n N] SessionStart hook output: warm-load the model + show the next task to the user
  worklog session-end [--summary S]  close the current session (for a Stop hook)
  worklog version

The database is resolved from the current directory: the nearest ancestor
`+"`.worklog/`"+` directory, else `+"`./.worklog/tasks.db`"+`. Override with --db or $WORKLOG_DB.
`)
}

// dbPath resolves the database from the flag, then $WORKLOG_DB, then the
// project directory. Under a Claude Code plugin the MCP server's working
// directory is not guaranteed to be the project root, so $CLAUDE_PROJECT_DIR is
// preferred over the cwd.
func dbPath(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if env := os.Getenv("WORKLOG_DB"); env != "" {
		return env, nil
	}
	base := os.Getenv("CLAUDE_PROJECT_DIR")
	if base == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		base = cwd
	}
	return store.Resolve(base)
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	db := fs.String("db", "", "database path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path, err := dbPath(*db)
	if err != nil {
		return err
	}
	// Attach-only: the server opens the database if it exists but never creates
	// it, so a globally-installed plugin stays inert until `worklog init` runs.
	fmt.Fprintln(os.Stderr, "worklog: serving project db", path)
	return mcp.Serve(path)
}

func cmdSessionEnd(args []string) error {
	fs := flag.NewFlagSet("session-end", flag.ContinueOnError)
	db := fs.String("db", "", "database path")
	summary := fs.String("summary", "", "summary of what the session did")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path, err := dbPath(*db)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		return nil // no database here; nothing to close
	}
	st, err := store.Open(path)
	if err != nil {
		return err
	}
	defer st.Close()
	_, err = st.EndLatestOpenSession(*summary)
	return err
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	dir := fs.String("dir", "", "project directory (default: $CLAUDE_PROJECT_DIR or .)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	target := *dir
	if target == "" {
		if target = os.Getenv("CLAUDE_PROJECT_DIR"); target == "" {
			target = "."
		}
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	path := filepath.Join(abs, store.DirName, store.FileName)
	st, err := store.Open(path)
	if err != nil {
		return err
	}
	defer st.Close()
	fmt.Println("initialized", path)
	return nil
}

func cmdContext(args []string) error {
	fs := flag.NewFlagSet("context", flag.ContinueOnError)
	db := fs.String("db", "", "database path")
	n := fs.Int("n", 15, "number of recent journal entries")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path, err := dbPath(*db)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		return nil // no database here yet; nothing to brief
	}
	st, err := store.Open(path)
	if err != nil {
		return err
	}
	defer st.Close()
	out, err := st.WarmContext(*n)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

// cmdSessionStart emits the Claude Code SessionStart hook payload in one JSON
// object over its two independent channels: `hookSpecificOutput.additionalContext`
// warm-loads the "where was I" briefing into the model's context (invisible to
// the user), and `systemMessage` shows the next task as a line the user sees
// (plain stdout would reach only the model). It prints nothing when there is no
// database yet, so it is safe to wire globally; `systemMessage` is omitted when
// nothing is actionable.
func cmdSessionStart(args []string) error {
	fs := flag.NewFlagSet("session-start", flag.ContinueOnError)
	db := fs.String("db", "", "database path")
	n := fs.Int("n", 15, "number of recent journal entries")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path, err := dbPath(*db)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		return nil // no database here yet; nothing to emit
	}
	st, err := store.Open(path)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, err := st.WarmContext(*n)
	if err != nil {
		return err
	}
	line, err := st.NextLine()
	if err != nil {
		return err
	}
	payload := struct {
		SystemMessage      string `json:"systemMessage,omitempty"`
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext,omitempty"`
		} `json:"hookSpecificOutput"`
	}{SystemMessage: line}
	payload.HookSpecificOutput.HookEventName = "SessionStart"
	payload.HookSpecificOutput.AdditionalContext = ctx
	out, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}
