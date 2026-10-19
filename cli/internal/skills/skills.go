// Package skills syncs a ROS project's skills into `<repo>/.claude/skills/ros-<name>/` (spec §7).
//
// Rules:
//   - ros-local writes only `ros-<name>/` directories and its own `.ros-*` bookkeeping in the skills dir.
//   - It prunes only directories listed in `.ros-manifest.json` (what it wrote itself). A developer's own
//     skills, including one they named `ros-something`, are never touched.
//   - A synced skill edited locally is reported. When the server has a new version, the edited copy is
//     moved to `.ros-backup/` before it is overwritten; otherwise it's left alone.
package skills

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hvoguetech/ros-claude-plugin/cli/internal/api"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/fsutil"
)

const (
	// Prefix is prepended to every synced skill's directory name.
	Prefix       = "ros-"
	manifestFile = ".ros-manifest.json"
	backupDir    = ".ros-backup"
	stagingDir   = ".ros-staging"

	maxFileBytes  = 5 << 20
	maxTotalBytes = 64 << 20
)

// A skill name must be one plain path segment.
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// API is the slice of the backend client Sync needs.
type API interface {
	SkillsManifest(ctx context.Context, projectID, repo, ifNoneMatch string) (*api.Manifest, bool, error)
	SkillsBundle(ctx context.Context, projectID, repo string, names []string) ([]byte, error)
}

// LocalSkill is one synced skill as recorded in `.ros-manifest.json`.
type LocalSkill struct {
	SkillID string   `json:"skill_id"`
	Version int      `json:"version"`
	Label   string   `json:"label,omitempty"`
	SHA256  string   `json:"sha256"`       // the server's hash for the version written
	Local   string   `json:"local_sha256"` // the hash of what was written (detects local edits)
	Files   []string `json:"files"`        // paths relative to the skill dir, in bundle order
}

// LocalManifest is `.claude/skills/.ros-manifest.json`.
type LocalManifest struct {
	ProjectID string                `json:"project_id"`
	ETag      string                `json:"etag"`
	SyncedAt  time.Time             `json:"synced_at"`
	Skills    map[string]LocalSkill `json:"skills"`
}

// Result says what a sync did.
type Result struct {
	NotModified bool
	Added       []string
	Updated     []string
	Removed     []string
	Edited      []string          // locally edited synced skills (left in place or backed up)
	BackedUp    map[string]string // name → backup dir
	Warnings    []string
	Total       int // synced skills after this run
	SyncedAt    time.Time
}

// Options for Sync.
type Options struct {
	ProjectID string
	Repo      string
	// KnownETag is the manifest ETag the session open already returned; when it matches the local copy
	// no request is made.
	KnownETag string
	Now       func() time.Time
}

// Dir is the skills dir of a working tree.
func Dir(root string) string { return filepath.Join(root, ".claude", "skills") }

// ReadManifest loads the local manifest; a missing one is empty.
func ReadManifest(skillsDir string) (*LocalManifest, error) {
	m := &LocalManifest{Skills: map[string]LocalSkill{}}
	b, err := os.ReadFile(filepath.Join(skillsDir, manifestFile))
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, m); err != nil {
		return nil, fmt.Errorf("%s is corrupt: %w", manifestFile, err)
	}
	if m.Skills == nil {
		m.Skills = map[string]LocalSkill{}
	}
	// Never trust a name from disk to address a path.
	for name := range m.Skills {
		if !validName.MatchString(name) {
			delete(m.Skills, name)
		}
	}
	return m, nil
}

func writeManifest(skillsDir string, m *LocalManifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(skillsDir, manifestFile), append(b, '\n'), 0o644)
}

// HashFiles is the backend's `files_sha256`: for each (path, text) in order, path NUL text NUL.
func HashFiles(paths []string, content func(p string) ([]byte, error)) (string, error) {
	h := sha256.New()
	for _, p := range paths {
		b, err := content(p)
		if err != nil {
			return "", err
		}
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Edited reports whether a synced skill's directory differs from what ros-local wrote.
func Edited(skillsDir, name string, ls LocalSkill) bool {
	dir := filepath.Join(skillsDir, Prefix+name)
	have, err := listFiles(dir)
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist) // a deleted dir isn't an edit worth backing up
	}
	want := append([]string(nil), ls.Files...)
	sort.Strings(want)
	if strings.Join(have, "\n") != strings.Join(want, "\n") {
		return true
	}
	paths := make([]string, len(ls.Files))
	for i, f := range ls.Files {
		paths[i] = name + "/" + f
	}
	sum, err := HashFiles(paths, func(p string) ([]byte, error) {
		return os.ReadFile(filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(p, name+"/"))))
	})
	return err != nil || sum != ls.Local
}

