#!/usr/bin/env node
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

import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';
import {
  callAgentBridge,
  CellOutputSummary,
  discoverBridgeSocket,
  loadCliSession,
  parseNotebookOutputsFromDisk,
  saveCliSession,
  SavedCliSessionEntry,
  writeCellExecutionToDiskNotebook,
} from './agentBridge';
import { JupyterClient, KernelExecutionResult, normalizeApiPath } from './core/jupyterClient';
import { SyncEngine } from './core/syncEngine';
import { KernelInitializer } from './kernelInitializer';

function printUsage(): void {
  console.log(`Usage: jupyter-sync <command> [options]

Commands:
  connect <url> [--remote-dir <name>]     Save remote Jupyter URL for this workspace and verify connection
  status                                  Show active VS Code bridge or remote connection status
  sync [--force] [--overwrite-notebooks]  Flush pending file changes and sync workspace now
  exec "<python>" [--notebook <path>]     Execute Python code on the remote kernel (after sync barrier)
  run-cell <notebook.ipynb> <cell-index>  Execute a notebook cell on the remote kernel and sync outputs to local & remote .ipynb
  outputs <notebook.ipynb> [--cell <idx>] Inspect live in-memory (or saved) notebook cell outputs
  sh "<shell-cmd>" [--cwd <rel-dir>]      Run a shell command on the remote workspace pod
  interrupt [--notebook <path>]           Interrupt the currently running remote kernel execution
  restart-kernel [--notebook <path>]      Restart the remote kernel (clears polluted os.environ and /dev/vfio/* locks)
  install-skill [--project]               Install the 'jupyter-workspace-sync' agent skill globally (or into _agents/skills/)

Options:
  --url <url>              Explicit Jupyter Server URL with token (saved automatically for subsequent commands)
  --notebook <path>        Target notebook path for working directory & kernel resolution
  --cwd <rel-dir>          Relative working directory inside the synced repository
  --cell <index>           0-based cell index for 'outputs'
  --remote-dir <name>      Remote base directory name (default: reads .vscode/settings.json or shared/<repo>)
  --force, -f              Bypass manifest hash cache and re-upload all workspace files (including .ipynb)
  --overwrite-notebooks    Upload modified local .ipynb files to the remote server during sync
  --timeout <ms>           Execution timeout in milliseconds (default: 600000; interrupts remote kernel on timeout)
  --project                Also copy the skill into _agents/skills/jupyter-workspace-sync/SKILL.md in the current directory
`);
}

function resolveRemoteDirFromVscodeSettings(workspaceRoot: string): string | undefined {
  try {
    const settingsPath = path.join(workspaceRoot, '.vscode', 'settings.json');
    if (!fs.existsSync(settingsPath)) {
      return undefined;
    }
    const rawJson = fs.readFileSync(settingsPath, 'utf-8');
    const parsed = JSON.parse(rawJson) as Record<string, unknown>;
    const configured = parsed['jupyterSync.remoteBaseDir'];
    if (typeof configured === 'string' && configured.trim().length > 0) {
      const folderName = path.basename(workspaceRoot);
      return configured.replace(/\$\{workspaceFolderBasename\}/g, folderName).trim();
    }
  } catch {
    // ignore malformed settings.json
  }
  return undefined;
}

interface ParsedCliArgs {
  command: string;
  positional: string[];
  code?: string;
  url?: string;
  notebook?: string;
  cwd?: string;
  cell?: number;
  remoteDir?: string;
  timeoutMs?: number;
  force?: boolean;
  overwriteNotebooks?: boolean;
}

