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

import * as crypto from 'crypto';
import * as fs from 'fs';
import * as net from 'net';
import * as os from 'os';
import * as path from 'path';
import { JupyterClient, KernelExecutionResult, normalizeApiPath } from './core/jupyterClient';
import { SyncEngine, SyncSummary } from './core/syncEngine';
import { KernelInitializer } from './kernelInitializer';

export interface CellOutputSummary {
  cellIndex: number;
  cellType: 'code' | 'markdown' | 'raw' | string;
  source: string;
  executionCount: number | null;
  stdout: string;
  stderr: string;
  results: string[];
  error?: {
    ename: string;
    evalue: string;
    traceback: string[];
  };
}

export interface NotebookHostAdapter {
  /**
   * Returns live in-memory cell outputs if the notebook is open in VS Code,
   * or null to fall back to reading from disk.
   */
  getLiveNotebookOutputs?: (
    absNotebookPath: string,
    cellIndex?: number
  ) => Promise<CellOutputSummary[] | null>;

  /**
   * Executes a cell through VS Code's NotebookDocument if attached to a kernel in UI,
   * or returns null to execute directly on the remote kernel via JupyterClient.
   */
  executeCellInEditor?: (
    absNotebookPath: string,
    cellIndex: number
  ) => Promise<KernelExecutionResult | null>;

  onAgentActivityStart?: (description: string) => void;
  onAgentActivityEnd?: () => void;
  onLog?: (msg: string) => void;
}

export interface BridgeConfig {
  workspaceRoot: string;
  getClient: () => JupyterClient | null;
  getSyncEngine: () => SyncEngine | null;
  getKernelInitializer: () => KernelInitializer;
  getRemoteBaseDir: () => string;
  getSetKernelWorkingDirectory: () => boolean;
  getEnableAutoreload: () => boolean;
  getAutoSaveOutputs: () => boolean;
  getKernelForNotebook?: (absNotebookPath: string) => string | undefined;
  triggerSyncNow: (options?: {
    force?: boolean;
    overwriteNotebooks?: boolean;
  }) => Promise<SyncSummary>;
  connectFromUrl?: (url: string, remoteDir?: string) => Promise<SyncSummary | null | void>;
  hostAdapter?: NotebookHostAdapter;
}

export interface BridgeRequest {
  id: string;
  action:
    | 'ping'
    | 'status'
    | 'connect'
    | 'sync'
    | 'exec'
    | 'run-cell'
    | 'outputs'
    | 'sh'
    | 'interrupt'
    | 'restart-kernel';
  params?: Record<string, any>;
}

export interface BridgeStreamMessage {
  id: string;
  type: 'stream';
  stream: 'stdout' | 'stderr';
  text: string;
}

export interface BridgeResponseMessage {
  id: string;
  type: 'response';
  ok: boolean;
  result?: any;
  error?: string;
}

export function getBridgeDir(): string {
  return path.join(os.homedir(), '.jupyter-sync');
}

export function computeWorkspaceHash(workspaceRoot: string): string {
  const norm = path.resolve(workspaceRoot);
  return crypto.createHash('sha256').update(norm).digest('hex').slice(0, 12);
}

export function getSocketPathForWorkspace(workspaceRoot: string): string {
  const hash = computeWorkspaceHash(workspaceRoot);
  return path.join(getBridgeDir(), `bridge-${hash}.sock`);
}

export function getSessionsFilePath(): string {
  return path.join(getBridgeDir(), 'sessions.json');
}

export interface SavedCliSessionEntry {
  url: string;
  remoteDir?: string;
  serverLabel?: string;
  baseUrl?: string;
  updatedAt: string;
}

export interface SavedCliSessionsFile {
  workspaces: Record<string, SavedCliSessionEntry>;
  lastUsed?: SavedCliSessionEntry;
}

export function getCliSessionsFilePath(): string {
  return path.join(getBridgeDir(), 'cli-sessions.json');
}

