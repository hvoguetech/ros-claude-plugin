package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v\n%s", path, err, b)
	}
	return m
}

// Claude Code fails an apiKeyHelper that prints anything but the credential.
func TestModelTokenPrintsOnlyTheToken(t *testing.T) {
	h := newHarness(t)
	h.signIn(time.Hour)
	h.linkTo("p1", "Payments")
	code, out, errs := h.run("", "model-token")
	if code != 0 || out != "model-token-1" || errs != "" {
		t.Fatalf("code %d out %q err %q", code, out, errs)
	}
	if got := h.srv.ModelTokenBodies[0]["project_id"]; got != "p1" {
		t.Fatalf("the repo's linked project pays: got %v", got)
	}
	// Claude Code runs the helper outside the repo; the flag (or $CLAUDE_PROJECT_DIR) names it.
	h.env["CLAUDE_PROJECT_DIR"] = h.repo
	if code, out, _ := h.run("", "model-token"); code != 0 || out != "model-token-1" {
		t.Fatalf("with CLAUDE_PROJECT_DIR: code %d out %q", code, out)
	}
}

func TestModelTokenSaysWhatToDoWhenRefused(t *testing.T) {
	for fail, want := range map[string]string{
		"model_access_own":   "your own model login",
		"no_provider_key":    "no usable Anthropic API key",
		"insufficient_scope": "ros-local login",
	} {
		h := newHarness(t)
		h.signIn(time.Hour)
		h.srv.ModelTokenError = fail
		code, out, errs := h.run("", "model-token")
		if code == 0 || out != "" || !strings.Contains(errs, want) {
			t.Fatalf("%s: code %d out %q err %q", fail, code, out, errs)
		}
	}
	h := newHarness(t) // not signed in at all
	if code, out, _ := h.run("", "model-token"); code == 0 || out != "" {
		t.Fatalf("signed out: code %d out %q", code, out)
	}
}

