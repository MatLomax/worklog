// Package mcp implements worklog's Model Context Protocol server: a
// newline-delimited JSON-RPC 2.0 loop over stdin/stdout exposing the task tree
// and its decision/journal record as tools any agent can call mid-session.
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

	"github.com/MatLomax/worklog/internal/store"
)

// protocolVersion is the MCP revision worklog implements; the client's own
// requested version is echoed back when it sends one.
const protocolVersion = "2025-06-18"

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// noDBMessage is returned by every tool when the project has no worklog
// database yet — serve is attach-only and never creates one.
const noDBMessage = "No worklog database in this project. Run `worklog init` (or the /worklog:init command) to create one, then retry."

// errNoDB is returned by Server.store when the project database does not exist.
var errNoDB = errors.New("no worklog database")

// Server wires the MCP protocol to a worklog store. The store is attached
// lazily from dbPath so a globally-installed server stays inert in projects
// that have not run `worklog init`.
type Server struct {
	dbPath string
	st     *store.Store // nil until the database exists and is attached
	agent  string       // client label from the handshake, for session attribution
	out    *bufio.Writer

	// The project's config file is the source of truth for every write:
	// syncConfig re-reads it before each tools/list and tools/call and, when
	// it has changed and parses as valid, swaps it into the attached store.
	// cfgSnap is the file state seen by the last read (nil before the first);
	// cfg is the configuration in force (nil while the server has never had a
	// usable one); cfgDefaults is set while cfg is the built-in defaults (put
	// in force by a missing file, or by a valid file without "statuses", such
	// as {}), rather than a status list loaded from a valid file;
	// seenFile records that some read has found the file present (a read that
	// failed found nothing either way); cfgErr is the file's current problem
	// (invalid content, a read error, or its removal after a read found it
	// present), nil while it is valid or missing with the defaults in force;
	// cfgRemoved is set when that problem is the removal, and is read only
	// while cfgErr is set.
	cfgSnap     *configSnapshot
	cfg         *store.Config
	cfgDefaults bool
	seenFile    bool
	cfgErr      error
	cfgRemoved  bool

	// readFile reads the config file; nil means os.ReadFile. stderr receives
	// diagnostics; nil means os.Stderr. Tests replace both.
	readFile func(name string) ([]byte, error)
	stderr   io.Writer
}

// logf writes one diagnostic line to the server's stderr.
func (s *Server) logf(format string, args ...any) {
	w := s.stderr
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, format+"\n", args...)
}

// configSnapshot is the outcome of one read of the config file: its content;
// a missing file (exists false), which loads as the defaults; or a read error
// (err non-nil).
type configSnapshot struct {
	exists bool
	data   []byte
	err    error
}

// same reports whether two reads saw the same file state: the same existence,
// the same bytes, and the same read error text, if any.
func (a *configSnapshot) same(b *configSnapshot) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.exists == b.exists && bytes.Equal(a.data, b.data) && errText(a.err) == errText(b.err)
}

// errText is err's message, or "" for nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// readConfig reads the config file once through the readFile hook.
func (s *Server) readConfig(path string) *configSnapshot {
	read := s.readFile
	if read == nil {
		read = os.ReadFile
	}
	data, err := read(path)
	switch {
	case err == nil:
		return &configSnapshot{exists: true, data: data}
	case errors.Is(err, fs.ErrNotExist):
		return &configSnapshot{}
	}
	return &configSnapshot{err: err}
}

// configPath is the config file beside the database.
func (s *Server) configPath() string {
	return filepath.Join(filepath.Dir(s.dbPath), store.ConfigFileName)
}