export function loadCliSession(workspaceRoot: string): SavedCliSessionEntry | undefined {
  try {
    const file = getCliSessionsFilePath();
    if (!fs.existsSync(file)) {
      return undefined;
    }
    const parsed = JSON.parse(fs.readFileSync(file, 'utf-8')) as SavedCliSessionsFile;
    const homeDir = path.resolve(os.homedir());
    const resolvedStart = path.resolve(workspaceRoot);
    let curr = resolvedStart;
    while (true) {
      if (curr === homeDir && resolvedStart !== homeDir) {
        break;
      }
      if (parsed.workspaces && parsed.workspaces[curr]) {
        return parsed.workspaces[curr];
      }
      const parent = path.dirname(curr);
      if (parent === curr) {
        break;
      }
      curr = parent;
    }
    return undefined;
  } catch {
    return undefined;
  }
}

export function saveCliSession(workspaceRoot: string, entry: SavedCliSessionEntry): void {
  try {
    const file = getCliSessionsFilePath();
    fs.mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 });
    let data: SavedCliSessionsFile = { workspaces: {} };
    if (fs.existsSync(file)) {
      try {
        data = JSON.parse(fs.readFileSync(file, 'utf-8')) as SavedCliSessionsFile;
        if (!data.workspaces) {
          data.workspaces = {};
        }
      } catch {
        data = { workspaces: {} };
      }
    }
    const normRoot = path.resolve(workspaceRoot);
    data.workspaces[normRoot] = entry;
    delete data.lastUsed;
    fs.writeFileSync(file, JSON.stringify(data, null, 2), { mode: 0o600 });
  } catch {
    // ignore write errors
  }
}

export function removeCliSession(workspaceRoot: string): void {
  try {
    const file = getCliSessionsFilePath();
    if (!fs.existsSync(file)) {
      return;
    }
    const data = JSON.parse(fs.readFileSync(file, 'utf-8')) as SavedCliSessionsFile;
    const normRoot = path.resolve(workspaceRoot);
    if (data.workspaces && data.workspaces[normRoot]) {
      delete data.workspaces[normRoot];
      delete data.lastUsed;
      fs.writeFileSync(file, JSON.stringify(data, null, 2), { mode: 0o600 });
    }
  } catch {
    // ignore
  }
}

interface SessionRegistryEntry {
  workspaceRoot: string;
  socketPath: string;
  pid: number;
  updatedAt: string;
}

function isProcessAlive(pid: number): boolean {
  if (!pid || pid <= 0) {
    return false;
  }
  try {
    process.kill(pid, 0);
    return true;
  } catch (err: any) {
    return err?.code === 'EPERM';
  }
}

/**
 * Discovers the active Unix domain socket for `startDir` (supporting nested subdirectories).
 */
