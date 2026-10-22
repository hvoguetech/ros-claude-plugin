package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hvoguetech/ros-claude-plugin/cli/internal/config"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/fakeros"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/gitx"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/link"
)

type harness struct {
	t      *testing.T
	srv    *fakeros.Server
	repo   string
	env    map[string]string
	spawns [][]string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	srv := fakeros.New()
	t.Cleanup(srv.Close)
	t.Setenv("ROS_HOME", t.TempDir())
	t.Setenv("ROS_STORE", "file")
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{URL: srv.URL, Store: "file", DeviceID: "dev1"}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, srv: srv, repo: repo, env: map[string]string{"ROS_URL": srv.URL}}
}

func (h *harness) signIn(expiresIn time.Duration) string {
	tok := h.srv.IssueToken()
	if err := (config.FileStore{}).Set(h.srv.URL, &config.Credentials{AccessToken: tok,
		AccessExpiresAt: time.Now().Add(expiresIn), RefreshToken: "refresh-1"}); err != nil {
		h.t.Fatal(err)
	}
	return tok
}

func (h *harness) run(stdin string, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	a := &App{Version: "0.1.0", Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb,
		Getenv: func(k string) string { return h.env[k] }, Getwd: func() (string, error) { return h.repo, nil },
		HTTPTimeout: 5 * time.Second,
		Spawn:       func(args ...string) error { h.spawns = append(h.spawns, args); return nil }}
	code := a.Run(args)
	return code, out.String(), errb.String()
}

func decodeHook(t *testing.T, s string) hookOutput {
	t.Helper()
	var out hookOutput
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("hook output isn't JSON: %v\n%s", err, s)
	}
	return out
}

func TestCapContext(t *testing.T) {
	item := "- [mem:abcd] " + strings.Repeat("é lesson text ", 20) + "\n"
	ctx := `<ros-memory source="ros" project="P" profile="everything">` + "\n" + strings.Repeat(item, 200) + "</ros-memory>"
	got := CapContext(ctx, MaxHookContext)
	if len(got) > MaxHookContext || !utf8.ValidString(got) {
		t.Fatalf("len %d valid %v", len(got), utf8.ValidString(got))
	}
	if !strings.HasSuffix(got, "</ros-memory>") || strings.Count(got, "</ros-memory>") != 1 {
		t.Fatal("truncated block must be closed exactly once")
	}
	if !strings.Contains(got, "truncated") {
		t.Fatal("truncation should be stated")
	}
	if short := "small"; CapContext(short, MaxHookContext) != short {
		t.Fatal("short context changed")
	}
}

func TestSessionStartSignedOut(t *testing.T) {
	h := newHarness(t)
	code, out, _ := h.run(`{"session_id":"s1","source":"startup"}`, "hook", "session-start")
	o := decodeHook(t, out)
	if code != 0 || o.SystemMessage != signedOutBanner {
		t.Fatalf("code %d banner %q", code, o.SystemMessage)
	}
	if o.HookSpecificOutput == nil || !strings.Contains(o.HookSpecificOutput.AdditionalContext, "ros-local login") {
		t.Fatal("model should be told ROS is signed out")
	}
}

func TestSessionStartRevokedDeviceIsSignedOut(t *testing.T) {
	h := newHarness(t)
	h.signIn(time.Minute) // inside the refresh window
	h.srv.RefreshError = "invalid_grant"
	_, out, _ := h.run(`{"session_id":"s1"}`, "hook", "session-start")
	if decodeHook(t, out).SystemMessage != signedOutBanner {
		t.Fatalf("got %s", out)
	}
}