// listFiles returns the regular files under dir, relative and slash-separated, sorted.
func listFiles(dir string) ([]string, error) {
	if _, err := os.Stat(dir); err != nil {
		return nil, err
	}
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(out)
	return out, err
}

// Sync brings `<root>/.claude/skills/ros-*` in line with the project's manifest.
func Sync(ctx context.Context, client API, root string, o Options) (*Result, error) {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	dir := Dir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	local, err := ReadManifest(dir)
	if err != nil {
		return nil, err
	}
	res := &Result{BackedUp: map[string]string{}}

	etag := local.ETag
	if local.ProjectID != o.ProjectID {
		etag = "" // another project's skills: fetch fresh, prune what no longer applies
	}
	var remote *api.Manifest
	notModified := etag != "" && etag == o.KnownETag
	if !notModified {
		remote, notModified, err = client.SkillsManifest(ctx, o.ProjectID, o.Repo, etag)
		if err != nil {
			return nil, err
		}
	}
	if notModified {
		res.NotModified = true
		for _, name := range sortedKeys(local.Skills) {
			if Edited(dir, name, local.Skills[name]) {
				res.Edited = append(res.Edited, name)
			}
		}
		res.Total, res.SyncedAt = len(local.Skills), local.SyncedAt
		return res, nil
	}

	want := map[string]api.SkillEntry{}
	for _, e := range remote.Skills {
		if !validName.MatchString(e.Name) {
			res.Warnings = append(res.Warnings, fmt.Sprintf("skipped skill with unusable name %q", e.Name))
			continue
		}
		want[e.Name] = e
	}

	backup := func(name string) {
		dst := filepath.Join(dir, backupDir, fmt.Sprintf("%s-%s", name, now().UTC().Format("20060102T150405Z")))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err == nil {
			if err := os.Rename(filepath.Join(dir, Prefix+name), dst); err == nil {
				res.BackedUp[name] = dst
				return
			}
		}
		res.Warnings = append(res.Warnings, fmt.Sprintf("could not back up locally edited skill %s", name))
	}

	// Prune: only names ros-local itself wrote.
	next := map[string]LocalSkill{}
	for _, name := range sortedKeys(local.Skills) {
		if _, keep := want[name]; keep {
			next[name] = local.Skills[name]
			continue
		}
		if Edited(dir, name, local.Skills[name]) {
			res.Edited = append(res.Edited, name)
			backup(name)
		}
		if err := os.RemoveAll(filepath.Join(dir, Prefix+name)); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("could not remove %s%s: %v", Prefix, name, err))
			next[name] = local.Skills[name]
			continue
		}
		res.Removed = append(res.Removed, name)
	}

	// What needs writing.
	var fetch []string
	for _, name := range sortedKeys(want) {
		e := want[name]
		ls, had := local.Skills[name]
		target := filepath.Join(dir, Prefix+name)
		if !had {
			if _, err := os.Stat(target); err == nil {
				// A directory ros-local didn't write: the developer's own. Leave it.
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"%s%s exists and wasn't written by ros-local; not syncing ROS skill %q over it", Prefix, name, name))
				continue
			}
			fetch = append(fetch, name)
			continue
		}
		edited := Edited(dir, name, ls)
		_, statErr := os.Stat(target)
		if ls.SHA256 == e.SHA256 && statErr == nil {
			if edited {
				res.Edited = append(res.Edited, name)
			}
			next[name] = LocalSkill{SkillID: e.SkillID, Version: e.Version, Label: label(e), SHA256: ls.SHA256,
				Local: ls.Local, Files: ls.Files}
			continue
		}
		if edited && statErr == nil {
			res.Edited = append(res.Edited, name)
			backup(name)
		}
		fetch = append(fetch, name)
	}

	if len(fetch) > 0 {
		data, err := client.SkillsBundle(ctx, o.ProjectID, o.Repo, fetch)
		if err != nil {
			return nil, fmt.Errorf("downloading skills: %w", err)
		}
		files, order, err := readBundle(data, fetch)
		if err != nil {
			return nil, err
		}
		for _, name := range fetch {
			e := want[name]
			if len(order[name]) == 0 {
				res.Warnings = append(res.Warnings, fmt.Sprintf("skill %q missing from the bundle", name))
				if ls, ok := local.Skills[name]; ok {
					next[name] = ls
				}
				continue
			}
			paths := make([]string, len(order[name]))
			for i, f := range order[name] {
				paths[i] = name + "/" + f
			}
			sum, _ := HashFiles(paths, func(p string) ([]byte, error) { return files[p], nil })
			if e.SHA256 != "" && sum != e.SHA256 {
				res.Warnings = append(res.Warnings, fmt.Sprintf("skill %q: content hash differs from the manifest", name))
			}
			if err := install(dir, name, order[name], func(f string) []byte { return files[name+"/"+f] }); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("could not write skill %q: %v", name, err))
				if ls, ok := local.Skills[name]; ok {
					next[name] = ls
				}
				continue
			}
			if _, had := local.Skills[name]; had {
				res.Updated = append(res.Updated, name)
			} else {
				res.Added = append(res.Added, name)
			}
			next[name] = LocalSkill{SkillID: e.SkillID, Version: e.Version, Label: label(e), SHA256: e.SHA256,
				Local: sum, Files: order[name]}
		}
	}
	_ = os.RemoveAll(filepath.Join(dir, stagingDir))

	out := &LocalManifest{ProjectID: o.ProjectID, ETag: remote.ETag, SyncedAt: now().UTC(), Skills: next}
	if len(res.Warnings) > 0 {
		out.ETag = "" // retry everything on the next start
	}
	if err := writeManifest(dir, out); err != nil {
		return nil, err
	}
	res.Total, res.SyncedAt = len(next), out.SyncedAt
	return res, nil
}

