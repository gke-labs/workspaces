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
import * as vscode from 'vscode';
import { isProxySafePath, JupyterClient, normalizeApiPath } from './core/jupyterClient';
import {
  computeFileSha256,
  DEFAULT_EXCLUDE_GLOBS,
  matchesAnyGlob,
  SyncEngine,
  SyncManifest,
} from './core/syncEngine';

interface PendingFileEvent {
  type: 'upsert' | 'delete';
  relPath: string;
  absPath: string;
}

export interface FileWatcherConfig {
  workspaceRoot: string;
  getRemoteBaseDir: () => string;
  getAutoSyncOnSave: () => boolean;
  getOverwriteNotebooks: () => boolean;
  getExcludeGlobs: () => string[];
  getMaxFileSizeMB: () => number;
  onSyncStart: (label: string) => void;
  onSyncEnd: (summaryText?: string) => void;
  onLog: (msg: string) => void;
  onTriggerBulkSync: () => Promise<void>;
}

export class WorkspaceFileWatcher implements vscode.Disposable {
  private readonly disposables: vscode.Disposable[] = [];
  private readonly pendingEvents = new Map<string, PendingFileEvent>();
  private debounceTimer: NodeJS.Timeout | null = null;
  private activeFlushPromise: Promise<void> | null = null;
  private cachedManifest: SyncManifest | null = null;

  constructor(
    private readonly client: JupyterClient,
    private readonly syncEngine: SyncEngine,
    private readonly config: FileWatcherConfig
  ) {
    // Register flush hook on SyncEngine so Pre-Cell-Execution Barrier and CLI Bridge
    // can synchronously flush any pending file changes before running code.
    this.syncEngine.setPendingFlushHook(() => this.flushPending());

    const pattern = new vscode.RelativePattern(config.workspaceRoot, '**/*');
    const watcher = vscode.workspace.createFileSystemWatcher(pattern);

    this.disposables.push(
      watcher,
      watcher.onDidCreate((uri) => this.enqueueUri(uri, 'upsert')),
      watcher.onDidChange((uri) => this.enqueueUri(uri, 'upsert')),
      watcher.onDidDelete((uri) => this.enqueueUri(uri, 'delete')),
      vscode.workspace.onDidSaveTextDocument((doc) => {
        if (doc.uri.scheme === 'file') {
          this.enqueueUri(doc.uri, 'upsert');
        }
      }),
      vscode.workspace.onDidRenameFiles((event) => {
        for (const f of event.files) {
          this.enqueueUri(f.oldUri, 'delete');
          this.enqueueUri(f.newUri, 'upsert');
        }
      })
    );
  }

  setCachedManifest(manifest: SyncManifest | null): void {
    this.cachedManifest = manifest;
  }

  private enqueueUri(uri: vscode.Uri, type: 'upsert' | 'delete'): void {
    if (!this.config.getAutoSyncOnSave()) {
      return;
    }
    if (uri.scheme !== 'file') {
      return;
    }

    const absWorkspace = path.resolve(this.config.workspaceRoot);
    const absFile = path.resolve(uri.fsPath);
    if (!absFile.startsWith(absWorkspace + path.sep)) {
      return;
    }

    const relPath = normalizeApiPath(path.relative(absWorkspace, absFile));
    if (!relPath) {
      return;
    }

    const excludeGlobs = this.config.getExcludeGlobs() || DEFAULT_EXCLUDE_GLOBS;
    if (matchesAnyGlob(relPath, excludeGlobs)) {
      return;
    }

    const isNotebook = relPath.toLowerCase().endsWith('.ipynb');
    if (isNotebook && !this.config.getOverwriteNotebooks()) {
      return;
    }

    if (type === 'upsert') {
      try {
        const stat = fs.lstatSync(absFile);
        if (!stat.isFile() || stat.isSymbolicLink()) {
          return;
        }
        const maxBytes = this.config.getMaxFileSizeMB() * 1024 * 1024;
        if (stat.size > maxBytes) {
          return;
        }
      } catch {
        return;
      }
    }

    this.pendingEvents.set(relPath, {
      type,
      relPath,
      absPath: absFile,
    });

    if (this.debounceTimer) {
      clearTimeout(this.debounceTimer);
    }
    this.debounceTimer = setTimeout(() => {
      this.debounceTimer = null;
      this.flushPending().catch((err) => {
        this.config.onLog(`[Watch Error] ${(err as Error).message}`);
      });
    }, 150);
  }

