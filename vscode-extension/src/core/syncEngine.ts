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

import * as childProcess from 'child_process';
import * as crypto from 'crypto';
import * as fs from 'fs';
import * as path from 'path';
import * as zlib from 'zlib';
import { isProxySafePath, JupyterClient, normalizeApiPath } from './jupyterClient';

export const MANIFEST_FILENAME = '.workspace-sync-manifest.json';
export const BATCH_TARBALL_FILENAME = '_jupyter_sync_batch.tar.gz';

export const DEFAULT_EXCLUDE_GLOBS: string[] = [
  '.git/**',
  '**/__pycache__/**',
  '**/.ipynb_checkpoints/**',
  '**/node_modules/**',
  '**/.venv/**',
  '**/venv/**',
  '**/.pytest_cache/**',
  '**/.mypy_cache/**',
  '**/dist/**',
  '**/out/**',
  '**/*.vsix',
  '**/*.mp4',
  '**/*.mov',
  '**/.DS_Store',
];

export interface LocalFileEntry {
  relPath: string;
  absPath: string;
  sha256: string;
  size: number;
  mtimeMs: number;
}

export interface ManifestFileRecord {
  sha256: string;
  size: number;
}

export interface SyncManifest {
  version: 1;
  repoName: string;
  updatedAt: string;
  files: Record<string, ManifestFileRecord>;
}

export interface SyncDiffResult {
  toUpload: LocalFileEntry[];
  toDelete: string[];
  unchanged: LocalFileEntry[];
}

export interface SyncProgressEvent {
  phase: 'scan' | 'diff' | 'transfer' | 'complete';
  percent: number;
  batchIndex?: number;
  totalBatches?: number;
  filesCompleted: number;
  totalFiles: number;
  bytesCompleted: number;
  totalBytes: number;
  currentFile?: string;
  message: string;
}

export interface SyncOptions {
  localRoot: string;
  remoteBaseDir: string;
  excludeGlobs?: string[];
  maxFileSizeMB?: number;
  overwriteNotebooks?: boolean;
  maxBatchBytes?: number;
  maxBatchFiles?: number;
  forceTarball?: boolean;
  signal?: AbortSignal;
  onProgress?: (event: SyncProgressEvent) => void;
  onLog?: (line: string) => void;
}

export interface SyncSummary {
  uploadedFiles: number;
  deletedFiles: number;
  unchangedFiles: number;
  skippedLargeFiles: string[];
  skippedNotebooks: number;
  totalBytesTransferred: number;
  durationMs: number;
  batchesUsed: number;
  transport: 'noop' | 'rest' | 'tarball';
}

/**
 * Converts a glob pattern (supporting **, *, ?) into a RegExp matching POSIX relative paths.
 */
export function globToRegExp(glob: string): RegExp {
  const normalized = glob.replace(/\\/g, '/').replace(/^\/+/, '');
  let regexStr = '^';
  let i = 0;
  while (i < normalized.length) {
    if (normalized.startsWith('**/', i)) {
      regexStr += '(?:.+/)?';
      i += 3;
    } else if (normalized.startsWith('/**', i) && i + 3 === normalized.length) {
      regexStr += '(?:/.*)?';
      i += 3;
    } else if (normalized.startsWith('**', i)) {
      regexStr += '.*';
      i += 2;
    } else if (normalized[i] === '*') {
      regexStr += '[^/]*';
      i += 1;
    } else if (normalized[i] === '?') {
      regexStr += '[^/]';
      i += 1;
    } else {
      const ch = normalized[i];
      if ('\\^$+?.()|{}[]'.includes(ch)) {
        regexStr += '\\' + ch;
      } else {
        regexStr += ch;
      }
      i += 1;
    }
  }
  regexStr += '$';
  return new RegExp(regexStr);
}

export function matchesAnyGlob(relPath: string, globs: string[]): boolean {
  const posixPath = relPath.replace(/\\/g, '/').replace(/^\/+/, '');
  if (
    posixPath === MANIFEST_FILENAME ||
    posixPath === BATCH_TARBALL_FILENAME ||
    posixPath.endsWith('/' + MANIFEST_FILENAME) ||
    posixPath.endsWith('/' + BATCH_TARBALL_FILENAME)
  ) {
    return true;
  }
  for (const g of globs) {
    if (globToRegExp(g).test(posixPath)) {
      return true;
    }
  }
  return false;
}

export function computeFileSha256(absPath: string): { sha256: string; size: number; mtimeMs: number } {
  const stat = fs.statSync(absPath);
  const buf = fs.readFileSync(absPath);
  const sha256 = crypto.createHash('sha256').update(buf).digest('hex');
  return { sha256, size: stat.size, mtimeMs: stat.mtimeMs };
}

/**
 * Discovers candidate relative file paths in localRoot using `git ls-files` when available,
 * or a fallback directory walk for non-Git directories.
 */
