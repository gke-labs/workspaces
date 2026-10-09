---
name: jupyter-workspace-sync
description: >-
  Use this skill whenever the user wants to run, test, or debug Python code, Jupyter notebooks (.ipynb),
  shell commands, or distributed GPU/TPU training jobs (e.g. TorchTPU, TorchTitan, JAX, Ray, Kubeflow TrainJob)
  on a remote JupyterLab server or GKE Workspace pod using `jupyter-sync` and the Jupyter Workspace Sync extension.
---

# Running and Debugging Code on Remote GKE Workspaces (`jupyter-sync`)

The **`jupyter-sync`** CLI (`~/.local/bin/jupyter-sync`) synchronizes your **local workspace repository** with a remote **GKE Workspace** (CPU, GPU, or Cloud TPU pod) and executes Python code, `.ipynb` notebook cells, and shell commands remotely over Jupyter REST & WebSocket APIs.

---

## 🚨 CRITICAL GOLDEN RULES (Read Before Running Any Command)

### Rule 1: NEVER Run Python, `pip`, `pytest`, or `torchrun` Locally
- Your shell tool (`run_command`) runs on the **user's local workstation**, **NOT** on the remote GKE Workspace pod.
- The local workstation has **NO TPUs/GPUs**, **NO `torch_tpu` / `libtpu`**, and **NO in-cluster Kubernetes ServiceAccount**.
- **NEVER** run bare `python`, `python3`, `pip`, `pytest`, or `torchrun` locally to test workspace code.
- **ALWAYS** execute code on the remote workspace pod using:
  - `jupyter-sync exec "<python-code>"` (for Python snippets)
  - `jupyter-sync run-cell <notebook.ipynb> <cell-index>` (for notebook cells)
  - `jupyter-sync sh "<shell-command>"` (for remote shell commands, `pip`, `ps`, `ls`, `pytest`, etc.)

### Rule 2: NEVER Write Code or Temp Scripts to Local `/tmp`
- `jupyter-sync` **ONLY** syncs files located **inside the current workspace repository directory (`$PWD`)**.
- Any file you write to local `/tmp/...` (via `write_to_file` or local shell redirects) stays on the local workstation and is **NEVER uploaded** to the remote Jupyter server.
- **What to do instead**:
  1. **To run a one-off or multi-line Python check without creating a file**: pass it directly to `jupyter-sync exec` using a heredoc:
     ```bash
     jupyter-sync exec "$(cat << 'EOF'
     import torch, torch_tpu
     print("TPU devices:", torch.tpu.device_count())
     EOF
     )"
     ```
  2. **To create or edit a script/module/notebook**: write it **inside the workspace repository directory** (e.g. `./debug_check.py` or `./torchtitan_singlehost.ipynb`) so `jupyter-sync` automatically uploads it to the remote pod before execution.
  3. **If you need a temporary file inside `/tmp` on the remote pod**: create it via `jupyter-sync sh`:
     ```bash
     jupyter-sync sh "cat << 'EOF' > /tmp/remote_helper.py
     print('running on remote pod')
     EOF
     python3 /tmp/remote_helper.py"
     ```

### Rule 3: NEVER Forget or Drop the Remote Connection URL
- Each `run_command` tool call runs in a **separate subshell** — running `export JUPYTER_URL="..."` in one command **does NOT persist** to the next command!
- **Always persist the connection in Step 1** using `jupyter-sync connect`:
  ```bash
  jupyter-sync connect "<JUPYTER_URL>" --remote-dir "shared/$(basename "$PWD")"
  ```
- What `jupyter-sync connect` does:
  1. Verifies the remote Jupyter server connection and token.
  2. Saves the URL and `remoteDir` persistently to `~/.jupyter-sync/cli-sessions.json` for your current workspace directory.
  3. Performs an initial workspace file sync.
