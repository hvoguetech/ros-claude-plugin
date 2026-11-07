package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeChat(t *testing.T) string {
	t.Helper()
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	lines := []string{
		`{"type":"user","uuid":"meta","isMeta":true,"timestamp":"` + ts + `","message":{"content":"caveat"}}`,
		`{"type":"user","uuid":"p1","timestamp":"` + ts + `","message":{"content":"add idempotency keys, my token is sk-ant-api03-SECRETSECRETSECRET99"}}`,
		`{"type":"assistant","timestamp":"` + ts + `","message":{"id":"m1","content":[{"type":"text","text":"Adding them."},{"type":"tool_use","name":"Edit"}]}}`,
		`{"type":"user","uuid":"tr","timestamp":"` + ts + `","message":{"content":[{"type":"tool_result","content":"ok"}]}}`,
		`{"type":"assistant","timestamp":"` + ts + `","message":{"id":"m2","content":[{"type":"text","text":"Done: keys on refunds."}]}}`,
		`{"type":"user","uuid":"cmd","timestamp":"` + ts + `","message":{"content":"<command-name>/clear</command-name>"}}`,
		`{"type":"user","uuid":"side","isSidechain":true,"timestamp":"` + ts + `","message":{"content":"subagent prompt"}}`,
		`{"type":"user","uuid":"p2","timestamp":"` + ts + `","message":{"content":"now the tests"}}`,
		`{"type":"assistant","timestamp":"` + ts + `","message":{"id":"m3","content":[{"type":"text","text":"Wrote 4 tests."}]}}`,
	}
	p := filepath.Join(t.TempDir(), "chat.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadTurnsKeepsRealPromptsAndScrubsSecrets(t *testing.T) {
	turns := readTurns(writeChat(t))
	if len(turns) != 2 || turns[0].ID != "p1" || turns[1].ID != "p2" {
		t.Fatalf("turns: %+v", turns)
	}
	if strings.Contains(turns[0].Prompt, "SECRET") || !strings.Contains(turns[0].Prompt, "<redacted>") {
		t.Fatalf("secret left the machine: %q", turns[0].Prompt)
	}
	if turns[0].Reply != "Adding them.\nDone: keys on refunds." || turns[1].Reply != "Wrote 4 tests." {
		t.Fatalf("replies: %q / %q", turns[0].Reply, turns[1].Reply)
	}
}

func TestAutoRetainSendsNewTurnsAfterStopOnce(t *testing.T) {
	h := newHarness(t)
	h.srv.RetainMode = "auto"
	h.session(t, "r1")
	chat := writeChat(t)
	_, out, _ := h.run(`{"session_id":"r2"}`, "hook", "session-start")
	if !strings.Contains(decodeHook(t, out).SystemMessage, "saved to your own memory as you work") {
		t.Fatalf("banner: %s", out)
	}
	h.run(hookJSON(map[string]any{"session_id": "r1", "transcript_path": chat}), "hook", "stop")
	h.run("", h.spawns[len(h.spawns)-1]...)
	h.run("", "flush")
	if len(h.srv.Retained) != 2 || h.srv.RetainExplicit[0] {
		t.Fatalf("auto retain: %v explicit %v", h.srv.Retained, h.srv.RetainExplicit)
	}
}

func TestAskRetainWaitsForRememberSession(t *testing.T) {
	h := newHarness(t)
	h.env["CLAUDE_PROJECT_DIR"] = h.repo
	h.srv.RetainMode = "ask"
	h.session(t, "a1")
	chat := writeChat(t)
	h.run(hookJSON(map[string]any{"session_id": "a1", "transcript_path": chat}), "hook", "stop")
	h.run("", h.spawns[len(h.spawns)-1]...)
	if len(h.srv.Retained) != 0 {
		t.Fatal("ask: nothing until the person asks")
	}
	code, out, _ := h.run("", "remember-session")
	if code != 0 || len(h.srv.Retained) != 2 || !h.srv.RetainExplicit[0] || !strings.Contains(out, "Saved 2 turn(s)") {
		t.Fatalf("remember-session: code %d out %q retained %v", code, out, h.srv.Retained)
	}
	h.srv.RetainMode = "off"
	if code, out, _ := h.run("", "remember-session"); code != 0 || !strings.Contains(out, "Saved 0") {
		// already sent turns aren't sent again (nothing new to ask about)
		t.Fatalf("second save: code %d out %q", code, out)
	}
}
