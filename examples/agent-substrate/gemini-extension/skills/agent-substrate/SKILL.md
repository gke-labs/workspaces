---
name: agent-substrate
description: Run coding tasks as background agents in isolated sandboxes on the cluster (Agent Substrate + AX) with the `ax-agents` CLI. Launch one or many agents on a project directory, monitor them, review and apply their patches, suspend and resume them, and clean up. Use when the user wants work done in the background, in parallel (for example one agent per failing test or per module), or isolated from this Workspace.
---

# Background coding agents on Agent Substrate

`ax-agents` (on `PATH`) runs coding agents **on the cluster, not in this
Workspace**. Each agent:

- gets its **own copy** of a local project directory (uploaded at launch),
- works on **one task** in its own gVisor sandbox, with a Gemini-powered coding
  agent inside, and runs an optional **check command** when it is done,
- keeps its files on a durable volume and hands back a **patch** you review and
  apply here.

Many agents share a few warm worker pods, so starting one takes seconds. A
**suspended** agent is snapshotted to Cloud Storage and uses no CPU or memory;
**resuming** it brings back its files (not its running processes) in seconds,
on any free worker.

## Workflow

### 1. Plan the work

- Find out what needs doing before you launch anything. For a test suite, run it
  locally first (for example `python3 -m unittest discover -s tests -t .`) and
  note which tests fail.
- Split the job into **independent** tasks, one per agent, that touch different
  code where possible (one failing test module, one module to refactor, one
  feature). Agents cannot see each other or this Workspace.
- Pick short names: lowercase letters, digits and `-`, at most 40 characters,
  for example `fix-roman`. Names must not clash with existing agents or AX
  tasks; if `launch` reports a clash, pick other names (`fix-roman-2`). **Never
  delete agents or tasks you did not start to free a name**: they may hold
  someone's unmerged work.

### 2. Launch

One agent:

```bash
ax-agents launch --dir ~/textkit \
  --check "python3 -m unittest -q tests.test_roman" \
  --agent fix-roman "Fix the bug in textkit that makes tests/test_roman.py fail. Change only library code under textkit/."
```

Several agents, each with its own check: pass a JSON list on stdin. This is
the usual form for a fan-out.

```bash
ax-agents launch --dir ~/textkit --agents-file - <<'EOF'
[
  {"name": "fix-roman", "task": "Fix the bug that makes tests/test_roman.py fail. Change only library code.",
   "check": "python3 -m unittest -q tests.test_roman"},
  {"name": "fix-slugify", "task": "Fix the bug that makes tests/test_slugify.py fail. Change only library code.",
   "check": "python3 -m unittest -q tests.test_slugify"}
]
EOF
```

Options: `--timeout 30m` (when to stop an agent), `--cpu 1`, `--memory 2Gi`
(per-agent limits), `--check CMD` (default check for every agent).

After launching, tell the user which agents you started and that they run in
the background: they can keep working, close the browser, and come back later.

### 3. Monitor

- `ax-agents list` shows every agent: `STATE` is `working`, `done`, `failed`,
  `timed-out` or `suspended`. `CHECK` is `pass` or `fail` once the agent's check
  has run. `FILES` is how many files it changed, `WORKER` is the worker pod it
  runs on. The footer shows how many agents share each worker pod.
- `ax-agents wait [NAMES] --timeout 15` blocks and prints a line whenever an
  agent changes state, then prints the table. An agent typically needs 2 to 8
  minutes. If `wait` times out, run it again, or let the user carry on and check
  back later.
- `ax-agents logs NAME` shows what the agent did (files viewed and edited,
  commands run) and, once finished, its own summary.

### 4. Review

- `ax-agents diff NAME --stat` lists the changed files; `ax-agents diff NAME`
  prints the full patch. Read it before applying. Check that it does what the
  task asked, stays small, and does **not** weaken or delete tests.
- Show the user the important parts of each patch, and say which agents
  passed their check and which did not.
- If an agent failed or its patch is wrong, you can look around its sandbox with
  `ax-agents sh NAME -- <command>` (for example `-- cat /workspace/.agent/check.log`).
  Then either fix the problem yourself, or delete the agent and launch a new one
  with a better task.

### 5. Apply and verify

- `ax-agents apply NAME [NAME ...]` applies the patches to the directory each
  agent was launched from (or `--to DIR`). Apply the agents whose check passed.