- Once connected (or after any `jupyter-sync` command is called once with `--url "<JUPYTER_URL>"`), **all subsequent `jupyter-sync` commands in that workspace automatically load the saved URL** from `~/.jupyter-sync/cli-sessions.json` (and `.vscode/settings.json`), even in new subshells!
- If the user did not paste a URL in the current message, **always run `jupyter-sync status` first** to check whether an active VS Code IPC bridge (`~/.jupyter-sync/bridge-*.sock`) or a saved session URL in `~/.jupyter-sync/cli-sessions.json` is already connected.

---

## Anti-Patterns vs. Correct Patterns

| ❌ DO NOT DO THIS (Runs Locally / Fails) | ✅ DO THIS INSTEAD (Runs Remotely via `jupyter-sync`) |
| :--- | :--- |
| `write_to_file` to `/tmp/test.py` then `python3 /tmp/test.py` | `jupyter-sync exec "$(cat << 'EOF'\n...\nEOF\n)"` (or save file inside repo root) |
| `python3 -c "import torch_tpu"` | `jupyter-sync exec "import torch_tpu; print(torch_tpu.__file__)"` |
| `pip install -r torchtitan/requirements.txt` | `jupyter-sync sh "PYTHONUSERBASE=/home/jovyan/shared/.pydeps pip install --user -r torchtitan/requirements.txt"` |
| `export JUPYTER_URL="..."` in one tool call, then bare commands without saving | `jupyter-sync connect "<JUPYTER_URL>" --remote-dir "shared/$(basename "$PWD")"` (persists URL across subshells) |
| Editing `.ipynb` raw JSON with text replacement tools | Use `notebook_edit` tool, then run `jupyter-sync run-cell <notebook.ipynb> <cell_idx>` (or `jupyter-sync sync --overwrite-notebooks`) |

---

## 1. Connection Setup & Status Check

Always begin your workflow by checking or establishing the remote connection:

```bash
# 1. Check if VS Code IPC bridge or a saved CLI session is already active:
jupyter-sync status
```

- **If the user provided a connection URL** (e.g. `https://connect.<IP>.sslip.io/workspace/connect/<NS>/<WORKSPACE>/jupyterlab/?token=<JWT>`), bind and save it immediately:
  ```bash
  jupyter-sync connect "<JUPYTER_URL>" --remote-dir "shared/$(basename "$PWD")"
  ```
- **Shared Filestore PVC Convention (`shared/<repo-name>`)**:
  GKE Workspaces mount a shared `ReadWriteMany` Filestore PVC at `/home/jovyan/shared`. Always set `.vscode/settings.json` in the local project root so both VS Code and `jupyter-sync` sync into `/home/jovyan/shared/<repo-name>`:
  ```json
  {
    "jupyterSync.remoteBaseDir": "shared/${workspaceFolderBasename}"
  }
  ```
- **If the token is expired (`HTTP 401` / `HTTP 403`)**:
  Ask the user to generate a fresh connection URL from the **Kubeflow Workspaces Dashboard** (`/workspaces/connections` → select workspace → port `jupyterlab` → **Generate connection**), then run `jupyter-sync connect "<NEW_URL>"`.

---

## 2. Core `jupyter-sync` Commands

Once connected via `jupyter-sync connect` (or VS Code IPC Bridge), you do not need to repeat `--url` on every command:

