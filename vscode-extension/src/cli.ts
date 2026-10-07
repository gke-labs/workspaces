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
import * as path from 'path';
import {
  callAgentBridge,
  CellOutputSummary,
  discoverBridgeSocket,
  parseNotebookOutputsFromDisk,
  writeCellExecutionToDiskNotebook,
} from './agentBridge';
import { JupyterClient, KernelExecutionResult, normalizeApiPath } from './core/jupyterClient';
import { SyncEngine } from './core/syncEngine';
import { KernelInitializer } from './kernelInitializer';

function printUsage(): void {
  console.log(`Usage: jupyter-sync <command> [options]

Commands:
  status                                  Show active VS Code bridge or remote connection status
  sync [--url <url>]                      Flush pending file changes and sync workspace now
  exec "<python>" [--notebook <path>]     Execute Python code on the remote kernel (after sync barrier)
  run-cell <notebook.ipynb> <cell-index>  Execute a notebook cell on the remote kernel (after sync barrier)
  outputs <notebook.ipynb> [--cell <idx>] Inspect live in-memory (or saved) notebook cell outputs
  sh "<shell-cmd>" [--cwd <rel-dir>]      Run a shell command on the remote workspace pod

Options:
  --url <url>          Explicit Jupyter Server URL with token (or set JUPYTER_URL for headless mode)
  --notebook <path>    Target notebook path for working directory & kernel resolution
  --cwd <rel-dir>      Relative working directory inside the synced repository
  --cell <index>       0-based cell index for 'outputs'
  --remote-dir <name>  Remote base directory name (default: workspace folder basename)
`);
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
    } else if (arg === '-h' || arg === '--help') {
      return { command: 'help', positional: [] };
    } else {
      positional.push(arg);
    }
    i++;
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

  const discovered = discoverBridgeSocket(process.cwd());

  // 1. Try active VS Code IPC Bridge first
  if (discovered) {
    try {
      await callAgentBridge(discovered.socketPath, 'ping', undefined, undefined, 2000);
      return await runViaBridge(discovered.socketPath, args);
    } catch {
      // Socket was stale; fall through to headless mode if URL is available
    }
  }

  // 2. Headless standalone mode using JUPYTER_URL / --url
  if (args.command === 'outputs' && args.positional[0] && !args.url) {
    const absNb = path.resolve(process.cwd(), args.positional[0]);
    const cells = parseNotebookOutputsFromDisk(absNb, args.cell);
    printCellOutputs(cells);
    return 0;
  }

  if (!args.url) {
    if (args.command === 'status') {
      console.log(JSON.stringify({ connected: false, mode: 'disconnected' }, null, 2));
      return 0;
    }
    console.error(
      'Error: No active VS Code Jupyter Sync bridge found for this directory, and no JUPYTER_URL / --url was provided.'
    );
    return 1;
  }

  return await runHeadless(args.url, args);
}

async function runViaBridge(socketPath: string, args: ParsedCliArgs): Promise<number> {
  if (args.command === 'status') {
    const status = await callAgentBridge(socketPath, 'status');
    console.log(JSON.stringify({ mode: 'ipc-bridge', socketPath, ...status }, null, 2));
    return 0;
  }

  if (args.command === 'sync' || args.command === 'push') {
    const summary = await callAgentBridge(socketPath, 'sync');
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
    let streamed = false;
    const res = (await callAgentBridge(
      socketPath,
      'exec',
      {
        code,
        notebookPath: args.notebook,
        cwd: args.cwd,
      },
      (stream, text) => {
        streamed = true;
        if (stream === 'stderr') {
          process.stderr.write(text);
        } else {
          process.stdout.write(text);
        }
      }
    )) as KernelExecutionResult;
    return printExecutionResult(res, streamed);
  }

  if (args.command === 'run-cell') {
    const nbPath = args.positional[0];
    const cellIdxStr = args.positional[1];
    if (!nbPath || cellIdxStr === undefined) {
      console.error('Usage: jupyter-sync run-cell <notebook.ipynb> <cell-index>');
      return 1;
    }
    let streamed = false;
    const res = (await callAgentBridge(
      socketPath,
      'run-cell',
      {
        notebookPath: path.resolve(process.cwd(), nbPath),
        cellIndex: parseInt(cellIdxStr, 10),
      },
      (stream, text) => {
        streamed = true;
        if (stream === 'stderr') {
          process.stderr.write(text);
        } else {
          process.stdout.write(text);
        }
      }
    )) as KernelExecutionResult;
    return printExecutionResult(res, streamed);
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
    let streamed = false;
    const res = await callAgentBridge(
      socketPath,
      'sh',
      {
        command: cmd,
        cwd: args.cwd,
      },
      (_stream, text) => {
        streamed = true;
        process.stdout.write(text);
      }
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

async function runHeadless(rawUrl: string, args: ParsedCliArgs): Promise<number> {
  const workspaceRoot = process.cwd();
  const remoteBaseDir = args.remoteDir ?? path.basename(workspaceRoot);
  const client = new JupyterClient(rawUrl);
  await client.verifyConnection();
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

  // Always sync before execution in headless mode
  const summary = await syncEngine.syncWorkspace({
    localRoot: workspaceRoot,
    remoteBaseDir,
  });

  if (args.command === 'sync' || args.command === 'push') {
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

    return await client.withIdleOrTempKernel(async (kernelId) => {
      await initializer.initializeKernelDirect(
        client,
        kernelId,
        workspaceRoot,
        remoteBaseDir,
        targetPath
      );
      let streamed = false;
      const res = await client.executeCode(kernelId, code, {
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
    });
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
    return await client.withIdleOrTempKernel(async (kernelId) => {
      await initializer.initializeKernelDirect(
        client,
        kernelId,
        workspaceRoot,
        remoteBaseDir,
        absNbPath
      );
      let streamed = false;
      const res = await client.executeCode(kernelId, cells[0].source, {
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
      return printExecutionResult(res, streamed);
    });
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
