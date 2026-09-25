# worklog

[![CI](https://github.com/MatLomax/worklog/actions/workflows/ci.yml/badge.svg)](https://github.com/MatLomax/worklog/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/MatLomax/worklog)](https://goreportcard.com/report/github.com/MatLomax/worklog)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A per-project, SQLite-backed **task and decision log for AI coding agents**,
exposed over MCP so work is remembered across sessions. One static binary, no
runtime, no server to run, no cloud — the database is a single file in your
project's `.worklog/` directory.

It tracks not just *what needs doing* but *what was done and why*: every task
carries its blocking dependencies, the GitHub issues/PRs it touches, the
decisions made while executing it, and a running journal — so a fresh session
can answer "where was I, and why did we do it this way?"

## Why it exists

In-session task lists die when the session ends; prose notes aren't queryable.
worklog fills the gap between them: structured, durable, cross-session
work-state the agent reads on start and writes as it goes.

## Install

```sh
go install github.com/MatLomax/worklog/cmd/worklog@latest   # needs Go 1.27+
```

Or download a prebuilt binary from the [releases](https://github.com/MatLomax/worklog/releases),
or build from source (`make build`). Put `worklog` (or `worklog.exe`) on your
PATH. It's a single static, dependency-free binary for Linux, Windows, and
macOS — SQLite is compiled in (pure-Go `modernc.org/sqlite`, no cgo).

## Per-project setup

```sh
cd your-project
worklog init          # creates ./.worklog/tasks.db (idempotent)
```

The database is resolved as `--db` / `$WORKLOG_DB` / `$CLAUDE_PROJECT_DIR` /
current directory — walking up to the nearest ancestor `.worklog/`, else
`./.worklog/tasks.db`. It's local, gitignored working state — not committed.

The MCP server is **attach-only**: it opens an existing database but never
creates one. So a globally-installed worklog stays inert in any project until
`worklog init` (or `/worklog:init`) has been run there — it won't seed a
database into every project you open.

## Configuring statuses

With no config, a task's status is one of `pending` (the default),
`in_progress`, `blocked`, `done`, or `dropped`. To change that list, add an
optional `.worklog/config.jsonc` beside `tasks.db`. worklog writes no ignore
rules for it, so whether it is committed is the project's choice. The format is
JSON with `//` and `/* */` comments and trailing commas allowed (LF, CRLF and
CR line endings all work). `statuses` replaces the whole list, in the order
given; this example adds a `review` status to the defaults:

```jsonc
{
  "statuses": [
    { "name": "pending",     "kind": "open", "default": true }, // new tasks start here
    { "name": "in_progress", "kind": "active" },
    { "name": "review",      "kind": "active" },  // awaiting review; still actionable
    { "name": "blocked",     "kind": "blocked" },
    { "name": "done",        "kind": "closed" },
    { "name": "dropped",     "kind": "closed" },
  ],
}
```

A status's `kind` decides how the rest of worklog treats a task holding it:

| Kind | Meaning |
|---|---|
| `open` | actionable, not started |
| `active` | actionable, underway |
| `blocked` | a manual hold; not actionable |
| `closed` | terminal: stamps `closed_at` (on create and update), unblocks dependents, hidden from default lists |

The config must satisfy these rules. An error names the file and points at the
offending value as `path:line:col` (columns count Unicode code points); a rule
about a missing field points at its status entry, and a list-wide rule at the
`statuses` array:

- each name matches `^[a-z][a-z0-9]*(_[a-z0-9]+)*$` (lowercase letters and
  digits, words joined by single underscores, starting with a letter) and is
  unique;
- exactly one status has `"default": true`, and it is of kind `open` or `active`;
- at least one status is `open` or `active`, and at least one is `closed`;
- an `open` or `active` status may not be named so its briefing heading reads
  "Blocked" or "Recent activity" (`blocked` and `closed` statuses get no
  section of their own, so any name is fine for them);
- keys are exact-case (`statuses`, `name`, `kind`, `default`), unknown or
  duplicate keys are rejected, and no value may be `null`. A missing file, or
  `{}`, means the defaults; a file that exists but holds no JSON value (empty,
  or only comments or a byte-order mark) is an error. A file that lists the
  default statuses explicitly counts as a loaded config, not as the defaults.

The session briefing (`worklog session-start` / `worklog context`) lists open
work in one section per `active` status, then one per `open` status (in config
order), then Blocked. A heading is the status name with `_` as a space and the
first letter capitalised (`in_progress` → "In progress").

The MCP server re-reads the config before each `tools/list` and tool call, and
switches statuses only when it reads a complete, valid file — so an edit
applies to the next call without a restart, and a file caught mid-save never
changes the statuses in force. While the file is invalid, empty or unreadable,
the server keeps the statuses in force (the last valid config, or the defaults
if none was loaded) and adds a note to each tool result naming the error. If a
file whose `statuses` list was loaded disappears, those statuses stay in force,
with a note saying so, until the file is recreated or the server restarts;
while the defaults are in force (no file, or `{}`) a missing file simply means
the defaults. A read error never counts as
the file having been there. Only a server that has never had usable statuses
(the file was invalid or unreadable when it started) refuses tool calls, until
the file can be read and is valid. If a read has ever found the file (even
invalid) and it is then deleted, calls stay refused until it is recreated or
the server restarts; if no read ever found it (it was only unreadable), a
missing file just means the defaults. The tool schemas (status enums, and the
descriptions naming which statuses are closed and actionable) are built from
the statuses in force; clients usually cache the tool list, so a changed enum
shows up from the next session, while the checks apply at once.

An invalid config never stops session bookkeeping: `worklog session-start`
shows the error in place of the briefing, `worklog session-end` and
`worklog init` carry on (init warns), and `worklog context` fails with it.

**Removing a status** leaves tasks that already hold it untouched: they keep
it and show it as saved. Such a status counts as not closed (the task still
blocks its dependents and still appears in lists) and not actionable, and it
can't be set on create or update. The `task-list` / `work-find` status filters
accept any well-formed name, so these tasks stay reachable, and the briefing
gives each leftover status its own section after Blocked, sorted by name and
headed with a `(not in config)` suffix (e.g. "Review (not in config)"). In
ordering, a leftover-status task sorts with the blocked ones.

## Wiring it to an agent

### Claude Code (plugin)

worklog ships as a Claude Code **plugin** — distributed through the
[`MatLomax/claude-plugins`](https://github.com/MatLomax/claude-plugins)
marketplace — that wires up the MCP server, the warm-load/session hooks, and a
`/worklog:init` command in one install. The plugin is pure glue: it expects the
`worklog` binary on your PATH (see Install).

```
/plugin marketplace add MatLomax/claude-plugins
/plugin install worklog@matlomax --scope project
```

Then, in each project you want tracked:

```
/worklog:init          # creates .worklog/tasks.db (idempotent)
```

Because the server is attach-only, the globally-installed plugin does nothing in
a project until `/worklog:init` runs there. After it does, the SessionStart hook
warm-loads open work (into the model's context) and shows the next task as a
visible line to you, the Stop hook closes the session, and the worklog MCP tools
are live.

<details><summary>Manual setup (without the plugin)</summary>

```sh
claude mcp add worklog -- worklog serve
```

Hooks in your settings:

```json
{
  "hooks": {
    "SessionStart": [{ "hooks": [{ "type": "command", "command": "worklog session-start" }] }],
    "Stop":         [{ "hooks": [{ "type": "command", "command": "worklog session-end" }] }]
  }
}
```

`worklog session-start` emits one SessionStart payload over both of the hook's
channels: `additionalContext` feeds the full briefing into the model's context
(invisible to you), and `systemMessage` shows the next task as a visible line to
you. It is safe to wire globally — it prints nothing until `worklog init` has run
in a project. Ready-made hook scripts (bash and PowerShell) for both events are
in `hooks/`.
</details>

### Codex CLI

Codex is an MCP client too. Add to `~/.codex/config.toml`:

```toml
[mcp_servers.worklog]
command = "worklog"
args = ["serve"]
```

All tools work identically. Session boundaries are handled automatically — the
server closes its session when Codex disconnects, so no Stop hook is needed.
Codex has no SessionStart-style context hook, so to warm-load either call
`task-next` yourself at the start or add to your `AGENTS.md`:

> At the start of a task, run `worklog context` (or call the `task-next` tool) to
> see open work.

## Tools

Status enums and descriptions in the tool schemas follow the project's
[status config](#configuring-statuses).

| Tool | Purpose |
|---|---|
| `task-list` | tasks in actionable order (unblocked first, then priority); filter by status, parent, closed |
| `task-get` | full detail: body, blockers, subtasks, links, decisions, journal |
| `task-next` | the single highest-priority actionable task |
| `task-tree` | the task forest / a subtree, nested |
| `task-create` | create a task, optionally under a parent, with blockers; status defaults to the configured default |
| `task-update` | change slug/status/priority/parent/body; status must be a [configured](#configuring-statuses) one, and a closed status unblocks dependents |
| `task-add-blocker` / `task-remove-blocker` | add or remove blocking deps on an existing task |
| `task-toc` / `task-section-get` / `task-section-set` | read/edit one `##` section of a body |
| `task-section-append` | append a block to the end of a section (after its subsections) |
| `task-section-insert` | insert a new section relative to a heading path (or the document) |
| `task-section-delete` | delete a section and its subsections |
| `task-section-move` | move a section (with subsections) before/after another heading |
| `task-body-replace` | replace an exact, unique substring anywhere in a body |
| `task-link` | attach a GitHub issue/PR (full URL), commit, file, or URL |
| `task-decide` | record a decision + rationale made during execution |
| `task-journal` | append a freeform note to the running log |
| `record-edit` | correct a decision/journal field in place, silently |
| `record-delete` | delete a decision or journal entry by id |
| `work-find` | search tasks, decisions, and journal — find what was *done* |
| `session-summary` | close the session with a summary for the next one |

### Referencing tasks in a body

Refer to another task from within a task body by its slug, in either a wikilink
`[[some-slug]]` or a code span `` `some-slug` ``. Renaming a task with
`task-update`'s `new_slug` rewrites both forms of the old slug across every task
body in the same operation, so cross-references never dangle. The exact
delimited token is matched, so renaming `build-it` never touches `[[build-it-2]]`
or the bare word `build-it` in prose.

## CLI

```
worklog serve       [--db PATH]        run the MCP server over stdio
worklog init        [--dir DIR]        create .worklog/tasks.db
worklog context     [--db PATH] [-n N] print the "where was I" briefing (plain text; Codex / debugging)
worklog session-start [--db PATH] [-n N] SessionStart hook output: warm-load the model + show the next task
worklog session-end [--summary S]      close the current session
worklog update      [--check|--auto|--force] update the binary in place from the latest release
worklog version
```

`worklog update` self-updates the on-PATH binary from the latest GitHub release
(SHA-256 verified, atomic in-place swap). It follows the shared self-update
contract in [`MatLomax/claude-plugins` `CONVENTIONS.md`](https://github.com/MatLomax/claude-plugins/blob/main/CONVENTIONS.md);
the worklog specifics are the `worklog-<goos>-<goarch>[.exe]` release asset and a
version taken from the build stamp, falling back to the Go build-info module
version.

The commands that read the project read its [status config](#configuring-statuses)
too; an invalid config makes `context` fail, while `session-start`, `session-end`
and `init` carry on (see that section).

See [design.md](design.md) for the data model and the reasoning behind it.

## License

[MIT](LICENSE) © Mathieu Lomax