function parseCliArgs(argv: string[]): ParsedCliArgs {
  const command = argv[0] || 'help';
  const positional: string[] = [];
  let code: string | undefined;
  let url = process.env.JUPYTER_URL || process.env.JUPYTER_TEST_URL;
  let notebook: string | undefined;
  let cwd: string | undefined;
  let cell: number | undefined;
  let remoteDir: string | undefined;
  let timeoutMs: number | undefined;
  let force = false;
  let overwriteNotebooks = false;

  let i = 1;
  while (i < argv.length) {
    const arg = argv[i];
    if ((arg === '-c' || arg === '--code') && i + 1 < argv.length) {
      code = argv[++i];
    } else if (arg === '--url' && i + 1 < argv.length) {
      url = argv[++i];
    } else if (arg === '--notebook' && i + 1 < argv.length) {
      notebook = argv[++i];
    } else if (arg === '--cwd' && i + 1 < argv.length) {
      cwd = argv[++i];
    } else if (arg === '--cell' && i + 1 < argv.length) {
      cell = parseInt(argv[++i], 10);
    } else if (arg === '--remote-dir' && i + 1 < argv.length) {
      remoteDir = argv[++i];
    } else if (arg === '--timeout' && i + 1 < argv.length) {
      timeoutMs = parseInt(argv[++i], 10);
    } else if (arg === '--force' || arg === '-f') {
      force = true;
      overwriteNotebooks = true;
    } else if (arg === '--overwrite-notebooks') {
      overwriteNotebooks = true;
    } else if (arg === '-h' || arg === '--help') {
      return { command: 'help', positional: [] };
    } else {
      positional.push(arg);
    }
    i++;
  }

  if (command === 'connect' && !url && positional[0]) {
    url = positional[0];
  }

  return {
    command,
    positional,
    code,
    url,
    notebook,
    cwd,
    cell,
    remoteDir,
    timeoutMs,
    force,
    overwriteNotebooks,
  };
}

function printExecutionResult(res: KernelExecutionResult, streamedAlready: boolean): number {
  if (!streamedAlready) {
    if (res.stdout) {
      process.stdout.write(res.stdout.endsWith('\n') ? res.stdout : res.stdout + '\n');
    }
    if (res.stderr) {
      process.stderr.write(res.stderr.endsWith('\n') ? res.stderr : res.stderr + '\n');
    }
  }
  for (const r of res.results || []) {
    console.log(r);
  }
  if (res.status === 'error') {
    if (res.traceback && res.traceback.length > 0) {
      console.error(res.traceback.join('\n'));
    } else {
      console.error(`${res.ename || 'Error'}: ${res.evalue || ''}`);
    }
    return 1;
  }
  return 0;
}

function printCellOutputs(cells: CellOutputSummary[]): void {
  for (const c of cells) {
    const countLabel = c.executionCount !== null ? `[${c.executionCount}]` : '[ ]';
    console.log(`=== Cell ${c.cellIndex} (${c.cellType}) ${countLabel} ===`);
    if (c.stdout) {
      process.stdout.write(c.stdout.endsWith('\n') ? c.stdout : c.stdout + '\n');
    }
    if (c.stderr) {
      process.stderr.write(c.stderr.endsWith('\n') ? c.stderr : c.stderr + '\n');
    }
    for (const r of c.results) {
      console.log(r);
    }
    if (c.error) {
      if (c.error.traceback && c.error.traceback.length > 0) {
        console.error(c.error.traceback.join('\n'));
      } else {
        console.error(`${c.error.ename}: ${c.error.evalue}`);
      }
    }
  }
}

