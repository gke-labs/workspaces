# Background agents on Agent Substrate

This Workspace can hand coding work to **background agents** that run on the
cluster, not in this Workspace. Each agent gets its own copy of a project
directory, works on one task in an isolated sandbox, and leaves a patch that you
review and apply here. Agents keep running when the browser is closed. Idle
agents can be suspended (then they cost only a snapshot in Cloud Storage) and
resumed later with their files intact.

Use them when the user asks to run work in the background or in parallel, to
"send agents", to fan a job out (for example one agent per failing test), or for
long-running work that should not tie up this session. For a quick, single edit,
just do it yourself.

The `ax-agents` command manages them. **Activate the `agent-substrate` skill
before using it**: it has the workflow, how to write good agent tasks, and how
to review, merge and clean up.
