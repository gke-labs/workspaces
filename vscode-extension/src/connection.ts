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
import * as path from 'path';
import * as vscode from 'vscode';
import WebSocket from 'ws';
import { JupyterAuthError, JupyterClient, normalizeApiPath } from './core/jupyterClient';
import { SyncEngine } from './core/syncEngine';
import { parseJupyterUrl, ParsedJupyterUrl } from './core/urlParser';
import {
  buildKernelInitSnippet,
  decodeJupyterWsMessage,
  encodeJupyterWsMessage,
  JupyterWireMessage,
  KernelInitializer,
  resolveNotebookRemotePaths,
} from './kernelInitializer';

export interface SavedServerMetadata {
  id: string;
  baseUrl: string;
  wsBaseUrl: string;
  origin: string;
  label: string;
  namespace?: string;
  workspace?: string;
  lastConnectedAt: string;
}

export interface JupyterServerConnectionInformation {
  baseUrl: vscode.Uri;
  token?: string;
  headers?: Record<string, string>;
  fetch?: any;
  WebSocket?: any;
}

export interface JupyterServerItem {
  id: string;
  label: string;
  connectionInformation?: JupyterServerConnectionInformation;
}

export interface JupyterServerCommand {
  label: string;
  description?: string;
  canBeAutoSelected?: boolean;
  url?: string;
}

const SAVED_SERVERS_KEY = 'jupyterSync.savedServers';
const ACTIVE_SERVER_ID_KEY = 'jupyterSync.activeServerId';
const SECRET_PREFIX = 'jupyterSync.token.';

export interface ConnectionManagerCallbacks {
  getWorkspaceRoot: () => string;
  getRemoteBaseDir: () => string;
  getSetKernelWorkingDirectory: () => boolean;
  getEnableAutoreload: () => boolean;
  getAutoSaveOutputs: () => boolean;
  onServerConnected: (client: JupyterClient, isReconnect: boolean) => Promise<void>;
  onTokenExpired: (server: SavedServerMetadata) => void;
  onPreCellWaitStart: (msg: string) => void;
  onPreCellWaitEnd: () => void;
  onLog: (msg: string) => void;
}

export class ConnectionManager implements vscode.Disposable {
  private readonly disposables: vscode.Disposable[] = [];
  private readonly onDidChangeServersEmitter = new vscode.EventEmitter<void>();
  public readonly onDidChangeServers = this.onDidChangeServersEmitter.event;

  private activeClient: JupyterClient | null = null;
  private activeServerMeta: SavedServerMetadata | null = null;
  private activeSyncEngine: SyncEngine | null = null;
  private readonly kernelInitializer = new KernelInitializer();
  private readonly kernelToNotebookPath = new Map<string, string>();
  private collectionDisposable: vscode.Disposable | null = null;

  constructor(
    private readonly context: vscode.ExtensionContext,
    private readonly callbacks: ConnectionManagerCallbacks
  ) {
    this.disposables.push(this.onDidChangeServersEmitter);
  }

  get client(): JupyterClient | null {
    return this.activeClient;
  }

  get serverMeta(): SavedServerMetadata | null {
    return this.activeServerMeta;
  }

  get syncEngine(): SyncEngine | null {
    return this.activeSyncEngine;
  }

  get initializer(): KernelInitializer {
    return this.kernelInitializer;
  }

  getKernelForNotebook(notebookFsPath: string): string | undefined {
    const norm = path.resolve(notebookFsPath);
    for (const [kId, nbPath] of this.kernelToNotebookPath.entries()) {
      if (path.resolve(nbPath) === norm) {
        return kId;
      }
    }
    return undefined;
  }