- If a patch conflicts with changes already applied, the clean hunks are applied
  and the rest are written to `*.rej` files next to the originals. Resolve
  them by hand, then delete the `.rej` files.
- **Always run the project's tests locally afterwards** and report the result.

### 6. Clean up

- `ax-agents delete NAME ...` or `ax-agents delete --all` removes agents and
  their sandboxes once their work is merged. Ask the user before deleting
  agents whose work has not been applied. It refuses AX tasks that were not
  started by `ax-agents` in this Workspace unless you pass `--force`; only do
  that when the user asks.
- To keep agents for later instead, use `ax-agents suspend NAME ...` or
  `ax-agents suspend --all`, then `ax-agents resume ...` when needed. `logs`,
  `diff`, `apply` and `sh` need the agent to be running, so resume it first.

## Writing good agent tasks

An agent only knows its task text, the check command and the project files.
Write each task so that it stands on its own:

- **State the goal and the done criterion**, for example "make
  tests/test_roman.py pass". Give a check command whenever there is a test or
  script that proves the work.
- **Point at the relevant files** if you know them, and say what must not change
  ("change only library code under textkit/, not the tests", "keep the public API").
- **Respect the sandbox.** Agents have Python 3.12, git, curl and a shell, but
  **no internet access apart from the Gemini API**. They cannot `pip install`,
  download files or clone repositories, so give them tasks that need only what is
  in the project and the standard library.
- **No questions.** Nobody answers, so give all the context up front.
- One agent per independent piece of work. Avoid giving two agents the same
  file to edit unless you plan to merge by hand.

## Suspend and resume

- Suspending saves the agent's `/workspace` (the project copy and its status
  files) to Cloud Storage and frees its CPU and memory. Running processes are
  not saved.
- If you suspend an agent while it is still `working`, resuming it restarts the
  coding agent, which continues from the files on disk ("attempt 2" in `logs`).
  A `done` agent stays done after a resume.
- `ax-agents resume --all` is quick (seconds per agent) because Substrate
  restores snapshots instead of scheduling new pods.

## Limits

- `launch` uploads the **text** files of `--dir` (at most 512 KiB each and
  3 MiB in total). It skips binaries, `.git`, virtualenvs, `node_modules` and
  caches. For big repositories, launch on the subdirectory that matters.
- Agents run on a worker pool shared by the namespace. If `launch` or `resume`
  fails because there is no room, suspend or delete some agents, or start fewer.
- Each agent stops after `--timeout` (default 30 minutes) and reports
  `timed-out`. Its partial changes are still available through `diff`.

## Troubleshooting

| Symptom | What to do |
| :--- | :--- |
| `the ax CLI is not installed` | The Workspace does not use the "Code Server + AX agents" image. Ask the user to create one with that image. |
| `launch`/`resume` fails with `ActorResumeFailed` or `crashed` | Retry once with `ax-agents resume NAME`. If it fails again, show the user the message. It usually means no worker capacity, or a platform problem for the administrator. |
| `STATE` is `failed` | `ax-agents logs NAME` shows the error (for example `GEMINI_API_KEY is not set in the sandbox`, which the administrator must fix). |
| `STATE` stays `unknown` | The agent is not reachable over `ax ssh`. Wait a few seconds, then retry. `ax-agents ax describe task NAME` shows its conditions. |
| `X is Suspended; run ax-agents resume X first` | Resume it, then retry. |
| `agent(s) already exist` | Choose new names. Only delete the old agents if the user confirms their work is no longer needed. |

## Under the hood

You rarely need these, but they help when debugging:

- Each agent is an **AX `Task`** that runs as an **Agent Substrate actor**. The
  uploaded project is an **AX `Workspace`** named `<dir>-<hash>`, shared by the
  agents launched from the same files.
- `ax-agents ax <args>` runs any `ax` command against this namespace's atespace,
  for example `ax-agents ax get tasks` or `ax-agents ax describe task fix-roman`.
- Inside a sandbox, the agent works on `/workspace/work/<dir>`: a Git
  repository whose original files are tagged `ax-baseline`. Its state is in
  `/workspace/.agent/` (`status.json`, `activity.log`, `summary.md`,
  `check.log`). Both live on the durable volume that survives suspend and resume.
- The sandboxes can reach only the Gemini API on the network, and nothing in the
  cluster.
