package app

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hvoguetech/ros-claude-plugin/cli/internal/api"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/gitx"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/link"
)

// send hands the work in this repository to a ROS agent in the cloud as a Task (local-agent-connector.md §14):
// uncommitted changes are committed on a new `ros/handoff-<time>` branch, the branch is pushed with the developer's
// own git credentials, and a task is created on the repo's project with the branch and the hand-off note. The task
// then runs on the warm devbox like any task, resuming from that branch, and lands in the inbox as usual.
func (a *App) send(args []string) int {
	fs := a.flags("send")
	message := fs.String("message", "", "the hand-off note for the agent: goal, done, remaining, gotchas (default: stdin)")
	title := fs.String("title", "", "task title (default: the first line of the note)")
	branch := fs.String("branch", "", "branch to push (default: the current one, or a new ros/handoff-<time> branch when there are uncommitted changes)")
	noPush := fs.Bool("no-push", false, "don't commit or push; the branch must already be on the remote")
	project := fs.String("project", "", "project id (default: this repo's link)")
	if fs.Parse(args) != nil {
		return 2
	}
	repo, err := gitx.Find(a.cwd())
	if err != nil {
		return a.errorf("%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	note := strings.TrimSpace(*message)
	if note == "" && !isTerminal(a.Stdin) {
		b, _ := io.ReadAll(io.LimitReader(a.Stdin, 64<<10))
		note = strings.TrimSpace(string(b))
	}
	if note == "" {
		return a.errorf("say what the agent should do: --message \"…\" (or pipe the note on stdin)")
	}
	_, c, err := a.signedInClient(ctx)
	if err != nil {
		return a.errorf("%v", err)
	}
	remote := repo.Remote(ctx)
	full := gitx.FullName(remote)
	current := repo.Branch(ctx)
	br := *branch
	if *noPush {
		if br == "" {
			br = current
		}
	} else {
		dirty := repo.Dirty(ctx)
		if br == "" {
			br = current
			if dirty || current == "" {
				br = "ros/handoff-" + time.Now().UTC().Format("20060102-1504")
			}
		}
		if br != current {
			if err := a.git(repo.Root, "checkout", "-q", "-B", br); err != nil {
				return a.errorf("switching to %s: %v", br, err)
			}
		}
		if dirty {
			if err := a.git(repo.Root, "add", "-A"); err != nil {
				return a.errorf("staging changes: %v", err)
			}
			if err := a.git(repo.Root, "commit", "-q", "-m", "WIP: handed to ROS\n\n"+note); err != nil {
				return a.errorf("committing: %v", err)
			}
		}
		if remote == "" {
			return a.errorf("this repository has no remote to push to; add one, or use --no-push with a branch the agent can reach")
		}
		if err := a.git(repo.Root, "push", "-q", "-u", "origin", br); err != nil {
			return a.errorf("pushing %s: %v", br, err)
		}
		fmt.Fprintf(a.Stdout, "Pushed %s to %s.\n", br, firstNonEmpty(full, remote))
	}
	req := &api.TaskRequest{ProjectID: *project, Repo: remote, Message: note, Title: *title}
	if req.ProjectID == "" {
		if l, _ := link.Read(repo); l != nil {
			req.ProjectID = l.ProjectID
		}
	}
	if full != "" {
		req.RepoFullNames = []string{full}
		if br != "" {
			req.RepoBranches = map[string]string{full: br}
		}
	} else {
		fmt.Fprintln(a.Stderr, "note: the remote isn't a GitHub-style owner/name, so the task carries no repository; the note should say where the code is")
	}
	res, err := c.CreateTask(ctx, req)
	if err != nil {
		if api.HasCode(err, api.CodeNoProject) || api.HasCode(err, api.CodeProjectUnavailable) {
			return a.errorf("%v — run `ros-local link` in this repo, or pass --project", err)
		}
		return a.errorf("creating the task: %v", err)
	}
	fmt.Fprintf(a.Stdout, "Task %s created%s: %s\n", res.TaskID, titled(res.Title), res.URL)
	fmt.Fprintln(a.Stdout, "The agent continues from the pushed branch; its questions and result land in your ROS inbox.")
	return 0
}

func titled(t string) string {
	if t == "" {
		return ""
	}
	return " (" + t + ")"
}