  /**
   * Registers the JupyterServerCollection with ms-toolsai.jupyter.
   */
  async registerWithJupyterExtension(): Promise<void> {
    const jupyterExt = vscode.extensions.getExtension('ms-toolsai.jupyter');
    if (!jupyterExt) {
      this.callbacks.onLog(
        '[Warn] ms-toolsai.jupyter extension is not installed; kernel picker integration skipped.'
      );
      return;
    }

    try {
      const jupyterApi = jupyterExt.isActive
        ? jupyterExt.exports
        : await jupyterExt.activate();

      if (!jupyterApi || typeof jupyterApi.createJupyterServerCollection !== 'function') {
        this.callbacks.onLog(
          '[Warn] ms-toolsai.jupyter API does not expose createJupyterServerCollection.'
        );
        return;
      }

      const serverProvider = {
        onDidChangeServers: this.onDidChangeServers,
        provideJupyterServers: async (
          _token: vscode.CancellationToken
        ): Promise<JupyterServerItem[]> => {
          const saved = this.getSavedServers();
          return saved.map((s) => ({
            id: s.id,
            label: `Jupyter Sync: ${s.label}`,
          }));
        },
        resolveJupyterServer: async (
          server: JupyterServerItem,
          _token: vscode.CancellationToken
        ): Promise<JupyterServerItem> => {
          const saved = this.getSavedServers().find((s) => s.id === server.id);
          if (!saved) {
            throw new Error(`Jupyter Sync server '${server.id}' not found.`);
          }
          const secretToken = (await this.context.secrets.get(`${SECRET_PREFIX}${saved.id}`)) || '';

          if (!this.activeClient || this.activeClient.serverId !== saved.id) {
            await this.activateServer(saved, secretToken, true);
          } else if (secretToken && this.activeClient.token !== secretToken) {
            this.activeClient.updateToken(secretToken);
          }

          const client = this.activeClient!;
          return {
            id: saved.id,
            label: `Jupyter Sync: ${saved.label}`,
            connectionInformation: this.buildConnectionInformation(client),
          };
        },
      };

      const commandProvider = {
        provideCommands: async (
          value: string | undefined,
          _token: vscode.CancellationToken
        ): Promise<JupyterServerCommand[]> => {
          const trimmed = (value || '').trim();
          if (trimmed.startsWith('http://') || trimmed.startsWith('https://')) {
            return [
              {
                label: `$(cloud-upload) Connect & Sync to ${trimmed}`,
                description: 'Jupyter Workspace Sync',
                url: trimmed,
                canBeAutoSelected: true,
              },
            ];
          }
          const hasExisting = this.getSavedServers().length > 0;
          return [
            {
              label: '$(cloud-upload) Connect & Sync Remote Jupyter Server...',
              description: 'Paste a JupyterLab, JupyterHub, or GKE Workspace URL with token',
              canBeAutoSelected: !hasExisting,
            },
          ];
        },
        handleCommand: async (
          command: JupyterServerCommand,
          _token: vscode.CancellationToken
        ): Promise<JupyterServerItem | undefined> => {
          const rawUrl = command.url || (await this.promptForJupyterUrl());
          if (!rawUrl) {
            return undefined;
          }
          const connected = await this.connectFromUrl(rawUrl);
          if (!connected || !this.activeClient) {
            return undefined;
          }
          return {
            id: connected.id,
            label: `Jupyter Sync: ${connected.label}`,
            connectionInformation: this.buildConnectionInformation(this.activeClient),
          };
        },
      };

      const collection = jupyterApi.createJupyterServerCollection(
        'jupyter-workspace-sync',
        'Jupyter Workspace Sync',
        serverProvider
      );
      collection.commandProvider = commandProvider;
      this.collectionDisposable = collection;
      this.disposables.push(collection);
    } catch (err) {
      this.callbacks.onLog(
        `[Warn] Failed to register JupyterServerCollection: ${(err as Error).message}`
      );
    }
  }