export function discoverBridgeSocket(startDir: string): {
  socketPath: string;
  workspaceRoot: string;
} | null {
  const homeDir = path.resolve(os.homedir());
  const resolvedStart = path.resolve(startDir);
  let curr = resolvedStart;
  while (true) {
    if (curr === homeDir && resolvedStart !== homeDir) {
      break;
    }
    const candidate = getSocketPathForWorkspace(curr);
    if (fs.existsSync(candidate)) {
      return { socketPath: candidate, workspaceRoot: curr };
    }
    const parent = path.dirname(curr);
    if (parent === curr) {
      break;
    }
    curr = parent;
  }

  // Also check sessions.json for longest prefix match
  const sessionsFile = getSessionsFilePath();
  if (fs.existsSync(sessionsFile)) {
    try {
      const entries = JSON.parse(fs.readFileSync(sessionsFile, 'utf-8')) as Record<
        string,
        SessionRegistryEntry
      >;
      let bestMatch: SessionRegistryEntry | null = null;
      let dirty = false;
      for (const [key, entry] of Object.entries(entries)) {
        const root = path.resolve(entry.workspaceRoot);
        if (!fs.existsSync(entry.socketPath) || !isProcessAlive(entry.pid)) {
          if (fs.existsSync(entry.socketPath)) {
            try {
              fs.unlinkSync(entry.socketPath);
            } catch {
              // ignore
            }
          }
          delete entries[key];
          dirty = true;
          continue;
        }
        if (root === homeDir && resolvedStart !== homeDir) {
          continue;
        }
        if (resolvedStart === root || resolvedStart.startsWith(root + path.sep)) {
          if (!bestMatch || root.length > bestMatch.workspaceRoot.length) {
            bestMatch = entry;
          }
        }
      }
      if (dirty) {
        try {
          fs.writeFileSync(sessionsFile, JSON.stringify(entries, null, 2), { mode: 0o600 });
        } catch {
          // ignore
        }
      }
      if (bestMatch) {
        return {
          socketPath: bestMatch.socketPath,
          workspaceRoot: bestMatch.workspaceRoot,
        };
      }
    } catch {
      // ignore
    }
  }

  return null;
}

/**
 * Parses cell outputs from a standard `.ipynb` JSON structure on disk.
 */
export function parseNotebookOutputsFromDisk(
  absNotebookPath: string,
  cellIndex?: number
): CellOutputSummary[] {
  const raw = fs.readFileSync(absNotebookPath, 'utf-8');
  const nb = JSON.parse(raw);
  const cells: any[] = Array.isArray(nb.cells) ? nb.cells : [];
  const summaries: CellOutputSummary[] = [];

  for (let i = 0; i < cells.length; i++) {
    if (cellIndex !== undefined && i !== cellIndex) {
      continue;
    }
    const cell = cells[i];
    const source = Array.isArray(cell.source)
      ? cell.source.join('')
      : String(cell.source || '');
    const outputs: any[] = Array.isArray(cell.outputs) ? cell.outputs : [];
    let stdout = '';
    let stderr = '';
    const results: string[] = [];
    let error: { ename: string; evalue: string; traceback: string[] } | undefined;

    for (const out of outputs) {
      if (out.output_type === 'stream') {
        const text = Array.isArray(out.text) ? out.text.join('') : String(out.text || '');
        if (out.name === 'stderr') {
          stderr += text;
        } else {
          stdout += text;
        }
      } else if (out.output_type === 'execute_result' || out.output_type === 'display_data') {
        const plain = out.data?.['text/plain'];
        if (plain !== undefined) {
          const text = Array.isArray(plain) ? plain.join('') : String(plain);
          results.push(text);
        }
      } else if (out.output_type === 'error') {
        error = {
          ename: String(out.ename || 'Error'),
          evalue: String(out.evalue || ''),
          traceback: Array.isArray(out.traceback) ? out.traceback.map(String) : [],
        };
      }
    }

    summaries.push({
      cellIndex: i,
      cellType: String(cell.cell_type || 'code'),
      source,
      executionCount:
        typeof cell.execution_count === 'number' ? cell.execution_count : null,
      stdout,
      stderr,
      results,
      error,
    });
  }

  return summaries;
}

/**
 * Updates a cell's outputs in a local `.ipynb` file on disk after remote execution.
 */
