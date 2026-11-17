package app

// The model gateway (ROS docs/design/model-gateway.md): the harness sends its model requests to ROS with a
// short-lived token and the PROJECT's provider key pays. The key stays on the server; this machine only ever
// holds a token that lasts minutes.
//
//   ros-local model-token      prints a fresh token and nothing else (Claude Code's apiKeyHelper runs it)
//   ros-local gateway setup    points Claude Code at the gateway, for this repo (or --global)
//   ros-local gateway off      undoes it
//   ros-local gateway status   what is configured, and what ROS answers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hvoguetech/ros-claude-plugin/cli/internal/api"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/config"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/fsutil"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/gitx"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/link"
)

// helperTTLMillis is how long Claude Code keeps one helper answer before asking again. ROS's model token lasts
// ten minutes; five leaves every request at least five minutes of validity when it starts.
const helperTTLMillis = "300000"

// helperBudget bounds model-token: Claude Code waits on the helper before it can send anything.
const helperBudget = 8 * time.Second

const (
	keyHelper  = "apiKeyHelper"
	keyBaseURL = "ANTHROPIC_BASE_URL"
	keyTTL     = "CLAUDE_CODE_API_KEY_HELPER_TTL_MS"
	// localSettings is Claude Code's per-developer, per-project settings file (never committed).
	localSettings   = ".claude/settings.local.json"
	gatewayExclude  = ".claude/settings.local.json"
	gatewayExcludeH = "# ros-local: Claude Code local settings (model gateway; never commit)"
)

// projectFor resolves which project a repo's model calls bill to: the repo's link, else its remote (the server
// matches it), else nothing (the server falls back to the device's project).
func projectFor(ctx context.Context, dir string) (projectID, remote string) {
	repo, err := gitx.Find(dir)
	if err != nil {
		return "", ""
	}
	if l, _ := link.Read(repo); l != nil && !l.Stale {
		return l.ProjectID, ""
	}
	return "", repo.Remote(ctx)
}

// gatewayErr turns ROS's refusal into what the developer should do.
func gatewayErr(err error) string {
	var e *api.Error
	switch {
	case api.HasCode(err, api.CodeModelAccessOwn):
		return "this project uses your own model login, not ROS's model gateway. A project admin can change it in " +
			"Settings → Local agent → Model access"
	case api.HasCode(err, api.CodeNoProviderKey):
		return "this project has no usable Anthropic API key in ROS (Settings → API keys)"
	case errors.As(err, &e) && strings.Contains(e.Detail, "insufficient_scope"):
		return "this device signed in before model access existed. Run `ros-local login` again"
	}
	return err.Error()
}

// modelToken prints a model token for Claude Code's apiKeyHelper: the token, no newline, nothing else on
// stdout (Claude Code fails a helper that prints anything more).
func (a *App) modelToken(args []string) int {
	fs := a.flags("model-token")
	projectDir := fs.String("project-dir", "", "the repo whose project pays (default: $CLAUDE_PROJECT_DIR, else here)")
	if fs.Parse(args) != nil {
		return 2
	}
	if a.HTTPTimeout > helperBudget-time.Second {
		a.HTTPTimeout = helperBudget - time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), helperBudget)
	defer cancel()
	_, c, err := a.signedInClient(ctx)
	if err != nil {
		return a.errorf("model-token: %v", err)
	}
	dir := resolveProjectDir(*projectDir, a.Getenv)
	if dir == "" {
		dir = a.cwd()
	}
	pid, remote := projectFor(ctx, dir)
	tok, err := c.ModelToken(ctx, pid, remote)
	if err != nil {
		return a.errorf("model-token: %s", gatewayErr(err))
	}
	fmt.Fprint(a.Stdout, tok.Token)
	return 0
}

func (a *App) gateway(args []string) int {
	if len(args) == 0 {
		return a.gatewayStatus(nil)
	}
	switch args[0] {
	case "setup":
		return a.gatewaySetup(args[1:])
	case "off":
		return a.gatewayOff(args[1:])
	case "status":
		return a.gatewayStatus(args[1:])
	}
	return a.errorf("gateway: unknown subcommand %q (setup | off | status)", args[0])
}