  /**
   * Restores the previously active server connection on window reload if present.
   */
  async restorePreviousSessionIfAny(): Promise<boolean> {
    const activeId = this.context.workspaceState.get<string>(ACTIVE_SERVER_ID_KEY);
    if (!activeId) {
      return false;
    }
    const saved = this.getSavedServers().find((s) => s.id === activeId);
    if (!saved) {
      return false;
    }
    const token = await this.context.secrets.get(`${SECRET_PREFIX}${saved.id}`);
    if (token === undefined) {
      return false;
    }

    try {
      await this.activateServer(saved, token, true);
      return true;
    } catch (err) {
      this.callbacks.onLog(
        `[Reconnect] Could not restore previous session to ${saved.label}: ${
          (err as Error).message
        }`
      );
      return false;
    }
  }

  async promptForJupyterUrl(promptText?: string): Promise<string | undefined> {
    return await vscode.window.showInputBox({
      title: 'Jupyter Workspace Sync: Connect to Remote Jupyter Server',
      prompt:
        promptText ||
        'Paste the remote Jupyter Server / GKE Workspace URL (including ?token=...)',
      placeHolder:
        'https://connect.<ip>.sslip.io/workspace/connect/<namespace>/<workspace>/jupyterlab/?token=...',
      ignoreFocusOut: true,
      password: false,
      validateInput: (val) => {
        if (!val || !val.trim()) {
          return 'Please enter a Jupyter Server URL.';
        }
        try {
          parseJupyterUrl(val);
          return null;
        } catch (err) {
          return (err as Error).message;
        }
      },
    });
  }

  /**
   * Connects to a remote Jupyter Server from a raw URL, stores token in SecretStorage,
   * and triggers initial/reconnect workspace sync.
   */
  async connectFromUrl(rawUrl: string, explicitToken?: string): Promise<SavedServerMetadata> {
    const parsed: ParsedJupyterUrl = parseJupyterUrl(rawUrl, explicitToken);
    const meta: SavedServerMetadata = {
      id: parsed.id,
      baseUrl: parsed.baseUrl,
      wsBaseUrl: parsed.wsBaseUrl,
      origin: parsed.origin,
      label: parsed.label,
      namespace: parsed.namespace,
      workspace: parsed.workspace,
      lastConnectedAt: new Date().toISOString(),
    };

    await this.activateServer(meta, parsed.token, false);
    return meta;
  }

  private async activateServer(
    meta: SavedServerMetadata,
    token: string,
    isReconnect: boolean
  ): Promise<void> {
    const client = new JupyterClient(
      {
        id: meta.id,
        baseUrl: meta.baseUrl,
        wsBaseUrl: meta.wsBaseUrl,
        origin: meta.origin,
        token,
        label: meta.label,
        namespace: meta.namespace,
        workspace: meta.workspace,
      },
      token
    );

    client.onAuthError(() => {
      this.callbacks.onTokenExpired(meta);
    });

    await client.verifyConnection();

    await this.context.secrets.store(`${SECRET_PREFIX}${meta.id}`, token);
    await this.saveServerMetadata(meta);
    await this.context.workspaceState.update(ACTIVE_SERVER_ID_KEY, meta.id);

    this.activeClient = client;
    this.activeServerMeta = meta;
    this.activeSyncEngine = new SyncEngine(client);
    this.kernelInitializer.invalidate();

    this.onDidChangeServersEmitter.fire();
    await this.callbacks.onServerConnected(client, isReconnect);
  }

  /**
   * Updates the token for the currently active server without losing the workspace binding.
   */
  async updateActiveToken(newTokenOrUrl: string): Promise<void> {
    if (!this.activeClient || !this.activeServerMeta) {
      await this.connectFromUrl(newTokenOrUrl);
      return;
    }

    this.activeClient.updateToken(newTokenOrUrl);
    await this.activeClient.verifyConnection();
    await this.context.secrets.store(
      `${SECRET_PREFIX}${this.activeServerMeta.id}`,
      this.activeClient.token
    );
    this.onDidChangeServersEmitter.fire();
  }

