// Package gitx finds the git repository a directory belongs to and reads the few facts ros-local needs
// (remote, branch, head, recent commit subjects).
//
// Repository discovery is pure Go (no git process), so `ros-local mcp-headers` stays fast. Worktrees and
// submodules are handled: a `.git` file points at the real git dir, and a `commondir` file inside it
// points at the shared one, which is where `info/exclude` and `ros.json` live.
package gitx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrNotRepo is returned when no enclosing git repository is found.
var ErrNotRepo = errors.New("not inside a git repository")

// Repo is a working tree and its git directories.
type Repo struct {
	Root      string // the working tree root
	GitDir    string // this worktree's git dir
	CommonDir string // the shared git dir (== GitDir outside linked worktrees)
}

// Find walks up from start to the nearest working tree root.
func Find(start string) (*Repo, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return nil, err
	}
	for {
		dotgit := filepath.Join(dir, ".git")
		if fi, err := os.Stat(dotgit); err == nil {
			if fi.IsDir() {
				return &Repo{Root: dir, GitDir: dotgit, CommonDir: commonDir(dotgit)}, nil
			}
			if gd, err := readGitFile(dotgit); err == nil {
				return &Repo{Root: dir, GitDir: gd, CommonDir: commonDir(gd)}, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, ErrNotRepo
		}
		dir = parent
	}
}

// readGitFile resolves a `.git` file (`gitdir: <path>`), as used by worktrees and submodules.
func readGitFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(b))
	if !strings.HasPrefix(line, "gitdir:") {
		return "", errors.New("not a gitdir file")
	}
	gd := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(filepath.Dir(path), gd)
	}
	if fi, err := os.Stat(gd); err != nil || !fi.IsDir() {
		return "", errors.New("gitdir does not exist")
	}
	return filepath.Clean(gd), nil
}

func commonDir(gitDir string) string {
	b, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return gitDir
	}
	cd := strings.TrimSpace(string(b))
	if !filepath.IsAbs(cd) {
		cd = filepath.Join(gitDir, cd)
	}
	return filepath.Clean(cd)
}

func (r *Repo) git(ctx context.Context, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", r.Root}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Remote is the `origin` URL, or the first remote's when there is no origin.
func (r *Repo) Remote(ctx context.Context) string {
	if u := r.git(ctx, "config", "--get", "remote.origin.url"); u != "" {
		return u
	}
	for _, name := range strings.Fields(r.git(ctx, "remote")) {
		if u := r.git(ctx, "config", "--get", "remote."+name+".url"); u != "" {
			return u
		}
	}
	return ""
}

// Branch is the current branch name, or "" when detached.
func (r *Repo) Branch(ctx context.Context) string {
	return r.git(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
}

// Head is the current commit sha, or "" in an empty repository.
func (r *Repo) Head(ctx context.Context) string {
	return r.git(ctx, "rev-parse", "--verify", "--quiet", "HEAD")
}

// RecentCommits returns up to n recent commit subjects (the recall query, §8.1).
func (r *Repo) RecentCommits(ctx context.Context, n int) []string {
	out := r.git(ctx, "log", "-n", strconv.Itoa(n), "--format=%s")
	if out == "" {
		return nil
	}
	var subjects []string
	for _, s := range strings.Split(out, "\n") {
		if s = strings.TrimSpace(s); s != "" {
			if len(s) > 200 {
				s = s[:200]
			}
			subjects = append(subjects, s)
		}
	}
	return subjects
}

var scp = regexp.MustCompile(`^(?:[^@/]+@)?([^:/]+):(.+)$`)

// RepoKey normalises a git remote to `host/owner/name` (lowercase, no scheme, user, port or .git), so the
// SSH and HTTPS spellings of one repo compare equal. It mirrors the backend's
// `ros.services.local_config.repo_key`. Returns "" when the value doesn't look like a remote.
func RepoKey(remote string) string {
	r := strings.TrimSpace(remote)
	if r == "" {
		return ""
	}
	var host, path string
	if i := strings.Index(r, "://"); i >= 0 {
		rest := r[i+3:]
		host, path, _ = strings.Cut(rest, "/")
		if j := strings.LastIndex(host, "@"); j >= 0 {
			host = host[j+1:]
		}
		host, _, _ = strings.Cut(host, ":")
	} else {
		m := scp.FindStringSubmatch(r)
		if m == nil {
			return ""
		}
		host, path = m[1], m[2]
	}
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.Trim(path, "/")
	if host == "" || !strings.Contains(path, "/") {
		return ""
	}
	return strings.ToLower(host + "/" + path)
}
