---
description: Hand this repo's work to a ROS agent in the cloud — commit, push, open a task with a hand-off note
argument-hint: [what the agent should do next]
allowed-tools: Bash(ros-local send:*), Bash(git status:*), Bash(git diff:*), Bash(git log:*)
---

The user wants a ROS agent in the cloud to continue the work in this repository. Instruction from the user, if any:
"$ARGUMENTS"

1. Look at `git status` and `git diff --stat` (and the recent `git log`) to see what changed in this session.
2. Write a hand-off note for the agent, in plain prose, with these parts: the goal; what is done; what remains
   (start with the user's instruction above when there is one); gotchas and decisions; the files touched. Keep it
   under 40 lines. Do not include secrets.
3. Run it, passing the note as one argument:

   ros-local send --message "<the note>"

   It commits uncommitted changes on a new `ros/handoff-<time>` branch, pushes it with the user's own git
   credentials, and creates a task on the repo's ROS project. Add `--title "…"` for a short title.
4. Report the task link it prints. If it says the repo isn't linked, tell the user to run `ros-local link` and try again.