export function writeCellExecutionToDiskNotebook(
  absNotebookPath: string,
  cellIndex: number,
  execRes: KernelExecutionResult
): void {
  const raw = fs.readFileSync(absNotebookPath, 'utf-8');
  const nb = JSON.parse(raw);
  if (!Array.isArray(nb.cells) || cellIndex < 0 || cellIndex >= nb.cells.length) {
    return;
  }

  const cell = nb.cells[cellIndex];
  const outputs: any[] = [];
  if (execRes.stdout) {
    outputs.push({
      output_type: 'stream',
      name: 'stdout',
      text: execRes.stdout.split(/(?<=\n)/),
    });
  }
  if (execRes.stderr) {
    outputs.push({
      output_type: 'stream',
      name: 'stderr',
      text: execRes.stderr.split(/(?<=\n)/),
    });
  }
  for (const r of execRes.results) {
    outputs.push({
      output_type: 'execute_result',
      execution_count: execRes.executionCount ?? 1,
      metadata: {},
      data: {
        'text/plain': r.split(/(?<=\n)/),
      },
    });
  }
  if (execRes.status === 'error') {
    outputs.push({
      output_type: 'error',
      ename: execRes.ename || 'Error',
      evalue: execRes.evalue || '',
      traceback: execRes.traceback || [],
    });
  }

  cell.outputs = outputs;
  if (execRes.executionCount !== undefined) {
    cell.execution_count = execRes.executionCount;
  }
  fs.writeFileSync(absNotebookPath, JSON.stringify(nb, null, 1) + '\n', 'utf-8');
}

export class AgentBridgeServer {
  private server: net.Server | null = null;
  private readonly socketPath: string;
  private dedicatedKernelId: string | null = null;

  constructor(private readonly config: BridgeConfig) {
    this.socketPath = getSocketPathForWorkspace(config.workspaceRoot);
  }

  get address(): string {
    return this.socketPath;
  }

  async start(): Promise<string> {
    const bridgeDir = getBridgeDir();
    fs.mkdirSync(bridgeDir, { recursive: true, mode: 0o700 });
    try {
      fs.chmodSync(bridgeDir, 0o700);
    } catch {
      // ignore
    }

    if (fs.existsSync(this.socketPath)) {
      try {
        fs.unlinkSync(this.socketPath);
      } catch {
        // ignore
      }
    }

    this.server = net.createServer((socket) => {
      this.handleConnection(socket);
    });

    await new Promise<void>((resolve, reject) => {
      this.server!.once('error', reject);
      this.server!.listen(this.socketPath, () => {
        try {
          fs.chmodSync(this.socketPath, 0o600);
        } catch {
          // ignore
        }
        resolve();
      });
    });

    this.registerSessionEntry();
    return this.socketPath;
  }

  private registerSessionEntry(): void {
    const sessionsFile = getSessionsFilePath();
    let entries: Record<string, SessionRegistryEntry> = {};
    if (fs.existsSync(sessionsFile)) {
      try {
        entries = JSON.parse(fs.readFileSync(sessionsFile, 'utf-8'));
      } catch {
        entries = {};
      }
    }
    const normRoot = path.resolve(this.config.workspaceRoot);
    entries[normRoot] = {
      workspaceRoot: normRoot,
      socketPath: this.socketPath,
      pid: process.pid,
      updatedAt: new Date().toISOString(),
    };
    fs.writeFileSync(sessionsFile, JSON.stringify(entries, null, 2), { mode: 0o600 });
  }

  private handleConnection(socket: net.Socket): void {
    const connAbort = new AbortController();
    socket.on('close', () => {
      connAbort.abort();
    });

    let buffer = '';
    socket.on('data', (chunk) => {
      buffer += chunk.toString('utf-8');
      let newlineIdx: number;
      while ((newlineIdx = buffer.indexOf('\n')) >= 0) {
        const line = buffer.slice(0, newlineIdx).trim();
        buffer = buffer.slice(newlineIdx + 1);
        if (!line) {
          continue;
        }
        let req: BridgeRequest;
        try {
          req = JSON.parse(line) as BridgeRequest;
        } catch {
          continue;
        }
        this.dispatchRequest(socket, req, connAbort.signal).catch((err) => {
          this.sendResponse(socket, {
            id: req.id,
            type: 'response',
            ok: false,
            error: (err as Error).message,
          });
        });
      }
    });
  }