export async function runCli(argv: string[]): Promise<number> {
  const args = parseCliArgs(argv);
  if (args.command === 'help' || args.command === '--help' || args.command === '-h') {
    printUsage();
    return 0;
  }

  if (args.command === 'install-skill') {
    const bundledSkillPath = path.resolve(
      __dirname,
      '..',
      'skills',
      'jupyter-workspace-sync',
      'SKILL.md'
    );
    if (!fs.existsSync(bundledSkillPath)) {
      console.error(`Error: Bundled skill not found at ${bundledSkillPath}`);
      return 1;
    }
    const installedPaths: string[] = [];
    for (const agentDir of ['.gemini', '.claude']) {
      try {
        const targetDir = path.join(os.homedir(), agentDir, 'skills', 'jupyter-workspace-sync');
        fs.mkdirSync(targetDir, { recursive: true });
        const dest = path.join(targetDir, 'SKILL.md');
        fs.copyFileSync(bundledSkillPath, dest);
        installedPaths.push(dest);
      } catch {
        // ignore optional global dirs
      }
    }
    if (argv.includes('--project')) {
      const projDir = path.join(process.cwd(), '_agents', 'skills', 'jupyter-workspace-sync');
      fs.mkdirSync(projDir, { recursive: true });
      const dest = path.join(projDir, 'SKILL.md');
      fs.copyFileSync(bundledSkillPath, dest);
      installedPaths.push(dest);
    }
    for (const p of installedPaths) {
      console.log(`✓ Installed skill: ${p}`);
    }
    return 0;
  }

  const savedSession = loadCliSession(process.cwd());
  const effectiveUrl = args.url || savedSession?.url;
  const discovered = discoverBridgeSocket(process.cwd());

  // 1. Try active VS Code IPC Bridge first
  if (discovered) {
    let bridgeAlive = false;
    let bridgeStatus: any = null;
    try {
      bridgeStatus = await callAgentBridge(
        discovered.socketPath,
        'status',
        undefined,
        undefined,
        2000
      );
      bridgeAlive = true;
    } catch {
      bridgeAlive = false;
    }

    if (bridgeAlive) {
      if (args.command === 'outputs') {
        return await runViaBridge(discovered.socketPath, args);
      }

      if (args.command === 'status') {
        const targetBaseUrl = args.url ? new JupyterClient(args.url).baseUrl : undefined;
        if (
          bridgeStatus?.connected &&
          (!targetBaseUrl || bridgeStatus.baseUrl === targetBaseUrl)
        ) {
          console.log(
            JSON.stringify(
              { mode: 'ipc-bridge', socketPath: discovered.socketPath, ...bridgeStatus },
              null,
              2
            )
          );
          return 0;
        }
        if (!effectiveUrl) {
          console.log(
            JSON.stringify(
              { mode: 'ipc-bridge', socketPath: discovered.socketPath, ...bridgeStatus },
              null,
              2
            )
          );
          return 0;
        }
        // If VS Code bridge is currently idle/disconnected, fall through to headless status check
        // so `jupyter-sync status` reports the saved session without triggering a sync in VS Code.
      } else {
        // Agent is initiating a sync or remote execution action (`connect`, `sync`, `exec`, `run-cell`, `sh`, etc.)
        const targetBaseUrl = args.url ? new JupyterClient(args.url).baseUrl : undefined;
        const needsBridgeConnect =
          args.command === 'connect' ||
          !bridgeStatus?.connected ||
          Boolean(targetBaseUrl && bridgeStatus?.baseUrl !== targetBaseUrl);

        if (needsBridgeConnect) {
          if (!effectiveUrl) {
            console.error(
              'Error: No active VS Code Jupyter Sync connection or saved session found. Run `jupyter-sync connect "<url>"` or pass `--url "<url>"` once to save the connection.'
            );
            return 1;
          }
          const connectRes = await callAgentBridge(
            discovered.socketPath,
            'connect',
            { url: effectiveUrl, remoteDir: args.remoteDir },
            undefined,
            args.timeoutMs ?? 600000
          );
          if (args.command === 'connect') {
            console.log(
              `✓ Connected to ${connectRes.serverLabel} (${connectRes.baseUrl}) -> remoteDir: '${connectRes.remoteBaseDir}' (saved for ${connectRes.workspaceRoot})`
            );
            if (connectRes.syncSummary) {
              const s = connectRes.syncSummary;
              console.log(
                `✓ Synced ${s.uploadedFiles} file(s), deleted ${s.deletedFiles} file(s), ${s.unchangedFiles} unchanged [${s.durationMs}ms]`
              );
            }
            return 0;
          }
          if (
            (args.command === 'sync' || args.command === 'push') &&
            !args.force &&
            !args.overwriteNotebooks &&
            connectRes?.syncSummary
          ) {
            const s = connectRes.syncSummary;
            console.log(
              `✓ Synced ${s.uploadedFiles} file(s), deleted ${s.deletedFiles} file(s), ${s.unchangedFiles} unchanged [${s.durationMs}ms]`
            );
            return 0;
          }
        }

        return await runViaBridge(discovered.socketPath, args);
      }
    }
  }

  // 2. Headless standalone mode using explicit --url, JUPYTER_URL, or saved session URL
  if (args.command === 'outputs' && args.positional[0] && !effectiveUrl) {
    const absNb = path.resolve(process.cwd(), args.positional[0]);
    const cells = parseNotebookOutputsFromDisk(absNb, args.cell);
    printCellOutputs(cells);
    return 0;
  }

  if (!effectiveUrl) {
    if (args.command === 'status') {
      console.log(JSON.stringify({ connected: false, mode: 'disconnected' }, null, 2));
      return 0;
    }
    console.error(
      'Error: No active VS Code Jupyter Sync bridge or saved session found. Run `jupyter-sync connect "<url>"` or pass `--url "<url>"` once to save the connection.'
    );
    return 1;
  }

  return await runHeadless(effectiveUrl, args, savedSession);
}