func TestGatewaySetupWritesTheReposLocalSettingsAndOffUndoesIt(t *testing.T) {
	h := newHarness(t)
	h.signIn(time.Hour)
	h.linkTo("p1", "Payments")
	h.srv.GatewayModels = []string{"claude-sonnet", "claude-haiku-5-5"}
	path := filepath.Join(h.repo, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// What the developer already had stays.
	if err := os.WriteFile(path, []byte(`{"permissions": {"allow": ["Bash(ls:*)"]}, "env": {"FOO": "bar"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := h.run("", "gateway", "setup")
	if code != 0 {
		t.Fatalf("setup: %d %s %s", code, out, errs)
	}
	s := readJSON(t, path)
	env := s["env"].(map[string]any)
	if s["apiKeyHelper"] != "ros-local model-token --project-dir "+quoteArg(h.repo) {
		t.Fatalf("apiKeyHelper = %v", s["apiKeyHelper"])
	}
	if env["ANTHROPIC_BASE_URL"] != h.srv.URL+"/model-gateway" || env["CLAUDE_CODE_API_KEY_HELPER_TTL_MS"] != "300000" || env["FOO"] != "bar" {
		t.Fatalf("env = %v", env)
	}
	if s["permissions"] == nil {
		t.Fatal("the developer's own settings were dropped")
	}
	if !strings.Contains(out, "claude-sonnet, claude-haiku-5-5") || !strings.Contains(out, "never reaches this machine") {
		t.Fatalf("output: %s", out)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("settings mode %v", fi.Mode().Perm())
	}
	excl, _ := os.ReadFile(filepath.Join(h.repo, ".git", "info", "exclude"))
	if strings.Count(string(excl), ".claude/settings.local.json\n") != 1 {
		t.Fatalf("exclude: %q", excl)
	}
	if code, _, _ := h.run("", "gateway", "setup"); code != 0 { // again: same result, no duplicate exclude
		t.Fatal("second setup failed")
	}
	excl, _ = os.ReadFile(filepath.Join(h.repo, ".git", "info", "exclude"))
	if strings.Count(string(excl), ".claude/settings.local.json\n") != 1 {
		t.Fatalf("exclude repeated: %q", excl)
	}
	if _, out, _ := h.run("", "gateway", "status"); !strings.Contains(out, "on for this repo") {
		t.Fatalf("status: %s", out)
	}

	if code, out, _ := h.run("", "gateway", "off"); code != 0 || !strings.Contains(out, "your own model login again") {
		t.Fatalf("off: %d %s", code, out)
	}
	s = readJSON(t, path)
	if _, has := s["apiKeyHelper"]; has || s["permissions"] == nil {
		t.Fatalf("after off: %v", s)
	}
	if env := s["env"].(map[string]any); len(env) != 1 || env["FOO"] != "bar" {
		t.Fatalf("after off env: %v", env)
	}
	if _, out, _ := h.run("", "gateway", "off"); !strings.Contains(out, "Nothing changed") {
		t.Fatalf("second off: %s", out)
	}
}

func TestGatewaySetupRefusesBeforeWritingAnything(t *testing.T) {
	h := newHarness(t)
	h.signIn(time.Hour)
	path := filepath.Join(h.repo, ".claude", "settings.local.json")

	h.srv.ModelTokenError = "model_access_own" // ROS says no: nothing is written
	if code, _, errs := h.run("", "gateway", "setup"); code == 0 || !strings.Contains(errs, "your own model login") {
		t.Fatalf("refused setup: %d %s", code, errs)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("settings were written although ROS refused")
	}
	h.srv.ModelTokenError = ""

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	theirs := `{"apiKeyHelper": "vault kv get -field=key secret/llm"}`
	if err := os.WriteFile(path, []byte(theirs), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := h.run("", "gateway", "setup"); code == 0 || !strings.Contains(errs, "--force") {
		t.Fatalf("someone else's helper: %d %s", code, errs)
	}
	if b, _ := os.ReadFile(path); string(b) != theirs {
		t.Fatalf("their settings were changed: %s", b)
	}
	if _, out, _ := h.run("", "gateway", "off"); !strings.Contains(out, "Nothing changed") { // off never removes theirs
		t.Fatalf("off on someone else's helper: %s", out)
	}
	if code, _, _ := h.run("", "gateway", "setup", "--force"); code != 0 {
		t.Fatal("--force should replace it")
	}

	if err := os.WriteFile(path, []byte(`{not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := h.run("", "gateway", "setup"); code == 0 || !strings.Contains(errs, "isn't valid JSON") {
		t.Fatalf("broken settings: %d %s", code, errs)
	}
	if b, _ := os.ReadFile(path); string(b) != `{not json` {
		t.Fatal("a broken settings file must be left untouched")
	}
}

func TestGatewaySetupGlobalUsesTheClaudeConfigDir(t *testing.T) {
	h := newHarness(t)
	h.signIn(time.Hour)
	dir := t.TempDir()
	h.env["CLAUDE_CONFIG_DIR"] = dir
	if code, out, errs := h.run("", "gateway", "setup", "--global"); code != 0 || !strings.Contains(out, "every Claude Code session") {
		t.Fatalf("%d %s %s", code, out, errs)
	}
	s := readJSON(t, filepath.Join(dir, "settings.json"))
	if s["apiKeyHelper"] != "ros-local model-token" { // no repo: the server picks the device's project
		t.Fatalf("apiKeyHelper = %v", s["apiKeyHelper"])
	}
	if _, err := os.Stat(filepath.Join(h.repo, ".claude", "settings.local.json")); err == nil {
		t.Fatal("--global must not touch the repo")
	}
	if code, _, _ := h.run("", "gateway", "off", "--global"); code != 0 {
		t.Fatal("off --global failed")
	}
	if _, err := os.Stat(filepath.Join(dir, "settings.json")); err == nil {
		t.Fatal("a settings file that held only our keys should be removed")
	}
}
