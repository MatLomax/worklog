#!/usr/bin/env bash
# Claude Code SessionStart hook: warm-load worklog's open work into the model's
# context AND show the next task to the user as a visible line, in one payload.
# Prints nothing when the project has no worklog database yet, so it is safe to
# enable globally. `worklog` must be on PATH.
exec worklog session-start