  async disconnect(): Promise<void> {
    this.activeClient = null;
    this.activeServerMeta = null;
    this.activeSyncEngine = null;
    this.kernelInitializer.invalidate();
    this.kernelToNotebookPath.clear();
    await this.context.workspaceState.update(ACTIVE_SERVER_ID_KEY, undefined);
    this.onDidChangeServersEmitter.fire();
  }

  getSavedServers(): SavedServerMetadata[] {
    return this.context.globalState.get<SavedServerMetadata[]>(SAVED_SERVERS_KEY, []);
  }

  private async saveServerMetadata(meta: SavedServerMetadata): Promise<void> {
    const existing = this.getSavedServers().filter((s) => s.id !== meta.id);
    existing.unshift(meta);
    await this.context.globalState.update(SAVED_SERVERS_KEY, existing.slice(0, 10));
  }

  /**
   * Builds the connectionInformation object for `ms-toolsai.jupyter`, including
   * protocol-layer `fetch` and `WebSocket` interceptors.
   */
  private buildConnectionInformation(client: JupyterClient): JupyterServerConnectionInformation {
    return {
      baseUrl: vscode.Uri.parse(client.baseUrl),
      token: client.token,
      headers: client.buildHeaders(),
      fetch: this.createSyncFetch(client),
      WebSocket: this.createSyncWebSocketClass(client),
    };
  }

  /**
   * Protocol-layer `fetch` wrapper for `ms-toolsai.jupyter`:
   * - Normalizes `node-fetch` Request objects passed by `@jupyterlab/services`.
   * - Synchronizes `?token=` query parameter and `Authorization` header on token renewal.
   * - Rewrites `POST /api/sessions` path to match the notebook's remote directory.
   * - Tracks `kernelId -> notebookPath` and invalidates kernel init state on `/restart`.
   */
  private createSyncFetch(client: JupyterClient) {
    return async (input: any, init?: any): Promise<Response> => {
      let rawUrl = typeof input === 'string' ? input : String(input?.url || input);
      const method = String(init?.method || input?.method || 'GET').toUpperCase();

      // Extract headers from node-fetch Request or RequestInit
      const headers: Record<string, string> = {};
      const copyHeaders = (src: any) => {
        if (!src) {
          return;
        }
        if (typeof src.forEach === 'function') {
          src.forEach((val: string, key: string) => {
            headers[key] = val;
          });
        } else if (typeof src.raw === 'function') {
          const rawObj = src.raw();
          for (const k of Object.keys(rawObj)) {
            headers[k] = Array.isArray(rawObj[k]) ? rawObj[k].join(', ') : String(rawObj[k]);
          }
        } else if (typeof src === 'object') {
          for (const [k, v] of Object.entries(src)) {
            if (v !== undefined) {
              headers[k] = String(v);
            }
          }
        }
      };
      copyHeaders(input?.headers);
      copyHeaders(init?.headers);

      // Extract body if present
      let body: any = init?.body;
      if (body === undefined && method !== 'GET' && method !== 'HEAD') {
        if (typeof input?.text === 'function') {
          try {
            body = await input.text();
          } catch {
            body = undefined;
          }
        } else if (input?.body) {
          body = input.body;
        }
      }

      // Normalize URL & synchronize query token with latest client token
      const parsedUrl = new URL(rawUrl);
      parsedUrl.pathname = parsedUrl.pathname.replace(/\/+/g, '/');
      if (parsedUrl.searchParams.has('token')) {
        if (client.token) {
          parsedUrl.searchParams.set('token', client.token);
        } else {
          parsedUrl.searchParams.delete('token');
        }
      }

      // Apply latest auth, Origin, and XSRF headers (replacing any stale Authorization header)
      for (const k of Object.keys(headers)) {
        if (k.toLowerCase() === 'authorization' || k.toLowerCase() === 'origin') {
          delete headers[k];
        }
      }
      const mergedHeaders = {
        ...headers,
        ...client.buildHeaders(),
      };

      // Intercept POST /api/sessions or PATCH /api/sessions/<id> to map notebook path
      let matchedNotebookFsPath: string | undefined;
      if (
        (method === 'POST' || method === 'PATCH') &&
        /\/api\/sessions(?:\/[^/?]+)?$/.test(parsedUrl.pathname) &&
        typeof body === 'string'
      ) {
        try {
          const sessionReq = JSON.parse(body);
          const candidateName = String(sessionReq.name || sessionReq.path || '');
          matchedNotebookFsPath = this.findMatchingNotebookFsPath(candidateName);
          if (matchedNotebookFsPath && this.callbacks.getSetKernelWorkingDirectory()) {
            const remotePaths = resolveNotebookRemotePaths(
              this.callbacks.getWorkspaceRoot(),
              this.callbacks.getRemoteBaseDir(),
              matchedNotebookFsPath
            );
            if (remotePaths.relNotebookDir) {
              await client.ensureDirectory(remotePaths.relNotebookDir).catch(() => {});
              const baseSessionFile = path.posix.basename(
                String(sessionReq.path || `${path.basename(matchedNotebookFsPath)}`)
              );
              sessionReq.path = `${remotePaths.relNotebookDir}/${baseSessionFile}`;
              body = JSON.stringify(sessionReq);
            }
          }
        } catch {
          // ignore JSON parse error
        }
      }

      // Intercept POST /api/kernels/<id>/restart to invalidate kernel init state
      const restartMatch = parsedUrl.pathname.match(/\/api\/kernels\/([^/]+)\/restart$/);
      if (method === 'POST' && restartMatch) {
        this.kernelInitializer.invalidate(restartMatch[1]);
      }

      const resp = await fetch(parsedUrl.toString(), {
        method,
        headers: mergedHeaders,
        body,
      });

      if (resp.status === 401 || resp.status === 403) {
        if (this.activeServerMeta) {
          this.callbacks.onTokenExpired(this.activeServerMeta);
        }
      }

      // If this was a session creation/update response, record kernel.id -> notebookFsPath
      if (
        resp.ok &&
        (method === 'POST' || method === 'PATCH') &&
        /\/api\/sessions(?:\/[^/?]+)?$/.test(parsedUrl.pathname)
      ) {
        try {
          const cloned = resp.clone();
          const data = (await cloned.json()) as { kernel?: { id?: string } };
          if (data?.kernel?.id && matchedNotebookFsPath) {
            this.kernelToNotebookPath.set(data.kernel.id, matchedNotebookFsPath);
          }
        } catch {
          // ignore
        }
      }

      return resp;
    };
  }