func TestSessionStartOpensRunSyncsSkillsAndCapsRecall(t *testing.T) {
	h := newHarness(t)
	h.signIn(time.Hour)
	h.srv.Skills = []fakeros.Skill{{Name: "deploy", ID: "s1", Version: 1, Files: [][2]string{{"SKILL.md", "---\nname: deploy\n---\n"}}}}
	// A server block over the hook limit (the server should never send this, but the client must hold).
	h.srv.Context = `<ros-memory source="ros" project="Payments" profile="everything">` + "\n" +
		strings.Repeat("- [fact:1] a recalled project fact that is fairly long\n", 400) + "</ros-memory>"

	code, out, _ := h.run(`{"session_id":"sess-1","source":"startup","cwd":"`+h.repo+`"}`, "hook", "session-start")
	if code != 0 {
		t.Fatal(code)
	}
	o := decodeHook(t, out)
	ac := o.HookSpecificOutput.AdditionalContext
	if len(ac) > MaxHookContext || len(ac) >= 10000 {
		t.Fatalf("additionalContext is %d bytes", len(ac))
	}
	if !strings.HasPrefix(ac, `ROS · project "Payments"`) || !strings.HasSuffix(ac, "</ros-memory>") {
		t.Fatalf("context shape: %q … %q", ac[:60], ac[len(ac)-40:])
	}
	if !strings.Contains(o.SystemMessage, "1 skills (synced just now)") ||
		!strings.Contains(o.SystemMessage, "run /reload-skills") {
		t.Fatalf("banner %q", o.SystemMessage)
	}

	// The run was opened with the session's facts.
	body := h.srv.Sessions[0]
	if body["harness_session_id"] != "sess-1" || body["source"] != "startup" || body["harness"] != "claude-code" {
		t.Fatalf("session body %v", body)
	}
	// Skills landed, the repo got linked and git-excluded.
	if _, err := os.Stat(filepath.Join(h.repo, ".claude", "skills", "ros-deploy", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	r, _ := gitx.Find(h.repo)
	if l, _ := link.Read(r); l == nil || l.ProjectID != "p1" {
		t.Fatalf("link %+v", l)
	}
	if ex, _ := os.ReadFile(filepath.Join(h.repo, ".git", "info", "exclude")); !strings.Contains(string(ex), ".claude/skills/ros-*") {
		t.Fatalf("exclude %q", ex)
	}

	// A second session: the skills dir existed, so no reload hint, and nothing is re-downloaded.
	_, out, _ = h.run(`{"session_id":"sess-2","source":"resume"}`, "hook", "session-start")
	if o := decodeHook(t, out); strings.Contains(o.SystemMessage, "reload-skills") || len(h.srv.BundleCalls) != 1 {
		t.Fatalf("second start: %q bundles=%d", o.SystemMessage, len(h.srv.BundleCalls))
	}
	if h.srv.Sessions[1]["project_id"] != "p1" {
		t.Fatal("linked project not sent")
	}
}

func TestSessionStartProjectDisabled(t *testing.T) {
	h := newHarness(t)
	h.signIn(time.Hour)
	h.srv.SessionStatus, h.srv.SessionDetail = 403, "the local agent is not enabled for this project"
	code, out, _ := h.run(`{"session_id":"s"}`, "hook", "session-start")
	if code != 0 || decodeHook(t, out).SystemMessage != "ROS: the local agent is not enabled for this project" {
		t.Fatalf("%d %s", code, out)
	}
}

func TestSessionStartOffline(t *testing.T) {
	h := newHarness(t)
	h.signIn(time.Hour)
	h.srv.Close()
	code, out, _ := h.run(`{"session_id":"s"}`, "hook", "session-start")
	if code != 0 || !strings.HasPrefix(decodeHook(t, out).SystemMessage, "ROS: offline") {
		t.Fatalf("%d %s", code, out)
	}
}

func TestSessionEndDetachesThenEnds(t *testing.T) {
	h := newHarness(t)
	h.signIn(time.Hour)
	h.run(`{"session_id":"sess-1"}`, "hook", "session-start")

	start := time.Now()
	code, out, _ := h.run(`{"session_id":"sess-1","reason":"prompt_input_exit"}`, "hook", "session-end")
	if code != 0 || out != "" || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("session-end hook must return at once: code %d out %q", code, out)
	}
	if len(h.spawns) != 1 || len(h.srv.Ended) != 0 {
		t.Fatalf("spawns %v ended %v", h.spawns, h.srv.Ended)
	}
	// What the detached process does.
	h.run("", h.spawns[0]...)
	if len(h.srv.Ended) != 1 || h.srv.Ended[0] != "run-1" {
		t.Fatalf("ended %v", h.srv.Ended)
	}
	if loadState("sess-1") != nil {
		t.Fatal("state not cleaned up")
	}
	// Unknown session: nothing to do, nothing spawned.
	h.run(`{"session_id":"other"}`, "hook", "session-end")
	if len(h.spawns) != 1 {
		t.Fatal("spawned for an unknown session")
	}
}

func TestMCPHeaders(t *testing.T) {
	h := newHarness(t)
	tok := h.signIn(time.Hour)
	r, _ := gitx.Find(h.repo)
	_ = link.Write(r, &link.Link{ProjectID: "p1"})

	sub := filepath.Join(h.repo, "pkg")
	_ = os.MkdirAll(sub, 0o755)
	code, out, errOut := h.run("", "mcp-headers", "--project-dir", sub)
	if code != 0 {
		t.Fatalf("%d %s", code, errOut)
	}
	var headers map[string]string
	if err := json.Unmarshal([]byte(out), &headers); err != nil {
		t.Fatalf("not JSON: %q", out)
	}
	if headers["Authorization"] != "Bearer "+tok || headers["X-ROS-Project"] != "p1" || len(headers) != 2 {
		t.Fatalf("%v", headers)
	}

	// The placeholder wasn't substituted: fall back to $CLAUDE_PROJECT_DIR.
	for _, v := range []string{"${CLAUDE_PROJECT_DIR}", ""} {
		h.env["CLAUDE_PROJECT_DIR"] = sub
		_, out, _ = h.run("", "mcp-headers", "--project-dir", v)
		if !strings.Contains(out, `"X-ROS-Project":"p1"`) {
			t.Fatalf("fallback for %q: %s", v, out)
		}
	}
	// ...but never to the working directory (Claude Code runs the helper in the plugin dir).
	delete(h.env, "CLAUDE_PROJECT_DIR")
	_, out, _ = h.run("", "mcp-headers", "--project-dir", "${CLAUDE_PROJECT_DIR}")
	if strings.Contains(out, "X-ROS-Project") {
		t.Fatalf("used the working directory: %s", out)
	}

	// Outside a linked repo: token only, server uses the default project.
	_, out, _ = h.run("", "mcp-headers", "--project-dir", t.TempDir())
	if strings.Contains(out, "X-ROS-Project") || !strings.Contains(out, "Bearer "+tok) {
		t.Fatalf("unlinked: %s", out)
	}
}

func TestMCPHeadersRefreshesAheadOfExpiry(t *testing.T) {
	h := newHarness(t)
	old := h.signIn(2 * time.Minute)
	_, out, _ := h.run("", "mcp-headers", "--project-dir", h.repo)
	if h.srv.RefreshCalls != 1 || strings.Contains(out, old) || !strings.Contains(out, "Bearer access-") {
		t.Fatalf("refreshes %d out %s", h.srv.RefreshCalls, out)
	}
}

func TestMCPHeadersSignedOut(t *testing.T) {
	h := newHarness(t)
	code, out, errOut := h.run("", "mcp-headers", "--project-dir", h.repo)
	if code == 0 || out != "" || !strings.Contains(errOut, "ros-local login") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
}

func TestResolveProjectDir(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		flag string
		env  map[string]string
		want string
	}{
		{"/repo", map[string]string{"CLAUDE_PROJECT_DIR": "/env"}, "/repo"},
		{"${CLAUDE_PROJECT_DIR}", map[string]string{"CLAUDE_PROJECT_DIR": "/env"}, "/env"},
		{"", map[string]string{"CLAUDE_PROJECT_DIR": "/env"}, "/env"},
		{"$CLAUDE_PROJECT_DIR", nil, ""},
		{"", nil, ""},
	}
	for _, c := range cases {
		if got := resolveProjectDir(c.flag, env(c.env)); got != c.want {
			t.Errorf("resolveProjectDir(%q, %v) = %q, want %q", c.flag, c.env, got, c.want)
		}
	}
}