func label(e api.SkillEntry) string {
	if e.Label != nil {
		return *e.Label
	}
	return ""
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// readBundle unpacks the zip into memory, keeping only `<name>/<file>` entries for the requested names.
// Paths that could escape the skill dir are rejected.
func readBundle(data []byte, names []string) (map[string][]byte, map[string][]string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, nil, fmt.Errorf("reading skills bundle: %w", err)
	}
	wanted := map[string]bool{}
	for _, n := range names {
		wanted[n] = true
	}
	files := map[string][]byte{}
	order := map[string][]string{}
	var total int64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name, rel, ok := strings.Cut(f.Name, "/")
		if !ok || !wanted[name] || !safeRel(rel) {
			continue
		}
		if f.UncompressedSize64 > maxFileBytes {
			return nil, nil, fmt.Errorf("skill %q: %s is too large", name, rel)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, nil, err
		}
		b, err := io.ReadAll(io.LimitReader(rc, maxFileBytes+1))
		rc.Close()
		if err != nil {
			return nil, nil, err
		}
		if len(b) > maxFileBytes {
			return nil, nil, fmt.Errorf("skill %q: %s is too large", name, rel)
		}
		if total += int64(len(b)); total > maxTotalBytes {
			return nil, nil, errors.New("skills bundle is too large")
		}
		if _, dup := files[f.Name]; !dup {
			order[name] = append(order[name], rel)
		}
		files[f.Name] = b
	}
	return files, order, nil
}

func safeRel(rel string) bool {
	if rel == "" || strings.Contains(rel, "\\") || strings.HasPrefix(rel, "/") {
		return false
	}
	clean := path.Clean(rel)
	if clean != rel || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return false
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == ".." || seg == "" {
			return false
		}
	}
	return true
}

// install writes one skill to a staging dir and swaps it into `ros-<name>/`.
func install(dir, name string, files []string, content func(string) []byte) error {
	stage := filepath.Join(dir, stagingDir, name)
	_ = os.RemoveAll(stage)
	for _, f := range files {
		p := filepath.Join(stage, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, content(f), 0o644); err != nil {
			return err
		}
	}
	target := filepath.Join(dir, Prefix+name)
	old := filepath.Join(dir, stagingDir, name+".old")
	_ = os.RemoveAll(old)
	if _, err := os.Stat(target); err == nil {
		if err := os.Rename(target, old); err != nil {
			return err
		}
	}
	if err := os.Rename(stage, target); err != nil {
		_ = os.Rename(old, target)
		return err
	}
	return os.RemoveAll(old)
}