  /**
   * Protocol-layer `WebSocket` subclass for `ms-toolsai.jupyter`:
   * - Injects auth and Origin headers into the handshake.
   * - Implements the Pre-Cell-Execution Sync Barrier before every user `execute_request`.
   * - Silently executes the idempotent `cwd` / `sys.path` / `%autoreload 2` setup snippet
   *   on the socket before forwarding the first cell execution for a notebook directory.
   */
  private createSyncWebSocketClass(client: JupyterClient) {
    const manager = this;

    return class SyncWebSocket extends WebSocket {
      private readonly extractedKernelId?: string;
      private outboundQueue: Promise<void> = Promise.resolve();
      private readonly pendingInitWaiters = new Map<string, () => void>();

      constructor(address: string | URL, protocols?: string | string[], options?: any) {
        const rawUrl = address.toString();
        const parsedWs = new URL(rawUrl);
        parsedWs.pathname = parsedWs.pathname.replace(/\/+/g, '/');
        if (parsedWs.searchParams.has('token')) {
          if (client.token) {
            parsedWs.searchParams.set('token', client.token);
          } else {
            parsedWs.searchParams.delete('token');
          }
        }

        const mergedOptions = {
          ...(options || {}),
          headers: {
            ...(options?.headers || {}),
            ...client.buildHeaders(),
          },
        };

        if (protocols !== undefined) {
          super(parsedWs.toString(), protocols, mergedOptions);
        } else {
          super(parsedWs.toString(), mergedOptions);
        }

        const match = /\/api\/kernels\/([^/]+)\/channels/.exec(parsedWs.pathname);
        if (match && match[1]) {
          this.extractedKernelId = match[1];
        }
      }

      override emit(event: string | symbol, ...args: any[]): boolean {
        if (event === 'message' && args.length > 0 && this.pendingInitWaiters.size > 0) {
          const decoded = decodeJupyterWsMessage(args[0], this.protocol);
          const parentMsgId = decoded?.parent_header?.msg_id;
          if (typeof parentMsgId === 'string' && this.pendingInitWaiters.has(parentMsgId)) {
            if (decoded?.header?.msg_type === 'execute_reply') {
              const resolver = this.pendingInitWaiters.get(parentMsgId);
              this.pendingInitWaiters.delete(parentMsgId);
              resolver?.();
            }
            // Swallow internal silent setup replies so @jupyterlab/services never sees them
            return true;
          }
        }
        return super.emit(event, ...args);
      }

      override send(data: any, optionsOrCb?: any, cb?: any): void {
        const sendRaw = (payload: any, opt?: any, callback?: any) => {
          if (typeof opt === 'function') {
            super.send(payload, opt);
          } else if (opt !== undefined) {
            super.send(payload, opt, callback);
          } else {
            super.send(payload, callback);
          }
        };

        this.outboundQueue = this.outboundQueue
          .then(async () => {
            const decoded = decodeJupyterWsMessage(data, this.protocol);
            const isUserCellExec =
              decoded &&
              decoded.header?.msg_type === 'execute_request' &&
              decoded.content?.silent === false;

            if (isUserCellExec) {
              // 1. Pre-Cell-Execution Sync Barrier: flush pending file edits & wait for active sync
              if (manager.activeSyncEngine) {
                const needBanner = manager.activeSyncEngine.isSyncInProgress;
                if (needBanner) {
                  manager.callbacks.onPreCellWaitStart(
                    'Waiting for repository sync before running cell...'
                  );
                }
                try {
                  await manager.activeSyncEngine.flushAndWait();
                } finally {
                  if (needBanner) {
                    manager.callbacks.onPreCellWaitEnd();
                  }
                }
              }

              // 2. Silent Kernel Initialization (cwd, sys.path, %autoreload 2)
              if (this.extractedKernelId) {
                await this.ensureKernelInitializedOnSocket(
                  this.extractedKernelId,
                  decoded,
                  sendRaw
                );
              }
            }

            sendRaw(data, optionsOrCb, cb);
          })
          .catch((err) => {
            manager.callbacks.onLog(`[WebSocket Send Error] ${(err as Error).message}`);
            sendRaw(data, optionsOrCb, cb);
          });
      }

      private async ensureKernelInitializedOnSocket(
        kernelId: string,
        userExecMsg: JupyterWireMessage,
        sendRaw: (payload: any) => void
      ): Promise<void> {
        const setCwd = manager.callbacks.getSetKernelWorkingDirectory();
        const enableAutoreload = manager.callbacks.getEnableAutoreload();
        if (!setCwd && !enableAutoreload) {
          return;
        }

        const notebookFsPath = manager.resolveNotebookForKernel(kernelId, userExecMsg);
        const remotePaths = resolveNotebookRemotePaths(
          manager.callbacks.getWorkspaceRoot(),
          manager.callbacks.getRemoteBaseDir(),
          notebookFsPath
        );

        if (manager.kernelInitializer.isInitialized(kernelId, remotePaths.relNotebookDir)) {
          return;
        }

        const snippet = buildKernelInitSnippet(
          remotePaths.relRepoRoot,
          remotePaths.relNotebookDir,
          {
            setKernelWorkingDirectory: setCwd,
            enableAutoreload,
          }
        );

        const initMsgId = `sync-init-${crypto.randomUUID()}`;
        const initWireMsg: JupyterWireMessage = {
          channel: 'shell',
          header: {
            msg_id: initMsgId,
            username: userExecMsg.header?.username || 'jupyter-sync',
            session: userExecMsg.header?.session || crypto.randomUUID(),
            date: new Date().toISOString(),
            msg_type: 'execute_request',
            version: userExecMsg.header?.version || '5.3',
          },
          parent_header: {},
          metadata: {},
          content: {
            code: snippet,
            silent: true,
            store_history: false,
            user_expressions: {},
            allow_stdin: false,
            stop_on_error: false,
          },
          buffers: [],
        };

        await new Promise<void>((resolve) => {
          const timer = setTimeout(() => {
            this.pendingInitWaiters.delete(initMsgId);
            resolve();
          }, 8000);

          this.pendingInitWaiters.set(initMsgId, () => {
            clearTimeout(timer);
            resolve();
          });

          sendRaw(encodeJupyterWsMessage(initWireMsg, this.protocol));
        });

        manager.kernelInitializer.markInitialized(kernelId, remotePaths.relNotebookDir);
        manager.callbacks.onLog(
          `✓ Kernel ${kernelId.slice(0, 8)} initialized: cwd=<jupyter-root>/${
            remotePaths.relNotebookDir || '.'
          }${enableAutoreload ? ' (%autoreload 2 enabled)' : ''}`
        );
      }
    };
  }

