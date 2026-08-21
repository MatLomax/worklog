#!/usr/bin/env bash
# Claude Code Stop hook: close the current worklog session when the agent stops.
# The session's *summary* is written by the agent itself via the session-summary
# MCP tool during the session; this hook only stamps the end time. Harmless when
# the MCP server already closed the session on disconnect.
worklog session-end >/dev/null 2>&1 || true
