// Package mcp implements worklog's Model Context Protocol server: a
// newline-delimited JSON-RPC 2.0 loop over stdin/stdout exposing the task tree
// and its decision/journal record as tools any agent can call mid-session.
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"

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

// Server wires the MCP protocol to a worklog store. The store is attached
// lazily from dbPath so a globally-installed server stays inert in projects
// that have not run `worklog init`.
type Server struct {
	dbPath string
	st     *store.Store // nil until the database exists and is attached
	agent  string       // client label from the handshake, for session attribution
	out    *bufio.Writer
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

// store lazily attaches to the project database. It returns (nil, false) when
// the database does not yet exist — serve never creates it; `worklog init`
// does. Attaching after init runs mid-session means tools start working without
// restarting the server. Once attached, a session is ensured for attribution.
func (s *Server) store() (*store.Store, bool) {
	if s.st == nil {
		if _, err := os.Stat(s.dbPath); err != nil {
			return nil, false
		}
		st, err := store.Open(s.dbPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "worklog: open db:", err)
			return nil, false
		}
		s.st = st
	}
	if s.st.CurrentSessionID() == 0 {
		if _, err := s.st.StartSession(s.agent); err != nil {
			fmt.Fprintln(os.Stderr, "worklog: start session:", err)
		}
	}
	return s.st, true
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
		fmt.Fprintln(os.Stderr, "worklog: marshal response:", err)
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
		return map[string]any{"tools": toolList}, nil
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