  private sendStream(
    socket: net.Socket,
    id: string,
    stream: 'stdout' | 'stderr',
    text: string
  ): void {
    if (socket.destroyed) {
      return;
    }
    const msg: BridgeStreamMessage = { id, type: 'stream', stream, text };
    socket.write(JSON.stringify(msg) + '\n');
  }

  private sendResponse(socket: net.Socket, msg: BridgeResponseMessage): void {
    if (socket.destroyed) {
      return;
    }
    socket.write(JSON.stringify(msg) + '\n');
  }

  private async syncNotebookFileToRemote(
    client: JupyterClient,
    absNbPath: string
  ): Promise<void> {
    try {
      const normRoot = path.resolve(this.config.workspaceRoot);
      const normNb = path.resolve(absNbPath);
      if (normNb !== normRoot && !normNb.startsWith(normRoot + path.sep)) {
        return;
      }
      const relNb = path.relative(normRoot, normNb).split(path.sep).join('/');
      const cleanBase = normalizeApiPath(this.config.getRemoteBaseDir());
      const remoteNbPath = cleanBase ? `${cleanBase}/${relNb}` : relNb;
      const content = fs.readFileSync(normNb);
      await client.putFile(remoteNbPath, content, { overwriteNotebooks: true });
    } catch {
      // ignore non-fatal remote notebook sync error
    }
  }