```bash
# 1. Check active connection mode ('ipc-bridge' or 'headless'), server URL, and remoteBaseDir
jupyter-sync status

# 2. Save/update the remote workspace URL and run an immediate sync
jupyter-sync connect "<JUPYTER_URL>" [--remote-dir "shared/$(basename "$PWD")"]

# 3. Sync local Git-tracked / modified files in the current workspace to the remote pod
jupyter-sync sync

# 4. Sync and overwrite remote .ipynb files (e.g. after editing cells with notebook_edit without running them)
jupyter-sync sync --overwrite-notebooks

# 5. Force full re-upload of all workspace files (bypasses manifest hash cache if remote files were deleted out-of-band)
jupyter-sync sync --force

# 6. Execute a 0-based cell from a local .ipynb on the remote kernel and sync outputs to BOTH local and remote .ipynb
jupyter-sync run-cell <path/to/notebook.ipynb> <cell-index> [--timeout 600000]

# 7. Inspect saved or live in-memory cell outputs & tracebacks from a notebook
jupyter-sync outputs <path/to/notebook.ipynb> [--cell <cell-index>]

# 8. Execute Python code on the remote kernel (auto-syncs local workspace files first)
jupyter-sync exec "<python-code>" [--notebook <path/to/notebook.ipynb>] [--timeout 600000]

# 9. Interrupt or restart the remote Python kernel (clears stuck executions, os.environ mutations, and kernel TPU locks)
jupyter-sync interrupt [--notebook <path/to/notebook.ipynb>]
jupyter-sync restart-kernel [--notebook <path/to/notebook.ipynb>]

# 10. Execute a shell command on the remote pod via Jupyter Terminal WebSocket
#     (Works out-of-band even while the Python kernel is busy running training!)
jupyter-sync sh "<shell-command>" [--cwd <relative-subdir>] [--timeout 600000]
```

---

## 3. Iterative Notebook & Code Debugging Loop

1. **Edit Locally Inside the Workspace**:
   - Edit `.py`, `.yaml`, or `.toml` files inside the local repository.
   - Edit `.ipynb` cells locally using the `notebook_edit` tool (`list`, `get`, `update`, `add`, `delete`).
   - **Keeping Remote `.ipynb` Files in Sync**:
     - By default (`overwriteNotebooks: false`), background file sync skips existing `.ipynb` files so running remote outputs are protected.
     - `jupyter-sync run-cell <nb.ipynb> <idx>` automatically uploads your updated local `.ipynb` to the remote server **before** running the cell and uploads the updated `.ipynb` (with execution outputs) **after** running the cell, so the user viewing JupyterLab in their browser sees all cell edits and outputs immediately.
     - If you edit a `.ipynb` locally with `notebook_edit` and want to push those edits to the remote server *without* running a cell, run `jupyter-sync sync --overwrite-notebooks`.
   - **Re-uploading Files Deleted Out-of-Band (`--force`)**:
     - `jupyter-sync sync` relies on `.workspace-sync-manifest.json` to skip unchanged files (`Synced 0 file(s), N unchanged`). If a file was deleted or modified directly on the remote pod via `jupyter-sync sh`, run `jupyter-sync sync --force` to bypass the hash cache and re-upload all workspace files.
2. **Sync & Run Remotely**:
   - Run `jupyter-sync run-cell <nb.ipynb> <idx>` or `jupyter-sync exec "<python>"`.
   - Every `run-cell` and `exec` automatically runs a **Pre-Execution Sync Barrier** (uploading modified local files first) and initializes the remote kernel with:
     - `os.chdir()` set to the notebook's remote directory
     - `sys.path` containing both the notebook directory and the synced repository root
     - `%load_ext autoreload` + `%autoreload 2` so edits to imported `.py` files take effect without restarting the kernel.
3. **Recovering from Timed-Out or Killed `exec` / `run-cell` Calls**:
   - Because Jupyter kernels process execution requests sequentially over ZMQ, if a `jupyter-sync exec` or `run-cell` command times out or is killed while a subprocess (such as `torchrun`) is hung, any subsequent `jupyter-sync exec` will block in the kernel queue behind the stuck request.
   - `jupyter-sync` automatically sends a kernel interrupt (`POST /api/kernels/<id>/interrupt`) on timeout or `SIGINT`/`SIGTERM`, but C++ collectives or child `torchrun` processes may ignore `SIGINT`.
   - **Always use `jupyter-sync sh` (which uses the out-of-band terminal WebSocket) to kill stuck subprocesses and/or restart the kernel before issuing another `jupyter-sync exec`**:
     ```bash
     jupyter-sync sh "pkill -9 -f torchrun || true"
     jupyter-sync restart-kernel
     ```

