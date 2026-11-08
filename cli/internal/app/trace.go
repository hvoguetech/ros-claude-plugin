package app

// Local traces (spec §11). Claude Code's hooks append one event each to the session's spool —
// `~/.ros/spool/<run_id>.jsonl`, synchronously, no network — and a detached `ros-local flush` turns the
// spool into spans and ships what hasn't been sent. Text (prompts, tool input/output, replies) is only
// spooled when the developer chose a capture level above metadata; the server enforces the project's
// ceiling whatever arrives.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hvoguetech/ros-claude-plugin/cli/internal/api"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/config"
)

const (
	spoolCap  = 50 << 20 // a spool past this stops growing (the server has what was sent)
	textCap   = 8 << 10  // per text field in the spool
	batchSize = 500
	spoolTTL  = 8 * 24 * time.Hour // the server takes late spans for 7 days
)

// spoolEvent is one hook, as written to the spool.
type spoolEvent struct {
	Event      string   `json:"e"` // prompt | tool_start | tool_end | subagent | compact | stop
	At         float64  `json:"at"`
	ToolUseID  string   `json:"tool_use_id,omitempty"`
	Tool       string   `json:"tool,omitempty"`
	Files      []string `json:"files,omitempty"`
	IsError    bool     `json:"is_error,omitempty"`
	Agent      string   `json:"agent,omitempty"`
	Trigger    string   `json:"trigger,omitempty"`
	Transcript string   `json:"transcript,omitempty"`
	Retain     string   `json:"retain,omitempty"` // stop: the session's §8.3 mode (auto = send new turns)
	Text       string   `json:"text,omitempty"`   // the prompt (prompt), the tool input (tool_start)
	Output     string   `json:"output,omitempty"`
}

var traceEvents = map[string]string{
	"user-prompt-submit": "prompt", "UserPromptSubmit": "prompt",
	"pre-tool-use": "tool_start", "PreToolUse": "tool_start",
	"post-tool-use": "tool_end", "PostToolUse": "tool_end",
	"subagent-stop": "subagent", "SubagentStop": "subagent",
	"pre-compact": "compact", "PreCompact": "compact",
	"stop": "stop", "Stop": "stop",
}

func captureLevel(cfg *config.Config) string {
	if cfg != nil && (cfg.CaptureLevel == "off" || cfg.CaptureLevel == "prompts" || cfg.CaptureLevel == "full") {
		return cfg.CaptureLevel
	}
	return "metadata"
}

func spoolDir() string { return filepath.Join(config.Home(), "spool") }
func spoolPath(runID string) string {
	return filepath.Join(spoolDir(), unsafeID.ReplaceAllString(runID, "_")+".jsonl")
}
func sentPath(runID string) string { return strings.TrimSuffix(spoolPath(runID), ".jsonl") + ".sent" }
func levelAbove(level string) bool { return level == "prompts" || level == "full" }
func capText(s string, on bool) string {
	if !on {
		return ""
	}
	if len(s) > textCap {
		s = s[:textCap]
	}
	return s
}

func jsonText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// filesOf picks the paths a tool call names (Read/Edit/Write/NotebookEdit/Glob/Grep inputs).
func filesOf(input json.RawMessage) []string {
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return nil
	}
	var out []string
	for _, k := range []string{"file_path", "notebook_path", "path"} {
		if v, ok := m[k].(string); ok && v != "" {
			out = append(out, v)
		}
	}
	return out
}

// traceHook is every hook but session start/end: append one event and return at once. On Stop and
// PreCompact a detached flush ships the spool. It never prints (UserPromptSubmit stdout becomes context).
func (a *App) traceHook(event string) int {
	in := readHookInput(a.Stdin)
	st := loadState(in.SessionID)
	if st == nil {
		return 0
	}
	cfg, _ := config.Load()
	level := captureLevel(cfg)
	if level == "off" && !(event == "stop" && st.Retain == "auto") {
		return 0 // traces off: nothing spooled (a Stop still carries the transcript for session memory)
	}
	text := levelAbove(level)
	full := level == "full"
	ev := spoolEvent{Event: event, At: float64(time.Now().UnixNano()) / 1e9}
	switch event {
	case "prompt":
		ev.Text = capText(in.Prompt, text)
	case "tool_start":
		ev.ToolUseID, ev.Tool, ev.Files = in.ToolUseID, in.ToolName, filesOf(in.ToolInput)
		ev.Text = capText(jsonText(in.ToolInput), full)
	case "tool_end":
		ev.ToolUseID, ev.Tool = in.ToolUseID, in.ToolName
		var resp map[string]any
		if json.Unmarshal(in.ToolResponse, &resp) == nil {
			if v, ok := resp["is_error"].(bool); ok {
				ev.IsError = v
			}
		}
		ev.Output = capText(jsonText(in.ToolResponse), full)
	case "subagent":
		ev.Agent = in.AgentType
	case "compact":
		ev.Trigger = in.Trigger
	case "stop":
		ev.Transcript, ev.Retain = in.TranscriptPath, st.Retain
	}
	_ = appendEvent(st.RunID, ev)
	if event == "stop" || event == "compact" {
		if a.Spawn != nil && a.Spawn("flush", "--run", st.RunID) == nil {
			return 0
		}
	}
	return 0
}