  private async dispatchRequest(
    socket: net.Socket,
    req: BridgeRequest,
    connSignal?: AbortSignal
  ): Promise<void> {
    const params = req.params || {};

    if (req.action === 'ping') {
      this.sendResponse(socket, {
        id: req.id,
        type: 'response',
        ok: true,
        result: { pong: true, workspaceRoot: this.config.workspaceRoot },
      });
      return;
    }

    if (req.action === 'connect') {
      const url = String(params.url || '');
      const remoteDir = params.remoteDir ? String(params.remoteDir) : undefined;
      if (!url) {
        throw new Error('Missing url parameter for connect');
      }
      if (!this.config.connectFromUrl) {
        throw new Error('connectFromUrl is not supported by this bridge host');
      }
      const syncSummary = await this.config.connectFromUrl(url, remoteDir);
      const updatedClient = this.config.getClient();
      this.sendResponse(socket, {
        id: req.id,
        type: 'response',
        ok: true,
        result: {
          connected: Boolean(updatedClient),
          serverLabel: updatedClient?.label || null,
          baseUrl: updatedClient?.baseUrl || null,
          workspaceRoot: this.config.workspaceRoot,
          remoteBaseDir: this.config.getRemoteBaseDir(),
          syncSummary: syncSummary || null,
        },
      });
      return;
    }

    if (req.action === 'outputs') {
      const rawNbPath = String(params.notebookPath || '');
      const cellIndex =
        params.cellIndex !== undefined && params.cellIndex !== null
          ? Number(params.cellIndex)
          : undefined;
      const absNbPath = path.isAbsolute(rawNbPath)
        ? rawNbPath
        : path.resolve(this.config.workspaceRoot, rawNbPath);

      let outputs: CellOutputSummary[] | null = null;
      if (this.config.hostAdapter?.getLiveNotebookOutputs) {
        outputs = await this.config.hostAdapter.getLiveNotebookOutputs(absNbPath, cellIndex);
      }
      if (!outputs) {
        if (!fs.existsSync(absNbPath)) {
          throw new Error(`Notebook file not found: ${absNbPath}`);
        }
        outputs = parseNotebookOutputsFromDisk(absNbPath, cellIndex);
      }

      this.sendResponse(socket, {
        id: req.id,
        type: 'response',
        ok: true,
        result: outputs,
      });
      return;
    }

    let client = this.config.getClient();
    let syncEngine = this.config.getSyncEngine();

    if (req.action === 'status') {
      this.sendResponse(socket, {
        id: req.id,
        type: 'response',
        ok: true,
        result: {
          connected: Boolean(client),
          serverLabel: client?.label || null,
          baseUrl: client?.baseUrl || null,
          workspaceRoot: this.config.workspaceRoot,
          remoteBaseDir: this.config.getRemoteBaseDir(),
        },
      });
      return;
    }

    // When an agent explicitly initiates a sync or remote execution action via the bridge,
    // activate the connection from the workspace's saved CLI session if not yet active.
    if ((!client || !syncEngine) && this.config.connectFromUrl) {
      const fallbackUrl =
        (params.url ? String(params.url) : undefined) ||
        loadCliSession(this.config.workspaceRoot)?.url;
      if (fallbackUrl) {
        await this.config.connectFromUrl(fallbackUrl);
        client = this.config.getClient();
        syncEngine = this.config.getSyncEngine();
      }
    }

    if (!client || !syncEngine) {
      throw new Error(
        'No active remote Jupyter Server connection in VS Code. Run "Jupyter Sync: Connect & Sync Workspace..." first.'
      );
    }

    if (req.action === 'interrupt') {
      const notebookRelOrAbs = params.notebookPath ? String(params.notebookPath) : undefined;
      const targetPath = notebookRelOrAbs
        ? path.resolve(this.config.workspaceRoot, notebookRelOrAbs)
        : undefined;
      const kernelId = await this.resolveOrCreateKernel(client, targetPath);
      await client.interruptKernel(kernelId);
      this.sendResponse(socket, {
        id: req.id,
        type: 'response',
        ok: true,
        result: { kernelId, interrupted: true },
      });
      return;
    }

    if (req.action === 'restart-kernel') {
      const notebookRelOrAbs = params.notebookPath ? String(params.notebookPath) : undefined;
      const targetPath = notebookRelOrAbs
        ? path.resolve(this.config.workspaceRoot, notebookRelOrAbs)
        : undefined;
      const kernelId = await this.resolveOrCreateKernel(client, targetPath);
      await client.restartKernel(kernelId);
      this.config.getKernelInitializer().invalidate(kernelId);
      this.sendResponse(socket, {
        id: req.id,
        type: 'response',
        ok: true,
        result: { kernelId, restarted: true },
      });
      return;
    }

    // Synchronous Pre-Execution Sync Barrier for all mutating/execution commands
    await syncEngine.flushAndWait();

    if (req.action === 'sync') {
      const summary = await this.config.triggerSyncNow({
        force: Boolean(params.force),
        overwriteNotebooks: Boolean(params.overwriteNotebooks || params.force),
      });
      this.sendResponse(socket, {
        id: req.id,
        type: 'response',
        ok: true,
        result: summary,
      });
      return;
    }

    if (req.action === 'exec') {
      const code = String(params.code || '');
      const notebookRelOrAbs = params.notebookPath ? String(params.notebookPath) : undefined;
      const cwdRel = params.cwd ? String(params.cwd) : undefined;
      const targetPath = notebookRelOrAbs
        ? path.resolve(this.config.workspaceRoot, notebookRelOrAbs)
        : cwdRel
        ? path.resolve(this.config.workspaceRoot, cwdRel, '_placeholder.ipynb')
        : undefined;

      this.config.hostAdapter?.onAgentActivityStart?.('Agent executing Python on kernel...');
      try {
        const kernelId = await this.resolveOrCreateKernel(client, targetPath);
        await this.config
          .getKernelInitializer()
          .initializeKernelDirect(
            client,
            kernelId,
            this.config.workspaceRoot,
            this.config.getRemoteBaseDir(),
            targetPath,
            {
              setKernelWorkingDirectory: this.config.getSetKernelWorkingDirectory(),
              enableAutoreload: this.config.getEnableAutoreload(),
            }
          );

        const res = await client.executeCode(kernelId, code, {
          silent: false,
          storeHistory: true,
          timeoutMs: params.timeoutMs ?? 120000,
          signal: connSignal,
          onStream: (stream, text) => {
            this.sendStream(socket, req.id, stream, text);
          },
        });

        this.sendResponse(socket, {
          id: req.id,
          type: 'response',
          ok: true,
          result: res,
        });
      } finally {
        this.config.hostAdapter?.onAgentActivityEnd?.();
      }
      return;
    }

    if (req.action === 'run-cell') {
      const rawNbPath = String(params.notebookPath || '');
      const cellIndex = Number(params.cellIndex ?? 0);
      const absNbPath = path.isAbsolute(rawNbPath)
        ? rawNbPath
        : path.resolve(this.config.workspaceRoot, rawNbPath);

      if (!fs.existsSync(absNbPath)) {
        throw new Error(`Notebook file not found: ${absNbPath}`);
      }

      this.config.hostAdapter?.onAgentActivityStart?.(
        `Agent running cell ${cellIndex} in ${path.basename(absNbPath)}...`
      );
      try {
        // Push any local notebook_edit cell additions/updates to the remote .ipynb before execution
        await this.syncNotebookFileToRemote(client, absNbPath);

        let execRes: KernelExecutionResult | null = null;
        if (this.config.hostAdapter?.executeCellInEditor) {
          execRes = await this.config.hostAdapter.executeCellInEditor(absNbPath, cellIndex);
        }

        if (!execRes) {
          const cells = parseNotebookOutputsFromDisk(absNbPath, cellIndex);
          if (cells.length === 0) {
            throw new Error(`Cell index ${cellIndex} out of range in ${absNbPath}`);
          }
          const cellSource = cells[0].source;
          const kernelId = await this.resolveOrCreateKernel(client, absNbPath);
          await this.config
            .getKernelInitializer()
            .initializeKernelDirect(
              client,
              kernelId,
              this.config.workspaceRoot,
              this.config.getRemoteBaseDir(),
              absNbPath,
              {
                setKernelWorkingDirectory: this.config.getSetKernelWorkingDirectory(),
                enableAutoreload: this.config.getEnableAutoreload(),
              }
            );

          execRes = await client.executeCode(kernelId, cellSource, {
            silent: false,
            storeHistory: true,
            timeoutMs: params.timeoutMs ?? 120000,
            signal: connSignal,
            onStream: (stream, text) => {
              this.sendStream(socket, req.id, stream, text);
            },
          });

          if (this.config.getAutoSaveOutputs()) {
            writeCellExecutionToDiskNotebook(absNbPath, cellIndex, execRes);
          }
        }

        // Push updated .ipynb (with cell outputs) to the remote server so JupyterLab browser UI is in sync
        await this.syncNotebookFileToRemote(client, absNbPath);

        this.sendResponse(socket, {
          id: req.id,
          type: 'response',
          ok: true,
          result: execRes,
        });
      } finally {
        this.config.hostAdapter?.onAgentActivityEnd?.();
      }
      return;
    }

    if (req.action === 'sh') {
      const command = String(params.command || '');
      const relCwd = params.cwd ? normalizeApiPath(String(params.cwd)) : '';
      const baseDir = normalizeApiPath(this.config.getRemoteBaseDir());
      const fullRemoteCwd =
        baseDir && relCwd ? `${baseDir}/${relCwd}` : baseDir || relCwd;

      this.config.hostAdapter?.onAgentActivityStart?.(`Agent running shell: ${command.slice(0, 30)}...`);
      try {
        const res = await client.executeShellCommand(command, {
          cwd: fullRemoteCwd,
          timeoutMs: params.timeoutMs ?? 120000,
          onOutput: (chunk) => {
            this.sendStream(socket, req.id, 'stdout', chunk);
          },
        });
        this.sendResponse(socket, {
          id: req.id,
          type: 'response',
          ok: true,
          result: res,
        });
      } finally {
        this.config.hostAdapter?.onAgentActivityEnd?.();
      }
      return;
    }

    throw new Error(`Unknown bridge action: ${(req as any).action}`);
  }