// settingsTarget is the Claude Code settings file a gateway command edits, and the repo it belongs to (nil for
// the user-wide file).
func (a *App) settingsTarget(global bool) (path string, repo *gitx.Repo, err error) {
	if global {
		dir := a.Getenv("CLAUDE_CONFIG_DIR")
		if dir == "" {
			home, herr := os.UserHomeDir()
			if herr != nil {
				return "", nil, herr
			}
			dir = filepath.Join(home, ".claude")
		}
		return filepath.Join(dir, "settings.json"), nil, nil
	}
	repo, err = gitx.Find(a.cwd())
	if err != nil {
		return "", nil, fmt.Errorf("%v (run it inside a repo, or pass --global)", err)
	}
	return filepath.Join(repo.Root, localSettings), repo, nil
}

func readSettings(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && len(strings.TrimSpace(string(b))) == 0) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		return nil, fmt.Errorf("%s isn't valid JSON; fix it first (it was left untouched)", path)
	}
	return m, nil
}

func writeSettings(path string, m map[string]any) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, append(b, '\n'), 0o600)
}

// oursHelper reports whether an apiKeyHelper value is one ros-local wrote.
func oursHelper(v any) bool {
	s, _ := v.(string)
	return strings.Contains(s, "ros-local") && strings.Contains(s, "model-token")
}

func (a *App) helperCommand(repo *gitx.Repo) string {
	exe := "ros-local"
	if a.Executable != nil {
		if p, err := a.Executable(); err == nil && p != "" {
			exe = p
		}
	}
	cmd := quoteArg(exe) + " model-token"
	if repo != nil {
		cmd += " --project-dir " + quoteArg(repo.Root)
	}
	return cmd
}

func quoteArg(s string) string {
	if !strings.ContainsAny(s, " \t'\"\\$") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// excludeLocalSettings keeps the repo's Claude Code local settings out of git (it names this machine's helper).
func excludeLocalSettings(r *gitx.Repo) error {
	path := filepath.Join(r.CommonDir, "info", "exclude")
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == gatewayExclude {
			return nil
		}
	}
	out := string(existing)
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	out += gatewayExcludeH + "\n" + gatewayExclude + "\n"
	return fsutil.WriteFileAtomic(path, []byte(out), 0o644)
}