// syncConfig brings the server's configuration in line with the config file
// beside dbPath, under one rule: a configuration is adopted only from a valid
// parse of a file that exists. Each call reads the file once and does nothing
// while it reads the same as last time (same existence, bytes and read error),
// so a steady file is not re-parsed. A read that differs is handled by what
// it found:
//
//   - A valid file: parsed from exactly the bytes read
//     (store.ParseConfigFileDefaults), it becomes cfg and is swapped into the
//     attached store. A file without "statuses" (such as {}) means the
//     defaults, so it puts them in force just as a missing file does, and a
//     later removal or problem is treated as for the defaults.
//   - A missing file that no read has ever found present (a failed read finds
//     nothing), or while the defaults are already in force: the defaults
//     apply, with no problem to report, since that is what a missing file
//     means.
//   - A missing file that an earlier read found present, while a status list
//     loaded from a valid file is in force: cfg is kept, and cfgErr is set (with
//     cfgRemoved) so every tool result carries a note saying so. A deletion
//     cannot be told apart from the gap an editor leaves while saving (write a
//     new file, rename the old one away), so deleting the config takes effect
//     only when the server restarts.
//   - An invalid, empty or unreadable file: cfg is kept (the last status list
//     loaded from a valid file, or the defaults), and cfgErr is
//     set so every tool result carries a note saying so. Only a server that has
//     never had a usable config (its first read found an invalid or unreadable
//     file) refuses tool calls.
//
// A save may still be observed half-written; what cannot happen is that a
// half-written, empty or missing file replaces the configuration in force.
// A read that finds a problem state logs one line to stderr when that state
// differs from what the previous read found: each distinct state (existence
// plus bytes or read error) is logged, even when its error text matches the
// previous one, since a new invalid edit is news to whoever made it. A steady
// state is not logged again, but a state that returns after any other state
// in between is logged again. A server without a dbPath (tests that inject a
// store) has no file to follow.
func (s *Server) syncConfig() {
	if s.dbPath == "" {
		return
	}
	path := s.configPath()
	snap := s.readConfig(path)
	if snap.same(s.cfgSnap) {
		return
	}
	s.cfgSnap = snap
	if snap.err != nil {
		s.problem(snap.err)
		return
	}
	if !snap.exists {
		switch {
		case !s.seenFile || (s.cfg != nil && s.cfgDefaults):
			s.adopt(store.DefaultConfig(), true)
		case s.cfg != nil:
			s.removed(fmt.Errorf("%s is missing; still using the last valid statuses (deleting the file takes effect when the server restarts)", path))
		default:
			s.removed(fmt.Errorf("%s: the file was removed; recreate it, or restart the server to use the default statuses", path))
		}
		return
	}
	s.seenFile = true
	cfg, defaults, err := store.ParseConfigFileDefaults(path, snap.data)
	if err != nil {
		s.problem(err)
		return
	}
	s.adopt(cfg, defaults)
}

// adopt puts cfg in force, in the attached store as well, and clears any
// problem; defaults says it is the built-in defaults a missing file, or a
// file without "statuses", stands for, not a status list from a file. A
// store that rejects it (it validates as the parse did, so this does not
// happen in practice) leaves the old configuration in force as for an invalid
// file.
func (s *Server) adopt(cfg *store.Config, defaults bool) {
	if s.st != nil {
		if err := s.st.SetConfig(cfg); err != nil {
			s.problem(err)
			return
		}
	}
	s.cfg, s.cfgDefaults, s.cfgErr = cfg, defaults, nil
}

// problem records err, a problem with the config file's content or its read,
// as the file's current problem and logs it.
func (s *Server) problem(err error) {
	s.cfgErr, s.cfgRemoved = err, false
	s.logf("worklog: config: %s", err)
}

// removed records the removal of a config file an earlier read found present
// as the file's current problem, and logs err, which describes it.
func (s *Server) removed(err error) {
	s.cfgErr, s.cfgRemoved = err, true
	s.logf("worklog: config: %s", err)
}

// configNote is the line appended to a tool result while the config file has
// a problem (it is invalid, unreadable or removed); "" otherwise. It is only
// called for a call that store let through, so a configuration is in force.
// It names what is in force: the last valid statuses loaded from a file, or
// the default statuses. A removal is noted only in the first case (with the
// defaults in force, a missing file is no problem).
func (s *Server) configNote() string {
	switch {
	case s.cfgErr == nil:
		return ""
	case s.cfgRemoved:
		return fmt.Sprintf("note: worklog config %s was removed, so this call used the last valid statuses; they stay in force until the server restarts (recreate the file to change them)", s.configPath())
	case s.cfgDefaults:
		return fmt.Sprintf("note: worklog config can't be used, so this call used the default statuses: %s (fix the file to apply your changes)", s.cfgErr)
	}
	return fmt.Sprintf("note: worklog config can't be used, so this call used the last valid statuses: %s (fix the file to apply your changes)", s.cfgErr)
}

