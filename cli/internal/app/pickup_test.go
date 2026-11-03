package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const briefing = "Continue this work from ROS (run r1, Coder).\n\nIt is waiting on: which vault?\n\nThe agent's last report:\nPatched auth.py."

func (h *harness) pickupRun() {
	h.srv.Artifacts["a1"] = tarGz(h.t, map[string]string{"shop/README.md": "# shop\n", "shop/.git/HEAD": "ref: refs/heads/fix/login\n"})
	h.srv.Pickups["r1"] = map[string]any{
		"run_id": "r1", "status": "interrupted", "project_id": "p1", "workflow_name": "Coder",
		"snapshot":      map[string]any{"artifact_id": "a1", "filename": "workspace.tar.gz", "size": 4096, "taken_at": "2026-11-03T10:00:00Z", "url": "/v1/local/artifacts/a1"},
		"repos":         []any{map[string]any{"full_name": "acme/shop", "url": "https://github.com/acme/shop", "branch": "fix/login"}},
		"pull_requests": []any{}, "question": "which vault?", "report": "Patched auth.py.", "folder": "ros-r1",
		"steps": []any{"mkdir -p ~/ros-r1"}, "cli": "ros-local pickup r1", "briefing": briefing,
	}
}

func TestPickupUnpacksTheSnapshotRecordsItAndStartsClaudeWithTheBriefing(t *testing.T) {
	h := newHarness(t)
	h.signIn(time.Hour)
	h.pickupRun()
	dir := filepath.Join(t.TempDir(), "pick")
	var gotDir string
	var gotArgv []string
	h.exec = func(d string, argv []string) error { gotDir, gotArgv = d, argv; return nil }
	code, out, errb := h.run("", "pickup", "r1", "--dir", dir)
	if code != 0 {
		t.Fatalf("code %d: %s %s", code, out, errb)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "shop", "README.md")); err != nil || string(b) != "# shop\n" {
		t.Fatalf("snapshot not unpacked: %v %q", err, b)
	}
	if b, err := os.ReadFile(filepath.Join(dir, BriefingFile)); err != nil || !strings.HasPrefix(string(b), briefing) {
		t.Fatalf("briefing file: %v %q", err, b)
	}
	if gotDir != dir || len(gotArgv) != 2 || gotArgv[0] != "claude" || gotArgv[1] != briefing {
		t.Fatalf("exec %q %q", gotDir, gotArgv)
	}
	if len(h.srv.PickupMarks) != 1 || h.srv.PickupMarks[0]["folder"] != dir || h.srv.PickupMarks[0]["run_id"] != "r1" {
		t.Fatalf("pickup not recorded: %v", h.srv.PickupMarks)
	}
	for _, want := range []string{"Unpacked 2 files", "It is waiting on: which vault?", "Repository: acme/shop (fix/login)", "Starting Claude Code"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %s", want, out)
		}
	}
}

func TestPickupAcceptsAConsoleURLAndCanStopBeforeClaude(t *testing.T) {
	h := newHarness(t)
	h.signIn(time.Hour)
	h.pickupRun()
	dir := filepath.Join(t.TempDir(), "pick")
	h.exec = func(string, []string) error { t.Fatal("claude must not start with --no-claude"); return nil }
	code, out, errb := h.run("", "pickup", "http://console.local/?project=p1&screen=traces&run=r1", "--dir", dir, "--no-claude")
	if code != 0 || !strings.Contains(out, "Next: cd "+dir+" && claude") {
		t.Fatalf("code %d: %s %s", code, out, errb)
	}
	// a second pickup into the same place is refused unless forced
	code, _, errb = h.run("", "pickup", "r1", "--dir", dir, "--no-claude")
	if code == 0 || !strings.Contains(errb, "already has files") {
		t.Fatalf("code %d: %s", code, errb)
	}
	if code, _, errb = h.run("", "pickup", "r1", "--dir", dir, "--no-claude", "--force"); code != 0 {
		t.Fatalf("--force: %s", errb)
	}
}

func TestPickupRefusesAnUnknownRunAndAnEscapingArchive(t *testing.T) {
	h := newHarness(t)
	h.signIn(time.Hour)
	code, _, errb := h.run("", "pickup", "nope", "--dir", filepath.Join(t.TempDir(), "x"), "--no-claude")
	if code == 0 || !strings.Contains(errb, "no run nope") {
		t.Fatalf("code %d: %s", code, errb)
	}
	h.pickupRun()
	h.srv.Artifacts["a1"] = tarGz(t, map[string]string{"../evil.sh": "rm -rf /\n"})
	dir := filepath.Join(t.TempDir(), "pick")
	code, _, errb = h.run("", "pickup", "r1", "--dir", dir, "--no-claude")
	if code == 0 || !strings.Contains(errb, "outside") {
		t.Fatalf("code %d: %s", code, errb)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil.sh")); err == nil {
		t.Fatal("the archive escaped the pickup directory")
	}
	if len(h.srv.PickupMarks) != 0 {
		t.Fatal("a failed pickup must not be recorded")
	}
}

func TestRunIDFrom(t *testing.T) {
	for in, want := range map[string]string{
		"r1": "r1", "  r1 ": "r1",
		"https://console/?project=p&screen=traces&run=r9": "r9",
		"https://console/?screen=traces&id=th1":           "th1",
		"https://console/?screen=traces":                  "",
	} {
		if got := runIDFrom(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}
