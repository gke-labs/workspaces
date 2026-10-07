# Jupyter Workspace Sync (`jupyter-workspace-sync`)

Connect local VS Code notebooks (`.ipynb`) to remote **Kubeflow / GKE Workspaces** (CPU, GPU, or Cloud TPU pods) or any remote **JupyterLab / JupyterHub** server — with **automatic repository synchronization**, **nested relative imports**, **live `%autoreload 2`**, and an **agentic CLI (`jupyter-sync`)**.

---

## Why Jupyter Workspace Sync?

When you connect standard VS Code (`ms-toolsai.jupyter`) to a remote Jupyter server URL, **only the code inside the executed notebook cell is sent to the remote kernel**. Your local repository files (`utils.py`, `models/`, `configs/default.yaml`) remain on your laptop, causing `ModuleNotFoundError` and `FileNotFoundError` on the remote kernel.

**Jupyter Workspace Sync** solves this cleanly using only the standard Jupyter REST & WebSocket APIs (no SSH required):

1. **One-Paste Connection**: Paste your GKE Workspace / JupyterLab URL (including `?token=...` JWTs) into the Command Palette or the Notebook Kernel Picker.
2. **Fast Initial & Incremental File Sync**:
   - Compresses your Git-tracked workspace into a single `.tar.gz` archive (for >20 files) and extracts it on the remote pod via the Jupyter Terminal API — syncing hundreds of files in seconds even while a TPU training cell is busy.
   - Watches local file saves and pushes incremental deltas over `/api/contents` within ~300 ms.
   - **Protects `.ipynb` Outputs**: Remote notebooks are never overwritten by background file syncs.
3. **Zero-Config Relative Imports & Working Directory**:
   - Automatically intercepts `execute_request` over the Jupyter WebSocket channel before your first cell runs.
   - Sets the remote kernel's `os.chdir()` to the **exact directory of your active `.ipynb` file** (e.g. `experiments/exp1/`) and adds both the notebook directory and repository root to `sys.path`.
   - Enables IPython `%autoreload 2` automatically so edits you save to local `.py` files take effect on the next cell execution without restarting your TPU/GPU kernel.
4. **Zero-Disconnect Token Renewal**:
   - Update expiring GKE `desktopToken` JWTs in-place via **`Jupyter Sync: Update Connection Token`** without restarting the kernel or losing in-memory model weights.
5. **Agentic CLI (`jupyter-sync`)**:
   - Automatically installs the `jupyter-sync` CLI into `~/.local/bin/jupyter-sync` so terminal sessions and AI coding agents (`jetski-cli`, Claude Code, Cursor) can run code, execute notebook cells, inspect outputs, and run shell commands on the remote TPU/GPU pod.

---

## Installation from This Repository

### Prerequisites

1. **Visual Studio Code** (`v1.85.0` or newer)
2. **Microsoft Jupyter Extension** (`ms-toolsai.jupyter`):
   ```bash
   code --install-extension ms-toolsai.jupyter
   ```

### Option A: Install Pre-Built `.vsix` from the Repo (Fastest)

From the root of the `gke-workspaces` repository:

```bash
code --install-extension vscode-extension/jupyter-workspace-sync.vsix --force
```

Or install via the VS Code UI:
1. Open the **Extensions** view (`Ctrl+Shift+X` / `Cmd+Shift+X`).
2. Click the **`...`** menu in the top-right of the Extensions sidebar and select **Install from VSIX...**.
3. Select `vscode-extension/jupyter-workspace-sync.vsix` and reload VS Code if prompted.

### Option B: Build and Package from Source

If you modify the extension source code under `vscode-extension/`:

```bash
cd vscode-extension
npm install
npm test
npm run package
code --install-extension jupyter-workspace-sync.vsix --force
```

Verify the installation:

```bash
code --list-extensions --show-versions | grep jupyter-workspace-sync
# kubeflow.jupyter-workspace-sync@0.1.0
```

### Updating an Already-Installed Extension to a New Build

After pulling new commits or making local edits in `vscode-extension/`:

1. **Rebuild and overwrite the installed extension using `--force`** (the `--force` flag is required when the version in `package.json` has not changed):
   ```bash
   cd vscode-extension
   npm run package
   code --install-extension jupyter-workspace-sync.vsix --force
   ```
   *(Or in the VS Code UI: **Extensions (`Ctrl+Shift+X`) → `...` → Install from VSIX...** and select the updated `vscode-extension/jupyter-workspace-sync.vsix`.)*
