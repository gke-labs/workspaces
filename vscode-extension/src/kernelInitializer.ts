// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

import * as path from 'path';
import {
  JupyterClient,
  KernelExecutionResult,
  normalizeApiPath,
  V1_KERNEL_WS_PROTOCOL,
  JupyterWireMessage,
  decodeJupyterWsMessage,
  encodeJupyterWsMessage,
} from './core/jupyterClient';
import { MANIFEST_FILENAME } from './core/syncEngine';

export {
  V1_KERNEL_WS_PROTOCOL,
  JupyterWireMessage,
  decodeJupyterWsMessage,
  encodeJupyterWsMessage,
};

export interface NotebookRemotePaths {
  relRepoRoot: string;
  relNotebookDir: string;
  relNotebookFile: string;
}

export interface KernelInitOptions {
  setKernelWorkingDirectory?: boolean;
  enableAutoreload?: boolean;
}

/**
 * Resolves the remote repository root and remote notebook directory relative to
 * the Jupyter Server's root directory.
 */
export function resolveNotebookRemotePaths(
  workspaceRoot: string,
  remoteBaseDir: string,
  notebookPath?: string
): NotebookRemotePaths {
  const cleanBase = normalizeApiPath(remoteBaseDir);
  if (!notebookPath) {
    return {
      relRepoRoot: cleanBase,
      relNotebookDir: cleanBase,
      relNotebookFile: '',
    };
  }

  const absWorkspace = path.resolve(workspaceRoot);
  const absNotebook = path.isAbsolute(notebookPath)
    ? path.resolve(notebookPath)
    : path.resolve(absWorkspace, notebookPath);

  let relFile = path.relative(absWorkspace, absNotebook).replace(/\\/g, '/');
  if (relFile.startsWith('..')) {
    relFile = path.basename(absNotebook);
  }
  relFile = normalizeApiPath(relFile);

  const relDirPart = path.posix.dirname(relFile);
  const cleanSubDir = relDirPart === '.' ? '' : normalizeApiPath(relDirPart);

  const relNotebookDir =
    cleanBase && cleanSubDir
      ? `${cleanBase}/${cleanSubDir}`
      : cleanBase || cleanSubDir;

  const relNotebookFile =
    cleanBase && relFile ? `${cleanBase}/${relFile}` : relFile;

  return {
    relRepoRoot: cleanBase,
    relNotebookDir,
    relNotebookFile,
  };
}

/**
 * Generates the idempotent Python snippet executed silently on the remote kernel
 * to configure `os.chdir`, `sys.path[0]`, `sys.path[1]`, and `%autoreload 2`.
 */
