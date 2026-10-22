package app

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/hvoguetech/ros-claude-plugin/cli/internal/config"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/gitx"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/link"
)

// ProjectHeader is how `/v1/mcp/me` learns the repo's project (spec §12).
const ProjectHeader = "X-ROS-Project"

// headersBudget keeps mcp-headers well inside Claude Code's 10 s headersHelper timeout, even when it has to
// refresh the token.
const headersBudget = 6 * time.Second

// resolveProjectDir picks the project dir for mcp-headers. The plugin passes
// `--project-dir "${CLAUDE_PROJECT_DIR}"`, which Claude Code substitutes (checked against Claude Code
// 2.1.303; spec §18a left it open). If it ever arrives empty or as the literal placeholder, fall back to
// $CLAUDE_PROJECT_DIR. Never to the working directory: Claude Code runs the helper in the plugin's own
// directory, not the project. "" means unknown, and the header is left out.
func resolveProjectDir(flagVal string, getenv func(string) string) string {
	for _, v := range []string{flagVal, getenv("CLAUDE_PROJECT_DIR")} {
		if v = strings.TrimSpace(v); v != "" && !strings.Contains(v, "$") {
			return v
		}
	}
	return ""
}

// mcpHeaders prints `{"Authorization": "Bearer …", "X-ROS-Project": "…"}` for Claude Code's
// headersHelper. Claude Code runs it on every MCP connection and again after a 401/403.
func (a *App) mcpHeaders(args []string) int {
	fs := a.flags("mcp-headers")
	projectDir := fs.String("project-dir", "", "the Claude Code project dir")
	if fs.Parse(args) != nil {
		return 2
	}
	if a.HTTPTimeout > headersBudget-time.Second {
		a.HTTPTimeout = headersBudget - time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), headersBudget)
	defer cancel()
	cfg, err := config.Load()
	if err != nil {
		return a.errorf("reading config: %v", err)
	}
	token, err := a.session(cfg).AccessToken(ctx)
	if err != nil {
		return a.errorf("%v", err)
	}
	headers := map[string]string{"Authorization": "Bearer " + token}
	if dir := resolveProjectDir(*projectDir, a.Getenv); dir != "" {
		if repo, err := gitx.Find(dir); err == nil {
			// A stale link (ROS refused its project) is left out so the server falls back, instead of
			// every tool call failing on project_unavailable; SessionStart keeps telling the developer.
			if l, _ := link.Read(repo); l != nil && !l.Stale {
				headers[ProjectHeader] = l.ProjectID
			}
		}
	}
	// No usable link: the header is left out and the server resolves the project itself (§12).
	enc := json.NewEncoder(a.Stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(headers); err != nil {
		return a.errorf("%v", err)
	}
	return 0
}
