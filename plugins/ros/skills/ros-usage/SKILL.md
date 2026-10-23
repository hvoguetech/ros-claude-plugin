---
name: ros-usage
description: How to use ROS (the team's agent platform) from this session — its recalled memory, the ros MCP tools (recall_memory, remember, ask_team_memory, recall_knowledge, search_knowledge_base, search_past_runs) and the synced ros-* skills. Use when the user mentions ROS, team or project memory, past runs, or asks you to remember something for later.
---

# Using ROS in a local session

ROS is the team's agent platform. This session is connected to it through the `ros` plugin. ROS keeps the
team's memory, approved skills and connectors centrally; nothing of it is stored on this laptop except
the synced skills.

## What you already have

- **A recalled-memory block.** At session start the plugin adds a `ROS · project "…"` status line and,
  when there is anything to recall, a `<ros-memory>` block. Each item has an id such as `[fact:…]` or
  `[mem:…]`. Cite the id when you rely on an item. If an item is wrong or stale, say so and offer to
  correct it with `remember`.
- **Project skills** in `.claude/skills/ros-*`. They are the project's approved skills, synced from ROS
  and git-excluded. Use them like any other skill. Don't edit them in place: local edits are overwritten
  (after a backup) when ROS publishes a new version. Suggest the user proposes the change in ROS instead.

## The `ros` MCP tools

| Tool | Use it to |
|---|---|
| `recall_memory(query)` | Look up the user's and the project's memory beyond what was recalled at start. |
| `ask_team_memory(question)` | Ask a question answered from the team's shared knowledge. |
| `search_knowledge_base(query)` | Search the project's knowledge base documents. |
| `recall_knowledge(query)` | Look up team knowledge: facts people or agents saved in the project (past decisions, conventions, names). |
| `search_past_runs(query)`, `get_past_run(run_id)` | Find how a similar task went before (devbox runs, workflows, local sessions). |
| `search_past_chats(query)` | Find earlier ROS chats about a topic. |
| `memory_view`, `memory_append` | Read or add to the user's personal memory blocks. |
| `remember(text, scope)` | Save a durable fact. `scope="me"` (default) saves to the user's own memory. `scope="project"` only *proposes* a project fact; a project admin reviews it before anyone else sees it. |

Guidelines:

- **Project.** The tools act on the project named in the status line. Pass `project="<id>"` (the id from
  the status line) only if a call needs a different project or a tool asks for it. An unknown project is
  an error, never a silent fallback: if a call fails on the project, tell the user rather than guessing.
- **Available tools depend on sign-in scopes.** If a tool above isn't listed, the device wasn't granted it.
- **Tool errors that need the user.** A tool result with `"error": "project_unavailable"` means this
  repo's ROS project can't be used (its `reason` says why: deleted, archived, no access, local agent
  off). `"no_project"` means no project could be resolved. In both cases tell the user to run
  `ros-local link` in a terminal; don't retry with a guessed project. `"local_agent_disabled"` means an
  admin has to turn the local agent on.
- **Remember sparingly.** Save decisions, conventions and gotchas that will matter in a later session,
  not a log of this one. Ask before saving anything about a person, and never save secrets, tokens or
  customer data.
- **Prefer ROS over guessing.** Before re-deriving a team convention, check `recall_memory` or
  `ask_team_memory`.

## When ROS isn't available

If the status line says `ROS: signed out`, `ROS: offline` or that `ros-local` isn't installed, the
tools above won't work this session. Tell the user the fix shown in the banner (usually run
`ros-local login` in a terminal) and carry on without ROS.