func appendEvent(runID string, ev spoolEvent) error {
	if err := os.MkdirAll(spoolDir(), 0o700); err != nil {
		return err
	}
	p := spoolPath(runID)
	if fi, err := os.Stat(p); err == nil && fi.Size() > spoolCap {
		return errors.New("spool full")
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

func readEvents(runID string) []spoolEvent {
	f, err := os.Open(spoolPath(runID))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []spoolEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var ev spoolEvent
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Event != "" {
			out = append(out, ev)
		}
	}
	return out
}

func spanID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:12])
}

func ms(start, end float64) float64 {
	if end < start {
		return 0
	}
	return (end - start) * 1000
}

// buildSpans turns the spool into finished spans: a turn per prompt (until its Stop), the tool calls in it,
// subagents, compactions, and the model calls read from the transcript (with token usage).
func buildSpans(events []spoolEvent, transcriptText bool) []api.Span {
	var spans []api.Span
	var turn *api.Span
	closeTurn := func(at float64) {
		if turn != nil {
			turn.EndWall, turn.LatencyMS = at, ms(turn.StartWall, at)
			spans = append(spans, *turn)
			turn = nil
		}
	}
	starts := map[string]spoolEvent{}
	parent := func() string {
		if turn != nil {
			return turn.ID
		}
		return ""
	}
	transcripts := map[string]bool{}
	type window struct {
		id         string
		start, end float64
	}
	var windows []window
	for _, ev := range events {
		switch ev.Event {
		case "prompt":
			closeTurn(ev.At)
			turn = &api.Span{ID: spanID("turn", ftoa(ev.At)), Name: "prompt", Kind: "chain", StartWall: ev.At,
				Input: ev.Text}
		case "tool_start":
			starts[ev.ToolUseID] = ev
		case "tool_end":
			st, ok := starts[ev.ToolUseID]
			if !ok || ev.ToolUseID == "" {
				continue
			}
			delete(starts, ev.ToolUseID)
			attrs := map[string]any{"tool": st.Tool}
			if len(st.Files) > 0 {
				attrs["files"] = st.Files
			}
			if ev.IsError {
				attrs["is_error"] = true
			}
			sp := api.Span{ID: spanID("tool", ev.ToolUseID), ParentID: parent(), Name: "tool · " + st.Tool,
				Kind: "tool", StartWall: st.At, EndWall: ev.At, LatencyMS: ms(st.At, ev.At), Attributes: attrs,
				Input: st.Text, Output: ev.Output}
			if ev.IsError {
				sp.Error = "tool error"
			}
			spans = append(spans, sp)
		case "subagent":
			name := "subagent"
			if ev.Agent != "" {
				name += " · " + ev.Agent
			}
			spans = append(spans, api.Span{ID: spanID("subagent", ftoa(ev.At)), ParentID: parent(), Name: name,
				Kind: "subagent", StartWall: ev.At, EndWall: ev.At,
				Attributes: map[string]any{"subagent_type": ev.Agent}})
		case "compact":
			spans = append(spans, api.Span{ID: spanID("compact", ftoa(ev.At)), ParentID: parent(), Name: "compact",
				Kind: "chain", StartWall: ev.At, EndWall: ev.At, Attributes: map[string]any{"trigger": ev.Trigger}})
		case "stop":
			if turn != nil {
				windows = append(windows, window{turn.ID, turn.StartWall, ev.At})
			}
			closeTurn(ev.At)
			if ev.Transcript != "" {
				transcripts[ev.Transcript] = true
			}
		}
	}
	// The model calls: every assistant message in the transcript, in the turn whose window holds it.
	paths := make([]string, 0, len(transcripts))
	for p := range transcripts {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		for _, m := range readTranscript(p, transcriptText) {
			sp := m
			for _, w := range windows {
				if m.StartWall >= w.start && m.StartWall <= w.end {
					sp.ParentID = w.id
					break
				}
			}
			spans = append(spans, sp)
		}
	}
	return spans
}

func ftoa(f float64) string { return time.Unix(0, int64(f*1e9)).UTC().Format(time.RFC3339Nano) }