  private async resolveOrCreateKernel(
    client: JupyterClient,
    absNotebookPath?: string
  ): Promise<string> {
    if (absNotebookPath && this.config.getKernelForNotebook) {
      const mapped = this.config.getKernelForNotebook(absNotebookPath);
      if (mapped) {
        return mapped;
      }
    }

    const kernels = await client.listKernels();
    if (this.dedicatedKernelId && kernels.some((k) => k.id === this.dedicatedKernelId)) {
      return this.dedicatedKernelId;
    }

    const idle = kernels.find((k) => k.execution_state === 'idle') || kernels[0];
    if (idle) {
      this.dedicatedKernelId = idle.id;
      return idle.id;
    }

    const started = await client.startKernel('python3');
    this.dedicatedKernelId = started.id;
    return started.id;
  }

  async stop(): Promise<void> {
    if (this.server) {
      await new Promise<void>((resolve) => {
        this.server!.close(() => resolve());
      });
      this.server = null;
    }
    if (fs.existsSync(this.socketPath)) {
      try {
        fs.unlinkSync(this.socketPath);
      } catch {
        // ignore
      }
    }
    try {
      const sessionsFile = getSessionsFilePath();
      if (fs.existsSync(sessionsFile)) {
        const entries = JSON.parse(fs.readFileSync(sessionsFile, 'utf-8')) as Record<
          string,
          SessionRegistryEntry
        >;
        const normRoot = path.resolve(this.config.workspaceRoot);
        if (entries[normRoot]?.socketPath === this.socketPath) {
          delete entries[normRoot];
          fs.writeFileSync(sessionsFile, JSON.stringify(entries, null, 2), { mode: 0o600 });
        }
      }
    } catch {
      // ignore
    }
  }
}