export function discoverWorkspaceFiles(localRoot: string): string[] {
  try {
    const out = childProcess.execFileSync(
      'git',
      ['ls-files', '--cached', '--others', '--exclude-standard', '-z'],
      {
        cwd: localRoot,
        stdio: ['ignore', 'pipe', 'ignore'],
        maxBuffer: 32 * 1024 * 1024,
      }
    );
    return out
      .toString('utf-8')
      .split('\0')
      .map((s) => s.trim())
      .filter((s) => s.length > 0);
  } catch {
    // Fallback for non-Git directories
    const results: string[] = [];
    const ignoreDirs = new Set([
      '.git',
      '__pycache__',
      '.ipynb_checkpoints',
      '.pytest_cache',
      '.mypy_cache',
      '.venv',
      'venv',
      'node_modules',
      'dist',
      'out',
    ]);
    const walk = (dir: string) => {
      const entries = fs.readdirSync(dir, { withFileTypes: true });
      for (const entry of entries) {
        if (entry.isDirectory()) {
          if (!ignoreDirs.has(entry.name)) {
            walk(path.join(dir, entry.name));
          }
        } else if (entry.isFile()) {
          if (entry.name === '.DS_Store') {
            continue;
          }
          const rel = path.relative(localRoot, path.join(dir, entry.name)).replace(/\\/g, '/');
          results.push(rel);
        }
      }
    };
    walk(localRoot);
    return results.sort();
  }
}

export interface ScanResult {
  files: LocalFileEntry[];
  skippedLargeFiles: string[];
  excludedCount: number;
  totalBytes: number;
}

/**
 * Scans the local workspace, applies exclude globs & maxFileSizeMB, and computes SHA-256 hashes.
 */
export function scanLocalWorkspace(
  localRoot: string,
  excludeGlobs: string[] = DEFAULT_EXCLUDE_GLOBS,
  maxFileSizeMB = 10
): ScanResult {
  const maxBytes = maxFileSizeMB * 1024 * 1024;
  const candidateRelPaths = discoverWorkspaceFiles(localRoot);
  const files: LocalFileEntry[] = [];
  const skippedLargeFiles: string[] = [];
  let excludedCount = 0;
  let totalBytes = 0;

  for (const rawRel of candidateRelPaths) {
    const relPath = rawRel.replace(/\\/g, '/').replace(/^\/+/, '');
    if (matchesAnyGlob(relPath, excludeGlobs)) {
      excludedCount++;
      continue;
    }

    const absPath = path.join(localRoot, relPath);
    let stat: fs.Stats;
    try {
      stat = fs.lstatSync(absPath);
    } catch {
      continue;
    }
    if (!stat.isFile() || stat.isSymbolicLink()) {
      continue;
    }
    if (stat.size > maxBytes) {
      skippedLargeFiles.push(relPath);
      continue;
    }

    try {
      const buf = fs.readFileSync(absPath);
      const sha256 = crypto.createHash('sha256').update(buf).digest('hex');
      files.push({
        relPath,
        absPath,
        sha256,
        size: stat.size,
        mtimeMs: stat.mtimeMs,
      });
      totalBytes += stat.size;
    } catch {
      // Skip unreadable file
    }
  }

  return {
    files,
    skippedLargeFiles,
    excludedCount,
    totalBytes,
  };
}

/**
 * Computes the 3-way differential sync plan between local files and the remote manifest.
 * Untracked remote files (checkpoints, logs, datasets) are never in the manifest and thus
 * never appear in `toDelete`.
 */
export function computeSyncDiff(
  localFiles: LocalFileEntry[],
  remoteManifest: SyncManifest | null,
  options?: {
    overwriteNotebooks?: boolean;
    skippedLargeFiles?: Set<string>;
  }
): SyncDiffResult {
  const overwriteNotebooks = options?.overwriteNotebooks ?? false;
  const skippedLarge = options?.skippedLargeFiles ?? new Set<string>();
  const manifestFiles = remoteManifest?.files ?? {};

  const toUpload: LocalFileEntry[] = [];
  const unchanged: LocalFileEntry[] = [];
  const localMap = new Map<string, LocalFileEntry>();

  for (const file of localFiles) {
    localMap.set(file.relPath, file);
    const prev = manifestFiles[file.relPath];
    const isNotebook = file.relPath.toLowerCase().endsWith('.ipynb');

    if (isNotebook && !overwriteNotebooks && prev) {
      // Notebook was already seeded previously; protect remote outputs/checkpoints
      unchanged.push(file);
      continue;
    }

    if (prev && prev.sha256 === file.sha256) {
      unchanged.push(file);
    } else {
      toUpload.push(file);
    }
  }

  const toDelete: string[] = [];
  for (const trackedPath of Object.keys(manifestFiles)) {
    if (!localMap.has(trackedPath) && !skippedLarge.has(trackedPath)) {
      if (trackedPath.toLowerCase().endsWith('.ipynb') && !overwriteNotebooks) {
        continue;
      }
      toDelete.push(trackedPath);
    }
  }

  return {
    toUpload,
    toDelete,
    unchanged,
  };
}