async function runViaBridge(socketPath: string, args: ParsedCliArgs): Promise<number> {
  if (args.command === 'status') {
    const status = await callAgentBridge(socketPath, 'status');
    console.log(JSON.stringify({ mode: 'ipc-bridge', socketPath, ...status }, null, 2));
    return 0;
  }

  if (args.command === 'interrupt') {
    const res = await callAgentBridge(socketPath, 'interrupt', {
      notebookPath: args.notebook || args.positional[0],
    });
    console.log(`✓ Interrupted remote kernel ${res.kernelId}`);
    return 0;
  }

  if (args.command === 'restart-kernel') {
    const res = await callAgentBridge(socketPath, 'restart-kernel', {
      notebookPath: args.notebook || args.positional[0],
    });
    console.log(`✓ Restarted remote kernel ${res.kernelId}`);
    return 0;
  }

  if (args.command === 'sync' || args.command === 'push') {
    const summary = await callAgentBridge(socketPath, 'sync', {
      force: Boolean(args.force),
      overwriteNotebooks: Boolean(args.overwriteNotebooks || args.force),
    });
    console.log(
      `✓ Synced ${summary.uploadedFiles} file(s), deleted ${summary.deletedFiles} file(s), ${summary.unchangedFiles} unchanged [${summary.durationMs}ms]`
    );
    return 0;
  }

  if (args.command === 'exec') {
    const code = args.code || args.positional[0];
    if (!code) {
      console.error('Error: Missing Python code argument for "exec".');
      return 1;
    }
    const timeoutMs = args.timeoutMs ?? 600000;
    const onSig = () => {
      callAgentBridge(socketPath, 'interrupt', { notebookPath: args.notebook }, undefined, 5000)
        .catch(() => {})
        .finally(() => process.exit(130));
    };
    process.once('SIGINT', onSig);
    process.once('SIGTERM', onSig);
    let streamed = false;
    try {
      const res = (await callAgentBridge(
        socketPath,
        'exec',
        {
          code,
          notebookPath: args.notebook,
          cwd: args.cwd,
          timeoutMs,
        },
        (stream, text) => {
          streamed = true;
          if (stream === 'stderr') {
            process.stderr.write(text);
          } else {
            process.stdout.write(text);
          }
        },
        timeoutMs + 10000
      )) as KernelExecutionResult;
      return printExecutionResult(res, streamed);
    } finally {
      process.removeListener('SIGINT', onSig);
      process.removeListener('SIGTERM', onSig);
    }
  }

  if (args.command === 'run-cell') {
    const nbPath = args.positional[0];
    const cellIdxStr = args.positional[1];
    if (!nbPath || cellIdxStr === undefined) {
      console.error('Usage: jupyter-sync run-cell <notebook.ipynb> <cell-index>');
      return 1;
    }
    const absNb = path.resolve(process.cwd(), nbPath);
    const timeoutMs = args.timeoutMs ?? 600000;
    const onSig = () => {
      callAgentBridge(socketPath, 'interrupt', { notebookPath: absNb }, undefined, 5000)
        .catch(() => {})
        .finally(() => process.exit(130));
    };
    process.once('SIGINT', onSig);
    process.once('SIGTERM', onSig);
    let streamed = false;
    try {
      const res = (await callAgentBridge(
        socketPath,
        'run-cell',
        {
          notebookPath: absNb,
          cellIndex: parseInt(cellIdxStr, 10),
          timeoutMs,
        },
        (stream, text) => {
          streamed = true;
          if (stream === 'stderr') {
            process.stderr.write(text);
          } else {
            process.stdout.write(text);
          }
        },
        timeoutMs + 10000
      )) as KernelExecutionResult;
      return printExecutionResult(res, streamed);
    } finally {
      process.removeListener('SIGINT', onSig);
      process.removeListener('SIGTERM', onSig);
    }
  }

  if (args.command === 'outputs') {
    const nbPath = args.positional[0];
    if (!nbPath) {
      console.error('Usage: jupyter-sync outputs <notebook.ipynb> [--cell <index>]');
      return 1;
    }
    const cells = (await callAgentBridge(socketPath, 'outputs', {
      notebookPath: path.resolve(process.cwd(), nbPath),
      cellIndex: args.cell,
    })) as CellOutputSummary[];
    printCellOutputs(cells);
    return 0;
  }

  if (args.command === 'sh') {
    const cmd = args.code || args.positional[0];
    if (!cmd) {
      console.error('Usage: jupyter-sync sh "<shell-command>" [--cwd <rel-dir>]');
      return 1;
    }
    const timeoutMs = args.timeoutMs ?? 600000;
    let streamed = false;
    const res = await callAgentBridge(
      socketPath,
      'sh',
      {
        command: cmd,
        cwd: args.cwd,
        timeoutMs,
      },
      (_stream, text) => {
        streamed = true;
        process.stdout.write(text);
      },
      timeoutMs + 10000
    );
    if (!streamed && res.stdout) {
      process.stdout.write(res.stdout.endsWith('\n') ? res.stdout : res.stdout + '\n');
    }
    if (res.stderr) {
      process.stderr.write(res.stderr.endsWith('\n') ? res.stderr : res.stderr + '\n');
    }
    return res.exitCode ?? 0;
  }

  console.error(`Unknown command '${args.command}'.`);
  printUsage();
  return 1;
}

