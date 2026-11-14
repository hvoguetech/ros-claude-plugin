package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hvoguetech/ros-claude-plugin/cli/internal/api"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/config"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/fsutil"
)

// maxSnapshotBytes bounds what a workspace snapshot may unpack to (the server caps snapshots at 300 MB raw).
const maxSnapshotBytes = 2 << 30

// BriefingFile is written into the pickup directory with the briefing, so it survives the first session.
const BriefingFile = "ROS-PICKUP.md"

// pickup continues a cloud run on this machine: it fetches the run's pickup (the conversation's workspace
// snapshot, its last report, the question it waits on, the repositories), unpacks the snapshot (or clones the
// repositories) into a directory, records the pickup on the run, and starts Claude Code there with the briefing
// as the first message. The ros plugin then gives that session the project's memory and skills as usual.
func (a *App) pickup(args []string) int {
	fs := a.flags("pickup")
	dir := fs.String("dir", "", "where to put the files (default: ~/ros/<run folder>)")
	noClaude := fs.Bool("no-claude", false, "set the files up but don't start Claude Code")
	force := fs.Bool("force", false, "use the directory even when it isn't empty")
	if fs.Parse(args) != nil {
		return 2
	}
	// Flags may follow the run id (`pickup r1 --dir X`): take the id, then parse what came after it.
	if fs.NArg() < 1 {
		return a.errorf("usage: ros-local pickup <run id or console URL> [--dir DIR] [--no-claude]")
	}
	ref := fs.Arg(0)
	if fs.Parse(fs.Args()[1:]) != nil || fs.NArg() != 0 {
		return a.errorf("usage: ros-local pickup <run id or console URL> [--dir DIR] [--no-claude]")
	}
	runID := runIDFrom(ref)
	if runID == "" {
		return a.errorf("can't tell the run id from %q", fs.Arg(0))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	_, c, err := a.signedInClient(ctx)
	if err != nil {
		return a.errorf("%v", err)
	}
	p, err := c.RunPickup(ctx, runID)
	if err != nil {
		if api.IsStatus(err, 404) {
			return a.errorf("no run %s that you can see (is this the right ROS? %s)", runID, a.urlOf())
		}
		return a.errorf("asking ROS about run %s: %v", runID, err)
	}
	target, err := a.pickupDir(*dir, p.Folder)
	if err != nil {
		return a.errorf("%v", err)
	}
	if !*force {
		if entries, err := os.ReadDir(target); err == nil && len(entries) > 0 {
			return a.errorf("%s already has files; pass --dir for another place, or --force to use it anyway", target)
		}
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return a.errorf("%v", err)
	}
	switch {
	case p.Snapshot != nil:
		body, size, err := c.Download(ctx, p.Snapshot.URL)
		if err != nil {
			return a.errorf("fetching the workspace snapshot: %v", err)
		}
		n, err := fsutil.ExtractTarGz(body, target, maxSnapshotBytes)
		body.Close()
		if err != nil {
			return a.errorf("unpacking the workspace snapshot: %v", err)
		}
		fmt.Fprintf(a.Stdout, "Unpacked %d files (%s, taken %s) into %s\n", n, humanSize(firstNonZero(size, p.Snapshot.Size)),
			p.Snapshot.TakenAt, target)
	case len(p.Repos) > 0:
		fmt.Fprintln(a.Stdout, "No workspace snapshot for this conversation; cloning its repositories.")
		for _, r := range p.Repos {
			if r.URL == "" {
				continue
			}
			args := []string{"clone"}
			if r.Branch != "" {
				args = append(args, "--branch", r.Branch)
			}
			name := filepath.Base(strings.TrimSuffix(r.URL, ".git"))
			if err := a.git(target, append(args, r.URL, name)...); err != nil {
				return a.errorf("cloning %s: %v", r.URL, err)
			}
		}
	default:
		fmt.Fprintln(a.Stdout, "No workspace snapshot and no repositories on this run; starting with an empty directory.")
	}
	brief := p.Briefing
	if strings.TrimSpace(brief) == "" {
		brief = fmt.Sprintf("Continue this work from ROS (run %s).", p.RunID)
	}
	if err := fsutil.WriteFileAtomic(filepath.Join(target, BriefingFile),
		[]byte(brief+"\n\n---\nPicked up from ROS on "+time.Now().UTC().Format(time.RFC3339)+". Send it back with `ros-local send`.\n"), 0o644); err != nil {
		return a.errorf("writing %s: %v", BriefingFile, err)
	}
	host, _ := os.Hostname()
	if err := c.MarkPickup(ctx, p.RunID, host, target); err != nil {
		fmt.Fprintf(a.Stderr, "note: couldn't record the pickup on the run: %v\n", err)
	}
	if p.Question != "" {
		fmt.Fprintf(a.Stdout, "It is waiting on: %s\n", p.Question)
	}
	for _, r := range p.Repos {
		fmt.Fprintf(a.Stdout, "Repository: %s%s\n", firstNonEmpty(r.FullName, r.URL), branchNote(r.Branch))
	}
	for _, pr := range p.PullRequests {
		fmt.Fprintf(a.Stdout, "Pull request: %s\n", firstNonEmpty(pr.URL, pr.Title))
	}
	fmt.Fprintf(a.Stdout, "Briefing saved as %s\n", filepath.Join(target, BriefingFile))
	if *noClaude {
		fmt.Fprintf(a.Stdout, "Next: cd %s && claude   # paste the briefing, or start with the file above\n", target)
		return 0
	}
	fmt.Fprintf(a.Stdout, "Starting Claude Code in %s …\n", target)
	if err := a.exec(target, []string{"claude", brief}); err != nil {
		var pathErr *exec.Error
		if errors.As(err, &pathErr) {
			return a.errorf("Claude Code isn't on PATH; run: cd %s && claude", target)
		}
		return a.errorf("starting Claude Code: %v", err)
	}
	return 0
}

// pickupDir is where the files go: --dir, else ~/ros/<folder>.
func (a *App) pickupDir(flag, folder string) (string, error) {
	if flag != "" {
		return filepath.Abs(flag)
	}
	home := a.Getenv("HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = h
	}
	if folder == "" {
		folder = "ros-pickup"
	}
	return filepath.Join(home, "ros", folder), nil
}

// runIDFrom accepts a bare run id or a console URL carrying `run=<id>` (or `id=<id>`).
func runIDFrom(s string) string {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "://") || strings.Contains(s, "?") {
		if u, err := url.Parse(s); err == nil {
			q := u.Query()
			for _, k := range []string{"run", "run_id", "id"} {
				if v := q.Get(k); v != "" {
					return v
				}
			}
		}
		return ""
	}
	return s
}

func (a *App) exec(dir string, argv []string) error {
	if a.Exec != nil {
		return a.Exec(dir, argv)
	}
	return execProgram(dir, argv)
}

// git runs a git command with the developer's own credentials and shows its output.
func (a *App) git(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = a.Stdin, a.Stdout, a.Stderr
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	return cmd.Run()
}

func (a *App) urlOf() string {
	cfg, err := config.Load()
	if err != nil {
		return ""
	}
	return cfg.URL
}

func branchNote(b string) string {
	if b == "" {
		return ""
	}
	return " (" + b + ")"
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func firstNonZero(vals ...int64) int64 {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}