  private resolveNotebookForKernel(
    kernelId: string,
    execMsg?: JupyterWireMessage
  ): string | undefined {
    const mapped = this.kernelToNotebookPath.get(kernelId);
    if (mapped) {
      return mapped;
    }

    // Check cellId metadata if present in VS Code's execute_request metadata
    const cellUriStr = execMsg?.metadata?.vscode?.cellId;
    if (typeof cellUriStr === 'string') {
      try {
        const parsedUri = vscode.Uri.parse(cellUriStr);
        if (parsedUri.fsPath) {
          this.kernelToNotebookPath.set(kernelId, parsedUri.fsPath);
          return parsedUri.fsPath;
        }
      } catch {
        // ignore
      }
    }

    if (vscode.window.activeNotebookEditor?.notebook.uri.scheme === 'file') {
      const activePath = vscode.window.activeNotebookEditor.notebook.uri.fsPath;
      this.kernelToNotebookPath.set(kernelId, activePath);
      return activePath;
    }

    const firstNb = vscode.workspace.notebookDocuments.find((d) => d.uri.scheme === 'file');
    if (firstNb) {
      this.kernelToNotebookPath.set(kernelId, firstNb.uri.fsPath);
      return firstNb.uri.fsPath;
    }

    return undefined;
  }

  private findMatchingNotebookFsPath(sessionNameOrPath: string): string | undefined {
    if (!sessionNameOrPath) {
      return vscode.window.activeNotebookEditor?.notebook.uri.fsPath;
    }
    const base = path.posix.basename(sessionNameOrPath.replace(/\\/g, '/'));
    // Strip -jvsc-<uuid> suffix added by ms-toolsai.jupyter
    const prefix = base
      .replace(/-jvsc-[A-Za-z0-9\-]+(?:\.ipynb)?$/, '')
      .replace(/-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}(?:\.ipynb)?$/i, '')
      .replace(/\.ipynb$/i, '');

    for (const nb of vscode.workspace.notebookDocuments) {
      if (nb.uri.scheme !== 'file') {
        continue;
      }
      const nbBase = path.basename(nb.uri.fsPath, '.ipynb');
      if (nbBase === prefix || path.basename(nb.uri.fsPath) === base) {
        return nb.uri.fsPath;
      }
    }

    return vscode.window.activeNotebookEditor?.notebook.uri.fsPath;
  }

  dispose(): void {
    for (const d of this.disposables) {
      d.dispose();
    }
  }
}
