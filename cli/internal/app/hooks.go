package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hvoguetech/ros-claude-plugin/cli/internal/api"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/auth"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/config"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/fsutil"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/gitx"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/link"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/skills"
)

// MaxHookContext bounds the SessionStart `additionalContext`. Claude Code swaps anything over 10,000
// characters for a file path and a short preview (spec §8.1 / §18a). The server already keeps its block
// under 9,000; this is the client-side guarantee, counted in bytes (never fewer than characters).
const MaxHookContext = 9500

// sessionStartBudget bounds the whole SessionStart hook; it runs synchronously before the first prompt.
const sessionStartBudget = 12 * time.Second

// hookInput is the JSON Claude Code writes to a hook's stdin.
type hookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
	Source         string `json:"source"` // SessionStart: startup | resume | clear | compact | fork
	Reason         string `json:"reason"` // SessionEnd
}

type hookSpecific struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext,omitempty"`
}

// hookOutput is what a SessionStart hook prints. systemMessage is shown to the developer;
// additionalContext goes to the model.
type hookOutput struct {
	SystemMessage      string        `json:"systemMessage,omitempty"`
	HookSpecificOutput *hookSpecific `json:"hookSpecificOutput,omitempty"`
}

func (a *App) hook(args []string) int {
	if len(args) == 0 {
		return a.errorf("hook: missing event (session-start | session-end)")
	}
	switch args[0] {
	case "session-start", "SessionStart":
		return a.sessionStart()
	case "session-end", "SessionEnd":
		return a.sessionEnd(args[1:])
	default:
		// Unknown events are a no-op so a newer plugin never breaks an older binary.
		return 0
	}
}

func readHookInput(r io.Reader) hookInput {
	var in hookInput
	if r == nil {
		return in
	}
	b, _ := io.ReadAll(io.LimitReader(r, 1<<20))
	_ = json.Unmarshal(b, &in)
	return in
}

// CapContext keeps s within max bytes. It cuts at a line boundary, says so, and re-closes the
// `<ros-memory>` block if the cut removed its closing tag.
func CapContext(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const note = "\n[ros-local: recalled context truncated to fit the hook limit]"
	closing := ""
	if strings.Contains(s, "<ros-memory") {
		closing = "\n</ros-memory>"
	}
	budget := max - len(note) - len(closing)
	if budget <= 0 {
		return ""
	}
	for budget > 0 && !utf8.RuneStart(s[budget]) {
		budget--
	}
	cut := s[:budget]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	}
	if closing != "" && strings.Contains(cut, "</ros-memory>") {
		closing = ""
	}
	return cut + note + closing
}

// sessionState is what SessionStart leaves for SessionEnd: `~/.ros/sessions/<session_id>.json`.
type sessionState struct {
	RunID     string    `json:"run_id"`
	ProjectID string    `json:"project_id"`
	URL       string    `json:"url"`
	OpenedAt  time.Time `json:"opened_at"`
}

var unsafeID = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func statePath(sessionID string) string {
	id := unsafeID.ReplaceAllString(sessionID, "_")
	if len(id) > 128 {
		id = id[:128]
	}
	return filepath.Join(config.Home(), "sessions", id+".json")
}

func saveState(sessionID string, st *sessionState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(statePath(sessionID)), 0o700); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(statePath(sessionID), b, 0o600)
}

func loadState(sessionID string) *sessionState {
	b, err := os.ReadFile(statePath(sessionID))
	if err != nil {
		return nil
	}
	var st sessionState
	if json.Unmarshal(b, &st) != nil || st.RunID == "" {
		return nil
	}
	return &st
}

