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
import * as vscode from 'vscode';
import { AgentBridgeServer, CellOutputSummary, loadCliSession } from './agentBridge';
import { ConnectionManager, SavedServerMetadata } from './connection';
import { JupyterClient, KernelExecutionResult } from './core/jupyterClient';
import { DEFAULT_EXCLUDE_GLOBS, SyncProgressEvent, SyncSummary } from './core/syncEngine';
import { WorkspaceFileWatcher } from './fileWatcher';

let bridgeServer: AgentBridgeServer | null = null;
let fileWatcher: WorkspaceFileWatcher | null = null;
let activeSyncAbortController: AbortController | null = null;

export async function activate(context: vscode.ExtensionContext): Promise<void> {
  const outputChannel = vscode.window.createOutputChannel('Jupyter Workspace Sync');
  const statusBarItem = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Right, 100);
  context.subscriptions.push(outputChannel, statusBarItem);

  const log = (msg: string) => {
    const now = new Date();
    const ts = now.toTimeString().slice(0, 8);
    outputChannel.appendLine(`[${ts}] ${msg}`);
  };

  const hasWorkspaceFolder = (): boolean => {
    const folders = vscode.workspace.workspaceFolders;
    return Boolean(folders && folders.length > 0);
  };

  const getWorkspaceRoot = (): string => {
    const folders = vscode.workspace.workspaceFolders;
    if (folders && folders.length > 0) {
      return folders[0].uri.fsPath;
    }
    return process.cwd();
  };

  const getConfig = () => vscode.workspace.getConfiguration('jupyterSync');

  const getRemoteBaseDir = (): string => {
    const raw = getConfig().get<string>('remoteBaseDir', '${workspaceFolderBasename}');
    const rootName = path.basename(getWorkspaceRoot());
    const resolved = raw.replace(/\$\{workspaceFolderBasename\}/g, rootName).trim();
    if (resolved === rootName) {
      const cliSession = loadCliSession(getWorkspaceRoot());
      if (cliSession?.remoteDir) {
        return cliSession.remoteDir;
      }
    }
    return resolved;
  };

  const getAutoSyncOnSave = (): boolean => getConfig().get<boolean>('autoSyncOnSave', true);
  const getEnableAutoreload = (): boolean => getConfig().get<boolean>('enableAutoreload', true);
  const getSetKernelWorkingDirectory = (): boolean =>
    getConfig().get<boolean>('setKernelWorkingDirectory', true);
  const getEnableAgentBridge = (): boolean => getConfig().get<boolean>('enableAgentBridge', true);
  const getAutoSaveOutputs = (): boolean => getConfig().get<boolean>('autoSaveOutputs', true);
  const getOverwriteNotebooks = (): boolean =>
    getConfig().get<boolean>('overwriteNotebooks', false);
  const getExcludeGlobs = (): string[] =>
    getConfig().get<string[]>('exclude', DEFAULT_EXCLUDE_GLOBS);
  const getMaxFileSizeMB = (): number => getConfig().get<number>('maxFileSizeMB', 10);

  let lastSyncSummary: SyncSummary | null = null;
  let lastSyncTime: Date | null = null;

  const setStatusDisconnected = () => {
    statusBarItem.text = '$(cloud) Jupyter Sync: Off';
    const md = new vscode.MarkdownString('', true);
    md.isTrusted = true;
    md.appendMarkdown(
      '**Jupyter Workspace Sync** is disconnected.\n\nClick to connect to a remote Jupyter Server or GKE Workspace.\n\n[$(cloud-upload) Connect](command:jupyterSync.connect) · [$(trash) Remove Saved URLs](command:jupyterSync.removeServer)'
    );
    statusBarItem.tooltip = md;
    statusBarItem.command = 'jupyterSync.connect';
    statusBarItem.backgroundColor = undefined;
    statusBarItem.show();
  };

  const setStatusConnected = (meta: SavedServerMetadata) => {
    statusBarItem.text = `$(check) Jupyter Sync: ${meta.label}`;
    statusBarItem.command = 'jupyterSync.showLogs';
    statusBarItem.backgroundColor = undefined;

    const md = new vscode.MarkdownString('', true);
    md.isTrusted = true;
    md.appendMarkdown(`### $(cloud-upload) Jupyter Workspace Sync\n\n`);
    md.appendMarkdown(`- **Server**: \`${meta.baseUrl}\`\n`);
    md.appendMarkdown(`- **Remote Folder**: \`<jupyter-root>/${getRemoteBaseDir() || '.'}\`\n`);
    if (lastSyncSummary && lastSyncTime) {
      const mb = (lastSyncSummary.totalBytesTransferred / (1024 * 1024)).toFixed(2);
      const sec = Math.max(0.1, lastSyncSummary.durationMs / 1000).toFixed(1);
      md.appendMarkdown(
        `- **Last Sync**: ${lastSyncSummary.uploadedFiles} uploaded, ${lastSyncSummary.unchangedFiles} unchanged (${mb} MB in ${sec}s)\n`
      );
    }
    md.appendMarkdown(
      `\n[$(sync) Sync Now](command:jupyterSync.syncNow) · [$(output) Show Logs](command:jupyterSync.showLogs) · [$(key) Update Token](command:jupyterSync.updateToken) · [$(trash) Remove Saved URLs](command:jupyterSync.removeServer) · [$(debug-disconnect) Disconnect](command:jupyterSync.disconnect)`
    );
    statusBarItem.tooltip = md;
    statusBarItem.show();
  };

  const setStatusSyncing = (text: string) => {
    statusBarItem.text = `$(sync~spin) Jupyter Sync: ${text}`;
    statusBarItem.tooltip = 'Jupyter Workspace Sync in progress — click to view live logs';
    statusBarItem.command = 'jupyterSync.showLogs';
    statusBarItem.backgroundColor = undefined;
    statusBarItem.show();
  };

  const setStatusTokenExpired = (meta: SavedServerMetadata) => {
    statusBarItem.text = '$(warning) Jupyter Sync: Token Expired';
    statusBarItem.tooltip = `Connection token for ${meta.label} has expired. Click to paste a replacement URL/token.`;
    statusBarItem.command = 'jupyterSync.updateToken';
    statusBarItem.backgroundColor = new vscode.ThemeColor('statusBarItem.warningBackground');
    statusBarItem.show();
  };

  setStatusDisconnected();

  const runSyncWithUiProgress = async (
    client: JupyterClient,
    options?: {
      isReconnect?: boolean;
      isFullResync?: boolean;
      force?: boolean;
      overwriteNotebooks?: boolean;
    }
  ): Promise<SyncSummary> => {
    const syncEngine = connectionManager.syncEngine;
    if (!syncEngine) {
      throw new Error('SyncEngine is not initialized');
    }

    if (activeSyncAbortController) {
      activeSyncAbortController.abort();
    }
    const abortController = new AbortController();
    activeSyncAbortController = abortController;

    const workspaceRoot = getWorkspaceRoot();
    const remoteBaseDir = getRemoteBaseDir();
    const titlePrefix = options?.isFullResync
      ? `Jupyter Sync: Full Re-sync to ${client.label}`
      : options?.isReconnect
      ? `Jupyter Sync: Checking ${client.label}`
      : `Jupyter Sync: Syncing to ${client.label}`;

    return await vscode.window.withProgress(
      {
        location: vscode.ProgressLocation.Notification,
        title: titlePrefix,
        cancellable: true,
      },
      async (progress, cancelToken) => {
        cancelToken.onCancellationRequested(() => {
          abortController.abort();
          log('Cancellation requested by user.');
        });

        let lastPercent = 0;
        const onProgress = (ev: SyncProgressEvent) => {
          const increment = Math.max(0, ev.percent - lastPercent);
          lastPercent = ev.percent;
          progress.report({
            increment,
            message: ev.message,
          });
          if (ev.phase === 'transfer' && ev.totalFiles > 0) {
            setStatusSyncing(`${ev.percent}% (${ev.filesCompleted}/${ev.totalFiles})`);
          } else {
            setStatusSyncing(`${ev.percent}%`);
          }
        };

        try {
          const syncOpts = {
            localRoot: workspaceRoot,
            remoteBaseDir,
            excludeGlobs: getExcludeGlobs(),
            maxFileSizeMB: getMaxFileSizeMB(),
            overwriteNotebooks: options?.overwriteNotebooks ?? getOverwriteNotebooks(),
            force: options?.force ?? false,
            signal: abortController.signal,
            onProgress,
            onLog: log,
          };

          const summary = options?.isFullResync
            ? await syncEngine.fullResyncWorkspace(syncOpts)
            : await syncEngine.syncWorkspace(syncOpts);

          lastSyncSummary = summary;
          lastSyncTime = new Date();

          if (fileWatcher) {
            const latestManifest = await syncEngine.fetchRemoteManifest(remoteBaseDir);
            fileWatcher.setCachedManifest(latestManifest);
          }

          if (connectionManager.serverMeta) {
            setStatusConnected(connectionManager.serverMeta);
          }

          if (summary.uploadedFiles > 0 || summary.deletedFiles > 0 || !options?.isReconnect) {
            const mb = (summary.totalBytesTransferred / (1024 * 1024)).toFixed(2);
            const sec = (summary.durationMs / 1000).toFixed(1);
            vscode.window
              .showInformationMessage(
                `✓ Synced ${summary.uploadedFiles} file(s) (${mb} MB) to ${client.label} in ${sec}s`,
                'Show Logs'
              )
              .then((choice) => {
                if (choice === 'Show Logs') {
                  outputChannel.show(true);
                }
              });
          }

          return summary;
        } catch (err) {
          if (connectionManager.serverMeta) {
            setStatusConnected(connectionManager.serverMeta);
          } else {
            setStatusDisconnected();
          }
          throw err;
        } finally {
          if (activeSyncAbortController === abortController) {
            activeSyncAbortController = null;
          }
        }
      }
    );
  };

  const connectionManager = new ConnectionManager(context, {
    getWorkspaceRoot,
    getRemoteBaseDir,
    getSetKernelWorkingDirectory,
    getEnableAutoreload,
    getAutoSaveOutputs,
    onServerConnected: async (client, isReconnect) => {
      log(
        `${isReconnect ? 'Reconnecting' : 'Connected'} to ${client.baseUrl} (${client.label})`
      );

      if (fileWatcher) {
        fileWatcher.dispose();
        fileWatcher = null;
      }

      if (connectionManager.syncEngine) {
        fileWatcher = new WorkspaceFileWatcher(client, connectionManager.syncEngine, {
          workspaceRoot: getWorkspaceRoot(),
          getRemoteBaseDir,
          getAutoSyncOnSave,
          getOverwriteNotebooks,
          getExcludeGlobs,
          getMaxFileSizeMB,
          onSyncStart: (label) => setStatusSyncing(label),
          onSyncEnd: () => {
            if (connectionManager.serverMeta) {
              setStatusConnected(connectionManager.serverMeta);
            }
          },
          onLog: log,
          onTriggerBulkSync: async () => {
            await runSyncWithUiProgress(client, { isReconnect: true });
          },
        });
      }

      await runSyncWithUiProgress(client, { isReconnect });
    },
    onServerDisconnected: () => {
      if (fileWatcher) {
        fileWatcher.dispose();
        fileWatcher = null;
      }
      setStatusDisconnected();
    },
    onTokenExpired: (meta) => {
      log(`[Auth] Connection token expired for ${meta.label} (${meta.baseUrl}).`);
      setStatusTokenExpired(meta);
      vscode.window
        .showWarningMessage(
          `Jupyter Sync: Connection token for '${meta.label}' has expired.`,
          'Update Token',
          'Disconnect'
        )
        .then((choice) => {
          if (choice === 'Update Token') {
            vscode.commands.executeCommand('jupyterSync.updateToken');
          } else if (choice === 'Disconnect') {
            vscode.commands.executeCommand('jupyterSync.disconnect');
          }
        });
    },
    onPreCellWaitStart: (msg) => {
      setStatusSyncing(msg);
    },
    onPreCellWaitEnd: () => {
      if (connectionManager.serverMeta) {
        setStatusConnected(connectionManager.serverMeta);
      }
    },
    onLog: log,
  });
  context.subscriptions.push(connectionManager);

  // Auto-save notebook outputs when cell execution finishes in VS Code
  context.subscriptions.push(
    vscode.workspace.onDidChangeNotebookDocument((event) => {
      if (!getAutoSaveOutputs()) {
        return;
      }
      if (event.notebook.uri.scheme !== 'file' || event.notebook.isUntitled) {
        return;
      }
      const completedExecution = event.cellChanges.some(
        (change) =>
          change.executionSummary?.timing?.endTime !== undefined &&
          change.outputs !== undefined
      );
      if (completedExecution && event.notebook.isDirty) {
        event.notebook.save().then(
          () => {},
          () => {}
        );
      }
    })
  );

  // Install companion `jupyter-sync` CLI into ~/.local/bin and agent skill into ~/.gemini/skills
  const installCompanionCli = (showNotification = false): string | null => {
    try {
      const cliJsPath = path.join(context.extensionPath, 'dist', 'cli.js');
      if (!fs.existsSync(cliJsPath)) {
        return null;
      }
      try {
        fs.chmodSync(cliJsPath, 0o755);
      } catch {
        // ignore
      }
      const localBinDir = path.join(os.homedir(), '.local', 'bin');
      fs.mkdirSync(localBinDir, { recursive: true });
      const wrapperPath = path.join(localBinDir, 'jupyter-sync');
      const wrapperScript = [
        '#!/usr/bin/env sh',
        'if command -v node >/dev/null 2>&1; then',
        `  exec node "${cliJsPath}" "$@"`,
        'fi',
        `ELECTRON_RUN_AS_NODE=1 exec "${process.execPath}" "${cliJsPath}" "$@"`,
        '',
      ].join('\n');
      fs.writeFileSync(wrapperPath, wrapperScript, { mode: 0o755 });
      context.environmentVariableCollection.prepend('PATH', `${localBinDir}${path.delimiter}`);

      // Also install the bundled `jupyter-workspace-sync` skill for Jetski / Gemini CLI & Claude Code
      const bundledSkillPath = path.join(
        context.extensionPath,
        'skills',
        'jupyter-workspace-sync',
        'SKILL.md'
      );
      if (fs.existsSync(bundledSkillPath)) {
        for (const agentDir of ['.gemini', '.claude']) {
          try {
            const targetSkillDir = path.join(
              os.homedir(),
              agentDir,
              'skills',
              'jupyter-workspace-sync'
            );
            fs.mkdirSync(targetSkillDir, { recursive: true });
            fs.copyFileSync(bundledSkillPath, path.join(targetSkillDir, 'SKILL.md'));
          } catch {
            // ignore permission errors on optional global skill dirs
          }
        }
      }

      if (showNotification) {
        vscode.window.showInformationMessage(
          `Installed 'jupyter-sync' CLI (${wrapperPath}) and 'jupyter-workspace-sync' agent skill.`
        );
      }
      return wrapperPath;
    } catch (err) {
      log(`[CLI Install] Could not install ~/.local/bin/jupyter-sync: ${(err as Error).message}`);
      return null;
    }
  };

  installCompanionCli(false);

  // Start Local Agent IPC Bridge (~/.jupyter-sync/bridge-<hash>.sock) when a workspace folder is open
  if (getEnableAgentBridge() && hasWorkspaceFolder()) {
    bridgeServer = new AgentBridgeServer({
      workspaceRoot: getWorkspaceRoot(),
      getClient: () => connectionManager.client,
      getSyncEngine: () => connectionManager.syncEngine,
      getKernelInitializer: () => connectionManager.initializer,
      getRemoteBaseDir,
      getSetKernelWorkingDirectory,
      getEnableAutoreload,
      getAutoSaveOutputs,
      getKernelForNotebook: (absNb) => connectionManager.getKernelForNotebook(absNb),
      triggerSyncNow: async (opts) => {
        if (!connectionManager.client) {
          throw new Error('Not connected to a remote Jupyter Server');
        }
        return await runSyncWithUiProgress(connectionManager.client, {
          isReconnect: true,
          force: opts?.force,
          overwriteNotebooks: opts?.overwriteNotebooks,
        });
      },
      connectFromUrl: async (url: string, remoteDir?: string) => {
        await connectionManager.connectFromUrl(url, undefined, remoteDir);
        return lastSyncSummary;
      },
      hostAdapter: {
        getLiveNotebookOutputs: async (
          absNotebookPath: string,
          cellIndex?: number
        ): Promise<CellOutputSummary[] | null> => {
          const normTarget = path.resolve(absNotebookPath);
          const openDoc = vscode.workspace.notebookDocuments.find(
            (d) => d.uri.scheme === 'file' && path.resolve(d.uri.fsPath) === normTarget
          );
          if (!openDoc) {
            return null;
          }
          const cells = openDoc.getCells();
          const summaries: CellOutputSummary[] = [];
          for (let i = 0; i < cells.length; i++) {
            if (cellIndex !== undefined && i !== cellIndex) {
              continue;
            }
            const c = cells[i];
            let stdout = '';
            let stderr = '';
            const results: string[] = [];
            let error: { ename: string; evalue: string; traceback: string[] } | undefined;

            for (const out of c.outputs) {
              for (const item of out.items) {
                const text = Buffer.from(item.data).toString('utf-8');
                if (item.mime === 'application/vnd.code.notebook.stdout') {
                  stdout += text;
                } else if (item.mime === 'application/vnd.code.notebook.stderr') {
                  stderr += text;
                } else if (item.mime === 'application/vnd.code.notebook.error') {
                  try {
                    const parsedErr = JSON.parse(text);
                    error = {
                      ename: String(parsedErr.name || 'Error'),
                      evalue: String(parsedErr.message || ''),
                      traceback: parsedErr.stack ? String(parsedErr.stack).split('\n') : [],
                    };
                  } catch {
                    error = { ename: 'Error', evalue: text, traceback: [text] };
                  }
                } else if (item.mime === 'text/plain') {
                  results.push(text);
                }
              }
            }

            summaries.push({
              cellIndex: i,
              cellType:
                c.kind === vscode.NotebookCellKind.Code ? 'code' : 'markdown',
              source: c.document.getText(),
              executionCount: c.executionSummary?.executionOrder ?? null,
              stdout,
              stderr,
              results,
              error,
            });
          }
          return summaries;
        },
        executeCellInEditor: async (
          absNotebookPath: string,
          cellIndex: number
        ): Promise<KernelExecutionResult | null> => {
          // Only execute via UI if the notebook is already open and mapped to a kernel in VS Code
          if (!connectionManager.getKernelForNotebook(absNotebookPath)) {
            return null;
          }
          const normTarget = path.resolve(absNotebookPath);
          const openDoc = vscode.workspace.notebookDocuments.find(
            (d) => d.uri.scheme === 'file' && path.resolve(d.uri.fsPath) === normTarget
          );
          if (!openDoc || cellIndex < 0 || cellIndex >= openDoc.cellCount) {
            return null;
          }
          await vscode.commands.executeCommand('notebook.cell.execute', {
            ranges: [{ start: cellIndex, end: cellIndex + 1 }],
            document: openDoc.uri,
          });
          const live = await bridgeServer?.['config'].hostAdapter?.getLiveNotebookOutputs?.(
            absNotebookPath,
            cellIndex
          );
          if (!live || live.length === 0) {
            return null;
          }
          const cellOut = live[0];
          return {
            status: cellOut.error ? 'error' : 'ok',
            stdout: cellOut.stdout,
            stderr: cellOut.stderr,
            results: cellOut.results,
            ename: cellOut.error?.ename,
            evalue: cellOut.error?.evalue,
            traceback: cellOut.error?.traceback,
            executionCount: cellOut.executionCount,
          };
        },
        onAgentActivityStart: (desc) => {
          statusBarItem.text = `$(hubot) Jupyter Sync: ${desc}`;
          statusBarItem.show();
        },
        onAgentActivityEnd: () => {
          if (connectionManager.serverMeta) {
            setStatusConnected(connectionManager.serverMeta);
          } else {
            setStatusDisconnected();
          }
        },
        onLog: log,
      },
    });

    bridgeServer
      .start()
      .then((sock) => {
        log(`Agent IPC bridge listening at ${sock}`);
      })
      .catch((err) => {
        log(`[Agent Bridge] Failed to start socket server: ${(err as Error).message}`);
      });
  }

  // Register extension commands
  context.subscriptions.push(
    vscode.commands.registerCommand('jupyterSync.connect', async (explicitUrl?: string) => {
      try {
        if (explicitUrl) {
          await connectionManager.connectFromUrl(explicitUrl);
        } else {
          await connectionManager.selectOrConnectServer();
        }
      } catch (err) {
        vscode.window.showErrorMessage(
          `Jupyter Sync connection failed: ${(err as Error).message}`
        );
      }
    }),

    vscode.commands.registerCommand('jupyterSync.removeServer', async () => {
      try {
        await connectionManager.promptToRemoveServers();
      } catch (err) {
        vscode.window.showErrorMessage(
          `Failed to remove saved server: ${(err as Error).message}`
        );
      }
    }),

    vscode.commands.registerCommand('jupyterSync.syncNow', async () => {
      if (!connectionManager.client) {
        const choice = await vscode.window.showInformationMessage(
          'Jupyter Workspace Sync is not connected to a remote server.',
          'Connect Now'
        );
        if (choice === 'Connect Now') {
          await vscode.commands.executeCommand('jupyterSync.connect');
        }
        return;
      }
      try {
        if (fileWatcher) {
          await fileWatcher.flushPending();
        }
        await runSyncWithUiProgress(connectionManager.client, { isReconnect: false });
      } catch (err) {
        vscode.window.showErrorMessage(`Jupyter Sync failed: ${(err as Error).message}`);
      }
    }),

    vscode.commands.registerCommand('jupyterSync.cancelSync', () => {
      if (activeSyncAbortController) {
        activeSyncAbortController.abort();
        vscode.window.showInformationMessage('Cancelled active Jupyter Workspace Sync.');
      } else {
        vscode.window.showInformationMessage('No Jupyter Workspace Sync is currently running.');
      }
    }),

    vscode.commands.registerCommand('jupyterSync.fullResync', async () => {
      if (!connectionManager.client) {
        vscode.window.showWarningMessage('Connect to a remote Jupyter Server first.');
        return;
      }
      try {
        await runSyncWithUiProgress(connectionManager.client, { isFullResync: true });
      } catch (err) {
        vscode.window.showErrorMessage(`Full Re-sync failed: ${(err as Error).message}`);
      }
    }),

    vscode.commands.registerCommand('jupyterSync.updateToken', async (explicitTokenOrUrl?: string) => {
      const input =
        explicitTokenOrUrl ||
        (await vscode.window.showInputBox({
          title: 'Jupyter Workspace Sync: Update Connection Token',
          prompt: 'Paste the new connection URL (with ?token=...) or raw token',
          placeHolder: 'https://connect.../jupyterlab/?token=... or raw token',
          ignoreFocusOut: true,
        }));
      if (!input) {
        return;
      }
      try {
        await connectionManager.updateActiveToken(input);
        if (connectionManager.serverMeta) {
          setStatusConnected(connectionManager.serverMeta);
        }
        vscode.window.showInformationMessage('✓ Updated Jupyter Server connection token.');
      } catch (err) {
        vscode.window.showErrorMessage(
          `Failed to update token: ${(err as Error).message}`
        );
      }
    }),

    vscode.commands.registerCommand('jupyterSync.disconnect', async () => {
      if (fileWatcher) {
        fileWatcher.dispose();
        fileWatcher = null;
      }
      await connectionManager.disconnect();
      setStatusDisconnected();
      log('Disconnected from remote Jupyter Server.');
      vscode.window.showInformationMessage('Disconnected from remote Jupyter Server.');
    }),

    vscode.commands.registerCommand('jupyterSync.showLogs', () => {
      outputChannel.show(true);
    }),

    vscode.commands.registerCommand('jupyterSync.installCli', () => {
      installCompanionCli(true);
    })
  );

  // Register server provider with ms-toolsai.jupyter without auto-connecting or syncing on startup.
  // Syncing will only start when the user connects to a remote kernel or an agent initiates syncing.
  await connectionManager.registerWithJupyterExtension();
}

export async function deactivate(): Promise<void> {
  if (fileWatcher) {
    fileWatcher.dispose();
    fileWatcher = null;
  }
  if (bridgeServer) {
    await bridgeServer.stop().catch(() => {});
    bridgeServer = null;
  }
}