  /**
   * Immediately flushes any debounced file events and awaits completion.
   */
  async flushPending(): Promise<void> {
    if (this.debounceTimer) {
      clearTimeout(this.debounceTimer);
      this.debounceTimer = null;
    }

    if (this.pendingEvents.size === 0) {
      if (this.activeFlushPromise) {
        await this.activeFlushPromise;
      }
      return;
    }

    const batch = Array.from(this.pendingEvents.values());
    this.pendingEvents.clear();

    const runFlush = async () => {
      // Wait if a previous flush is still running
      if (this.activeFlushPromise) {
        await this.activeFlushPromise.catch(() => {});
      }

      const hasUnsafePath = batch.some((e) => !isProxySafePath(e.relPath));
      if (batch.length > 10 || hasUnsafePath) {
        this.config.onLog(
          `Coalesced ${batch.length} file changes — switching to differential batch sync...`
        );
        await this.config.onTriggerBulkSync();
        return;
      }

      const remoteBaseDir = normalizeApiPath(this.config.getRemoteBaseDir());
      const overwriteNotebooks = this.config.getOverwriteNotebooks();
      const firstFileLabel =
        batch.length === 1 ? path.posix.basename(batch[0].relPath) : `${batch.length} files`;
      this.config.onSyncStart(firstFileLabel);

      try {
        if (!this.cachedManifest) {
          this.cachedManifest = (await this.syncEngine.fetchRemoteManifest(remoteBaseDir)) || {
            version: 1,
            repoName: remoteBaseDir || path.basename(this.config.workspaceRoot),
            updatedAt: new Date().toISOString(),
            files: {},
          };
        }

        let changedManifest = false;
        for (const ev of batch) {
          const remotePath = remoteBaseDir ? `${remoteBaseDir}/${ev.relPath}` : ev.relPath;
          if (ev.type === 'delete') {
            await this.client.deleteFile(remotePath);
            if (this.cachedManifest.files[ev.relPath]) {
              delete this.cachedManifest.files[ev.relPath];
              changedManifest = true;
            }
            this.config.onLog(`  🗑 Synced delete: ${ev.relPath}`);
          } else {
            if (!fs.existsSync(ev.absPath)) {
              continue;
            }
            const { sha256, size } = computeFileSha256(ev.absPath);
            if (this.cachedManifest.files[ev.relPath]?.sha256 === sha256) {
              continue;
            }
            const content = fs.readFileSync(ev.absPath);
            const uploaded = await this.client.putFile(remotePath, content, {
              overwriteNotebooks,
            });
            if (uploaded) {
              this.cachedManifest.files[ev.relPath] = { sha256, size };
              changedManifest = true;
              this.config.onLog(`  ✓ Synced save: ${ev.relPath}`);
            }
          }
        }

        if (changedManifest) {
          this.cachedManifest.updatedAt = new Date().toISOString();
          await this.syncEngine.saveRemoteManifest(remoteBaseDir, this.cachedManifest);
        }
      } finally {
        this.config.onSyncEnd();
      }
    };

    const p = runFlush();
    this.activeFlushPromise = p;
    try {
      await p;
    } finally {
      if (this.activeFlushPromise === p) {
        this.activeFlushPromise = null;
      }
    }
  }

  dispose(): void {
    if (this.debounceTimer) {
      clearTimeout(this.debounceTimer);
      this.debounceTimer = null;
    }
    for (const d of this.disposables) {
      d.dispose();
    }
  }
}
