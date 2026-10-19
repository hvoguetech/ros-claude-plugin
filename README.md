# ROS for Claude Code

The `ros` Claude Code plugin and its helper CLI, `ros-local`. Together they connect your local Claude Code
sessions to ROS:

- **Memory.** Each session starts with what ROS remembers about you, the project and this repo. The
  `ros` MCP tools (`recall_memory`, `remember`, `ask_team_memory`, `search_knowledge_base`,
  `search_past_runs`) are available throughout the session.
- **Skills.** The project's approved skills are synced into `.claude/skills/ros-*` in the repo. They are
  git-excluded and never committed.
- **Identity.** Sign-in uses a device code. The refresh token stays in your OS keychain. Connector
  credentials never leave ROS.

Local sessions are *assisted, not governed*. The plugin runs on your machine and you can turn it off.
Enforced guarantees stay on the ROS devbox.

## Install

1. Install the helper CLI. Use Homebrew (macOS, Linux):

   ```bash
   brew install hvoguetech/tap/ros-local
   ```

   Or use the install script. It verifies the SHA-256 checksum, and also the cosign signature when
   `cosign` is installed:

   ```bash
   curl -fsSL https://raw.githubusercontent.com/hvoguetech/ros-claude-plugin/main/install.sh | sh
   ```

2. Add the marketplace and install the plugin:

   ```bash
   claude plugin marketplace add hvoguetech/ros-claude-plugin
   claude plugin install ros@ros
   ```

   Claude Code doesn't auto-update third-party marketplaces by default. Turn on auto-update for the
   `ros` marketplace in `/plugin` → Marketplaces.

3. Sign in:

   ```bash
   ros-local login                                   # production
   ros-local login --url https://ros.example.com     # self-hosted or staging
   ```

   If you sign in to a URL other than production, also export `ROS_URL` with the same value wherever
   you start Claude Code, so the plugin's MCP server points there too:

   ```bash
   export ROS_URL=https://ros.example.com
   ```

4. In each repo, link it once. This is optional: the first session links it automatically.

   ```bash
   cd ~/src/payments-api
   ros-local link            # matches the git remote to a ROS project; --project <id> to choose
   ```

   Linking writes `.git/ros.json`, creates `.claude/skills/`, and adds the ROS skill paths to
   `.git/info/exclude`.

## Every session

```text
ROS · project "Payments" · 14 skills (synced 2m ago) · memory: personal_agent · recalled 5
```

- `/ros:status` shows sign-in, link and sync state.
- `/ros:sync` syncs the project's skills now.
- If the banner says `ROS skills installed — run /reload-skills`, run `/reload-skills` once.
- If it says `ROS: signed out — run ros-local login`, do that.

**Synced skills** live in `.claude/skills/ros-<name>/`. `ros-local` only writes and removes the
directories it lists in `.claude/skills/.ros-manifest.json`. Your own skills are never touched. If you
edit a synced skill, the next sync warns you. When ROS publishes a new version, your edited copy is moved
to `.claude/skills/.ros-backup/` before it is replaced.

## `ros-local` commands

| Command | What it does |
|---|---|
| `ros-local login [--url URL] [--project ID] [--no-browser]` | Sign in with a device code and store the refresh token in the OS keychain (or `~/.ros/credentials.json`, mode 0600, where there is no keychain). |
| `ros-local link [--project ID]` | Link the current repo to a ROS project. |
| `ros-local sync` | Sync the project's skills into this repo. |
| `ros-local status` | Show sign-in, link and sync state. |
| `ros-local logout` | Forget this device's tokens. To revoke the device everywhere, use Settings → Devices in ROS. |
| `ros-local mcp-headers --project-dir DIR` | Used by the plugin: prints the MCP auth and project headers. |
| `ros-local hook session-start` / `session-end` | Used by the plugin's hooks. |

State lives in `~/.ros/` (override with `ROS_HOME`).

## How it works

- **SessionStart** runs `ros-local hook session-start`. It opens a local session in ROS
  (`POST /v1/local/sessions`), syncs skills if the manifest changed (an ETag check), and gives Claude the
  recalled memory. The recalled memory is capped below Claude Code's 10,000-character hook limit.
- **MCP**: `.mcp.json` points at `/v1/mcp/me`. Its `headersHelper` runs `ros-local mcp-headers` on every
  connection. The helper prints a fresh access token (refreshed well before expiry) and the repo's
  project as `X-ROS-Project`.
- **SessionEnd** runs `ros-local hook session-end`. It hands the "end session" call to a detached
  process and returns immediately, because Claude Code gives this hook about 1.5 s.
- **Hooks never download or run anything else.** If `ros-local` isn't installed, the plugin's launcher
  (`plugins/ros/bin/ros-local`) prints install instructions and the session starts normally.

## Develop

```bash
cd cli
go test -race ./...          # unit tests, against an in-process fake ROS backend
go vet ./...
go build -o ros-local .
```

Plugin checks:

```bash
claude plugin validate --strict .
claude plugin validate --strict plugins/ros
python3 scripts/check_plugin.py
```

Release: push a `v<semver>` tag. `.github/workflows/release.yml` runs GoReleaser
(`.goreleaser.yaml`). It builds macOS and Linux binaries for amd64 and arm64, publishes `checksums.txt`
with a keyless cosign signature bundle, and updates the formula in `hvoguetech/homebrew-tap`. Keep the
version in `.claude-plugin/marketplace.json` and `plugins/ros/.claude-plugin/plugin.json` in step with
the tag.

To dry-run a release locally:

```bash
goreleaser release --snapshot --clean --skip=sign,publish
```
