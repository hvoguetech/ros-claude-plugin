package app

// Retain (spec §8.3): the session's turns — what the person asked, what Claude answered — into the person's own
// ROS memory, as the project's `retain` setting allows. `auto` sends each new turn after Stop; `ask` sends the
// session when the person runs /ros:remember-session. Secrets are scrubbed here first and again by ROS.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/hvoguetech/ros-claude-plugin/cli/internal/api"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/config"
)

// secretPatterns: credentials that must never leave the machine in a turn (ROS scrubs again, more broadly).
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{10,}`),
	regexp.MustCompile(`sk-[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`xox[abpr]-[A-Za-z0-9\-]{10,}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)\b(password|passwd|secret|api[_-]?key|token)\s*[:=]\s*["']?[^\s"']{6,}`),
}

func scrub(s string) string {
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, "<redacted>")
	}
	return s
}

func retainedPath(runID string) string {
	return strings.TrimSuffix(spoolPath(runID), ".jsonl") + ".retained"
}

// readTurns pairs each real prompt in the transcript with Claude's text until the next one. Tool results,
// meta lines, sidechains (subagents) and command echoes aren't prompts.
func readTurns(path string) []api.Turn {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var turns []api.Turn
	var cur *api.Turn
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 256<<10), 16<<20)
	for sc.Scan() {
		var ln struct {
			Type        string `json:"type"`
			UUID        string `json:"uuid"`
			Timestamp   string `json:"timestamp"`
			IsMeta      bool   `json:"isMeta"`
			IsSidechain bool   `json:"isSidechain"`
			Message     struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &ln) != nil || ln.IsSidechain {
			continue
		}
		switch ln.Type {
		case "user":
			var text string
			if ln.IsMeta || json.Unmarshal(ln.Message.Content, &text) != nil || ln.UUID == "" {
				continue // tool results come as arrays; meta lines aren't the person
			}
			text = strings.TrimSpace(text)
			if text == "" || strings.HasPrefix(text, "<") {
				continue // <command-name>, <local-command-stdout>, … echoes
			}
			if cur != nil {
				turns = append(turns, *cur)
			}
			cur = &api.Turn{ID: ln.UUID, Prompt: text, At: ln.Timestamp}
		case "assistant":
			if cur != nil {
				if t := assistantText(ln.Message.Content); t != "" {
					if cur.Reply != "" {
						cur.Reply += "\n"
					}
					cur.Reply += t
				}
			}
		}
	}
	if cur != nil {
		turns = append(turns, *cur)
	}
	for i := range turns {
		turns[i].Prompt, turns[i].Reply = scrub(turns[i].Prompt), scrub(turns[i].Reply)
	}
	return turns
}

// retainTurns sends the transcript's turns not yet retained for this run; returns how many ROS saved.
func retainTurns(ctx context.Context, c *api.Client, runID, transcript string, explicit bool) (*api.RetainResult, error) {
	done := map[string]bool{}
	if b, err := os.ReadFile(retainedPath(runID)); err == nil {
		for _, id := range strings.Split(string(b), "\n") {
			done[id] = id != ""
		}
	}
	var todo []api.Turn
	for _, t := range readTurns(transcript) {
		if !done[t.ID] {
			todo = append(todo, t)
		}
	}
	if len(todo) > 200 {
		todo = todo[len(todo)-200:]
	}
	if len(todo) == 0 {
		return &api.RetainResult{}, nil
	}
	res, err := c.Retain(ctx, runID, todo, explicit)
	if err != nil {
		return nil, err
	}
	if res.Skipped == "" {
		f, ferr := os.OpenFile(retainedPath(runID), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if ferr == nil {
			for _, t := range todo {
				fmt.Fprintln(f, t.ID)
			}
			f.Close()
		}
	}
	return res, nil
}

// lastTranscript is the newest transcript the session's Stop hooks recorded.
func lastTranscript(runID string) string {
	path := ""
	for _, ev := range readEvents(runID) {
		if ev.Transcript != "" {
			path = ev.Transcript
		}
	}
	return path
}

// rememberSession is /ros:remember-session: this repo's current session into the person's memory, now.
func (a *App) rememberSession() int {
	st := a.currentSession()
	if st == nil {
		return a.errorf("no open ROS session for this repo")
	}
	transcript := firstNonEmpty(a.Getenv("CLAUDE_TRANSCRIPT_PATH"), lastTranscript(st.RunID))
	if transcript == "" {
		return a.errorf("nothing to save yet: finish a turn first")
	}
	cfg, err := config.Load()
	if err != nil || cfg.URL != st.URL {
		return a.errorf("this session belongs to another ROS sign-in")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	token, err := a.session(cfg).AccessToken(ctx)
	if err != nil {
		return a.errorf("%v", err)
	}
	res, err := retainTurns(ctx, a.client(cfg.URL, token), st.RunID, transcript, true)
	if err != nil {
		return a.errorf("%v", err)
	}
	switch res.Skipped {
	case "retain_off":
		fmt.Fprintln(a.Stdout, "This project doesn't keep sessions in memory (Local agent → Retain from sessions is off).")
		return 1
	case "":
		fmt.Fprintf(a.Stdout, "Saved %d turn(s) of this session to your own ROS memory (secrets removed).\n", res.Saved)
	}
	return 0
}