// tools returns the tool list for tools/list, rendered from the configuration
// in force: a config file that has since gone bad or missing leaves the list
// as it was, and a server that has never had a usable configuration lists the
// default statuses. The status enums are a hint to the
// client; the store's validation on each call is authoritative.
func (s *Server) tools() []toolDef {
	s.syncConfig()
	switch {
	case s.cfg != nil:
		return toolsFor(s.cfg)
	case s.st != nil && s.dbPath == "":
		return toolsFor(s.st.Config())
	}
	return toolsFor(store.DefaultConfig())
}

// Serve runs the stdio loop until stdin closes. It attaches to the database at
// dbPath on demand; if that file does not exist the server still answers the
// handshake and tools/list, but tool calls report that init is needed.
func Serve(dbPath string) error {
	s := &Server{dbPath: dbPath, out: bufio.NewWriter(os.Stdout)}
	r := bufio.NewReader(os.Stdin)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			s.handleLine(line)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			s.shutdown()
			return err
		}
	}
	s.shutdown()
	return nil
}

// shutdown closes the handshake-opened session and the database, if attached.
func (s *Server) shutdown() {
	if s.st != nil {
		s.st.EndCurrentSession()
		s.st.Close()
	}
}

// store lazily attaches to the project database. It returns errNoDB when the
// database does not yet exist — serve never creates it; `worklog init` does.
// Attaching after init runs mid-session means tools start working without
// restarting the server. The store is opened with the server's configuration
// (the one syncConfig put in force, which tools/list advertises), never
// re-read independently, so the two cannot disagree. A server that has never
// had a usable configuration (its first read found an invalid or unreadable
// file) refuses the call, saying what to do: fix the file, or, once it has
// been removed, recreate it or restart the server for the default statuses. A
// valid file recovers on the next call.
// Once attached, a session is ensured for attribution. Callers run syncConfig
// first.
func (s *Server) store() (*store.Store, error) {
	if s.st == nil {
		if _, err := os.Stat(s.dbPath); err != nil {
			return nil, errNoDB
		}
		if s.cfg == nil {
			if s.cfgRemoved {
				return nil, fmt.Errorf("worklog config %s was removed before the server loaded a usable one, so tool calls are refused: recreate the file and retry, or restart the server to use the default statuses (if it is being saved, just retry)", s.configPath())
			}
			return nil, fmt.Errorf("worklog config can't be used, so tool calls are refused: %w (fix or restore the file and retry; if it is being saved, just retry)", s.cfgErr)
		}
		st, err := store.OpenWithConfig(s.dbPath, s.cfg)
		if err != nil {
			s.logf("worklog: open db: %v", err)
			return nil, fmt.Errorf("open worklog database %s: %w", s.dbPath, err)
		}
		s.st = st
	}
	if s.st.CurrentSessionID() == 0 {
		if _, err := s.st.StartSession(s.agent); err != nil {
			s.logf("worklog: start session: %v", err)
		}
	}
	return s.st, nil
}

func (s *Server) handleLine(line []byte) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return // not valid JSON; ignore per JSON-RPC batching leniency
	}
	if req.Method == "" {
		return
	}
	isNotification := len(req.ID) == 0
	result, rerr := s.dispatch(req)
	if isNotification {
		return // notifications get no response
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	if rerr != nil {
		resp.Error = rerr
	} else {
		resp.Result = result
	}
	s.write(resp)
}

func (s *Server) write(resp rpcResponse) {
	b, err := json.Marshal(resp)
	if err != nil {
		s.logf("worklog: marshal response: %v", err)
		return
	}
	s.out.Write(b)
	s.out.WriteByte('\n')
	s.out.Flush()
}

func (s *Server) dispatch(req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return s.initialize(req.Params), nil
	case "notifications/initialized":
		return nil, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.tools()}, nil
	case "tools/call":
		return s.toolsCall(req.Params)
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
}

func (s *Server) initialize(params json.RawMessage) any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
		ClientInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
	}
	json.Unmarshal(params, &p)

	s.agent = p.ClientInfo.Name
	if p.ClientInfo.Version != "" {
		s.agent += " " + p.ClientInfo.Version
	}
	s.syncConfig()
	s.store() // attach and open a session if the database already exists

	ver := p.ProtocolVersion
	if ver == "" {
		ver = protocolVersion
	}
	return map[string]any{
		"protocolVersion": ver,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "worklog", "version": Version},
	}
}

// Version is the server version reported in the handshake; set from main.
var Version = "dev"
