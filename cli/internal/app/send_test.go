package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hvoguetech/ros-claude-plugin/cli/internal/gitx"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/link"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// workRepo is a repository with one commit on main and `remote` as origin, linked to project p1.
func (h *harness) workRepo(remote string) string {
	t := h.t
	work := t.TempDir()
	gitIn(t, work, "init", "-q", "-b", "main")
	gitIn(t, work, "config", "user.email", "t@x")
	gitIn(t, work, "config", "user.name", "t")
	gitIn(t, work, "remote", "add", "origin", remote)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# shop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, work, "add", "-A")
	gitIn(t, work, "commit", "-q", "-m", "init")
	repo, err := gitx.Find(work)
	if err != nil {
		t.Fatal(err)
	}
	if err := link.Write(repo, &link.Link{ProjectID: "p1", ProjectName: "Payments"}); err != nil {
		t.Fatal(err)
	}
	h.repo = work
	return work
}

func TestSendCommitsPushesANewBranchAndCreatesTheTask(t *testing.T) {
	h := newHarness(t)
	h.signIn(time.Hour)
	bare := filepath.Join(t.TempDir(), "remote.git")
	gitIn(t, t.TempDir(), "init", "-q", "--bare", bare)
	work := h.workRepo(bare)
	if err := os.WriteFile(filepath.Join(work, "auth.py"), []byte("patched\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errb := h.run("", "send", "--message", "Finish the login fix; tests are red on the token path.")
	if code != 0 {
		t.Fatalf("code %d: %s %s", code, out, errb)
	}
	branches := gitIn(t, bare, "branch", "--list", "ros/handoff-*")
	if branches == "" {
		t.Fatal("no ros/handoff branch reached the remote")
	}
	if gitIn(t, work, "status", "--porcelain") != "" {
		t.Fatal("the working tree should be clean after the hand-off commit")
	}
	if len(h.srv.Tasks) != 1 {
		t.Fatalf("tasks %v", h.srv.Tasks)
	}
	body := h.srv.Tasks[0]
	if body["message"] != "Finish the login fix; tests are red on the token path." || body["project_id"] != "p1" || body["repo"] != bare {
		t.Fatalf("task body %v", body)
	}
	if _, has := body["repo_full_names"]; has {
		t.Fatal("a local-path remote has no owner/name to send")
	}
	if !strings.Contains(out, "Pushed ros/handoff-") || !strings.Contains(out, "Task t1 created") {
		t.Fatalf("out %s", out)
	}
}

func TestSendNamesAGitHubRepoAndItsBranchWithoutPushing(t *testing.T) {
	h := newHarness(t)
	h.signIn(time.Hour)
	h.workRepo("git@github.com:Acme/Shop.git")
	code, out, errb := h.run("", "send", "--no-push", "--message", "Review the branch", "--title", "Review")
	if code != 0 {
		t.Fatalf("code %d: %s %s", code, out, errb)
	}
	body := h.srv.Tasks[0]
	if names, _ := body["repo_full_names"].([]any); len(names) != 1 || names[0] != "acme/shop" {
		t.Fatalf("repo_full_names %v", body["repo_full_names"])
	}
	if br, _ := body["repo_branches"].(map[string]any); br["acme/shop"] != "main" {
		t.Fatalf("repo_branches %v", body["repo_branches"])
	}
	if body["title"] != "Review" || !strings.Contains(out, "(Review)") {
		t.Fatalf("title %v %s", body["title"], out)
	}
}

func TestSendNeedsANoteAndAProject(t *testing.T) {
	h := newHarness(t)
	h.signIn(time.Hour)
	h.workRepo("git@github.com:acme/shop.git")
	code, _, errb := h.run("", "send", "--no-push")
	if code == 0 || !strings.Contains(errb, "say what the agent should do") {
		t.Fatalf("code %d: %s", code, errb)
	}
	// the note may come on stdin
	if code, _, errb = h.run("do it\n", "send", "--no-push"); code != 0 || h.srv.Tasks[0]["message"] != "do it" {
		t.Fatalf("stdin note: %d %s %v", code, errb, h.srv.Tasks)
	}
	h.srv.NoProject = true
	code, _, errb = h.run("", "send", "--no-push", "--message", "x", "--project", "")
	// the repo link names p1, so this still works; drop the link to see the no_project advice
	if code != 0 {
		t.Fatalf("linked repo: %d %s", code, errb)
	}
	repo, _ := gitx.Find(h.repo)
	os.Remove(link.Path(repo))
	code, _, errb = h.run("", "send", "--no-push", "--message", "x")
	if code == 0 || !strings.Contains(errb, "ros-local link") {
		t.Fatalf("code %d: %s", code, errb)
	}
}