// pruneStates removes session files older than a week (sessions that never got a SessionEnd).
func pruneStates(now time.Time) {
	dir := filepath.Join(config.Home(), "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > 7*24*time.Hour {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func (a *App) emit(out *hookOutput) int {
	if out.HookSpecificOutput != nil {
		out.HookSpecificOutput.AdditionalContext = CapContext(out.HookSpecificOutput.AdditionalContext, MaxHookContext)
	}
	enc := json.NewEncoder(a.Stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(out)
	return 0
}

func startOutput(banner, modelNote string) *hookOutput {
	out := &hookOutput{SystemMessage: banner}
	if modelNote != "" {
		out.HookSpecificOutput = &hookSpecific{HookEventName: "SessionStart", AdditionalContext: modelNote}
	}
	return out
}

const signedOutBanner = "ROS: signed out — run `ros-local login`"

func signedOut() *hookOutput {
	return startOutput(signedOutBanner,
		"ROS is signed out on this machine, so ROS memory and skills are unavailable in this session. "+
			"If the user asks for ROS features, tell them to run `ros-local login` in a terminal.")
}

// sessionStart opens the local Run, syncs skills and hands the recalled memory to the model (spec §5, §7,
// §8.1). It never fails the session: every problem becomes a one-line banner and exit 0.
func (a *App) sessionStart() int {
	in := readHookInput(a.Stdin)
	ctx, cancel := context.WithTimeout(context.Background(), sessionStartBudget)
	defer cancel()
	now := time.Now()
	go pruneStates(now)

	cfg, err := config.Load()
	if err != nil {
		return a.emit(startOutput("ROS: can't read ~/.ros/config.json — "+err.Error(), ""))
	}
	dir := firstNonEmpty(a.Getenv("CLAUDE_PROJECT_DIR"), in.Cwd, a.cwd())
	repo, _ := gitx.Find(dir)
	var lnk *link.Link
	skillsDirExisted := true
	if repo != nil {
		lnk, _ = link.Read(repo)
		_, statErr := os.Stat(skills.Dir(repo.Root))
		skillsDirExisted = statErr == nil
	}

	token, err := a.session(cfg).AccessToken(ctx)
	if errors.Is(err, auth.ErrSignedOut) {
		return a.emit(signedOut())
	}
	if err != nil {
		return a.emit(a.offline(repo, now, err))
	}
	c := a.client(cfg.URL, token)

	req := &api.SessionRequest{Harness: "claude-code", HarnessSessionID: in.SessionID, Source: in.Source,
		PluginVersion: a.pluginVersion(), CwdName: filepath.Base(dir)}
	if req.HarnessSessionID == "" {
		req.HarnessSessionID = fmt.Sprintf("ros-local-%d", now.UnixNano())
	}
	if !validSource(req.Source) {
		req.Source = "startup"
	}
	if repo != nil {
		req.Repo, req.Branch, req.Head = repo.Remote(ctx), repo.Branch(ctx), repo.Head(ctx)
		req.RecentCommits = repo.RecentCommits(ctx, 5)
	}
	if lnk != nil {
		req.ProjectID = lnk.ProjectID
	}
	s, err := c.OpenSession(ctx, req)
	if err != nil {
		var apiErr *api.Error
		switch {
		case errors.As(err, &apiErr) && apiErr.Status == 401:
			return a.emit(signedOut())
		case errors.As(err, &apiErr) && apiErr.Status < 500:
			return a.emit(startOutput("ROS: "+apiErr.Detail, ""))
		default:
			return a.emit(a.offline(repo, now, err))
		}
	}
	if in.SessionID != "" {
		_ = saveState(in.SessionID, &sessionState{RunID: s.RunID, ProjectID: s.ProjectID, URL: cfg.URL, OpenedAt: now.UTC()})
	}

	var notes []string
	if v := a.pluginVersion(); s.MinPluginVersion != "" && versionLess(v, s.MinPluginVersion) {
		return a.emit(startOutput("ROS plugin out of date — run `claude plugin update ros`; ROS is off for this session", ""))
	}
	if s.MinCLIVersion != "" && versionLess(a.Version, s.MinCLIVersion) {
		return a.emit(startOutput("ros-local out of date — run `brew upgrade ros-local`; ROS is off for this session", ""))
	}
	if mcp := config.NormalizeURL(firstNonEmpty(a.Getenv("ROS_URL"), config.DefaultURL)); mcp != cfg.URL {
		// The plugin's .mcp.json URL comes from $ROS_URL (default: production), not from `login --url`.
		notes = append(notes, fmt.Sprintf("ROS: signed in to %s but the MCP server uses %s — export ROS_URL=%s",
			cfg.URL, mcp, cfg.URL))
	}

	skillsPart := ""
	if repo != nil {
		if lnk == nil || lnk.ProjectID != s.ProjectID && lnk.Source == "session" {
			_ = link.Write(repo, &link.Link{ProjectID: s.ProjectID, ProjectName: s.ProjectName,
				Repo: gitx.RepoKey(req.Repo), Source: "session"})
		}
		if _, err := link.EnsureExclude(repo); err != nil {
			notes = append(notes, "ROS: couldn't update .git/info/exclude: "+err.Error())
		}
		if s.SkillsManifestETag != nil {
			res, err := skills.Sync(ctx, c, repo.Root, skills.Options{ProjectID: s.ProjectID, KnownETag: *s.SkillsManifestETag})
			if err != nil {
				m, _ := skills.ReadManifest(skills.Dir(repo.Root))
				skillsPart = fmt.Sprintf(" · %d skills (sync failed, last synced %s)", len(m.Skills), ago(m.SyncedAt, now))
				notes = append(notes, "ROS: skills sync failed: "+err.Error())
			} else {
				skillsPart = fmt.Sprintf(" · %d skills (synced %s)", res.Total, ago(res.SyncedAt, now))
				notes = append(notes, syncNotes(res, !skillsDirExisted)...)
			}
		}
	}

	memPart, recalled := "", 0
	if s.Recall != nil {
		recalled = len(s.Recall.ItemIDs)
		memPart = fmt.Sprintf(" · memory: %s · recalled %d", s.Recall.Profile, recalled)
	}
	status := fmt.Sprintf("ROS · project %q%s%s", s.ProjectName, skillsPart, memPart)
	banner := strings.Join(append([]string{status}, notes...), "\n")

	model := status + "\n" +
		fmt.Sprintf("ROS project for this repo: %q (id %s). ROS MCP tools use it by default; pass project=%q if a call needs to be explicit.",
			s.ProjectName, s.ProjectID, s.ProjectID)
	if s.Context != "" {
		model += "\n\n" + s.Context
	}
	return a.emit(&hookOutput{SystemMessage: banner,
		HookSpecificOutput: &hookSpecific{HookEventName: "SessionStart", AdditionalContext: model}})
}

func validSource(s string) bool {
	switch s {
	case "startup", "resume", "clear", "compact", "fork":
		return true
	}
	return false
}

// offline is the banner when ROS can't be reached: the last synced skills keep working.
func (a *App) offline(repo *gitx.Repo, now time.Time, err error) *hookOutput {
	msg := "ROS: offline"
	if repo != nil {
		if m, merr := skills.ReadManifest(skills.Dir(repo.Root)); merr == nil && len(m.Skills) > 0 {
			msg += fmt.Sprintf(" · using %d skills synced %s", len(m.Skills), ago(m.SyncedAt, now))
		}
	}
	fmt.Fprintf(a.Stderr, "ros-local: %v\n", err)
	return startOutput(msg, "")
}

// sessionEnd ends the local Run (spec §11). Claude Code gives SessionEnd hooks about 1.5 s, so the hook
// only hands the work to a detached ros-local and returns; the server's 2 h reaper is the backstop.
func (a *App) sessionEnd(args []string) int {
	fs := a.flags("hook session-end")
	detached := fs.Bool("detached", false, "internal: do the work (spawned by the hook)")
	sid := fs.String("session-id", "", "internal")
	reason := fs.String("reason", "", "internal")
	if fs.Parse(args) != nil {
		return 0
	}
	if *detached {
		return a.endSession(*sid, *reason, 20*time.Second)
	}
	in := readHookInput(a.Stdin)
	if in.SessionID == "" || loadState(in.SessionID) == nil {
		return 0
	}
	if a.Spawn != nil {
		if err := a.Spawn("hook", "session-end", "--detached", "--session-id", in.SessionID, "--reason", in.Reason); err == nil {
			return 0
		}
	}
	return a.endSession(in.SessionID, in.Reason, time.Second)
}

func (a *App) endSession(sessionID, reason string, budget time.Duration) int {
	st := loadState(sessionID)
	if st == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	cfg, err := config.Load()
	if err != nil || cfg.URL != st.URL {
		_ = os.Remove(statePath(sessionID))
		return 0
	}
	token, err := a.session(cfg).AccessToken(ctx)
	if err != nil {
		return 0
	}
	err = a.client(cfg.URL, token).EndSession(ctx, st.RunID, reason)
	if err == nil || api.IsStatus(err, 404) || api.IsStatus(err, 409) {
		_ = os.Remove(statePath(sessionID))
	}
	return 0
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