func TestLinkCommandMatchesRemote(t *testing.T) {
	h := newHarness(t)
	h.signIn(time.Hour)
	h.srv.Projects["p2"] = "Search"
	h.srv.Repos["github.com/acme/search"] = "p2"
	if err := os.WriteFile(filepath.Join(h.repo, ".git", "config"),
		[]byte("[remote \"origin\"]\n\turl = git@github.com:acme/search.git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(h.repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(h.repo, ".git", "objects"), 0o755)
	_ = os.MkdirAll(filepath.Join(h.repo, ".git", "refs"), 0o755)
	code, out, errOut := h.run("", "link")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	r, _ := gitx.Find(h.repo)
	l, _ := link.Read(r)
	if l == nil || l.ProjectID != "p2" || l.Source != "remote" {
		t.Fatalf("link %+v (%s)", l, out)
	}
	if _, err := os.Stat(filepath.Join(h.repo, ".claude", "skills")); err != nil {
		t.Fatal("link must pre-create .claude/skills")
	}
}

func TestVersionLess(t *testing.T) {
	cases := []struct {
		have, min string
		want      bool
	}{
		{"0.1.0", "0.2.0", true}, {"v1.2.3", "1.2.3", false}, {"1.10.0", "1.9.9", false},
		{"1.2", "1.2.1", true}, {"dev", "1.0.0", false}, {"1.0.0-rc1", "1.0.0", false},
	}
	for _, c := range cases {
		if got := versionLess(c.have, c.min); got != c.want {
			t.Errorf("versionLess(%q, %q) = %v", c.have, c.min, got)
		}
	}
}

func (h *harness) linkTo(projectID, name string) *gitx.Repo {
	h.t.Helper()
	r, _ := gitx.Find(h.repo)
	if err := link.Write(r, &link.Link{ProjectID: projectID, ProjectName: name, Source: "explicit"}); err != nil {
		h.t.Fatal(err)
	}
	return r
}

func TestStaleLinkIsReportedAndBypassed(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "project_unavailable", true: "legacy 404"}[legacy], func(t *testing.T) {
			h := newHarness(t)
			h.signIn(time.Hour)
			h.srv.UnavailableProjects["p-gone"] = "deleted"
			h.srv.LegacyUnavailable = legacy
			r := h.linkTo("p-gone", "Old Payments")

			code, out, _ := h.run(`{"session_id":"s1"}`, "hook", "session-start")
			o := decodeHook(t, out)
			if code != 0 || !strings.Contains(o.SystemMessage, `linked to project "Old Payments", which is unavailable`) ||
				!strings.Contains(o.SystemMessage, "run `ros-local link`") {
				t.Fatalf("banner %q", o.SystemMessage)
			}
			// The session still opened, on the server's fallback project.
			if !strings.HasPrefix(o.SystemMessage, `ROS · project "Payments"`) || len(h.srv.Sessions) != 2 {
				t.Fatalf("no fallback session: %q, %d calls", o.SystemMessage, len(h.srv.Sessions))
			}
			if _, sent := h.srv.Sessions[1]["project_id"]; sent {
				t.Fatal("retry still sent the stale project")
			}
			l, _ := link.Read(r)
			if !l.Stale || l.ProjectID != "p-gone" {
				t.Fatalf("link not marked stale: %+v", l)
			}
			if !legacy && l.StaleReason != "deleted" {
				t.Fatalf("reason %q", l.StaleReason)
			}

			// Next session: the stale project isn't sent at all, and the banner keeps saying so.
			_, out, _ = h.run(`{"session_id":"s2"}`, "hook", "session-start")
			if !strings.Contains(decodeHook(t, out).SystemMessage, "run `ros-local link`") {
				t.Fatal("stale banner went quiet")
			}
			if _, sent := h.srv.Sessions[2]["project_id"]; sent || len(h.srv.Sessions) != 3 {
				t.Fatal("stale project sent again")
			}

			// mcp-headers leaves the stale project out so tool calls fall back too.
			_, out, _ = h.run("", "mcp-headers", "--project-dir", h.repo)
			if strings.Contains(out, "X-ROS-Project") || !strings.Contains(out, "Bearer") {
				t.Fatalf("headers %s", out)
			}

			// `ros-local link` replaces it, and the banner clears.
			if code, out, errOut := h.run("", "link", "--project", "p1"); code != 0 {
				t.Fatalf("link: %s %s", out, errOut)
			}
			_, out, _ = h.run(`{"session_id":"s3"}`, "hook", "session-start")
			if strings.Contains(decodeHook(t, out).SystemMessage, "unavailable") {
				t.Fatal("banner still stale after relinking")
			}
		})
	}
}

