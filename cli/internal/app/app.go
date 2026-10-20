// Package app implements the ros-local commands.
package app

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/hvoguetech/ros-claude-plugin/cli/internal/api"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/auth"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/config"
)

// App carries the process environment, so commands can be tested in-process.
type App struct {
	Version string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Getenv  func(string) string
	Getwd   func() (string, error)
	// HTTPTimeout bounds every backend request.
	HTTPTimeout time.Duration
	// OpenBrowser opens the sign-in page; nil disables it.
	OpenBrowser func(url string) error
	// Spawn starts a detached copy of ros-local (SessionEnd); nil runs the work inline.
	Spawn func(args ...string) error
	// LoginSleep replaces the device-flow poll wait (tests).
	LoginSleep func(ctx context.Context, d time.Duration) error
}

// New returns the App for the real process.
func New(version string) *App {
	return &App{
		Version: version, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		Getenv: os.Getenv, Getwd: os.Getwd, HTTPTimeout: 8 * time.Second,
		OpenBrowser: openBrowser, Spawn: spawnDetached,
	}
}

const usage = `ros-local: the ROS helper for the Claude Code "ros" plugin.

Usage:
  ros-local login [--url URL] [--project ID] [--no-browser]   sign in with a device code
  ros-local link [--project ID]                               link this repo to a ROS project
  ros-local sync                                              sync the project's skills now
  ros-local status                                            show sign-in, link and sync state
  ros-local logout                                            forget this device's tokens
  ros-local version

Called by the plugin (not by hand):
  ros-local mcp-headers --project-dir DIR                     MCP headersHelper
  ros-local hook session-start | session-end                  Claude Code hooks
`

// Run executes one command and returns the exit code.
func (a *App) Run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(a.Stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "login":
		return a.login(rest)
	case "logout":
		return a.logout(rest)
	case "link":
		return a.link(rest)
	case "sync":
		return a.sync(rest)
	case "status":
		return a.status(rest)
	case "mcp-headers":
		return a.mcpHeaders(rest)
	case "hook":
		return a.hook(rest)
	case "version", "--version", "-v":
		fmt.Fprintf(a.Stdout, "ros-local %s (%s/%s)\n", a.Version, runtime.GOOS, runtime.GOARCH)
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(a.Stdout, usage)
		return 0
	default:
		fmt.Fprintf(a.Stderr, "ros-local: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
}

func (a *App) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("ros-local "+name, flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	return fs
}

func (a *App) userAgent() string {
	return fmt.Sprintf("ros-local/%s (%s/%s)", a.Version, runtime.GOOS, runtime.GOARCH)
}

func (a *App) client(baseURL, token string) *api.Client {
	return api.New(baseURL, token, a.userAgent(), a.HTTPTimeout)
}

func (a *App) session(cfg *config.Config) *auth.Session {
	return &auth.Session{Config: cfg, Store: config.StoreFor(cfg), NewClient: a.client}
}

func (a *App) cwd() string {
	if wd, err := a.Getwd(); err == nil {
		return wd
	}
	return "."
}

func (a *App) errorf(format string, args ...any) int {
	fmt.Fprintf(a.Stderr, "ros-local: "+format+"\n", args...)
	return 1
}

// pluginVersion reads the version of the plugin that invoked us, when there is one.
func (a *App) pluginVersion() string {
	root := a.Getenv("CLAUDE_PLUGIN_ROOT")
	if root == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(root, ".claude-plugin", "plugin.json"))
	if err != nil {
		return ""
	}
	var m struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal(b, &m)
	return m.Version
}

// versionLess compares dotted numeric versions ("v1.2.3", "1.2"); pre-release suffixes are ignored.
// Unparseable versions never compare as less, so a dev build is never told it's out of date.
func versionLess(have, min string) bool {
	parse := func(v string) ([]int, bool) {
		v = strings.TrimPrefix(strings.TrimSpace(v), "v")
		v, _, _ = strings.Cut(v, "-")
		v, _, _ = strings.Cut(v, "+")
		if v == "" {
			return nil, false
		}
		var out []int
		for _, p := range strings.Split(v, ".") {
			n, err := strconv.Atoi(p)
			if err != nil {
				return nil, false
			}
			out = append(out, n)
		}
		return out, true
	}
	h, ok1 := parse(have)
	m, ok2 := parse(min)
	if !ok1 || !ok2 {
		return false
	}
	for i := 0; i < max(len(h), len(m)); i++ {
		var x, y int
		if i < len(h) {
			x = h[i]
		}
		if i < len(m) {
			y = m[i]
		}
		if x != y {
			return x < y
		}
	}
	return false
}

func ago(t time.Time, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