export function buildKernelInitSnippet(
  relRepoRoot: string,
  relNotebookDir: string,
  options?: KernelInitOptions
): string {
  const setCwd = options?.setKernelWorkingDirectory ?? true;
  const enableAutoreload = options?.enableAutoreload ?? true;
  const cleanRepo = normalizeApiPath(relRepoRoot);
  const cleanNbDir = normalizeApiPath(relNotebookDir);

  const lines: string[] = ['import os, sys'];

  if (setCwd) {
    lines.push(`
# Capture the Jupyter Server root directory once per kernel process
if not hasattr(sys, "_jupyter_sync_server_root"):
    _rel_repo = ${JSON.stringify(cleanRepo)}
    _rel_nb = ${JSON.stringify(cleanNbDir)}
    try:
        _cwd = os.getcwd().replace("\\\\", "/")
    except OSError:
        _cwd = os.path.expanduser("~").replace("\\\\", "/")
    _candidates = []
    if os.environ.get("JUPYTER_SERVER_ROOT"):
        _candidates.append(os.environ["JUPYTER_SERVER_ROOT"])
    for _suffix in ([_s for _s in ("/" + _rel_nb, "/" + _rel_repo) if _s != "/"]):
        if _cwd.endswith(_suffix):
            _candidates.append(_cwd[:-len(_suffix)] or "/")
    _candidates.extend([_cwd, os.path.expanduser("~")])
    sys._jupyter_sync_server_root = _candidates[0]
    for _c in _candidates:
        if _c and os.path.exists(os.path.join(_c, _rel_repo, ${JSON.stringify(MANIFEST_FILENAME)})):
            sys._jupyter_sync_server_root = os.path.abspath(_c)
            break
    else:
        for _c in _candidates:
            if _c and _rel_repo and os.path.isdir(os.path.join(_c, _rel_repo)):
                sys._jupyter_sync_server_root = os.path.abspath(_c)
                break

_sync_repo_root = os.path.abspath(
    os.path.join(sys._jupyter_sync_server_root, ${JSON.stringify(cleanRepo)})
)
_sync_notebook_dir = os.path.abspath(
    os.path.join(sys._jupyter_sync_server_root, ${JSON.stringify(cleanNbDir)})
)

# 1. Ensure target directory exists and chdir into the notebook's folder
os.makedirs(_sync_notebook_dir, exist_ok=True)
try:
    _cur_cwd = os.getcwd()
except OSError:
    _cur_cwd = None
if _cur_cwd != _sync_notebook_dir:
    os.chdir(_sync_notebook_dir)

# 2. Remove previous synced notebook dir if kernel switched notebooks, then set sys.path[0]
_prev_nb_dir = getattr(sys, "_jupyter_sync_prev_notebook_dir", None)
if _prev_nb_dir and _prev_nb_dir != _sync_notebook_dir and _prev_nb_dir in sys.path:
    sys.path.remove(_prev_nb_dir)
sys._jupyter_sync_prev_notebook_dir = _sync_notebook_dir

if _sync_notebook_dir in sys.path:
    sys.path.remove(_sync_notebook_dir)
sys.path.insert(0, _sync_notebook_dir)

# 3. Put repo root at sys.path[1] (for repo-wide imports like \`from examples.distributed.jobs import pipeline\`)
if _sync_repo_root != _sync_notebook_dir:
    if _sync_repo_root in sys.path:
        sys.path.remove(_sync_repo_root)
    sys.path.insert(1, _sync_repo_root)

del _sync_repo_root, _sync_notebook_dir, _cur_cwd, _prev_nb_dir
`);
  }

  if (enableAutoreload) {
    lines.push(`
# 4. Enable IPython %autoreload 2 so local edits to .py files reload automatically on save
try:
    _ip = get_ipython()
    if _ip is not None:
        _ip.run_line_magic("load_ext", "autoreload")
        _ip.run_line_magic("autoreload", "2")
except Exception:
    pass
`);
  }

  return lines.join('\n').trim() + '\n';
}

export class KernelInitializer {
  private readonly initializedDirsByKernel = new Map<string, string>();

  /**
   * Returns true if `kernelId` has already been initialized for `relNotebookDir`.
   */
  isInitialized(kernelId: string, relNotebookDir: string): boolean {
    return this.initializedDirsByKernel.get(kernelId) === normalizeApiPath(relNotebookDir);
  }

  /**
   * Records that `kernelId` is now initialized for `relNotebookDir`.
   */
  markInitialized(kernelId: string, relNotebookDir: string): void {
    this.initializedDirsByKernel.set(kernelId, normalizeApiPath(relNotebookDir));
  }

  /**
   * Clears initialization state for a specific kernel (e.g., on restart) or all kernels.
   */
  invalidate(kernelId?: string): void {
    if (kernelId) {
      this.initializedDirsByKernel.delete(kernelId);
    } else {
      this.initializedDirsByKernel.clear();
    }
  }

  /**
   * Executes the silent initialization snippet on a remote kernel using `JupyterClient.executeCode`.
   */
  async initializeKernelDirect(
    client: JupyterClient,
    kernelId: string,
    workspaceRoot: string,
    remoteBaseDir: string,
    notebookPath?: string,
    options?: KernelInitOptions
  ): Promise<KernelExecutionResult> {
    const paths = resolveNotebookRemotePaths(workspaceRoot, remoteBaseDir, notebookPath);
    const snippet = buildKernelInitSnippet(paths.relRepoRoot, paths.relNotebookDir, options);
    const res = await client.executeCode(kernelId, snippet, {
      silent: true,
      storeHistory: false,
      timeoutMs: 20000,
    });
    if (res.status === 'ok') {
      this.markInitialized(kernelId, paths.relNotebookDir);
    }
    return res;
  }
}