func TestLocalAgentDisabled(t *testing.T) {
	h := newHarness(t)
	h.signIn(time.Hour)
	h.srv.LocalAgentDisabled = true
	code, out, _ := h.run(`{"session_id":"s1"}`, "hook", "session-start")
	if code != 0 || decodeHook(t, out).SystemMessage != "ROS: Local agent isn't enabled for your org/project" {
		t.Fatalf("%d %s", code, out)
	}
	code, _, errOut := h.run("", "link")
	if code == 0 || !strings.Contains(errOut, "Local agent isn't enabled for your org/project") {
		t.Fatalf("link: %d %s", code, errOut)
	}
	_, out, _ = h.run("", "status")
	if !strings.Contains(out, "Local agent isn't enabled for your org/project") {
		t.Fatalf("status: %s", out)
	}
}

func TestMinVersionsTurnROSOff(t *testing.T) {
	plugin := t.TempDir()
	_ = os.MkdirAll(filepath.Join(plugin, ".claude-plugin"), 0o755)
	_ = os.WriteFile(filepath.Join(plugin, ".claude-plugin", "plugin.json"), []byte(`{"name":"ros","version":"0.1.0"}`), 0o644)
	cases := []struct {
		minPlugin, minCLI, want string
	}{
		{"0.2.0", "", "ROS plugin out of date — run `claude plugin update ros`; ROS is off for this session"},
		{"", "0.3.1", "ros-local out of date — run `brew upgrade ros-local`; ROS is off for this session"},
		{"0.1.0", "0.1.0", `ROS · project "Payments"`},
	}
	for _, c := range cases {
		h := newHarness(t)
		h.signIn(time.Hour)
		h.env["CLAUDE_PLUGIN_ROOT"] = plugin
		h.srv.MinPluginVersion, h.srv.MinCLIVersion = c.minPlugin, c.minCLI
		_, out, _ := h.run(`{"session_id":"s"}`, "hook", "session-start")
		o := decodeHook(t, out)
		if !strings.HasPrefix(o.SystemMessage, c.want) {
			t.Errorf("min plugin %q cli %q: banner %q", c.minPlugin, c.minCLI, o.SystemMessage)
		}
		if c.want != cases[2].want && o.HookSpecificOutput != nil {
			t.Errorf("ROS context given to an out-of-date client: %+v", o.HookSpecificOutput)
		}
		if h.srv.Sessions[0]["plugin_version"] != "0.1.0" {
			t.Errorf("plugin version not sent: %v", h.srv.Sessions[0]["plugin_version"])
		}
	}
}