/**
 * Partitions files to upload into batches of at most `maxBatchBytes` (default 5 MB)
 * and at most `maxBatchFiles` (default 100 files).
 */
export function partitionIntoBatches(
  files: LocalFileEntry[],
  maxBatchBytes = 5 * 1024 * 1024,
  maxBatchFiles = 100
): LocalFileEntry[][] {
  if (files.length === 0) {
    return [];
  }
  const batches: LocalFileEntry[][] = [];
  let currentBatch: LocalFileEntry[] = [];
  let currentBytes = 0;

  for (const file of files) {
    if (
      currentBatch.length > 0 &&
      (currentBatch.length >= maxBatchFiles || currentBytes + file.size > maxBatchBytes)
    ) {
      batches.push(currentBatch);
      currentBatch = [];
      currentBytes = 0;
    }
    currentBatch.push(file);
    currentBytes += file.size;
  }

  if (currentBatch.length > 0) {
    batches.push(currentBatch);
  }
  return batches;
}

/**
 * Builds an in-memory POSIX USTAR + PAX `.tar.gz` buffer from a list of files.
 */
export function createTarGzBuffer(
  entries: Array<{ relPath: string; content: Buffer; mode?: number; mtime?: number }>
): Buffer {
  const blocks: Buffer[] = [];

  const writeOctal = (buf: Buffer, val: number, offset: number, length: number) => {
    const str = val.toString(8).padStart(length - 1, '0') + '\0';
    buf.write(str, offset, length, 'ascii');
  };

  const buildHeader = (
    name: string,
    size: number,
    typeflag: string,
    mode = 0o644,
    mtime = Math.floor(Date.now() / 1000)
  ): Buffer => {
    const header = Buffer.alloc(512, 0);
    let prefix = '';
    let shortName = name;

    if (Buffer.byteLength(name, 'utf-8') > 100) {
      const slashIdx = name.lastIndexOf('/');
      if (slashIdx > 0) {
        const candPrefix = name.slice(0, slashIdx);
        const candName = name.slice(slashIdx + 1);
        if (
          Buffer.byteLength(candPrefix, 'utf-8') <= 155 &&
          Buffer.byteLength(candName, 'utf-8') <= 100
        ) {
          prefix = candPrefix;
          shortName = candName;
        } else {
          shortName = name.slice(0, 100);
        }
      } else {
        shortName = name.slice(0, 100);
      }
    }

    header.write(shortName, 0, 100, 'utf-8');
    writeOctal(header, mode & 0o7777, 100, 8);
    writeOctal(header, 0, 108, 8); // uid
    writeOctal(header, 0, 116, 8); // gid
    writeOctal(header, size, 124, 12);
    writeOctal(header, mtime, 136, 12);
    header.fill(0x20, 148, 156); // checksum placeholder spaces
    header.write(typeflag, 156, 1, 'ascii');
    header.write('ustar\0', 257, 6, 'ascii');
    header.write('00', 263, 2, 'ascii');
    if (prefix) {
      header.write(prefix, 345, 155, 'utf-8');
    }

    let chksum = 0;
    for (let i = 0; i < 512; i++) {
      chksum += header[i];
    }
    const chkStr = chksum.toString(8).padStart(6, '0') + '\0 ';
    header.write(chkStr, 148, 8, 'ascii');
    return header;
  };

  const appendDataBlocks = (data: Buffer) => {
    blocks.push(data);
    const remainder = data.length % 512;
    if (remainder > 0) {
      blocks.push(Buffer.alloc(512 - remainder, 0));
    }
  };

  for (const entry of entries) {
    const cleanName = entry.relPath.replace(/\\/g, '/').replace(/^\/+/, '');
    const nameBytes = Buffer.byteLength(cleanName, 'utf-8');
    const needsPax = nameBytes > 100 || !/^[\x20-\x7e]+$/.test(cleanName);

    if (needsPax) {
      // Emit PAX extended header ('x') for full UTF-8 path
      const lineBody = ` path=${cleanName}\n`;
      let totalLen = Buffer.byteLength(lineBody, 'utf-8') + 3;
      let paxRecord = `${totalLen}${lineBody}`;
      while (Buffer.byteLength(paxRecord, 'utf-8') !== totalLen) {
        totalLen = Buffer.byteLength(lineBody, 'utf-8') + String(totalLen).length;
        paxRecord = `${totalLen}${lineBody}`;
      }
      const paxBuf = Buffer.from(paxRecord, 'utf-8');
      blocks.push(buildHeader('PaxHeader/' + path.posix.basename(cleanName).slice(0, 60), paxBuf.length, 'x'));
      appendDataBlocks(paxBuf);
    }

    blocks.push(
      buildHeader(
        cleanName,
        entry.content.length,
        '0',
        entry.mode ?? 0o644,
        entry.mtime ?? Math.floor(Date.now() / 1000)
      )
    );
    if (entry.content.length > 0) {
      appendDataBlocks(entry.content);
    }
  }

  // Two 512-byte zero blocks mark EOF
  blocks.push(Buffer.alloc(1024, 0));
  const tarBuffer = Buffer.concat(blocks);
  return zlib.gzipSync(tarBuffer, { level: 6 });
}