2. **Reload VS Code** so the running Extension Host loads the new bundle:
   - Open the Command Palette (`Ctrl+Shift+P` / `Cmd+Shift+P`) and run **`Developer: Reload Window`** (or **`Developer: Restart Extension Host`**).

---

## Quick Start Guide

### 1. Generate a Remote Workspace URL

For a **Kubeflow / GKE Workspace**, generate a presigned connection URL (or copy your JupyterLab URL with `?token=...`):

```
https://connect.<INGRESS_IP>.sslip.io/workspace/connect/<NAMESPACE>/<WORKSPACE_NAME>/jupyterlab/?token=<BEARER_JWT>
```

Standard JupyterLab / JupyterHub URLs also work out of the box:
- `http://localhost:8888/lab?token=abcdef...`
- `https://jupyter.example.com/user/alice/lab/tree/notebook.ipynb?token=...`

### 2. Connect Your Local Workspace in VS Code

Open your local repository folder in VS Code (`code /path/to/your/repo`), then connect using either method:

#### Method A — Via the Notebook Kernel Picker (Recommended)
1. Open any `.ipynb` file in VS Code.
2. Click **Select Kernel** in the top-right corner of the notebook editor.
3. Click **Select Another Kernel...** $\rightarrow$ **Existing Jupyter Server...** $\rightarrow$ **Jupyter Workspace Sync (Remote)...**.
4. Paste your full JupyterLab URL (with `?token=...`) and press **Enter**.
5. Pick your remote kernel (e.g. `Python 3 (ipykernel)`).

#### Method B — Via the Command Palette
1. Open the Command Palette (`Ctrl+Shift+P` / `Cmd+Shift+P`).
2. Run **`Jupyter Sync: Connect to Remote Workspace`**.
3. Paste your full JupyterLab URL and press **Enter**.
4. Select **Select Kernel** in your notebook $\rightarrow$ **Existing Jupyter Server...** $\rightarrow$ **Jupyter Workspace Sync (Remote)...** and pick the newly connected server.

### 3. Edit Locally, Execute Remotely on TPU / GPU

Once connected:
- Look at the bottom status bar: `$(cloud-upload) Syncing (N)...` $\rightarrow$ `$(check) Jupyter Synced (<repo-name>)`.
- Your local repository is mirrored to `/<remote-jupyter-root>/<repo-name>/`.
- Run any cell in your notebook:
  ```python
  import os
  import jax
  print("Working directory:", os.getcwd())
  print("TPU devices:", jax.devices())
  ```
- Import sibling or parent modules directly from nested notebooks (e.g. `experiments/tpu_run/train.ipynb`):
  ```python
  import local_helper              # Resolves experiments/tpu_run/local_helper.py
  from shared.model import Transformer  # Resolves <repo-root>/shared/model.py
  ```
- Edit `local_helper.py` or `shared/model.py` in VS Code and press **Save (`Ctrl+S`)**. Wait ~300 ms for the status bar to show `$(check) Jupyter Synced`, then re-run your cell — `%autoreload 2` automatically reloads your modified functions without restarting the kernel!

---

## Renewing Expired Tokens Without Losing Kernel State

GKE Workspace connection tokens (`desktopToken`) expire periodically. If a token expires while your kernel is running:
1. VS Code will display a notification (**`Jupyter Workspace Sync: Authentication failed (401/403)...`**) with an **Update Token** button, or you can click the status bar / run **`Jupyter Sync: Update Connection Token`** from the Command Palette.
2. Paste either a **new full connection URL** or a **raw token string**.
3. The extension updates its HTTP & WebSocket credentials in-place and immediately syncs any pending file edits — **your running remote kernel and in-memory TPU/GPU state remain untouched**.

---

## Using `~/.local/bin/jupyter-sync` with `jetski-cli`

The extension includes a companion CLI at `~/.local/bin/jupyter-sync` designed specifically for terminal-based AI coding agents like **`jetski-cli`**.

### Step 1: Ensure `~/.local/bin/jupyter-sync` Is Installed

- **If you installed the VS Code extension**: The extension automatically installs `~/.local/bin/jupyter-sync` on startup (or you can run **`Jupyter Sync: Install 'jupyter-sync' CLI to PATH`** from the Command Palette).
- **If you are using `jetski-cli` without VS Code**: Build and link the CLI directly from this repo:
  ```bash
  cd vscode-extension && npm install && npm run compile
  mkdir -p ~/.local/bin
  ln -sf "$(pwd)/dist/cli.js" ~/.local/bin/jupyter-sync
  chmod +x dist/cli.js ~/.local/bin/jupyter-sync
  ```

