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

## Using the `jupyter-sync` CLI (Terminal & AI Coding Agents)

When the extension activates, it starts a local authenticated Unix domain socket bridge and installs the `jupyter-sync` CLI to `~/.local/bin/jupyter-sync`.

Ensure `~/.local/bin` is on your `PATH`, then run `jupyter-sync` from any terminal inside your workspace (or any subdirectory):

```bash
# 1. Check connection status, remote root, and active kernels
jupyter-sync status

# 2. Force an immediate sync and wait for completion
jupyter-sync sync

# 3. Execute Python code on the remote TPU/GPU kernel (auto-syncs dirty files first!)
jupyter-sync exec -c "import jax; print(jax.devices()); print('device count:', jax.device_count())"

# 4. Execute a local Python script on the remote kernel
jupyter-sync exec --file scripts/verify_tpu.py

# 5. Execute specific cells of a notebook on the remote kernel
jupyter-sync run-cell notebooks/train.ipynb --cell 0
jupyter-sync run-cell notebooks/train.ipynb --all

# 6. Read cell outputs (stdout, stderr, tracebacks) from a notebook
jupyter-sync outputs notebooks/train.ipynb

# 7. Run an arbitrary shell command on the remote pod (works even while kernel is busy!)
jupyter-sync sh "pip install einops && nvidia-smi || ls -la /dev/vfio"
```

Pass `--json` to any command for structured JSON output ideal for AI agents (`jetski-cli`, Claude Code, etc.).

---

## Commands & Settings

### VS Code Commands

| Command | Description |
| :--- | :--- |
| `Jupyter Sync: Connect to Remote Workspace` | Connect to a remote JupyterLab / GKE Workspace URL and run initial sync |
| `Jupyter Sync: Sync Workspace Now` | Force a full 3-way diff sync of the local workspace to the remote server |
| `Jupyter Sync: Update Connection Token` | Hot-swap the Bearer token / URL without disconnecting active kernels |
| `Jupyter Sync: Disconnect` | Disconnect from the remote server and stop file watchers |
| `Jupyter Sync: Show Output Log` | Open the `Jupyter Workspace Sync` output channel for detailed diagnostics |

### Extension Settings (`settings.json`)

| Setting | Default | Description |
| :--- | :--- | :--- |
| `jupyterSync.autoSyncOnSave` | `true` | Automatically sync modified local files to the remote workspace on save |
| `jupyterSync.enableAutoReload` | `true` | Automatically configure `%load_ext autoreload` and `%autoreload 2` on remote Python kernels |
| `jupyterSync.respectGitignore` | `true` | Use `git ls-files` (when inside a Git repository) so gitignored files are never uploaded |
| `jupyterSync.overwriteNotebooks` | `false` | When `false`, existing remote `.ipynb` files are skipped during background sync so remote outputs are preserved |
| `jupyterSync.maxFileSizeMB` | `10` | Skip syncing individual files larger than this size in MB |
| `jupyterSync.exclude` | `[".git/**", "**/.venv/**", "**/node_modules/**", "**/__pycache__/**", "**/*.pyc", ".ipynb_checkpoints/**", "**/*.vsix", "**/dist/**", "**/out/**", "**/*.mp4", "**/*.mov"]` | Glob patterns excluded from file synchronization |
| `jupyterSync.enableAgentBridge` | `true` | Enable the local Unix socket bridge (`~/.jupyter-sync/`) for the `jupyter-sync` CLI |

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