async function resolveOrStartPersistentKernel(client: JupyterClient): Promise<string> {
  const kernels = await client.listKernels();
  kernels.sort(
    (a, b) => new Date(b.last_activity || 0).getTime() - new Date(a.last_activity || 0).getTime()
  );
  const existing = kernels.find((k) => k.execution_state === 'idle') || kernels[0];
  if (existing) {
    return existing.id;
  }
  const started = await client.startKernel('python3');
  return started.id;
}

async function syncSingleNotebookToRemote(
  client: JupyterClient,
  workspaceRoot: string,
  remoteBaseDir: string,
  absNbPath: string
): Promise<void> {
  try {
    const normRoot = path.resolve(workspaceRoot);
    const normNb = path.resolve(absNbPath);
    if (normNb !== normRoot && !normNb.startsWith(normRoot + path.sep)) {
      return;
    }
    const relNb = path.relative(normRoot, normNb).split(path.sep).join('/');
    const cleanBase = normalizeApiPath(remoteBaseDir);
    const remoteNbPath = cleanBase ? `${cleanBase}/${relNb}` : relNb;
    const content = fs.readFileSync(normNb);
    await client.putFile(remoteNbPath, content, { overwriteNotebooks: true });
  } catch {
    // ignore non-fatal remote notebook sync error
  }
}

