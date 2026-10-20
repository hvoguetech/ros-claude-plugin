package skills

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hvoguetech/ros-claude-plugin/cli/internal/api"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/fakeros"
)

func setup(t *testing.T) (*fakeros.Server, *api.Client, string) {
	t.Helper()
	srv := fakeros.New()
	t.Cleanup(srv.Close)
	c := api.New(srv.URL, srv.IssueToken(), "test", 5*time.Second)
	return srv, c, t.TempDir()
}

func skill(name, body string) fakeros.Skill {
	return fakeros.Skill{Name: name, ID: "id-" + name, Version: 1, Files: [][2]string{
		{"SKILL.md", "---\nname: " + name + "\ndescription: " + name + "\n---\n" + body + "\n"},
		{"ref/notes.md", "notes for " + name},
	}}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func sync(t *testing.T, c *api.Client, root string) *Result {
	t.Helper()
	res, err := Sync(context.Background(), c, root, Options{ProjectID: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// The backend's files_sha256 for {"/demo/SKILL.md": ..., "/demo/ref/notes.md": "notes"}, computed with the
// Python implementation.
func TestHashFilesMatchesBackend(t *testing.T) {
	files := map[string]string{"demo/SKILL.md": "---\nname: demo\ndescription: d\n---\nbody\n", "demo/ref/notes.md": "notes"}
	got, _ := HashFiles([]string{"demo/SKILL.md", "demo/ref/notes.md"}, func(p string) ([]byte, error) {
		return []byte(files[p]), nil
	})
	if want := "ce38b635d8e20957a61a5fa230a435f603d30351a777a9bc6573a8d0063a477d"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestSyncWritesPrefixedDirs(t *testing.T) {
	srv, c, root := setup(t)
	srv.Skills = []fakeros.Skill{skill("deploy", "v1"), skill("review", "v1")}
	res := sync(t, c, root)
	if len(res.Added) != 2 || res.Total != 2 || len(res.Warnings) != 0 {
		t.Fatalf("%+v", res)
	}
	dir := Dir(root)
	if !strings.Contains(read(t, filepath.Join(dir, "ros-deploy", "SKILL.md")), "v1") {
		t.Fatal("SKILL.md not written")
	}
	if read(t, filepath.Join(dir, "ros-review", "ref", "notes.md")) != "notes for review" {
		t.Fatal("supporting file not written")
	}
	if exists(filepath.Join(dir, "deploy")) || exists(filepath.Join(dir, ".ros-staging")) {
		t.Fatal("wrote outside ros-<name>/ or left staging behind")
	}
	m, _ := ReadManifest(dir)
	if len(m.Skills) != 2 || m.ETag == "" {
		t.Fatalf("manifest %+v", m)
	}
}

func TestSyncUsesETag(t *testing.T) {
	srv, c, root := setup(t)
	srv.Skills = []fakeros.Skill{skill("deploy", "v1")}
	sync(t, c, root)
	res := sync(t, c, root)
	if !res.NotModified || len(srv.BundleCalls) != 1 {
		t.Fatalf("second sync should be a 304: %+v, bundles=%d", res, len(srv.BundleCalls))
	}
	// The ETag from the session open short-circuits the manifest request entirely.
	m, _ := ReadManifest(Dir(root))
	before := srv.ManifestCalls
	res, err := Sync(context.Background(), c, root, Options{ProjectID: "p1", KnownETag: m.ETag})
	if err != nil || !res.NotModified || srv.ManifestCalls != before {
		t.Fatalf("known etag: %+v err=%v calls %d→%d", res, err, before, srv.ManifestCalls)
	}
}

func TestPruneOnlyTouchesManifestListedDirs(t *testing.T) {
	srv, c, root := setup(t)
	dir := Dir(root)
	// The developer's own skills, including one that happens to use the ros- prefix.
	for _, own := range []string{"my-skill", "ros-mine"} {
		_ = os.MkdirAll(filepath.Join(dir, own), 0o755)
		_ = os.WriteFile(filepath.Join(dir, own, "SKILL.md"), []byte("mine"), 0o644)
	}
	srv.Skills = []fakeros.Skill{skill("deploy", "v1"), skill("review", "v1")}
	sync(t, c, root)

	srv.Skills = []fakeros.Skill{skill("review", "v1")} // deploy removed on the server
	res := sync(t, c, root)
	if len(res.Removed) != 1 || res.Removed[0] != "deploy" {
		t.Fatalf("removed %v", res.Removed)
	}
	if exists(filepath.Join(dir, "ros-deploy")) {
		t.Fatal("ros-deploy should be pruned")
	}
	for _, keep := range []string{"my-skill", "ros-mine", "ros-review"} {
		if !exists(filepath.Join(dir, keep, "SKILL.md")) {
			t.Fatalf("%s must not be touched", keep)
		}
	}

	srv.Skills = nil // everything removed: the developer's dirs still stay
	sync(t, c, root)
	if exists(filepath.Join(dir, "ros-review")) || !exists(filepath.Join(dir, "ros-mine")) || !exists(filepath.Join(dir, "my-skill")) {
		t.Fatal("prune went beyond the manifest")
	}
}

func TestDoesNotOverwriteUnmanagedDir(t *testing.T) {
	srv, c, root := setup(t)
	dir := Dir(root)
	_ = os.MkdirAll(filepath.Join(dir, "ros-deploy"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "ros-deploy", "SKILL.md"), []byte("hand-made"), 0o644)
	srv.Skills = []fakeros.Skill{skill("deploy", "v1")}
	res := sync(t, c, root)
	if read(t, filepath.Join(dir, "ros-deploy", "SKILL.md")) != "hand-made" || len(res.Warnings) != 1 {
		t.Fatalf("an unmanaged ros-deploy was overwritten: %+v", res)
	}
}

func TestLocalEditWarnedThenBackedUpOnUpdate(t *testing.T) {
	srv, c, root := setup(t)
	dir := Dir(root)
	srv.Skills = []fakeros.Skill{skill("deploy", "v1")}
	sync(t, c, root)

	p := filepath.Join(dir, "ros-deploy", "SKILL.md")
	_ = os.WriteFile(p, []byte("my local tweak"), 0o644)

	// Server unchanged (304): the edit is reported and left alone.
	res := sync(t, c, root)
	if len(res.Edited) != 1 || len(res.BackedUp) != 0 || read(t, p) != "my local tweak" {
		t.Fatalf("unchanged server: %+v", res)
	}
	// An added file counts as an edit too.
	_ = os.WriteFile(filepath.Join(dir, "ros-deploy", "extra.md"), []byte("x"), 0o644)
	if !Edited(dir, "deploy", mustManifest(t, dir).Skills["deploy"]) {
		t.Fatal("added file not detected")
	}

	// Server publishes v2: the edited copy is backed up, then replaced.
	srv.Skills = []fakeros.Skill{skill("deploy", "v2")}
	srv.Skills[0].Version = 2
	res = sync(t, c, root)
	backup, ok := res.BackedUp["deploy"]
	if !ok || len(res.Updated) != 1 {
		t.Fatalf("update over an edit: %+v", res)
	}
	if read(t, filepath.Join(backup, "SKILL.md")) != "my local tweak" {
		t.Fatal("backup doesn't hold the local edit")
	}
	if !strings.HasPrefix(backup, filepath.Join(dir, ".ros-backup")) {
		t.Fatalf("backup outside .ros-backup: %s", backup)
	}
	if !strings.Contains(read(t, p), "v2") || exists(filepath.Join(dir, "ros-deploy", "extra.md")) {
		t.Fatal("skill not replaced by v2")
	}
	// Clean after the update.
	if Edited(dir, "deploy", mustManifest(t, dir).Skills["deploy"]) {
		t.Fatal("freshly synced skill reported as edited")
	}
}

func TestUnchangedSkillsAreNotRedownloaded(t *testing.T) {
	srv, c, root := setup(t)
	srv.Skills = []fakeros.Skill{skill("deploy", "v1"), skill("review", "v1")}
	sync(t, c, root)
	srv.Skills[1] = skill("review", "v2")
	res := sync(t, c, root)
	last := srv.BundleCalls[len(srv.BundleCalls)-1]
	if len(last) != 1 || last[0] != "review" || len(res.Updated) != 1 {
		t.Fatalf("bundle names %v, result %+v", last, res)
	}
}

func TestDeletedSyncedDirIsRestored(t *testing.T) {
	srv, c, root := setup(t)
	srv.Skills = []fakeros.Skill{skill("deploy", "v1")}
	sync(t, c, root)
	_ = os.RemoveAll(filepath.Join(Dir(root), "ros-deploy"))
	srv.Skills = append(srv.Skills, skill("review", "v1")) // forces a fresh manifest
	sync(t, c, root)
	if !exists(filepath.Join(Dir(root), "ros-deploy", "SKILL.md")) {
		t.Fatal("deleted synced skill not restored")
	}
}

func TestReadBundleRejectsEscapes(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{"deploy/SKILL.md", "deploy/../../evil", "deploy//x", "/abs/SKILL.md", "other/SKILL.md", `deploy/a\b`} {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte("x"))
	}
	_ = zw.Close()
	files, order, err := readBundle(buf.Bytes(), []string{"deploy"})
	if err != nil {
		t.Fatal(err)
	}
	if len(order["deploy"]) != 1 || order["deploy"][0] != "SKILL.md" || len(files) != 1 {
		t.Fatalf("unsafe entries kept: %v", order)
	}
}

func TestManifestNamesFromDiskAreValidated(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, manifestFile), []byte(`{"skills":{"../../etc":{},"ok":{}}}`), 0o644)
	m, err := ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, bad := m.Skills["../../etc"]; bad || len(m.Skills) != 1 {
		t.Fatalf("%v", m.Skills)
	}
}

func mustManifest(t *testing.T, dir string) *LocalManifest {
	t.Helper()
	m, err := ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