Verify it works:
```bash
~/.local/bin/jupyter-sync --help
```

---

### Step 2: Choose How `jetski-cli` Connects to the Remote Workspace

`~/.local/bin/jupyter-sync` supports **two connection modes** depending on whether VS Code is open alongside `jetski-cli`:

#### Mode A — Paired with VS Code (Zero-Config IPC Bridge)
1. Open your repository in **VS Code** and connect to your remote Jupyter / GKE TPU workspace (`Jupyter Sync: Connect & Sync Workspace...`).
2. Open a terminal in the **same repository directory** (or any subdirectory) and start `jetski-cli`.
3. `jupyter-sync` automatically discovers the live VS Code IPC socket for your workspace (`~/.jupyter-sync/bridge-<hash>.sock`) — **you do not need to pass `--url` or set `JUPYTER_URL`**.
4. When `jetski-cli` runs `jupyter-sync exec` or `jupyter-sync run-cell`:
   - Dirty local files are flushed to the remote pod first (**Pre-Execution Sync Barrier**).
   - Code executes on the **same live kernel** attached to your open notebook in VS Code (preserving loaded model weights and TPU state).
   - Updated cell outputs and tracebacks are automatically saved back to the local `.ipynb` file on disk so `jetski-cli` can read them.

#### Mode B — Standalone / Headless `jetski-cli` (No VS Code Required)
If VS Code is **not** running, export `JUPYTER_URL` in your shell before starting `jetski-cli` (or pass `--url "<url>"` to individual commands):

```bash
export JUPYTER_URL="https://connect.<INGRESS_IP>.sslip.io/workspace/connect/<NAMESPACE>/<WORKSPACE_NAME>/jupyterlab/?token=<BEARER_JWT>"
jetski-cli
```

In headless mode, `~/.local/bin/jupyter-sync` talks directly to the remote Jupyter Server:
- Automatically syncs your local Git repository to `/<jupyter-root>/<repo-name>/` before every `exec` or `run-cell` call.
- Reuses an existing idle kernel (or starts a temporary one) and initializes `os.chdir()`, `sys.path`, and `%autoreload 2`.
- Writes updated cell outputs back to the local `.ipynb` file after `run-cell`.

---

### Step 3: CLI Command Reference

```bash
# 1. Check connection mode ('ipc-bridge' vs 'headless'), server URL, and active kernels
~/.local/bin/jupyter-sync status

# 2. Flush pending local file edits to the remote workspace immediately
~/.local/bin/jupyter-sync sync

# 3. Execute Python code on the remote TPU/GPU kernel (auto-syncs local files first!)
~/.local/bin/jupyter-sync exec "import jax; print(jax.devices())"

# 4. Execute Python code in the working directory & kernel context of a specific notebook
~/.local/bin/jupyter-sync exec "import local_helper; print(local_helper.run())" \
  --notebook experiments/exp1/train.ipynb

# 5. Execute a specific 0-based cell of a local .ipynb on the remote kernel and save outputs back to disk
~/.local/bin/jupyter-sync run-cell experiments/exp1/train.ipynb 0

# 6. Read live in-memory (or saved on-disk) cell outputs & tracebacks from a notebook
~/.local/bin/jupyter-sync outputs experiments/exp1/train.ipynb
~/.local/bin/jupyter-sync outputs experiments/exp1/train.ipynb --cell 0

# 7. Run a shell command on the remote pod (via Jupyter Terminal API — works even while kernel is busy!)
~/.local/bin/jupyter-sync sh "pip install einops && ls -la"
~/.local/bin/jupyter-sync sh "pytest tests/" --cwd src/subpackage
```

---

### Step 4: How to Prompt or Configure `jetski-cli`

#### Option 1 — Ask `jetski-cli` Directly in Your Prompt
> *"Use `~/.local/bin/jupyter-sync` to run and debug `experiments/train.ipynb` on my remote TPU workspace. Whenever you edit `.py` files or notebook cells, verify them on the remote TPU using `~/.local/bin/jupyter-sync run-cell` or `~/.local/bin/jupyter-sync exec`."*

#### Option 2 — Add a Workspace Rule (`.jetski/rules/jupyter-sync.md`)
Create `.jetski/rules/jupyter-sync.md` in your repository so `jetski-cli` automatically knows how to execute code on the remote TPU/GPU pod in every session:

