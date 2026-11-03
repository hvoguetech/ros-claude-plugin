package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func request(id, text string, seq int) map[string]any {
	return map[string]any{"message_id": id, "sender_task_id": "reviewer-task", "kind": "request",
		"body": map[string]any{"text": text, "data": map[string]any{}}, "sequence": seq, "response_to": nil}
}

func (h *harness) mcpCall(name string) map[string]any {
	for _, c := range h.srv.MCPCalls {
		if c["name"] == name {
			return c["arguments"].(map[string]any)
		}
	}
	return nil
}

func TestWatchAnswersARequestWithHeadlessClaudeCode(t *testing.T) {
	h := newHarness(t)
	h.signIn(time.Hour)
	h.srv.WaitPages = [][]map[string]any{{request("m1", "Does the build pass on your machine?", 7)}}
	var ran []string
	var ranIn string
	h.runProgram = func(_ context.Context, dir string, argv []string) (string, error) {
		ran, ranIn = argv, dir
		return "Yes — 212 tests pass.\n", nil
	}
	code, out, errb := h.run("", "watch", "--once", "--name", "Marut's laptop", "--dir", h.repo, "--max-turns", "5")
	if code != 0 {
		t.Fatalf("code %d: %s %s", code, out, errb)
	}
	if h.srv.Sessions[0]["harness"] != "ros-local-watch" {
		t.Fatalf("session %v", h.srv.Sessions[0])
	}
	join := h.mcpCall("peer_join")
	if join == nil || join["title"] != "Marut's laptop" || join["open"] != true || join["session"] != "run-1" {
		t.Fatalf("peer_join %v", join)
	}
	if ack := h.mcpCall("peer_ack"); ack == nil || ack["message_ids"].([]any)[0] != "m1" {
		t.Fatalf("peer_ack %v", ack)
	}
	reply := h.mcpCall("peer_reply")
	if reply == nil || reply["request_id"] != "m1" || reply["text"] != "Yes — 212 tests pass." {
		t.Fatalf("peer_reply %v", reply)
	}
	if ranIn != h.repo || len(ran) < 7 || ran[0] != "claude" || ran[1] != "-p" || !strings.Contains(ran[2], "Does the build pass on your machine?") ||
		ran[3] != "--output-format" || ran[5] != "--max-turns" || ran[6] != "5" {
		t.Fatalf("claude argv %v in %s", ran, ranIn)
	}
	if !strings.Contains(out, `Watching as "Marut's laptop"`) || !strings.Contains(out, "replied to m1") {
		t.Fatalf("out %s", out)
	}
	if len(h.srv.Ended) != 1 || h.srv.Ended[0] != "run-1" {
		t.Fatalf("the watch session should be ended on exit: %v", h.srv.Ended)
	}
}

func TestWatchTellsTheAskerWhenItCannotAnswer(t *testing.T) {
	h := newHarness(t)
	h.signIn(time.Hour)
	h.srv.WaitPages = [][]map[string]any{
		{{"message_id": "n0", "sender_task_id": "x", "kind": "note", "body": map[string]any{"text": "fyi"}, "sequence": 1}},
		{request("m2", "Run the full suite", 2)},
	}
	h.runProgram = func(context.Context, string, []string) (string, error) { return "", errors.New("claude: not found") }
	code, out, _ := h.run("", "watch", "--once", "--dir", h.repo, "--permission-mode", "acceptEdits")
	if code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	reply := h.mcpCall("peer_reply")
	if reply == nil || !strings.HasPrefix(reply["text"].(string), "Sorry — I couldn't work this out") {
		t.Fatalf("reply %v", reply)
	}
	acked := 0
	for _, c := range h.srv.MCPCalls {
		if c["name"] == "peer_ack" {
			acked++
		}
	}
	if acked != 2 || !strings.Contains(out, "note from x: fyi") {
		t.Fatalf("acks %d out %s", acked, out)
	}
}

func TestDefaultWatchName(t *testing.T) {
	if got := defaultWatchName("marut@example.com"); got != "marut's laptop" {
		t.Fatal(got)
	}
	if got := defaultWatchName(""); got == "" {
		t.Fatal("empty")
	}
}

func TestPromptHookSurfacesWaitingPeerMessages(t *testing.T) {
	h := newHarness(t)
	h.signIn(time.Hour)
	if err := saveState("s1", &sessionState{RunID: "run-9", ProjectID: "p1", URL: h.srv.URL, OpenedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// nothing pending: the hook stays silent
	code, out, _ := h.run(`{"session_id":"s1","hook_event_name":"UserPromptSubmit"}`, "hook", "user-prompt-submit")
	if code != 0 || strings.TrimSpace(out) != "" {
		t.Fatalf("code %d out %q", code, out)
	}
	h.srv.PeerSummary = map[string]any{"pending": "5+", "latest": []map[string]any{
		{"message_id": "m1", "from": "reviewer-task", "kind": "request", "preview": "Does the build pass?"}}}
	code, out, _ = h.run(`{"session_id":"s1","hook_event_name":"UserPromptSubmit"}`, "hook", "user-prompt-submit")
	if code != 0 {
		t.Fatal(code)
	}
	o := decodeHook(t, out)
	ac := o.HookSpecificOutput.AdditionalContext
	if o.HookSpecificOutput.HookEventName != "UserPromptSubmit" || !strings.Contains(ac, `pending="5+"`) ||
		!strings.Contains(ac, "[m1] request from reviewer-task: Does the build pass?") || !strings.Contains(ac, "session=run-9") {
		t.Fatalf("context %q", ac)
	}
	// an unknown session (no state file) is silent too
	if code, out, _ = h.run(`{"session_id":"nope"}`, "hook", "user-prompt-submit"); code != 0 || out != "" {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestWatchLongPollOutlivesTheRequestTimeout(t *testing.T) {
	h := newHarness(t)
	h.signIn(time.Hour)
	h.httpTimeout = 300 * time.Millisecond   // the usual per-request budget…
	h.srv.WaitDelay = 900 * time.Millisecond // …which an empty long-poll exceeds, and must not trip
	h.srv.WaitPages = [][]map[string]any{nil, {request("m3", "ping", 3)}}
	h.runProgram = func(context.Context, string, []string) (string, error) { return "pong", nil }
	code, _, errb := h.run("", "watch", "--once", "--dir", h.repo)
	if code != 0 || strings.Contains(errb, "retrying") {
		t.Fatalf("code %d: %s", code, errb)
	}
	if reply := h.mcpCall("peer_reply"); reply == nil || reply["text"] != "pong" {
		t.Fatalf("reply %v", reply)
	}
}