---

## 4. Cloud TPU & GKE Distributed Debugging Playbook

### A. Preventing `/dev/vfio/*` TPU Lock Contention & Kernel `os.environ` Pollution
Cloud TPU devices (`/dev/vfio/0`, `/dev/vfio/1`, ...) can only be opened by **one process at a time**. `libtpu` / `PJRT` holds the device lock for the entire lifetime of the process (`del tensor` and `gc.collect()` do **not** release it). Furthermore, `jupyter-sync exec` and `jupyter-sync run-cell` share a **persistent remote IPython kernel**, so any in-process mutation to `os.environ` persists across calls and is inherited by child `torchrun` subprocesses.

- **Rule 1 — Do Not Call Distributed/Device Init Helpers Directly in the Parent Kernel**:
  Do **not** call distributed or device initialization functions (such as `run_distributed(...)`, `torch.device("tpu")`, or `jax.devices()`) directly inside `jupyter-sync exec` on the parent notebook kernel:
  - Calling `torch.device("tpu")` or `jax.devices()` in the parent kernel locks `/dev/vfio/0`, causing child `torchrun` workers to fail with `Device or resource busy`.
  - Calling helpers like `torchtitan.experiments.tpu.distributed.run_distributed(..., AcceleratorDeviceType.CPU)` mutates `os.environ` in the persistent kernel (e.g. setting `TORCH_DEVICE_BACKEND_AUTOLOAD=0`, `MASTER_ADDR`, `RANK`), causing subsequent child `torchrun` jobs to inherit those variables and silently fall back to CPU!
  - If the parent kernel's `os.environ` or `/dev/vfio/*` state is ever polluted, reset it immediately with:
    ```bash
    jupyter-sync restart-kernel
    ```
- **Rule 2 — Clear Orphaned Locks Before Launching**:
  If a previous `torchrun` run crashed or hung, clear orphaned worker processes using `tpu_lock.py` (or `jupyter-sync sh`):
  ```bash
  jupyter-sync exec "import tpu_lock; tpu_lock.preflight_check(is_distributed=True, auto_clear=True)"
  ```

### B. Installing Dependencies onto the Shared RWX Volume (`/home/jovyan/shared/.pydeps`)
Never install project requirements locally on the workstation, and avoid ephemeral container root installs when using multi-host `TrainJob`:
1. Install repo requirements into `/home/jovyan/shared/.pydeps` (`PYTHONUSERBASE`) on the remote pod:
   ```bash
   jupyter-sync sh "PYTHONUSERBASE=/home/jovyan/shared/.pydeps pip install --user -q -r torchtitan/requirements.txt"
   ```
2. Call `tpu_trainer.configure_runtime_env(user_base="/home/jovyan/shared/.pydeps", extra_pythonpath=repo_dir)` so both the interactive kernel and `torchrun` subprocesses use `/home/jovyan/shared/.pydeps`.
3. When submitting multi-host Kubeflow `TrainJob` runs via `tpu_trainer.submit_multihost_training(..., src_dir=repo_dir, user_base="/home/jovyan/shared/.pydeps")`, `tpu_trainer` mounts `src_dir` at `/workspace` and `user_base` at `/workspace-deps` (`PYTHONUSERBASE=/workspace-deps`) across all TPU worker pods — enabling **zero-Docker-rebuild** multi-host iteration.

### C. Querying Kubernetes from Inside the Workspace Pod
If local `kubectl` / `gcloud` credentials on the workstation are expired (e.g. RAPT token expiration) or unavailable, query the GKE API directly from the remote Workspace pod using its in-cluster ServiceAccount:

```bash
jupyter-sync exec "$(cat << 'EOF'
from kubernetes import client, config
config.load_incluster_config()
v1 = client.CoreV1Api()
for p in v1.list_namespaced_pod("kubeflow-user").items:
    print(p.metadata.name, p.status.phase, p.spec.node_name)
EOF
)"
```