export class SyncEngine {
  private readonly client: JupyterClient;
  private activeSyncPromise: Promise<SyncSummary> | null = null;
  private pendingFlushHook?: () => Promise<void>;

  constructor(client: JupyterClient) {
    this.client = client;
  }

  setPendingFlushHook(hook: () => Promise<void>): void {
    this.pendingFlushHook = hook;
  }

  /**
   * Used by the Pre-Cell-Execution Barrier and Agent IPC Bridge to guarantee all
   * pending debounced file edits and any active sync have completed before code runs.
   */
  async flushAndWait(): Promise<void> {
    if (this.pendingFlushHook) {
      await this.pendingFlushHook();
    }
    if (this.activeSyncPromise) {
      await this.activeSyncPromise;
    }
  }

  get isSyncInProgress(): boolean {
    return this.activeSyncPromise !== null;
  }

  /**
   * Fetches and parses `.workspace-sync-manifest.json` from `<remoteBaseDir>/.workspace-sync-manifest.json`.
   */
  async fetchRemoteManifest(remoteBaseDir: string): Promise<SyncManifest | null> {
    const cleanBase = normalizeApiPath(remoteBaseDir);
    const manifestPath = cleanBase ? `${cleanBase}/${MANIFEST_FILENAME}` : MANIFEST_FILENAME;
    const model = await this.client.getContents(manifestPath, true);
    if (!model || !model.content) {
      return null;
    }
    try {
      const raw =
        typeof model.content === 'string'
          ? model.format === 'base64'
            ? Buffer.from(model.content, 'base64').toString('utf-8')
            : model.content
          : JSON.stringify(model.content);
      const parsed = JSON.parse(raw) as SyncManifest;
      if (parsed && parsed.version === 1 && typeof parsed.files === 'object') {
        return parsed;
      }
      return null;
    } catch {
      return null;
    }
  }

  /**
   * Saves `.workspace-sync-manifest.json` to the remote server.
   */
  async saveRemoteManifest(remoteBaseDir: string, manifest: SyncManifest): Promise<void> {
    const cleanBase = normalizeApiPath(remoteBaseDir);
    const manifestPath = cleanBase ? `${cleanBase}/${MANIFEST_FILENAME}` : MANIFEST_FILENAME;
    await this.client.putFile(manifestPath, JSON.stringify(manifest, null, 2), {
      overwriteNotebooks: true,
    });
  }

