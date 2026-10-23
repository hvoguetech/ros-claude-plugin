// Package link stores which ROS project a repository belongs to (`.git/ros.json`, never committed) and
// prepares the repository for synced skills: it pre-creates `.claude/skills/` (Claude Code only watches a
// skills dir that existed at session start, spec §7 / §18a) and git-excludes ros-local's files.
package link

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hvoguetech/ros-claude-plugin/cli/internal/fsutil"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/gitx"
)

// FileName is the link file inside the (common) git dir.
const FileName = "ros.json"

// SkillsDir is the project skills directory, relative to the working tree root.
const SkillsDir = ".claude/skills"

// ExcludePatterns are what ros-local adds to `.git/info/exclude`: the synced `ros-<name>` skill dirs and
// ros-local's own bookkeeping (`.ros-manifest.json`, `.ros-backup/`, `.ros-staging/`).
var ExcludePatterns = []string{
	".claude/skills/ros-*",
	".claude/skills/.ros-*",
}

const excludeHeader = "# ros-local: ROS skills synced by the ros plugin (never commit)"

// Link is the content of `.git/ros.json`.
type Link struct {
	ProjectID   string    `json:"project_id"`
	ProjectName string    `json:"project_name,omitempty"`
	Repo        string    `json:"repo,omitempty"`   // host/owner/name, when the repo has a remote
	Source      string    `json:"source,omitempty"` // explicit | remote | default | prompt | session
	LinkedAt    time.Time `json:"linked_at"`
	// Stale is set when ROS reports the linked project unavailable (deleted, no access, local agent
	// off). A stale link isn't sent to ROS, and every session says so until `ros-local link` replaces it.
	Stale       bool   `json:"stale,omitempty"`
	StaleReason string `json:"stale_reason,omitempty"`
}

// MarkStale records that ROS refused the linked project.
func MarkStale(r *gitx.Repo, l *Link, reason string) error {
	l.Stale, l.StaleReason = true, reason
	return Write(r, l)
}

// Path is where the link for repo is stored.
func Path(r *gitx.Repo) string { return filepath.Join(r.CommonDir, FileName) }

// Read returns the repo's link, or (nil, nil) when it isn't linked.
func Read(r *gitx.Repo) (*Link, error) {
	b, err := os.ReadFile(Path(r))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var l Link
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, err
	}
	if l.ProjectID == "" {
		return nil, nil
	}
	return &l, nil
}

// Write stores the link.
func Write(r *gitx.Repo, l *Link) error {
	if l.LinkedAt.IsZero() {
		l.LinkedAt = time.Now().UTC()
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(Path(r), append(b, '\n'), 0o644)
}

// EnsureSkillsDir creates `<root>/.claude/skills` and reports whether it had to.
func EnsureSkillsDir(root string) (created bool, err error) {
	dir := filepath.Join(root, SkillsDir)
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return false, nil
	}
	return true, os.MkdirAll(dir, 0o755)
}

// EnsureExclude adds ExcludePatterns to `<common git dir>/info/exclude`, once. Running it again changes
// nothing; patterns the developer already has (in any position) are not repeated.
func EnsureExclude(r *gitx.Repo) (changed bool, err error) {
	path := filepath.Join(r.CommonDir, "info", "exclude")
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	have := map[string]bool{}
	for _, line := range strings.Split(string(existing), "\n") {
		have[strings.TrimSpace(line)] = true
	}
	var missing []string
	for _, p := range ExcludePatterns {
		if !have[p] {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		return false, nil
	}
	var b strings.Builder
	b.Write(existing)
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		b.WriteString("\n")
	}
	if !have[excludeHeader] {
		b.WriteString(excludeHeader + "\n")
	}
	for _, p := range missing {
		b.WriteString(p + "\n")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, fsutil.WriteFileAtomic(path, []byte(b.String()), 0o644)
}

// Prepare pre-creates the skills dir and the exclude entries. It is what `ros-local link` and `login` do
// for the current repo, ahead of the first session.
func Prepare(r *gitx.Repo) (createdSkillsDir bool, err error) {
	if _, err := EnsureExclude(r); err != nil {
		return false, err
	}
	return EnsureSkillsDir(r.Root)
}