async function runHeadless(
  rawUrl: string,
  args: ParsedCliArgs,
  savedSession?: SavedCliSessionEntry
): Promise<number> {
  const workspaceRoot = process.cwd();
  const remoteBaseDir =
    args.remoteDir ??
    resolveRemoteDirFromVscodeSettings(workspaceRoot) ??
    savedSession?.remoteDir ??
    path.basename(workspaceRoot);
  const timeoutMs = args.timeoutMs ?? 600000;
  const client = new JupyterClient(rawUrl);
  await client.verifyConnection();

  // Persist the verified URL and remoteBaseDir so subsequent CLI invocations in new subshells remember it
  saveCliSession(workspaceRoot, {
    url: rawUrl,
    remoteDir: remoteBaseDir,
    serverLabel: client.label,
    baseUrl: client.baseUrl,
    updatedAt: new Date().toISOString(),
  });

  const syncEngine = new SyncEngine(client);
  const initializer = new KernelInitializer();

  if (args.command === 'status') {
    console.log(
      JSON.stringify(
        {
          mode: 'headless',
          connected: true,
          serverLabel: client.label,
          baseUrl: client.baseUrl,
          workspaceRoot,
          remoteBaseDir,
        },
        null,
        2
      )
    );
    return 0;
  }

  if (args.command === 'interrupt') {
    const kernelId = await resolveOrStartPersistentKernel(client);
    await client.interruptKernel(kernelId);
    console.log(`✓ Interrupted remote kernel ${kernelId}`);
    return 0;
  }

  if (args.command === 'restart-kernel') {
    const kernelId = await resolveOrStartPersistentKernel(client);
    await client.restartKernel(kernelId);
    initializer.invalidate(kernelId);
    console.log(`✓ Restarted remote kernel ${kernelId}`);
    return 0;
  }

  // Always sync before execution in headless mode
  const summary = await syncEngine.syncWorkspace({
    localRoot: workspaceRoot,
    remoteBaseDir,
    force: Boolean(args.force),
    overwriteNotebooks: Boolean(args.overwriteNotebooks || args.force),
  });

  if (args.command === 'connect' || args.command === 'sync' || args.command === 'push') {
    if (args.command === 'connect') {
      console.log(
        `✓ Connected to ${client.label} (${client.baseUrl}) -> remoteDir: '${remoteBaseDir}' (saved for ${workspaceRoot})`
      );
    }
    console.log(
      `✓ Synced ${summary.uploadedFiles} file(s), deleted ${summary.deletedFiles} file(s), ${summary.unchangedFiles} unchanged [${summary.durationMs}ms]`
    );
    return 0;
  }

  if (args.command === 'exec') {
    const code = args.code || args.positional[0];
    if (!code) {
      console.error('Error: Missing Python code argument for "exec".');
      return 1;
    }
    const targetPath = args.notebook
      ? path.resolve(workspaceRoot, args.notebook)
      : args.cwd
      ? path.resolve(workspaceRoot, args.cwd, '_placeholder.ipynb')
      : undefined;

    const kernelId = await resolveOrStartPersistentKernel(client);
    await initializer.initializeKernelDirect(
      client,
      kernelId,
      workspaceRoot,
      remoteBaseDir,
      targetPath
    );

    const execAbort = new AbortController();
    const onSig = () => {
      execAbort.abort();
    };
    process.once('SIGINT', onSig);
    process.once('SIGTERM', onSig);

    let streamed = false;
    try {
      const res = await client.executeCode(kernelId, code, {
        timeoutMs,
        signal: execAbort.signal,
        onStream: (stream, text) => {
          streamed = true;
          if (stream === 'stderr') {
            process.stderr.write(text);
          } else {
            process.stdout.write(text);
          }
        },
      });
      return printExecutionResult(res, streamed);
    } finally {
      process.removeListener('SIGINT', onSig);
      process.removeListener('SIGTERM', onSig);
    }
  }

  if (args.command === 'run-cell') {
    const nbPath = args.positional[0];
    const cellIdx = parseInt(args.positional[1] || '0', 10);
    const absNbPath = path.resolve(workspaceRoot, nbPath);
    const cells = parseNotebookOutputsFromDisk(absNbPath, cellIdx);
    if (cells.length === 0) {
      console.error(`Cell index ${cellIdx} out of range in ${absNbPath}`);
      return 1;
    }

    // Push local notebook_edit cell changes to remote .ipynb before running cell
    await syncSingleNotebookToRemote(client, workspaceRoot, remoteBaseDir, absNbPath);

    const kernelId = await resolveOrStartPersistentKernel(client);
    await initializer.initializeKernelDirect(
      client,
      kernelId,
      workspaceRoot,
      remoteBaseDir,
      absNbPath
    );

    const execAbort = new AbortController();
    const onSig = () => {
      execAbort.abort();
    };
    process.once('SIGINT', onSig);
    process.once('SIGTERM', onSig);

    let streamed = false;
    try {
      const res = await client.executeCode(kernelId, cells[0].source, {
        timeoutMs,
        signal: execAbort.signal,
        onStream: (stream, text) => {
          streamed = true;
          if (stream === 'stderr') {
            process.stderr.write(text);
          } else {
            process.stdout.write(text);
          }
        },
      });
      writeCellExecutionToDiskNotebook(absNbPath, cellIdx, res);
      // Push updated .ipynb (with cell outputs) to remote .ipynb so JupyterLab browser UI stays in sync
      await syncSingleNotebookToRemote(client, workspaceRoot, remoteBaseDir, absNbPath);
      return printExecutionResult(res, streamed);
    } finally {
      process.removeListener('SIGINT', onSig);
      process.removeListener('SIGTERM', onSig);
    }
  }

  if (args.command === 'outputs') {
    const absNbPath = path.resolve(workspaceRoot, args.positional[0]);
    const cells = parseNotebookOutputsFromDisk(absNbPath, args.cell);
    printCellOutputs(cells);
    return 0;
  }

  if (args.command === 'sh') {
    const cmd = args.code || args.positional[0];
    if (!cmd) {
      console.error('Usage: jupyter-sync sh "<shell-command>" [--cwd <rel-dir>]');
      return 1;
    }
    const relCwd = args.cwd ? normalizeApiPath(args.cwd) : '';
    const cleanBase = normalizeApiPath(remoteBaseDir);
    const fullCwd = cleanBase && relCwd ? `${cleanBase}/${relCwd}` : cleanBase || relCwd;
    let streamed = false;
    const res = await client.executeShellCommand(cmd, {
      cwd: fullCwd,
      timeoutMs,
      onOutput: (chunk) => {
        streamed = true;
        process.stdout.write(chunk);
      },
    });
    if (!streamed && res.stdout) {
      process.stdout.write(res.stdout.endsWith('\n') ? res.stdout : res.stdout + '\n');
    }
    return res.exitCode;
  }

  printUsage();
  return 1;
}

if (require.main === module) {
  runCli(process.argv.slice(2))
    .then((code) => process.exit(code))
    .catch((err) => {
      console.error(`Error: ${(err as Error).message}`);
      process.exit(1);
    });
}