  /**
   * Runs a full 3-way differential sync of `localRoot` to `<jupyter-root>/<remoteBaseDir>`.
   */
  async syncWorkspace(options: SyncOptions): Promise<SyncSummary> {
    const run = async (): Promise<SyncSummary> => {
      const startTime = Date.now();
      const cleanBase = normalizeApiPath(options.remoteBaseDir);
      const excludeGlobs = options.excludeGlobs ?? DEFAULT_EXCLUDE_GLOBS;
      const maxFileSizeMB = options.maxFileSizeMB ?? 10;
      const overwriteNotebooks = options.overwriteNotebooks ?? false;
      const maxBatchBytes = options.maxBatchBytes ?? 5 * 1024 * 1024;
      const maxBatchFiles = options.maxBatchFiles ?? 100;

      const log = (msg: string) => {
        options.onLog?.(msg);
      };

      options.onProgress?.({
        phase: 'scan',
        percent: 0,
        filesCompleted: 0,
        totalFiles: 0,
        bytesCompleted: 0,
        totalBytes: 0,
        message: 'Scanning local repository...',
      });

      const scan = scanLocalWorkspace(options.localRoot, excludeGlobs, maxFileSizeMB);
      const totalMB = (scan.totalBytes / (1024 * 1024)).toFixed(2);
      log(
        `Scanned ${scan.files.length} local files (${totalMB} MB) in '${path.basename(
          options.localRoot
        )}' (${scan.excludedCount} excluded by globs${
          scan.skippedLargeFiles.length > 0
            ? `, ${scan.skippedLargeFiles.length} skipped > ${maxFileSizeMB} MB`
            : ''
        })`
      );

      if (options.signal?.aborted) {
        throw new Error('Sync cancelled');
      }

      options.onProgress?.({
        phase: 'diff',
        percent: 5,
        filesCompleted: 0,
        totalFiles: scan.files.length,
        bytesCompleted: 0,
        totalBytes: scan.totalBytes,
        message: `Checking remote manifest on ${this.client.label}...`,
      });

      const remoteManifest = await this.fetchRemoteManifest(cleanBase);
      const diff = computeSyncDiff(scan.files, remoteManifest, {
        overwriteNotebooks,
        skippedLargeFiles: new Set(scan.skippedLargeFiles),
      });

      const totalChanges = diff.toUpload.length + diff.toDelete.length;
      if (totalChanges === 0) {
        const durationMs = Date.now() - startTime;
        log(
          `✓ Remote workspace is up to date (${diff.unchanged.length} files unchanged, 0 uploaded) [${durationMs}ms]`
        );
        options.onProgress?.({
          phase: 'complete',
          percent: 100,
          filesCompleted: scan.files.length,
          totalFiles: scan.files.length,
          bytesCompleted: scan.totalBytes,
          totalBytes: scan.totalBytes,
          message: `Up to date (${diff.unchanged.length} files)`,
        });
        return {
          uploadedFiles: 0,
          deletedFiles: 0,
          unchangedFiles: diff.unchanged.length,
          skippedLargeFiles: scan.skippedLargeFiles,
          skippedNotebooks: 0,
          totalBytesTransferred: 0,
          durationMs,
          batchesUsed: 0,
          transport: 'noop',
        };
      }

      const workingManifest: SyncManifest = {
        version: 1,
        repoName: cleanBase || path.basename(options.localRoot),
        updatedAt: new Date().toISOString(),
        files: { ...(remoteManifest?.files ?? {}) },
      };

      const hasUnsafePath =
        diff.toUpload.some((f) => !isProxySafePath(f.relPath)) ||
        diff.toDelete.some((p) => !isProxySafePath(p));
      const useTarball =
        options.forceTarball || totalChanges > 10 || hasUnsafePath;

      const bytesToUpload = diff.toUpload.reduce((acc, f) => acc + f.size, 0);

      if (!useTarball) {
        log(
          `Syncing ${diff.toUpload.length} modified/new file(s) and ${diff.toDelete.length} deleted file(s) via REST...`
        );
        const res = await this.syncViaRest(
          cleanBase,
          diff,
          workingManifest,
          overwriteNotebooks,
          bytesToUpload,
          startTime,
          scan.skippedLargeFiles,
          options
        );
        return res;
      }

      // Chunked Tarball Sync
      const batches = partitionIntoBatches(diff.toUpload, maxBatchBytes, maxBatchFiles);
      const totalBatches = Math.max(1, batches.length);
      log(
        `${
          remoteManifest ? 'Differential' : 'Initial'
        } sync: uploading ${diff.toUpload.length} file(s) (${(bytesToUpload / (1024 * 1024)).toFixed(
          2
        )} MB) and deleting ${diff.toDelete.length} file(s) across ${totalBatches} batch(es)`
      );

      if (cleanBase) {
        await this.client.ensureDirectory(cleanBase);
      }

      let filesCompleted = 0;
      let bytesCompleted = 0;
      let uploadedFiles = 0;
      let skippedNotebooks = 0;

      // Handle case where toUpload is empty and only toDelete is non-empty
      const effectiveBatches: LocalFileEntry[][] = batches.length > 0 ? batches : [[]];

      for (let bIdx = 0; bIdx < effectiveBatches.length; bIdx++) {
        if (options.signal?.aborted) {
          throw new Error('Sync cancelled');
        }

        const batch = effectiveBatches[bIdx];
        const isLastBatch = bIdx === effectiveBatches.length - 1;
        const batchDeletes = isLastBatch ? diff.toDelete : [];
        const batchBytes = batch.reduce((s, f) => s + f.size, 0);
        const batchStart = Date.now();

        const percent = Math.min(
          95,
          Math.round(
            10 +
              (85 * (bytesCompleted + Math.max(1, batchBytes * 0.2))) /
                Math.max(1, bytesToUpload)
          )
        );
        options.onProgress?.({
          phase: 'transfer',
          percent,
          batchIndex: bIdx + 1,
          totalBatches: effectiveBatches.length,
          filesCompleted,
          totalFiles: diff.toUpload.length,
          bytesCompleted,
          totalBytes: bytesToUpload,
          currentFile: batch[0]?.relPath,
          message: `Batch ${bIdx + 1}/${effectiveBatches.length} — ${percent}% (${filesCompleted}/${
            diff.toUpload.length
          } files)`,
        });

        try {
          const extractedInfo = await this.uploadAndExtractBatchTarball(
            cleanBase,
            batch,
            batchDeletes,
            workingManifest,
            overwriteNotebooks
          );
          uploadedFiles += extractedInfo.uploadedCount;
          skippedNotebooks += extractedInfo.skippedNotebookCount;
        } catch (tarErr) {
          log(
            `[Fallback] Tarball batch ${bIdx + 1} extraction unavailable (${
              (tarErr as Error).message
            }); falling back to REST upload for batch...`
          );
          for (const file of batch) {
            if (options.signal?.aborted) {
              throw new Error('Sync cancelled');
            }
            const remotePath = cleanBase ? `${cleanBase}/${file.relPath}` : file.relPath;
            const buf = fs.readFileSync(file.absPath);
            const didUpload = await this.client.putFile(remotePath, buf, { overwriteNotebooks });
            if (didUpload) {
              uploadedFiles++;
            } else {
              skippedNotebooks++;
            }
            workingManifest.files[file.relPath] = { sha256: file.sha256, size: file.size };
          }
          for (const delRel of batchDeletes) {
            const remotePath = cleanBase ? `${cleanBase}/${delRel}` : delRel;
            await this.client.deleteFile(remotePath);
            delete workingManifest.files[delRel];
          }
          workingManifest.updatedAt = new Date().toISOString();
          await this.saveRemoteManifest(cleanBase, workingManifest);
        }

        filesCompleted += batch.length;
        bytesCompleted += batchBytes;
        const batchSec = ((Date.now() - batchStart) / 1000).toFixed(1);
        log(
          `✓ Batch ${bIdx + 1}/${effectiveBatches.length} uploaded & extracted (${
            batch.length
          } files, ${(batchBytes / (1024 * 1024)).toFixed(2)} MB) [${batchSec}s]`
        );

        const donePercent = Math.min(
          99,
          Math.round(10 + (89 * Math.max(filesCompleted, 1)) / Math.max(diff.toUpload.length, 1))
        );
        options.onProgress?.({
          phase: 'transfer',
          percent: donePercent,
          batchIndex: bIdx + 1,
          totalBatches: effectiveBatches.length,
          filesCompleted,
          totalFiles: diff.toUpload.length,
          bytesCompleted,
          totalBytes: bytesToUpload,
          currentFile: batch[batch.length - 1]?.relPath,
          message: `Batch ${bIdx + 1}/${effectiveBatches.length} complete (${filesCompleted}/${
            diff.toUpload.length
          } files)`,
        });
      }

      const durationMs = Date.now() - startTime;
      log(
        `✓ Sync complete: ${uploadedFiles} uploaded, ${diff.toDelete.length} deleted, ${
          diff.unchanged.length
        } unchanged (${(bytesCompleted / (1024 * 1024)).toFixed(2)} MB) in ${(
          durationMs / 1000
        ).toFixed(1)}s`
      );

      options.onProgress?.({
        phase: 'complete',
        percent: 100,
        batchIndex: effectiveBatches.length,
        totalBatches: effectiveBatches.length,
        filesCompleted: diff.toUpload.length,
        totalFiles: diff.toUpload.length,
        bytesCompleted,
        totalBytes: bytesToUpload,
        message: `Synced ${uploadedFiles} files in ${(durationMs / 1000).toFixed(1)}s`,
      });

      return {
        uploadedFiles,
        deletedFiles: diff.toDelete.length,
        unchangedFiles: diff.unchanged.length,
        skippedLargeFiles: scan.skippedLargeFiles,
        skippedNotebooks,
        totalBytesTransferred: bytesCompleted,
        durationMs,
        batchesUsed: effectiveBatches.length,
        transport: 'tarball',
      };
    };

    const p = run();
    this.activeSyncPromise = p;
    try {
      return await p;
    } finally {
      if (this.activeSyncPromise === p) {
        this.activeSyncPromise = null;
      }
    }
  }