func (a *App) gatewaySetup(args []string) int {
	fs := a.flags("gateway setup")
	global := fs.Bool("global", false, "every Claude Code session on this machine, not only this repo")
	force := fs.Bool("force", false, "replace an apiKeyHelper that ros-local didn't write")
	if fs.Parse(args) != nil {
		return 2
	}
	if fs.NArg() > 0 && fs.Arg(0) != "claude-code" {
		return a.errorf("gateway setup: only claude-code is supported for now (got %q)", fs.Arg(0))
	}
	path, repo, err := a.settingsTarget(*global)
	if err != nil {
		return a.errorf("gateway setup: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, c, err := a.signedInClient(ctx)
	if err != nil {
		return a.errorf("gateway setup: %v", err)
	}
	var pid, remote string
	if repo != nil {
		pid, remote = projectFor(ctx, repo.Root)
	}
	tok, err := c.ModelToken(ctx, pid, remote) // proves it works before anything is written
	if err != nil {
		return a.errorf("gateway setup: %s", gatewayErr(err))
	}
	settings, err := readSettings(path)
	if err != nil {
		return a.errorf("gateway setup: %v", err)
	}
	if cur, ok := settings[keyHelper]; ok && cur != "" && !oursHelper(cur) && !*force {
		return a.errorf("gateway setup: %s already has an apiKeyHelper that ros-local didn't write (%v). "+
			"Remove it, or pass --force to replace it", path, cur)
	}
	env, _ := settings["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	settings[keyHelper] = a.helperCommand(repo)
	env[keyBaseURL], env[keyTTL] = tok.BaseURL, helperTTLMillis
	settings["env"] = env
	if err := writeSettings(path, settings); err != nil {
		return a.errorf("gateway setup: %v", err)
	}
	if repo != nil {
		if err := excludeLocalSettings(repo); err != nil {
			return a.errorf("gateway setup: %v", err)
		}
	}
	where := "this repo"
	if *global {
		where = "every Claude Code session on this machine"
	}
	fmt.Fprintf(a.Stdout, "Claude Code now calls models through ROS for %s (%s).\n", where, path)
	fmt.Fprintf(a.Stdout, "  Gateway: %s\n  The project's API key pays; it never reaches this machine (only a 10-minute token does).\n", tok.BaseURL)
	if len(tok.Models) > 0 {
		fmt.Fprintf(a.Stdout, "  Models this project allows: %s\n", strings.Join(tok.Models, ", "))
	}
	fmt.Fprintln(a.Stdout, "  Your saved Claude login isn't used for these sessions. In Claude Code, /status shows the gateway.\n"+
		"  Undo: ros-local gateway off"+map[bool]string{true: " --global", false: ""}[*global])
	return 0
}

func (a *App) gatewayOff(args []string) int {
	fs := a.flags("gateway off")
	global := fs.Bool("global", false, "the user-wide settings, not this repo's")
	if fs.Parse(args) != nil {
		return 2
	}
	path, _, err := a.settingsTarget(*global)
	if err != nil {
		return a.errorf("gateway off: %v", err)
	}
	settings, err := readSettings(path)
	if err != nil {
		return a.errorf("gateway off: %v", err)
	}
	if !oursHelper(settings[keyHelper]) {
		fmt.Fprintf(a.Stdout, "ROS's model gateway isn't set up in %s. Nothing changed.\n", path)
		return 0
	}
	delete(settings, keyHelper)
	if env, ok := settings["env"].(map[string]any); ok {
		delete(env, keyBaseURL)
		delete(env, keyTTL)
		if len(env) == 0 {
			delete(settings, "env")
		}
	}
	if len(settings) == 0 {
		err = os.Remove(path)
	} else {
		err = writeSettings(path, settings)
	}
	if err != nil {
		return a.errorf("gateway off: %v", err)
	}
	fmt.Fprintf(a.Stdout, "Claude Code uses your own model login again (%s).\n", path)
	return 0
}

func (a *App) gatewayStatus(args []string) int {
	if a.flags("gateway status").Parse(args) != nil {
		return 2
	}
	on := func(global bool) (string, bool) {
		path, _, err := a.settingsTarget(global)
		if err != nil {
			return "", false
		}
		s, err := readSettings(path)
		return path, err == nil && oursHelper(s[keyHelper])
	}
	repoPath, repoOn := on(false)
	globalPath, globalOn := on(true)
	switch {
	case repoOn:
		fmt.Fprintf(a.Stdout, "Model gateway: on for this repo (%s)\n", repoPath)
	case globalOn:
		fmt.Fprintf(a.Stdout, "Model gateway: on for every session (%s)\n", globalPath)
	default:
		fmt.Fprintln(a.Stdout, "Model gateway: off (Claude Code uses your own model login). `ros-local gateway setup` turns it on.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cfg, err := config.Load()
	if err != nil {
		return a.errorf("gateway status: %v", err)
	}
	token, err := a.session(cfg).AccessToken(ctx)
	if err != nil {
		fmt.Fprintf(a.Stdout, "ROS: not signed in (%v)\n", err)
		return 0
	}
	pid, remote := projectFor(ctx, a.cwd())
	tok, err := a.client(cfg.URL, token).ModelToken(ctx, pid, remote)
	if err != nil {
		fmt.Fprintf(a.Stdout, "ROS: no model access here — %s\n", gatewayErr(err))
		return 0
	}
	models := "any"
	if len(tok.Models) > 0 {
		models = strings.Join(tok.Models, ", ")
	}
	fmt.Fprintf(a.Stdout, "ROS: project %s allows model calls through %s (models: %s)\n", tok.ProjectID, tok.BaseURL, models)
	return 0
}
