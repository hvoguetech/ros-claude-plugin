package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hvoguetech/ros-claude-plugin/cli/internal/config"
)

// A transcript as Claude Code writes it: one assistant message streamed over two lines (same id, the last
// line has the final usage), and a user line that isn't a model call.
func writeTranscript(t *testing.T, at time.Time) string {
	t.Helper()
	ts := at.UTC().Format(time.RFC3339Nano)
	lines := []string{
		`{"type":"user","timestamp":"` + ts + `","message":{"role":"user","content":"fix it"}}`,
		`{"type":"assistant","timestamp":"` + ts + `","message":{"id":"msg_1","model":"claude-opus-5-5","content":[{"type":"text","text":"Looking"}],"usage":{"input_tokens":10,"output_tokens":1}}}`,
		`{"type":"assistant","timestamp":"` + ts + `","message":{"id":"msg_1","model":"claude-opus-5-5","content":[{"type":"text","text":" now."}],"usage":{"input_tokens":1200,"output_tokens":300,"cache_read_input_tokens":800,"cache_creation_input_tokens":50}}}`,
	}
	p := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (h *harness) session(t *testing.T, sid string) {
	t.Helper()
	h.signIn(time.Hour)
	if code, _, _ := h.run(`{"session_id":"`+sid+`"}`, "hook", "session-start"); code != 0 {
		t.Fatal("session-start failed")
	}
}

func hookJSON(m map[string]any) string {
	b, _ := json.Marshal(m)
	return string(b)
}

func TestTraceHooksSpoolQuietlyAndFlushShipsSpansOnce(t *testing.T) {
	h := newHarness(t)
	h.session(t, "s1")
	transcript := filepath.Join(t.TempDir(), "pending") // written just before Stop, after the prompt

	steps := []struct {
		event string
		in    map[string]any
	}{
		{"user-prompt-submit", map[string]any{"session_id": "s1", "prompt": "fix the idempotency bug"}},
		{"pre-tool-use", map[string]any{"session_id": "s1", "tool_name": "Edit", "tool_use_id": "tu1",
			"tool_input": map[string]any{"file_path": "src/pay.py", "old_string": "a", "new_string": "b"}}},
		{"post-tool-use", map[string]any{"session_id": "s1", "tool_name": "Edit", "tool_use_id": "tu1",
			"tool_response": map[string]any{"filePath": "src/pay.py"}}},
		{"pre-tool-use", map[string]any{"session_id": "s1", "tool_name": "Bash", "tool_use_id": "tu2",
			"tool_input": map[string]any{"command": "go test"}}}, // never finishes: not a span
		{"stop", map[string]any{"session_id": "s1", "transcript_path": transcript}}, // set below
	}
	for _, s := range steps {
		if s.event == "stop" {
			s.in["transcript_path"] = writeTranscript(t, time.Now())
		}
		start := time.Now()
		code, out, _ := h.run(hookJSON(s.in), "hook", s.event)
		if code != 0 || out != "" {
			t.Fatalf("%s: a trace hook must print nothing and exit 0 (code %d, out %q)", s.event, code, out)
		}
		if time.Since(start) > 200*time.Millisecond {
			t.Fatalf("%s took %v: hooks never touch the network", s.event, time.Since(start))
		}
	}
	if len(h.srv.Spans) != 0 {
		t.Fatal("a hook shipped spans itself")
	}
	if len(h.spawns) != 1 || h.spawns[0][0] != "flush" {
		t.Fatalf("Stop must hand the flush to a detached process: %v", h.spawns)
	}
	// Metadata (the default): no text was even spooled.
	raw, _ := os.ReadFile(spoolPath("run-1"))
	if strings.Contains(string(raw), "idempotency") || strings.Contains(string(raw), "old_string") {
		t.Fatalf("text spooled at metadata: %s", raw)
	}

	h.run("", h.spawns[0]...) // what the detached process does
	got := h.srv.Spans["run-1"]
	names := map[string]map[string]any{}
	for _, s := range got {
		names[s["name"].(string)] = s
	}
	if len(got) != 3 || names["prompt"] == nil || names["tool · Edit"] == nil || names["llm · claude-opus-5-5"] == nil {
		t.Fatalf("spans: %v", got)
	}
	edit := names["tool · Edit"]
	if edit["parent_id"] != names["prompt"]["id"] || edit["attributes"].(map[string]any)["files"].([]any)[0] != "src/pay.py" {
		t.Fatalf("edit span: %v", edit)
	}
	llm := names["llm · claude-opus-5-5"]
	if llm["input_tokens"].(float64) != 1200 || llm["output_tokens"].(float64) != 300 || llm["output"] != nil {
		t.Fatalf("llm span (final usage, no text at metadata): %v", llm)
	}
	if llm["attributes"].(map[string]any)["cache_read"].(float64) != 800 || llm["parent_id"] != names["prompt"]["id"] {
		t.Fatalf("llm span attrs/parent: %v", llm)
	}
	if h.srv.SpanLevels[0] != "metadata" {
		t.Fatalf("level %v", h.srv.SpanLevels)
	}

	h.run("", "flush") // nothing new: nothing sent again
	if len(h.srv.Spans["run-1"]) != 3 {
		t.Fatalf("re-sent: %d", len(h.srv.Spans["run-1"]))
	}
}

func TestPromptsLevelSpoolsPromptsAndReplies(t *testing.T) {
	h := newHarness(t)
	cfg, _ := config.Load()
	cfg.CaptureLevel = "prompts"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	h.session(t, "s2")
	h.run(hookJSON(map[string]any{"session_id": "s2", "prompt": "fix it please"}), "hook", "user-prompt-submit")
	h.run(hookJSON(map[string]any{"session_id": "s2", "tool_name": "Bash", "tool_use_id": "t",
		"tool_input": map[string]any{"command": "rm -rf build"}}), "hook", "pre-tool-use")
	h.run(hookJSON(map[string]any{"session_id": "s2", "tool_name": "Bash", "tool_use_id": "t",
		"tool_response": map[string]any{"stdout": "ok"}}), "hook", "post-tool-use")
	h.run(hookJSON(map[string]any{"session_id": "s2", "transcript_path": writeTranscript(t, time.Now())}), "hook", "stop")
	h.run("", "flush")
	byName := map[string]map[string]any{}
	for _, s := range h.srv.Spans["run-1"] {
		byName[s["name"].(string)] = s
	}
	if byName["prompt"]["input"] != "fix it please" || byName["llm · claude-opus-5-5"]["output"] != "Looking now." {
		t.Fatalf("prompts level: %v", byName)
	}
	if byName["tool · Bash"]["input"] != nil || byName["tool · Bash"]["output"] != nil {
		t.Fatalf("tool I/O is `full` only: %v", byName["tool · Bash"])
	}
	if h.srv.SpanLevels[0] != "prompts" {
		t.Fatalf("level %v", h.srv.SpanLevels)
	}
}

func TestSessionEndShipsTheLastTurnAndATooLateSpoolIsDropped(t *testing.T) {
	h := newHarness(t)
	h.session(t, "s3")
	h.run(hookJSON(map[string]any{"session_id": "s3", "prompt": "x"}), "hook", "user-prompt-submit")
	h.run(`{"session_id":"s3","reason":"logout"}`, "hook", "session-end") // no Stop came: the end closes the turn
	h.run("", h.spawns[len(h.spawns)-1]...)
	if len(h.srv.Spans["run-1"]) != 1 || h.srv.Ended[0] != "run-1" {
		t.Fatalf("spans %v ended %v", h.srv.Spans, h.srv.Ended)
	}

	h2 := newHarness(t)
	h2.session(t, "s4")
	h2.srv.SpansTooLate = true
	h2.run(hookJSON(map[string]any{"session_id": "s4", "prompt": "y"}), "hook", "user-prompt-submit")
	h2.run(hookJSON(map[string]any{"session_id": "s4"}), "hook", "stop")
	h2.run("", "flush")
	if _, err := os.Stat(spoolPath("run-1")); !os.IsNotExist(err) {
		t.Fatal("a spool the server won't take any more must be dropped")
	}
}

func TestUnknownSessionAndUnknownEventsAreNoOps(t *testing.T) {
	h := newHarness(t)
	h.signIn(time.Hour)
	if code, out, _ := h.run(`{"session_id":"nobody","prompt":"hi"}`, "hook", "user-prompt-submit"); code != 0 || out != "" {
		t.Fatal("no session: no-op")
	}
	if code, _, _ := h.run(`{}`, "hook", "some-future-event"); code != 0 {
		t.Fatal("a newer plugin's event must not break an older binary")
	}
	if entries, _ := os.ReadDir(spoolDir()); len(entries) != 0 {
		t.Fatalf("spooled for no session: %v", entries)
	}
}
