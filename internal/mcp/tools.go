package mcp

import (
	"encoding/json"
	"fmt"

	"github.com/MatLomax/worklog/internal/store"
)

// --- JSON Schema helpers ---------------------------------------------------

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}
func strp(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func enumP(desc string, vals ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": vals}
}
func intP(desc string) map[string]any  { return map[string]any{"type": "integer", "description": desc} }
func boolP(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }
func arrP(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

var statusEnum = []string{"pending", "in_progress", "blocked", "done", "dropped"}

// toolList is advertised via tools/list; handlers maps each name to its impl.
var toolList = []toolDef{
	{"task-list", "List tasks in actionable order (unblocked first, then by priority). Closed tasks are hidden unless include_closed is set.",
		obj(map[string]any{
			"status":         enumP("filter to one status", statusEnum...),
			"parent":         strp("only children of this task slug"),
			"include_closed": boolP("include done/dropped tasks"),
		})},
	{"task-get", "Full detail for one task: body, active blockers, subtasks, links, decisions, and recent journal — rendered as markdown and JSON.",
		obj(map[string]any{"slug": strp("task slug")}, "slug")},
	{"task-next", "The single highest-priority actionable task (pending/in_progress with no active blockers).", obj(map[string]any{})},
	{"task-tree", "The task forest, or the subtree under a given slug, nested with children.",
		obj(map[string]any{"slug": strp("root task slug; omit for the whole forest")})},
	{"task-create", "Create a task, optionally under a parent, with blocking dependencies.",
		obj(map[string]any{
			"title":      strp("task title"),
			"slug":       strp("explicit slug; derived from the title when omitted"),
			"parent":     strp("parent task slug"),
			"body":       strp("markdown body (use ## sections)"),
			"status":     enumP("initial status (default pending)", statusEnum...),
			"priority":   intP("1 (highest) to 5 (lowest); default 3"),
			"blocked_by": arrP("slugs of tasks that block this one"),
		}, "title")},
	{"task-update", "Update a task's fields. Setting status to done/dropped stamps its close time and unblocks dependents.",
		obj(map[string]any{
			"slug":     strp("task slug"),
			"new_slug": strp("rename the task's slug; normalized like a created slug, and must not collide with an existing task"),
			"title":    strp("new title"),
			"status":   enumP("new status", statusEnum...),
			"priority": intP("new priority 1-5"),
			"position": intP("new sibling position"),
			"parent":   strp("new parent slug; empty string detaches to top level"),
			"body":     strp("replace the full markdown body"),
		}, "slug")},
	{"task-add-blocker", "Add one or more blocking dependencies to an existing task — it stays blocked until each blocker is done or dropped.",
		obj(map[string]any{
			"slug":       strp("task slug"),
			"blocked_by": arrP("slugs of tasks that block this one"),
		}, "slug", "blocked_by")},
	{"task-remove-blocker", "Remove one or more blocking dependencies from a task. Both the task and each named blocker must exist; an edge that isn't set is a no-op.",
		obj(map[string]any{
			"slug":       strp("task slug"),
			"blocked_by": arrP("slugs of blocking tasks to detach"),
		}, "slug", "blocked_by")},
	{"task-toc", "List the ## section paths within a task's body.",
		obj(map[string]any{"slug": strp("task slug")}, "slug")},
	{"task-section-get", "Read one section of a task body by heading path (e.g. \"Design/Storage\").",
		obj(map[string]any{"slug": strp("task slug"), "path": strp("heading path")}, "slug", "path")},
	{"task-section-set", "Replace one section of a task body, leaving the rest untouched.",
		obj(map[string]any{"slug": strp("task slug"), "path": strp("heading path"), "content": strp("new section content")}, "slug", "path", "content")},
	{"task-link", "Attach an external link to a task — a GitHub issue/PR by full URL, a commit, a file, or any URL. Kind is auto-detected from the URL.",
		obj(map[string]any{
			"slug":  strp("task slug"),
			"url":   strp("full URL (e.g. https://github.com/org/repo/issues/412)"),
			"kind":  enumP("override the detected kind", "github_issue", "github_pr", "commit", "file", "url"),
			"label": strp("short label"),
		}, "slug", "url")},
	{"task-decide", "Record a decision made while executing a task — what was chosen and why. Kept as a queryable record and shown under the task's Decisions section.",
		obj(map[string]any{
			"slug":      strp("task slug"),
			"decision":  strp("the decision made"),
			"rationale": strp("why (optional)"),
		}, "slug", "decision")},
	{"task-journal", "Append a freeform note to the running log, optionally against a task.",
		obj(map[string]any{"slug": strp("task slug; omit for a project-level note"), "note": strp("the note")}, "note")},
	{"work-find", "Search tasks, decisions, and the journal — for finding what was done and decided, not just what's open.",
		obj(map[string]any{
			"query":         strp("free text matched against titles, bodies, decisions, journal, and link URLs"),
			"status":        enumP("restrict matched tasks to this status", statusEnum...),
			"since":         strp("RFC3339 lower bound on decision/journal timestamps"),
			"has_decisions": boolP("only tasks that carry a decision"),
		})},
	{"session-summary", "Close the current session with a summary of what it accomplished, for the next session to read.",
		obj(map[string]any{"summary": strp("what this session did")}, "summary")},
}

// --- dispatch --------------------------------------------------------------

func (s *Server) toolsCall(params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	st, ok := s.store()
	if !ok {
		return errorResult(noDBMessage), nil
	}
	h, ok := handlers[p.Name]
	if !ok {
		return errorResult("unknown tool: " + p.Name), nil
	}
	blocks, err := h(st, p.Arguments)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	return textResult(blocks, false), nil
}

func textResult(blocks []string, isError bool) any {
	content := make([]map[string]any, len(blocks))
	for i, b := range blocks {
		content[i] = map[string]any{"type": "text", "text": b}
	}
	return map[string]any{"content": content, "isError": isError}
}

func errorResult(msg string) any { return textResult([]string{msg}, true) }

// js pretty-prints a value as a single JSON text block.
func js(v any) []string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return []string{fmt.Sprintf("marshal error: %v", err)}
	}
	return []string{string(b)}
}