/**
 * Sends a single request to an active AgentBridgeServer over the Unix domain socket.
 */
export async function callAgentBridge(
  socketPath: string,
  action: BridgeRequest['action'],
  params?: Record<string, any>,
  onStream?: (stream: 'stdout' | 'stderr', text: string) => void,
  timeoutMs = 120000
): Promise<any> {
  const reqId = crypto.randomUUID();
  return new Promise((resolve, reject) => {
    const socket = net.createConnection(socketPath);
    let buffer = '';
    let settled = false;

    const finish = (err?: Error, result?: any) => {
      if (settled) {
        return;
      }
      settled = true;
      clearTimeout(timer);
      socket.destroy();
      if (err) {
        reject(err);
      } else {
        resolve(result);
      }
    };

    const timer = setTimeout(() => {
      finish(new Error(`IPC bridge request '${action}' timed out after ${timeoutMs}ms`));
    }, timeoutMs);

    socket.on('connect', () => {
      const req: BridgeRequest = { id: reqId, action, params };
      socket.write(JSON.stringify(req) + '\n');
    });

    socket.on('data', (chunk) => {
      buffer += chunk.toString('utf-8');
      let newlineIdx: number;
      while ((newlineIdx = buffer.indexOf('\n')) >= 0) {
        const line = buffer.slice(0, newlineIdx).trim();
        buffer = buffer.slice(newlineIdx + 1);
        if (!line) {
          continue;
        }
        let msg: BridgeStreamMessage | BridgeResponseMessage;
        try {
          msg = JSON.parse(line);
        } catch {
          continue;
        }
        if (msg.id !== reqId) {
          continue;
        }
        if (msg.type === 'stream') {
          onStream?.(msg.stream, msg.text);
        } else if (msg.type === 'response') {
          if (msg.ok) {
            finish(undefined, msg.result);
          } else {
            finish(new Error(msg.error || 'IPC bridge error'));
          }
        }
      }
    });

    socket.on('error', (err) => {
      finish(err);
    });

    socket.on('close', () => {
      if (!settled) {
        finish(new Error('IPC socket closed before response was received'));
      }
    });
  });
}
