package app

// Capture levels and sharing for local traces (spec §11). The developer's own level lives in
// ~/.ros/config.json and starts at metadata, so nothing above metadata leaves the machine unless they raise
// it here — after being shown what that means. The project's ceiling still caps it on the server.

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hvoguetech/ros-claude-plugin/cli/internal/config"
)

var levelRank = map[string]int{"metadata": 0, "prompts": 1, "full": 2}

var levelText = map[string]string{
	"metadata": "tool names, durations, file paths, model and token counts — no prompts, no code, no outputs",
	"prompts":  "metadata, plus your prompts and Claude's replies (secrets scrubbed)",
	"full":     "prompts, plus tool inputs and outputs — code, commands and their results (secrets scrubbed, clipped)",
}

// captureNotes are the SessionStart banner lines about the capture level: what the project would allow
// beyond what you share, or that it caps what you chose.
func captureNotes(mine, ceiling string) []string {
	if ceiling == "" {
		return nil
	}
	switch {
	case levelRank[ceiling] > levelRank[mine]:
		return []string{fmt.Sprintf("ROS: traces share %s; this project allows %s — `ros-local capture %s` to share more",
			mine, ceiling, ceiling)}
	case levelRank[ceiling] < levelRank[mine]:
		return []string{fmt.Sprintf("ROS: you chose %s traces; this project caps them at %s", mine, ceiling)}
	}
	return nil
}

// capture shows or sets this developer's capture level. Raising it above metadata asks first (or --yes).
func (a *App) capture(args []string) int {
	fs := a.flags("capture")
	yes := fs.Bool("yes", false, "don't ask")
	if fs.Parse(args) != nil {
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		return a.errorf("capture: %v", err)
	}
	cur := captureLevel(cfg)
	if fs.NArg() == 0 {
		fmt.Fprintf(a.Stdout, "Local traces share: %s (%s).\nLevels: metadata | prompts | full — `ros-local capture <level>`.\n",
			cur, levelText[cur])
		return 0
	}
	want := fs.Arg(0)
	if _, ok := levelRank[want]; !ok {
		return a.errorf("capture: %q isn't a level (metadata | prompts | full)", want)
	}
	if levelRank[want] > levelRank[cur] && !*yes {
		fmt.Fprintf(a.Stdout, "Raise local traces from %s to %s?\nThey'll carry: %s.\nVisible to your project "+
			"unless you make a session private (/ros:private). The project may cap it lower.\nType yes to confirm: ",
			cur, want, levelText[want])
		line, _ := bufio.NewReader(a.Stdin).ReadString('\n')
		if strings.ToLower(strings.TrimSpace(line)) != "yes" {
			fmt.Fprintln(a.Stdout, "Unchanged.")
			return 1
		}
	}
	cfg.CaptureLevel = want
	if want == "metadata" {
		cfg.CaptureLevel = ""
	}
	if err := cfg.Save(); err != nil {
		return a.errorf("capture: %v", err)
	}
	fmt.Fprintf(a.Stdout, "Local traces now share %s (%s). New spans only; what was sent stays as it was.\n",
		want, levelText[want])
	return 0
}

// currentSession is this repo's most recent open session (the slash commands run in Claude Code's shell).
func (a *App) currentSession() *sessionState {
	dir := firstNonEmpty(a.Getenv("CLAUDE_PROJECT_DIR"), a.cwd())
	entries, _ := os.ReadDir(filepath.Join(config.Home(), "sessions"))
	var all []*sessionState
	for _, e := range entries {
		if st := loadState(strings.TrimSuffix(e.Name(), ".json")); st != nil {
			all = append(all, st)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].OpenedAt.After(all[j].OpenedAt) })
	for _, st := range all {
		if st.Dir == dir {
			return st
		}
	}
	return nil
}

// share is /ros:share (project) and /ros:private (private) for this repo's current session.
func (a *App) share(visibility string) int {
	st := a.currentSession()
	if st == nil {
		return a.errorf("no open ROS session for this repo")
	}
	cfg, err := config.Load()
	if err != nil || cfg.URL != st.URL {
		return a.errorf("this session belongs to another ROS sign-in")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	token, err := a.session(cfg).AccessToken(ctx)
	if err != nil {
		return a.errorf("%v", err)
	}
	got, err := a.client(cfg.URL, token).SetVisibility(ctx, st.RunID, visibility)
	if err != nil {
		return a.errorf("%v", err)
	}
	if got == "private" {
		fmt.Fprintln(a.Stdout, "This session is private: only you see its trace. Project totals still count it, without content.")
	} else {
		fmt.Fprintln(a.Stdout, "This session is shared: project members who can read traces see it.")
	}
	return 0
}
