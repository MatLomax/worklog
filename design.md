# worklog — design

## Problem

AI coding agents lose the thread across sessions. In-session task lists are
ephemeral; global memory holds *facts*, not *work-state*; prose notes
(`decisions.md`, `lessons.md`) are durable and human-readable but not queryable.
The gap is **structured, durable, cross-session work-state**: what's open,
what's blocked, what the last session left half-done, and — the part prose can't
answer — what was decided and done, retrievable later.

worklog is the record that sits between the ephemeral task list and the prose
notes. It is the agent's working record, not a document for humans to maintain.

## Lineage

The task model is adapted from [solstice](../solstice)'s CouchDB task system —
its tree, blocking edges, priority/position ordering, and section-addressable
markdown bodies are proven and worth keeping. worklog reuses that **design**,
reimplemented on SQLite, and deliberately drops solstice's milestones,
workstreams, semver sorting, and orchestrator coupling — none of which serve a
single developer tracking local work. It **adds** two things solstice lacks:
first-class **links** (GitHub issues/PRs by full URL) and first-class
**decisions**, plus a per-session **journal**.

An earlier take (the JJO migration tracker) put this kind of tracker on a shared
Postgres instance. worklog is the opposite choice on purpose: **local,
single-writer-ish, zero-install**, one file per project.

## Stack

- **Go**, compiled to a single static binary per OS. Cross-compiles to
  `linux/amd64`, `windows/amd64`, `darwin/arm64` with `CGO_ENABLED=0` and no
  per-platform toolchain.
- **`modernc.org/sqlite`** — a pure-Go SQLite (no cgo), so the binary needs
  nothing installed on the target machine. This is what makes "drop it on any
  Windows or Linux box" true.
- **MCP over stdio** (newline-delimited JSON-RPC 2.0) — the one surface every
  client speaks (Claude Code, Codex, opencode), rather than a per-tool plugin.

## Data model

`.solstice/work.db`, WAL-mode so concurrent sessions sharing a tree don't
clobber each other. Six tables:

- **task** — `id, slug, parent_id, title, body_md, status, priority, position,
  created_at, updated_at, closed_at`. `parent_id` gives the tree; `slug` is the
  stable external handle. Status ∈ `pending, in_progress, blocked, done,
  dropped` (aligned with the harness's own task vocabulary).
- **dep** — `(task_id, blocked_by_id)` blocking edges.
- **link** — a task's external artifacts. `kind ∈ github_issue, github_pr,
  commit, file, url`, auto-detected from the URL, storing the *full* URL so
  "which task touched issue #412" is a lookup, not a grep.
- **decision** — `task_id, session_id, ts, decision_md, rationale_md`. A
  first-class row (queryable across tasks) that is also *rendered* as a
  `## Decisions` section when a task is shown.
- **session** — one agent working session; the unit decisions and journal
  entries are attributed to.
- **journal** — `task_id?, session_id?, ts, kind, text_md`. The running log
  (`note, status_change, link_added, decision, created`) a fresh session reads
  to reconstruct where the last one stopped.

Links and decisions are **rows, not text buried in the body** — the same mistake
the old migration spreadsheet made, where branch names and URLs lived in a Notes
blob and had to be regex'd back out. Rows are queryable and timestamped; the
markdown rendering is derived from them, never the source of truth.

## Semantics worth stating

- **Blocking (from solstice):** a task is actively blocked if it has a
  dependency that is not yet closed, or an explicit `blocked` status. Closing a
  dependency (`done`/`dropped`) automatically unblocks its dependents — a closed
  blocker is omitted from the dependent's blocker list.
- **Actionable order:** unblocked before blocked, then priority (1 highest),
  then sibling position, then id. `task-next` returns the first actionable
  (`pending`/`in_progress`, no active blockers) task.
- **Sections:** a task body is markdown; headings form a tree, and a section is
  addressable by its slash-joined heading path (`Design/Storage`). Editing a
  section replaces its content *and subsections*, leaving the rest of the body
  untouched, so an agent can update one part without rewriting the whole body.
- **Sessions:** created on the MCP `initialize` handshake and closed on client
  disconnect, so session boundaries are correct with no hook required. The agent
  may record an end-of-session summary via `session-summary`.
- **Attach-only serve:** `worklog serve` opens an existing database but never
  creates one — creation is `worklog init`'s job alone. This is what keeps a
  globally-installed server (see Distribution) inert in every project until it
  is explicitly initialized, instead of seeding a stray `.solstice/work.db` into
  every project a session opens. The server attaches lazily (per tool call), so
  running `init` mid-session lights up the tools without a restart. Under a
  plugin the project is resolved from `$CLAUDE_PROJECT_DIR`, since a plugin MCP
  server's working directory is not guaranteed to be the project root.

## Cross-session flow

1. **SessionStart** (Claude Code hook) runs `worklog context`, injecting open +
   in-progress tasks, their blockers, and the recent journal tail — the session
   opens warm. (Codex: call `task-next`/`task-list`, or point `AGENTS.md` at
   `worklog context`.)
2. **During the session** the agent reads/writes via the MCP tools; decisions
   and journal entries are attributed to the session automatically.
3. **On stop** the session closes (on disconnect, and/or via the Stop hook),
   optionally with an agent-written summary the next session will read.

## Distribution

This repo is the **client-agnostic tool** — the Go binary and its tests. The
Claude Code integration ships separately as a **plugin** in the
[`MatLomax/claude-skills`](https://github.com/MatLomax/claude-skills)
marketplace (`plugins/worklog/`), bundling the three integration pieces — the
MCP server (`.mcp.json`), the SessionStart/Stop hooks (`hooks/hooks.json`), and
a `/worklog:init` command — so one `/plugin install` replaces all the manual
wiring. The plugin is deliberately **glue only**: it expects the `worklog`
binary on PATH rather than bundling per-platform binaries, which keeps packaging
trivial and cross-platform-clean at the cost of one install step (`go install`
or a released binary). Keeping the tool in its own repo (not folded into the
skills marketplace) matches its identity: it also serves Codex, so it is not a
Claude-only artifact. For manual (non-plugin) use, this repo's `hooks/*.sh` and
`*.ps1` wrappers wire the same commands into a plain `settings.json`.

Codex CLI can't consume a Claude plugin, so it keeps the manual
`~/.codex/config.toml` MCP block. Both clients drive the same attach-only
server; Codex gets correct session boundaries for free because the server closes
its session on disconnect, no hook required.

## Non-goals

Not multi-user, not networked, not a shared source of truth. Not a replacement
for the human-facing `.solstice/*.md` notes — those stay. Binary databases are
not committed; the durable narrative for humans lives in the markdown, and
worklog is the agent's rebuildable working record.