type handler func(*store.Store, json.RawMessage) ([]string, error)

func parse(args json.RawMessage, v any) error {
	if len(args) == 0 {
		return nil
	}
	return json.Unmarshal(args, v)
}

var handlers = map[string]handler{
	"task-list": func(st *store.Store, a json.RawMessage) ([]string, error) {
		var in struct {
			Status        string `json:"status"`
			Parent        string `json:"parent"`
			IncludeClosed bool   `json:"include_closed"`
		}
		if err := parse(a, &in); err != nil {
			return nil, err
		}
		out, err := st.ListTasks(store.ListOpts{Status: in.Status, Parent: in.Parent, IncludeClosed: in.IncludeClosed})
		if err != nil {
			return nil, err
		}
		return js(out), nil
	},
	"task-get": func(st *store.Store, a json.RawMessage) ([]string, error) {
		var in struct {
			Slug string `json:"slug"`
		}
		if err := parse(a, &in); err != nil {
			return nil, err
		}
		d, err := st.Detail(in.Slug)
		if err != nil {
			return nil, err
		}
		return append([]string{d.Markdown()}, js(d)...), nil
	},
	"task-next": func(st *store.Store, a json.RawMessage) ([]string, error) {
		n, err := st.NextTask()
		if err != nil {
			return nil, err
		}
		if n == nil {
			return []string{"No actionable task — everything is blocked, done, or there are no tasks."}, nil
		}
		return js(n), nil
	},
	"task-tree": func(st *store.Store, a json.RawMessage) ([]string, error) {
		var in struct {
			Slug string `json:"slug"`
		}
		if err := parse(a, &in); err != nil {
			return nil, err
		}
		t, err := st.Tree(in.Slug)
		if err != nil {
			return nil, err
		}
		return js(t), nil
	},
	"task-create": func(st *store.Store, a json.RawMessage) ([]string, error) {
		var in struct {
			Title     string   `json:"title"`
			Slug      string   `json:"slug"`
			Parent    string   `json:"parent"`
			Body      string   `json:"body"`
			Status    string   `json:"status"`
			Priority  int      `json:"priority"`
			BlockedBy []string `json:"blocked_by"`
		}
		if err := parse(a, &in); err != nil {
			return nil, err
		}
		t, err := st.CreateTask(store.CreateTaskInput{
			Title: in.Title, Slug: in.Slug, Parent: in.Parent, Body: in.Body,
			Status: in.Status, Priority: in.Priority, BlockedBy: in.BlockedBy,
		})
		if err != nil {
			return nil, err
		}
		return js(t), nil
	},
	"task-update": func(st *store.Store, a json.RawMessage) ([]string, error) {
		var in struct {
			Slug     string  `json:"slug"`
			NewSlug  *string `json:"new_slug"`
			Title    *string `json:"title"`
			Status   *string `json:"status"`
			Priority *int    `json:"priority"`
			Position *int    `json:"position"`
			Parent   *string `json:"parent"`
			Body     *string `json:"body"`
		}
		if err := parse(a, &in); err != nil {
			return nil, err
		}
		t, err := st.UpdateTask(store.UpdateTaskInput{
			Slug: in.Slug, NewSlug: in.NewSlug, Title: in.Title, Status: in.Status, Priority: in.Priority,
			Position: in.Position, Parent: in.Parent, Body: in.Body,
		})
		if err != nil {
			return nil, err
		}
		return js(t), nil
	},
	"task-add-blocker": func(st *store.Store, a json.RawMessage) ([]string, error) {
		var in struct {
			Slug      string   `json:"slug"`
			BlockedBy []string `json:"blocked_by"`
		}
		if err := parse(a, &in); err != nil {
			return nil, err
		}
		for _, b := range in.BlockedBy {
			if err := st.AddDep(in.Slug, b); err != nil {
				return nil, err
			}
		}
		d, err := st.Detail(in.Slug)
		if err != nil {
			return nil, err
		}
		return js(d), nil
	},
	"task-remove-blocker": func(st *store.Store, a json.RawMessage) ([]string, error) {
		var in struct {
			Slug      string   `json:"slug"`
			BlockedBy []string `json:"blocked_by"`
		}
		if err := parse(a, &in); err != nil {
			return nil, err
		}
		for _, b := range in.BlockedBy {
			if err := st.RemoveDep(in.Slug, b); err != nil {
				return nil, err
			}
		}
		d, err := st.Detail(in.Slug)
		if err != nil {
			return nil, err
		}
		return js(d), nil
	},
	"task-toc": func(st *store.Store, a json.RawMessage) ([]string, error) {
		var in struct {
			Slug string `json:"slug"`
		}
		if err := parse(a, &in); err != nil {
			return nil, err
		}
		toc, err := st.TOC(in.Slug)
		if err != nil {
			return nil, err
		}
		return js(toc), nil
	},
	"task-section-get": func(st *store.Store, a json.RawMessage) ([]string, error) {
		var in struct {
			Slug string `json:"slug"`
			Path string `json:"path"`
		}
		if err := parse(a, &in); err != nil {
			return nil, err
		}
		content, err := st.GetSection(in.Slug, in.Path)
		if err != nil {
			return nil, err
		}
		return []string{content}, nil
	},
	"task-section-set": func(st *store.Store, a json.RawMessage) ([]string, error) {
		var in struct {
			Slug    string `json:"slug"`
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := parse(a, &in); err != nil {
			return nil, err
		}
		t, err := st.SetSection(in.Slug, in.Path, in.Content)
		if err != nil {
			return nil, err
		}
		return js(t), nil
	},
	"task-link": func(st *store.Store, a json.RawMessage) ([]string, error) {
		var in struct {
			Slug  string `json:"slug"`
			URL   string `json:"url"`
			Kind  string `json:"kind"`
			Label string `json:"label"`
		}
		if err := parse(a, &in); err != nil {
			return nil, err
		}
		l, err := st.AddLink(in.Slug, in.URL, in.Kind, in.Label)
		if err != nil {
			return nil, err
		}
		return js(l), nil
	},
	"task-decide": func(st *store.Store, a json.RawMessage) ([]string, error) {
		var in struct {
			Slug      string `json:"slug"`
			Decision  string `json:"decision"`
			Rationale string `json:"rationale"`
		}
		if err := parse(a, &in); err != nil {
			return nil, err
		}
		d, err := st.AddDecision(in.Slug, in.Decision, in.Rationale)
		if err != nil {
			return nil, err
		}
		return js(d), nil
	},
	"task-journal": func(st *store.Store, a json.RawMessage) ([]string, error) {
		var in struct {
			Slug string `json:"slug"`
			Note string `json:"note"`
		}
		if err := parse(a, &in); err != nil {
			return nil, err
		}
		e, err := st.AddJournal(in.Slug, in.Note)
		if err != nil {
			return nil, err
		}
		return js(e), nil
	},
	"work-find": func(st *store.Store, a json.RawMessage) ([]string, error) {
		var in struct {
			Query        string `json:"query"`
			Status       string `json:"status"`
			Since        string `json:"since"`
			HasDecisions bool   `json:"has_decisions"`
		}
		if err := parse(a, &in); err != nil {
			return nil, err
		}
		res, err := st.Find(store.FindOpts{Query: in.Query, Status: in.Status, Since: in.Since, HasDecisions: in.HasDecisions})
		if err != nil {
			return nil, err
		}
		return js(res), nil
	},
	"session-summary": func(st *store.Store, a json.RawMessage) ([]string, error) {
		var in struct {
			Summary string `json:"summary"`
		}
		if err := parse(a, &in); err != nil {
			return nil, err
		}
		id := st.CurrentSessionID()
		if id == 0 {
			return []string{"No active session."}, nil
		}
		se, err := st.EndSession(id, in.Summary)
		if err != nil {
			return nil, err
		}
		return js(se), nil
	},
}
