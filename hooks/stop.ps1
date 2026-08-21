# Claude Code Stop hook (Windows/PowerShell): close the current worklog session.
# The summary is written by the agent via the session-summary MCP tool; this
# only stamps the end time.
try { worklog session-end | Out-Null } catch {}