// transcriptLine is the part of a Claude Code transcript line a model span needs.
type transcriptLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   struct {
			Input         int `json:"input_tokens"`
			Output        int `json:"output_tokens"`
			CacheRead     int `json:"cache_read_input_tokens"`
			CacheCreation int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// readTranscript returns one llm span per assistant message (a streamed message spans several lines with
// the same id: the last carries the final usage).
func readTranscript(path string, withText bool) []api.Span {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	byID := map[string]api.Span{}
	var order []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 256<<10), 16<<20)
	for sc.Scan() {
		var ln transcriptLine
		if json.Unmarshal(sc.Bytes(), &ln) != nil || ln.Type != "assistant" || ln.Message.ID == "" {
			continue
		}
		at := 0.0
		if t, err := time.Parse(time.RFC3339Nano, ln.Timestamp); err == nil {
			at = float64(t.UnixNano()) / 1e9
		}
		u := ln.Message.Usage
		sp, seen := byID[ln.Message.ID]
		if !seen {
			order = append(order, ln.Message.ID)
			sp = api.Span{ID: spanID("llm", ln.Message.ID), Name: "llm · " + ln.Message.Model, Kind: "llm",
				Model: ln.Message.Model, StartWall: at}
		}
		sp.EndWall, sp.LatencyMS = at, ms(sp.StartWall, at)
		sp.InputTokens, sp.OutputTokens = u.Input, u.Output
		sp.Attributes = map[string]any{"cache_read": u.CacheRead, "cache_creation": u.CacheCreation}
		if withText {
			sp.Output = capText(sp.Output+assistantText(ln.Message.Content), true)
		}
		byID[ln.Message.ID] = sp
	}
	out := make([]api.Span, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out
}

func assistantText(raw json.RawMessage) string {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, bl := range blocks {
		if bl.Type == "text" {
			b.WriteString(bl.Text)
		}
	}
	return b.String()
}

func readSent(runID string) map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile(sentPath(runID))
	if err != nil {
		return out
	}
	for _, id := range strings.Split(string(b), "\n") {
		if id != "" {
			out[id] = true
		}
	}
	return out
}

func markSent(runID string, spans []api.Span) error {
	f, err := os.OpenFile(sentPath(runID), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	var b strings.Builder
	for _, s := range spans {
		b.WriteString(s.ID + "\n")
	}
	_, err = f.WriteString(b.String())
	return err
}

func dropSpool(runID string) {
	_ = os.Remove(spoolPath(runID))
	_ = os.Remove(sentPath(runID))
	_ = os.Remove(retainedPath(runID))
}

// flush ships every spool (or one run's): the spans not sent yet, in batches. A spool the server won't
// take any more (too late, or not this developer's) is dropped; so is one older than the server's window.
func (a *App) flush(args []string) int {
	fs := a.flags("flush")
	only := fs.String("run", "", "only this session's run")
	if fs.Parse(args) != nil {
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	token, err := a.session(cfg).AccessToken(ctx)
	if err != nil {
		return 0 // signed out or offline: the spool waits for the next flush
	}
	level := captureLevel(cfg)
	c := a.client(cfg.URL, token)
	entries, _ := os.ReadDir(spoolDir())
	for _, e := range entries {
		runID, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if !ok || (*only != "" && runID != unsafeID.ReplaceAllString(*only, "_")) {
			continue
		}
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > spoolTTL {
			dropSpool(runID)
			continue
		}
		a.flushRun(ctx, c, runID, level)
	}
	return 0
}

func (a *App) flushRun(ctx context.Context, c *api.Client, runID, level string) {
	// retain: auto — the turns finished so far go to the person's memory (ROS checks the project's setting).
	if events := readEvents(runID); len(events) > 0 {
		last := events[len(events)-1]
		for i := len(events) - 1; i >= 0; i-- {
			if events[i].Event == "stop" && events[i].Transcript != "" {
				last = events[i]
				break
			}
		}
		if last.Retain == "auto" && last.Transcript != "" {
			_, _ = retainTurns(ctx, c, runID, last.Transcript, false)
		}
	}
	if level == "off" {
		return // traces off: the spool only carried what session memory needs
	}
	sent := readSent(runID)
	var todo []api.Span
	for _, s := range buildSpans(readEvents(runID), levelAbove(level)) {
		if !sent[s.ID] {
			todo = append(todo, s)
		}
	}
	for len(todo) > 0 {
		n := min(batchSize, len(todo))
		res, err := c.PostSpans(ctx, runID, level, todo[:n])
		switch {
		case api.IsStatus(err, 404):
			dropSpool(runID) // not this developer's session (or gone): never retried
			return
		case err != nil:
			return // offline / server trouble: next flush
		case res.Skipped == "too_late":
			dropSpool(runID)
			return
		}
		_ = markSent(runID, todo[:n])
		todo = todo[n:]
	}
}
