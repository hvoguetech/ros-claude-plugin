#!/usr/bin/env python3
"""Repo-specific checks on the marketplace and the `ros` plugin, beyond `claude plugin validate`.

The invariants here come from the design (agent-studio-backend: docs/design/local-agent-connector.md):
- the marketplace and plugin versions agree;
- the MCP server is `/v1/mcp/me` over HTTP, with `ros-local mcp-headers` as its headersHelper;
- hooks only call the bundled launcher (`bin/ros-local`), which is executable and never downloads anything;
- the SessionEnd hook has no long timeout (it must fit Claude Code's ~1.5 s budget by detaching);
- skills have front matter with a name and description.
Exit status is non-zero on any failure.
"""

from __future__ import annotations

import json
import os
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
errors: list[str] = []


def fail(msg: str) -> None:
    errors.append(msg)


def load(path: Path) -> dict:
    try:
        return json.loads(path.read_text())
    except Exception as e:  # noqa: BLE001
        fail(f"{path.relative_to(ROOT)}: not valid JSON ({e})")
        return {}


market = load(ROOT / ".claude-plugin" / "marketplace.json")
entries = {p.get("name"): p for p in market.get("plugins", [])}
if market.get("name") != "ros" or "ros" not in entries:
    fail("marketplace must be named 'ros' and list a plugin 'ros' (install id ros@ros)")
entry = entries.get("ros", {})
plugin_dir = (ROOT / entry.get("source", "")).resolve()
if not (plugin_dir / ".claude-plugin" / "plugin.json").is_file():
    fail(f"plugin source {entry.get('source')!r} has no .claude-plugin/plugin.json")
else:
    manifest = load(plugin_dir / ".claude-plugin" / "plugin.json")
    if manifest.get("name") != "ros":
        fail("plugin.json name must be 'ros' (gives the /ros:* command namespace)")
    if entry.get("version") and entry.get("version") != manifest.get("version"):
        fail(f"version mismatch: marketplace {entry.get('version')} vs plugin.json {manifest.get('version')}")

    mcp = load(plugin_dir / ".mcp.json").get("mcpServers", {}).get("ros", {})
    if mcp.get("type") != "http" or not str(mcp.get("url", "")).endswith("/v1/mcp/me"):
        fail(".mcp.json: server 'ros' must be type http at …/v1/mcp/me")
    helper = str(mcp.get("headersHelper", ""))
    if "${CLAUDE_PLUGIN_ROOT}/bin/ros-local" not in helper or "mcp-headers" not in helper \
            or "${CLAUDE_PROJECT_DIR}" not in helper:
        fail(".mcp.json: headersHelper must run ${CLAUDE_PLUGIN_ROOT}/bin/ros-local mcp-headers --project-dir ${CLAUDE_PROJECT_DIR}")
    if re.search(r"(?i)bearer|token|secret|password", json.dumps(mcp.get("headers", {}))):
        fail(".mcp.json: no static credentials in headers")

    hooks = load(plugin_dir / "hooks" / "hooks.json").get("hooks", {})
    for event in ("SessionStart", "SessionEnd"):
        if event not in hooks:
            fail(f"hooks.json: missing {event}")
    for event, groups in hooks.items():
        for group in groups:
            for h in group.get("hooks", []):
                cmd = h.get("command", "")
                if not cmd.startswith('"${CLAUDE_PLUGIN_ROOT}/bin/ros-local" hook '):
                    fail(f"hooks.json {event}: hooks must only call the bundled launcher, got {cmd!r}")
                if re.search(r"\b(curl|wget|bash -c|sh -c)\b", cmd):
                    fail(f"hooks.json {event}: hooks must never download or execute anything else")
                if event == "SessionEnd" and h.get("timeout", 0) > 5:
                    fail("hooks.json SessionEnd: keep the default timeout; ros-local detaches instead")

    launcher = plugin_dir / "bin" / "ros-local"
    if not launcher.is_file() or not os.access(launcher, os.X_OK):
        fail("bin/ros-local must exist and be executable")
    elif re.search(r"\b(curl|wget)\b", launcher.read_text()):
        fail("bin/ros-local must never download anything")

    for skill in sorted((plugin_dir / "skills").glob("*/SKILL.md")):
        text = skill.read_text()
        m = re.match(r"^---\n(.*?)\n---\n", text, re.S)
        if not m or not re.search(r"^name:\s*\S", m.group(1), re.M) or not re.search(r"^description:\s*\S", m.group(1), re.M):
            fail(f"{skill.relative_to(ROOT)}: needs front matter with name and description")

if errors:
    for e in errors:
        print(f"check_plugin: {e}", file=sys.stderr)
    sys.exit(1)
print("check_plugin: ok")