  private async syncViaRest(
    cleanBase: string,
    diff: SyncDiffResult,
    workingManifest: SyncManifest,
    overwriteNotebooks: boolean,
    bytesToUpload: number,
    startTime: number,
    skippedLargeFiles: string[],
    options: SyncOptions
  ): Promise<SyncSummary> {
    let uploadedFiles = 0;
    let skippedNotebooks = 0;
    let filesCompleted = 0;
    let bytesCompleted = 0;
    const totalOps = diff.toUpload.length + diff.toDelete.length;

    for (const file of diff.toUpload) {
      if (options.signal?.aborted) {
        throw new Error('Sync cancelled');
      }
      const remotePath = cleanBase ? `${cleanBase}/${file.relPath}` : file.relPath;
      const buf = fs.readFileSync(file.absPath);
      const uploaded = await this.client.putFile(remotePath, buf, { overwriteNotebooks });
      if (uploaded) {
        uploadedFiles++;
        options.onLog?.(`  ✓ ${file.relPath}`);
      } else {
        skippedNotebooks++;
        options.onLog?.(`  ↷ ${file.relPath} (skipped existing notebook)`);
      }
      workingManifest.files[file.relPath] = {
        sha256: file.sha256,
        size: file.size,
      };
      filesCompleted++;
      bytesCompleted += file.size;
      const percent = Math.min(98, Math.round((filesCompleted / Math.max(1, totalOps)) * 100));
      options.onProgress?.({
        phase: 'transfer',
        percent,
        filesCompleted,
        totalFiles: totalOps,
        bytesCompleted,
        totalBytes: bytesToUpload,
        currentFile: file.relPath,
        message: `Syncing ${file.relPath} (${filesCompleted}/${totalOps})`,
      });
    }

    for (const delRel of diff.toDelete) {
      if (options.signal?.aborted) {
        throw new Error('Sync cancelled');
      }
      const remotePath = cleanBase ? `${cleanBase}/${delRel}` : delRel;
      await this.client.deleteFile(remotePath);
      delete workingManifest.files[delRel];
      options.onLog?.(`  🗑 ${delRel} (deleted)`);
      filesCompleted++;
    }

    workingManifest.updatedAt = new Date().toISOString();
    await this.saveRemoteManifest(cleanBase, workingManifest);

    const durationMs = Date.now() - startTime;
    options.onProgress?.({
      phase: 'complete',
      percent: 100,
      filesCompleted: totalOps,
      totalFiles: totalOps,
      bytesCompleted,
      totalBytes: bytesToUpload,
      message: `Synced ${uploadedFiles} file(s), deleted ${diff.toDelete.length} file(s)`,
    });

    return {
      uploadedFiles,
      deletedFiles: diff.toDelete.length,
      unchangedFiles: diff.unchanged.length,
      skippedLargeFiles,
      skippedNotebooks,
      totalBytesTransferred: bytesCompleted,
      durationMs,
      batchesUsed: 1,
      transport: 'rest',
    };
  }

