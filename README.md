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

| Tool | Purpose |
|---|---|
| `task-list` | tasks in actionable order (unblocked first, then priority) |
| `task-get` | full detail: body, blockers, subtasks, links, decisions, journal |
| `task-next` | the single highest-priority actionable task |
| `task-tree` | the task forest / a subtree, nested |
| `task-create` | create a task, optionally under a parent, with blockers |
| `task-update` | change slug/status/priority/parent/body; done unblocks dependents |
| `task-add-blocker` / `task-remove-blocker` | add or remove blocking deps on an existing task |
| `task-toc` / `task-section-get` / `task-section-set` | read/edit one `##` section of a body |
| `task-link` | attach a GitHub issue/PR (full URL), commit, file, or URL |
| `task-decide` | record a decision + rationale made during execution |
| `task-journal` | append a freeform note to the running log |
| `work-find` | search tasks, decisions, and journal — find what was *done* |
| `session-summary` | close the session with a summary for the next one |

## CLI

```
worklog serve       [--db PATH]        run the MCP server over stdio
worklog init        [--dir DIR]        create .worklog/tasks.db
worklog context     [--db PATH] [-n N] print the "where was I" briefing (plain text; Codex / debugging)
worklog session-start [--db PATH] [-n N] SessionStart hook output: warm-load the model + show the next task
worklog session-end [--summary S]      close the current session
worklog version
```

See [design.md](design.md) for the data model and the reasoning behind it.

## License

[MIT](LICENSE) © Mathieu Lomax
