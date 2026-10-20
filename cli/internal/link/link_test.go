package link

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hvoguetech/ros-claude-plugin/cli/internal/gitx"
)

func newRepo(t *testing.T) *gitx.Repo {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := gitx.Find(root)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestEnsureExcludeIsIdempotent(t *testing.T) {
	r := newRepo(t)
	path := filepath.Join(r.CommonDir, "info", "exclude")
	if err := os.WriteFile(path, []byte("# git's sample\n*.log"), 0o644); err != nil { // no trailing newline
		t.Fatal(err)
	}
	changed, err := EnsureExclude(r)
	if err != nil || !changed {
		t.Fatalf("first run: changed=%v err=%v", changed, err)
	}
	first, _ := os.ReadFile(path)
	for i := 0; i < 3; i++ {
		changed, err = EnsureExclude(r)
		if err != nil || changed {
			t.Fatalf("run %d: changed=%v err=%v", i+2, changed, err)
		}
	}
	again, _ := os.ReadFile(path)
	if string(first) != string(again) {
		t.Fatalf("exclude changed on a re-run:\n%s\n---\n%s", first, again)
	}
	got := string(again)
	if !strings.HasPrefix(got, "# git's sample\n*.log\n") {
		t.Fatalf("existing lines not preserved: %q", got)
	}
	for _, p := range ExcludePatterns {
		if strings.Count(got, "\n"+p+"\n") != 1 {
			t.Fatalf("pattern %q should appear exactly once in %q", p, got)
		}
	}
}

func TestEnsureExcludeKeepsDeveloperPatterns(t *testing.T) {
	r := newRepo(t)
	path := filepath.Join(r.CommonDir, "info", "exclude")
	_ = os.WriteFile(path, []byte(".claude/skills/ros-*\n"), 0o644)
	if _, err := EnsureExclude(r); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if strings.Count(string(got), ".claude/skills/ros-*\n") != 1 {
		t.Fatalf("an existing pattern was duplicated: %q", got)
	}
	if !strings.Contains(string(got), ".claude/skills/.ros-*\n") {
		t.Fatalf("missing pattern not added: %q", got)
	}
}

func TestEnsureExcludeCreatesInfoDir(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	r, _ := gitx.Find(root)
	if _, err := EnsureExclude(r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".git", "info", "exclude")); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareCreatesSkillsDirOnce(t *testing.T) {
	r := newRepo(t)
	created, err := Prepare(r)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	created, err = Prepare(r)
	if err != nil || created {
		t.Fatalf("second run: created=%v err=%v", created, err)
	}
}

func TestLinkRoundTripAndWorktree(t *testing.T) {
	main := newRepo(t)
	if l, err := Read(main); err != nil || l != nil {
		t.Fatalf("unlinked repo: %v %v", l, err)
	}
	if err := Write(main, &Link{ProjectID: "p1", ProjectName: "Payments", Source: "explicit"}); err != nil {
		t.Fatal(err)
	}
	// A linked worktree shares the main repo's link and exclude file.
	wtGit := filepath.Join(main.CommonDir, "worktrees", "wt")
	_ = os.MkdirAll(wtGit, 0o755)
	_ = os.WriteFile(filepath.Join(wtGit, "commondir"), []byte("../..\n"), 0o644)
	wt := t.TempDir()
	_ = os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+wtGit+"\n"), 0o644)
	r, err := gitx.Find(filepath.Join(wt))
	if err != nil {
		t.Fatal(err)
	}
	if r.CommonDir != main.CommonDir {
		t.Fatalf("common dir %s, want %s", r.CommonDir, main.CommonDir)
	}
	l, err := Read(r)
	if err != nil || l == nil || l.ProjectID != "p1" {
		t.Fatalf("worktree link: %+v %v", l, err)
	}
}