  private async uploadAndExtractBatchTarball(
    cleanBase: string,
    batch: LocalFileEntry[],
    batchDeletes: string[],
    workingManifest: SyncManifest,
    overwriteNotebooks: boolean
  ): Promise<{ uploadedCount: number; skippedNotebookCount: number }> {
    // Update workingManifest entries for this batch before bundling
    for (const file of batch) {
      workingManifest.files[file.relPath] = {
        sha256: file.sha256,
        size: file.size,
      };
    }
    for (const delRel of batchDeletes) {
      delete workingManifest.files[delRel];
    }
    workingManifest.updatedAt = new Date().toISOString();

    const tarEntries: Array<{ relPath: string; content: Buffer; mode?: number; mtime?: number }> =
      batch.map((f) => ({
        relPath: f.relPath,
        content: fs.readFileSync(f.absPath),
        mode: 0o644,
        mtime: Math.floor(f.mtimeMs / 1000),
      }));

    // Include the updated .workspace-sync-manifest.json inside the tarball so extraction + manifest update is atomic
    tarEntries.push({
      relPath: MANIFEST_FILENAME,
      content: Buffer.from(JSON.stringify(workingManifest, null, 2), 'utf-8'),
      mode: 0o644,
    });

    const tarGzBuf = createTarGzBuffer(tarEntries);
    const remoteTarPath = cleanBase
      ? `${cleanBase}/${BATCH_TARBALL_FILENAME}`
      : BATCH_TARBALL_FILENAME;

    await this.client.putFile(remoteTarPath, tarGzBuf, {
      overwriteNotebooks: true,
      forceBase64: true,
    });

    // Python script to safely extract _jupyter_sync_batch.tar.gz, protect existing .ipynb files,
    // delete batchDeletes, and remove _jupyter_sync_batch.tar.gz.
    const extractPy = `
import os, sys, tarfile, json

_rel_base = ${JSON.stringify(cleanBase)}
_overwrite_nb = ${overwriteNotebooks ? 'True' : 'False'}
_to_delete = ${JSON.stringify(batchDeletes)}
_tar_name = ${JSON.stringify(BATCH_TARBALL_FILENAME)}

_candidates = []
if os.environ.get("JUPYTER_SERVER_ROOT"):
    _candidates.append(os.environ["JUPYTER_SERVER_ROOT"])
_candidates.extend([os.getcwd(), os.path.expanduser("~")])

_target_dir = None
for _c in _candidates:
    _cand = os.path.abspath(os.path.join(_c, _rel_base)) if _rel_base else os.path.abspath(_c)
    if os.path.exists(os.path.join(_cand, _tar_name)):
        _target_dir = _cand
        break

if not _target_dir:
    raise RuntimeError("Could not locate uploaded tarball " + _tar_name)

_tar_path = os.path.join(_target_dir, _tar_name)
_uploaded = 0
_skipped_nb = 0

try:
    with tarfile.open(_tar_path, "r:gz") as _tf:
        for _member in _tf.getmembers():
            _norm = os.path.normpath(_member.name).lstrip("/\\\\")
            if _norm.startswith("..") or "/../" in _norm:
                continue
            _dest = os.path.abspath(os.path.join(_target_dir, _norm))
            if not (_dest == _target_dir or _dest.startswith(_target_dir + os.sep)):
                continue
            if _norm.lower().endswith(".ipynb") and not _overwrite_nb and os.path.exists(_dest):
                _skipped_nb += 1
                continue
            os.makedirs(os.path.dirname(_dest), exist_ok=True)
            _src = _tf.extractfile(_member)
            if _src is not None:
                with open(_dest, "wb") as _out:
                    _out.write(_src.read())
                if _norm != ${JSON.stringify(MANIFEST_FILENAME)}:
                    _uploaded += 1
    for _del_rel in _to_delete:
        _norm_del = os.path.normpath(_del_rel).lstrip("/\\\\")
        if _norm_del.startswith(".."):
            continue
        _del_dest = os.path.abspath(os.path.join(_target_dir, _norm_del))
        if _del_dest.startswith(_target_dir + os.sep) and os.path.isfile(_del_dest):
            try:
                os.remove(_del_dest)
            except OSError:
                pass
finally:
    if os.path.exists(_tar_path):
        try:
            os.remove(_tar_path)
        except OSError:
            pass

print("__JSYNC_TAR_STATS__:" + json.dumps({"uploaded": _uploaded, "skippedNb": _skipped_nb}))
`;

    const b64Py = Buffer.from(extractPy, 'utf-8').toString('base64');
    const shellCmd = `python3 -c "import base64; exec(base64.b64decode('${b64Py}').decode('utf-8'))"`;
    const shellRes = await this.client.executeShellCommand(shellCmd, { timeoutMs: 60000 });
    if (shellRes.exitCode !== 0) {
      throw new Error(
        `Remote tarball extraction exited with code ${shellRes.exitCode}: ${
          shellRes.stdout || shellRes.stderr
        }`
      );
    }

    const match = shellRes.stdout.match(/__JSYNC_TAR_STATS__:(\{.*\})/);
    if (match) {
      try {
        const stats = JSON.parse(match[1]) as { uploaded: number; skippedNb: number };
        return {
          uploadedCount: stats.uploaded,
          skippedNotebookCount: stats.skippedNb,
        };
      } catch {
        // fallback below
      }
    }

    return {
      uploadedCount: batch.length,
      skippedNotebookCount: 0,
    };
  }