```markdown
# Remote Jupyter / TPU Execution (`jupyter-sync`)

When running Python scripts, debugging `.ipynb` notebooks, or installing packages on the remote GPU/TPU workspace, use `~/.local/bin/jupyter-sync`:

- Check connection status: `~/.local/bin/jupyter-sync status`
- Sync local file edits to remote pod: `~/.local/bin/jupyter-sync sync`
- Run a Python snippet on the remote kernel: `~/.local/bin/jupyter-sync exec "<python_code>" [--notebook <path/to/notebook.ipynb>]`
- Run a specific 0-based notebook cell on the remote kernel (and persist outputs to disk): `~/.local/bin/jupyter-sync run-cell <path/to/notebook.ipynb> <cell_index>`
- Inspect cell outputs/tracebacks: `~/.local/bin/jupyter-sync outputs <path/to/notebook.ipynb> [--cell <cell_index>]`
- Run a remote shell command (e.g. `pip install`, `nvidia-smi`): `~/.local/bin/jupyter-sync sh "<command>" [--cwd <rel_dir>]`
```

---

## Commands & Settings

### VS Code Commands

| Command | Description |
| :--- | :--- |
| `Jupyter Sync: Connect & Sync Workspace...` | Connect to a remote JupyterLab / GKE Workspace URL and run initial sync |
| `Jupyter Sync: Sync Repository Now` | Force an incremental 3-way diff sync of the local workspace to the remote server |
| `Jupyter Sync: Clean & Full Re-sync Remote Repository` | Wipe the remote synced folder and re-upload all tracked workspace files |
| `Jupyter Sync: Cancel Active Sync` | Abort an in-progress bulk or incremental sync |
| `Jupyter Sync: Update Connection Token...` | Hot-swap the Bearer token / URL in-place without disconnecting active kernels |
| `Jupyter Sync: Disconnect Remote Server` | Disconnect from the remote server and stop file watchers |
| `Jupyter Sync: Show Sync Output Logs` | Open the `Jupyter Workspace Sync` output channel for detailed diagnostics |
| `Jupyter Sync: Install 'jupyter-sync' CLI to PATH` | Re-install the `~/.local/bin/jupyter-sync` CLI wrapper |

### Extension Settings (`settings.json`)

| Setting | Default | Description |
| :--- | :--- | :--- |
| `jupyterSync.autoSyncOnSave` | `true` | Automatically sync modified, created, renamed, and deleted files to the remote server |
| `jupyterSync.enableAutoreload` | `true` | Automatically configure `%load_ext autoreload` and `%autoreload 2` on remote Python kernels |
| `jupyterSync.setKernelWorkingDirectory` | `true` | Automatically set the remote kernel's `os.chdir()` and `sys.path` to match the open notebook's directory and repository root |
| `jupyterSync.enableAgentBridge` | `true` | Enable the local Unix socket IPC bridge (`~/.jupyter-sync/`) for `jupyter-sync` and `jetski-cli` |
| `jupyterSync.autoSaveOutputs` | `true` | Automatically persist updated cell outputs to the local `.ipynb` file after remote cell execution |
| `jupyterSync.overwriteNotebooks` | `false` | When `false`, existing remote `.ipynb` files are skipped during background sync so remote outputs are preserved |
| `jupyterSync.remoteBaseDir` | `"${workspaceFolderBasename}"` | Target subdirectory under the Jupyter Server root (set to `""` to sync directly into the server root) |
| `jupyterSync.maxFileSizeMB` | `10` | Skip syncing individual files larger than this size in MB |
| `jupyterSync.exclude` | `[".git/**", "**/__pycache__/**", "**/.ipynb_checkpoints/**", "**/node_modules/**", "**/.venv/**", "**/dist/**", "**/out/**", "**/*.vsix", "**/*.mp4", "**/*.mov"]` | Glob patterns excluded from file synchronization |

---

## Testing

- **Unit tests** (URL parser, 3-way sync diff, `.tar.gz` packer, glob matcher, binary/JSON ZMQ WebSocket codec):
  ```bash
  cd vscode-extension
  npm test
  ```
- **Live remote integration tests** (against a real GKE TPU / JupyterLab workspace):
  ```bash
  cd vscode-extension
  JUPYTER_TEST_URL="https://connect.../jupyterlab/?token=..." npm run test:live
  ```

