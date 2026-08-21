#!/usr/bin/env bash
# Claude Code SessionStart hook: warm-load worklog's open work into the new
# session's context. Prints nothing when the project has no worklog database
# yet, so it is safe to enable globally. `worklog` must be on PATH.
exec worklog context