  /**
   * Removes all previously manifest-tracked files on the remote server and re-uploads
   * the full workspace from scratch (`jupyterSync.fullResync`).
   */
  async fullResyncWorkspace(options: SyncOptions): Promise<SyncSummary> {
    const cleanBase = normalizeApiPath(options.remoteBaseDir);
    const existingManifest = await this.fetchRemoteManifest(cleanBase);
    if (existingManifest && Object.keys(existingManifest.files).length > 0) {
      const trackedFiles = Object.keys(existingManifest.files).filter(
        (f) => (options.overwriteNotebooks ?? false) || !f.toLowerCase().endsWith('.ipynb')
      );
      options.onLog?.(
        `Cleaning ${trackedFiles.length} previously tracked file(s) on remote server...`
      );
      const cleanPy = `
import os, json
_rel_base = ${JSON.stringify(cleanBase)}
_overwrite_nb = ${options.overwriteNotebooks ? 'True' : 'False'}
_root = os.environ.get("JUPYTER_SERVER_ROOT") or os.getcwd()
_base = os.path.abspath(os.path.join(_root, _rel_base)) if _rel_base else os.path.abspath(_root)
_m = os.path.join(_base, ${JSON.stringify(MANIFEST_FILENAME)})
if os.path.isfile(_m):
    try:
        with open(_m, "r", encoding="utf-8") as _mf:
            _data = json.load(_mf)
        for _f in list(_data.get("files", {}).keys()):
            if _f.lower().endswith(".ipynb") and not _overwrite_nb:
                continue
            _p = os.path.abspath(os.path.join(_base, _f))
            if _p.startswith(_base + os.sep) and os.path.isfile(_p):
                try:
                    os.remove(_p)
                except OSError:
                    pass
    except Exception:
        pass
    try:
        os.remove(_m)
    except OSError:
        pass
`;
      const b64Py = Buffer.from(cleanPy, 'utf-8').toString('base64');
      await this.client
        .executeShellCommand(
          `python3 -c "import base64; exec(base64.b64decode('${b64Py}').decode('utf-8'))"`
        )
        .catch(async () => {
          const manifestPath = cleanBase ? `${cleanBase}/${MANIFEST_FILENAME}` : MANIFEST_FILENAME;
          await this.client.deleteFile(manifestPath);
        });
    }
    this.client.clearDirectoryCache();
    return await this.syncWorkspace({ ...options, forceTarball: true });
  }
}
