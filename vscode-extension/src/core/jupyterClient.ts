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
import WebSocket from 'ws';
import { parseJupyterUrl, ParsedJupyterUrl } from './urlParser';

export interface JupyterContentsModel {
  name: string;
  path: string;
  type: 'file' | 'directory' | 'notebook';
  writable: boolean;
  created: string;
  last_modified: string;
  mimetype: string | null;
  content: any;
  format: 'text' | 'base64' | 'json' | null;
  size?: number | null;
}

export interface JupyterKernelModel {
  id: string;
  name: string;
  last_activity: string;
  execution_state: 'starting' | 'idle' | 'busy' | 'restarting' | 'dead' | string;
  connections: number;
}

export interface JupyterSessionModel {
  id: string;
  path: string;
  name: string;
  type: string;
  kernel: JupyterKernelModel;
}

export interface KernelExecutionResult {
  status: 'ok' | 'error' | 'abort';
  stdout: string;
  stderr: string;
  results: string[];
  ename?: string;
  evalue?: string;
  traceback?: string[];
  executionCount?: number | null;
}

export interface ShellExecutionResult {
  exitCode: number;
  stdout: string;
  stderr: string;
}

export const V1_KERNEL_WS_PROTOCOL = 'v1.kernel.websocket.jupyter.org';

export interface JupyterWireMessage {
  channel: string;
  header: {
    msg_id: string;
    msg_type: string;
    username?: string;
    session?: string;
    date?: string;
    version?: string;
    [key: string]: any;
  };
  parent_header: Record<string, any>;
  metadata: Record<string, any>;
  content: Record<string, any>;
  buffers?: DataView[];
}

/**
 * Decodes a Jupyter WebSocket message in either default JSON format or
 * `v1.kernel.websocket.jupyter.org` binary format.
 */
export function decodeJupyterWsMessage(
  data: unknown,
  protocol = ''
): JupyterWireMessage | null {
  try {
    if (protocol === V1_KERNEL_WS_PROTOCOL && typeof data !== 'string') {
      let u8: Uint8Array;
      if (Buffer.isBuffer(data)) {
        u8 = new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
      } else if (data instanceof ArrayBuffer) {
        u8 = new Uint8Array(data);
      } else if (ArrayBuffer.isView(data)) {
        u8 = new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
      } else if (Array.isArray(data)) {
        const merged = Buffer.concat(data);
        u8 = new Uint8Array(merged.buffer, merged.byteOffset, merged.byteLength);
      } else {
        return null;
      }

      const view = new DataView(u8.buffer, u8.byteOffset, u8.byteLength);
      const count = Number(view.getBigUint64(0, true));
      const offsets: number[] = [];
      for (let i = 0; i < count; i++) {
        offsets.push(Number(view.getBigUint64(8 * (i + 1), true)));
      }
      const decoder = new TextDecoder('utf-8');
      const channel = decoder.decode(u8.subarray(offsets[0], offsets[1]));
      const header = JSON.parse(decoder.decode(u8.subarray(offsets[1], offsets[2])));
      const parentHeaderText = decoder.decode(u8.subarray(offsets[2], offsets[3]));
      const parent_header = parentHeaderText ? JSON.parse(parentHeaderText) : {};
      const metadataText = decoder.decode(u8.subarray(offsets[3], offsets[4]));
      const metadata = metadataText ? JSON.parse(metadataText) : {};
      const contentText = decoder.decode(u8.subarray(offsets[4], offsets[5]));
      const content = contentText ? JSON.parse(contentText) : {};
      const buffers: DataView[] = [];
      for (let i = 5; i < offsets.length - 1; i++) {
        buffers.push(
          new DataView(u8.buffer, u8.byteOffset + offsets[i], offsets[i + 1] - offsets[i])
        );
      }
      return {
        channel,
        header,
        parent_header,
        metadata,
        content,
        buffers,
      };
    }

    const text =
      typeof data === 'string'
        ? data
        : Buffer.isBuffer(data)
        ? data.toString('utf-8')
        : data instanceof ArrayBuffer
        ? Buffer.from(data).toString('utf-8')
        : ArrayBuffer.isView(data)
        ? Buffer.from(data.buffer, data.byteOffset, data.byteLength).toString('utf-8')
        : null;
    if (!text) {
      return null;
    }
    const parsed = JSON.parse(text);
    if (parsed && typeof parsed.header === 'object') {
      return {
        channel: parsed.channel || 'shell',
        header: parsed.header,
        parent_header: parsed.parent_header || {},
        metadata: parsed.metadata || {},
        content: parsed.content || {},
        buffers: [],
      };
    }
    return null;
  } catch {
    return null;
  }
}

/**
 * Encodes a Jupyter WebSocket message in either default JSON format or
 * `v1.kernel.websocket.jupyter.org` binary format.
 */
export function encodeJupyterWsMessage(
  msg: JupyterWireMessage,
  protocol = ''
): string | ArrayBuffer {
  if (protocol === V1_KERNEL_WS_PROTOCOL) {
    const encoder = new TextEncoder();
    const channelBytes = encoder.encode(msg.channel || 'shell');
    const headerBytes = encoder.encode(JSON.stringify(msg.header));
    const parentBytes = encoder.encode(
      msg.parent_header == null ? '{}' : JSON.stringify(msg.parent_header)
    );
    const metadataBytes = encoder.encode(JSON.stringify(msg.metadata || {}));
    const contentBytes = encoder.encode(JSON.stringify(msg.content || {}));
    const buffers = msg.buffers || [];

    const offsetCount = 5 + buffers.length + 1;
    const offsets: number[] = [];
    offsets.push(8 * (1 + offsetCount));
    offsets.push(offsets[offsets.length - 1] + channelBytes.byteLength);
    offsets.push(offsets[offsets.length - 1] + headerBytes.byteLength);
    offsets.push(offsets[offsets.length - 1] + parentBytes.byteLength);
    offsets.push(offsets[offsets.length - 1] + metadataBytes.byteLength);
    offsets.push(offsets[offsets.length - 1] + contentBytes.byteLength);

    let extraBufBytes = 0;
    for (const b of buffers) {
      offsets.push(offsets[offsets.length - 1] + b.byteLength);
      extraBufBytes += b.byteLength;
    }

    const totalTextBytes =
      channelBytes.byteLength +
      headerBytes.byteLength +
      parentBytes.byteLength +
      metadataBytes.byteLength +
      contentBytes.byteLength;
    const out = new Uint8Array(8 * (1 + offsetCount) + totalTextBytes + extraBufBytes);
    const view = new DataView(out.buffer);
    view.setBigUint64(0, BigInt(offsetCount), true);
    for (let i = 0; i < offsets.length; i++) {
      view.setBigUint64(8 * (i + 1), BigInt(offsets[i]), true);
    }

    out.set(channelBytes, offsets[0]);
    out.set(headerBytes, offsets[1]);
    out.set(parentBytes, offsets[2]);
    out.set(metadataBytes, offsets[3]);
    out.set(contentBytes, offsets[4]);
    for (let i = 0; i < buffers.length; i++) {
      const b = buffers[i];
      out.set(new Uint8Array(b.buffer, b.byteOffset, b.byteLength), offsets[5 + i]);
    }
    return out.buffer;
  }

  return JSON.stringify({
    channel: msg.channel || 'shell',
    header: msg.header,
    parent_header: msg.parent_header || {},
    metadata: msg.metadata || {},
    content: msg.content || {},
    buffers: [],
  });
}

export class JupyterAuthError extends Error {
  public readonly statusCode: number;
  constructor(message: string, statusCode: number) {
    super(message);
    this.name = 'JupyterAuthError';
    this.statusCode = statusCode;
  }
}

/**
 * Normalizes a relative path for Jupyter /api/contents/<path>, ensuring
 * no leading/trailing slashes and no duplicate '//' segments (required by GKE proxy safePath).
 */
export function normalizeApiPath(relPath: string): string {
  return relPath
    .replace(/\\/g, '/')
    .replace(/\/+/g, '/')
    .replace(/^\/+/, '')
    .replace(/\/+$/, '');
}

/**
 * Checks whether a relative path is safe to send directly in a URL path through
 * strict reverse proxies (like GKE's safePath, which rejects '%' or special chars).
 */
export function isProxySafePath(relPath: string): boolean {
  const clean = normalizeApiPath(relPath);
  if (!clean) {
    return true;
  }
  const segments = clean.split('/');
  for (const seg of segments) {
    if (!seg || seg === '.' || seg === '..') {
      return false;
    }
    if (!/^[A-Za-z0-9._\-]+$/.test(seg)) {
      return false;
    }
  }
  return true;
}

/**
 * Token-bucket rate and concurrency limiter for Jupyter REST API calls.
 */
class RateLimiter {
  private active = 0;
  private readonly maxConcurrency: number;
  private readonly minIntervalMs: number;
  private lastDispatchTime = 0;
  private readonly queue: Array<() => void> = [];

  constructor(maxConcurrency = 4, maxRequestsPerSec = 20) {
    this.maxConcurrency = maxConcurrency;
    this.minIntervalMs = Math.ceil(1000 / maxRequestsPerSec);
  }

  async schedule<T>(fn: () => Promise<T>): Promise<T> {
    await this.acquire();
    try {
      return await fn();
    } finally {
      this.release();
    }
  }

  private acquire(): Promise<void> {
    return new Promise<void>((resolve) => {
      this.queue.push(resolve);
      this.drain();
    });
  }

  private release(): void {
    this.active--;
    this.drain();
  }

  private drain(): void {
    if (this.active >= this.maxConcurrency || this.queue.length === 0) {
      return;
    }
    const now = Date.now();
    const waitMs = Math.max(0, this.lastDispatchTime + this.minIntervalMs - now);
    if (waitMs > 0) {
      setTimeout(() => this.drain(), waitMs);
      return;
    }
    const next = this.queue.shift();
    if (!next) {
      return;
    }
    this.active++;
    this.lastDispatchTime = Date.now();
    next();
  }
}

export class JupyterClient {
  private parsedUrl: ParsedJupyterUrl;
  private xsrfToken?: string;
  private readonly createdDirs = new Set<string>();
  private readonly limiter = new RateLimiter(4, 25);
  private readonly onAuthErrorCallbacks: Array<(err: JupyterAuthError) => void> = [];

  constructor(rawUrlOrParsed: string | ParsedJupyterUrl, explicitToken?: string) {
    if (typeof rawUrlOrParsed === 'string') {
      this.parsedUrl = parseJupyterUrl(rawUrlOrParsed, explicitToken);
    } else {
      this.parsedUrl = {
        ...rawUrlOrParsed,
        token: explicitToken ?? rawUrlOrParsed.token,
      };
    }
  }

  get baseUrl(): string {
    return this.parsedUrl.baseUrl;
  }

  get wsBaseUrl(): string {
    return this.parsedUrl.wsBaseUrl;
  }

  get origin(): string {
    return this.parsedUrl.origin;
  }

  get token(): string {
    return this.parsedUrl.token;
  }

  get serverId(): string {
    return this.parsedUrl.id;
  }

  get label(): string {
    return this.parsedUrl.label;
  }

  get parsed(): ParsedJupyterUrl {
    return this.parsedUrl;
  }

  updateToken(newTokenOrUrl: string): void {
    const trimmed = newTokenOrUrl.trim();
    if (trimmed.startsWith('http://') || trimmed.startsWith('https://')) {
      const reparsed = parseJupyterUrl(trimmed);
      this.parsedUrl = {
        ...this.parsedUrl,
        token: reparsed.token,
      };
    } else {
      this.parsedUrl = {
        ...this.parsedUrl,
        token: trimmed,
      };
    }
  }

  onAuthError(cb: (err: JupyterAuthError) => void): void {
    this.onAuthErrorCallbacks.push(cb);
  }

  clearDirectoryCache(): void {
    this.createdDirs.clear();
  }

  /**
   * Builds HTTP headers required by Jupyter Server and reverse proxies (GKE desktopHandler).
   */
  buildHeaders(extraHeaders?: Record<string, string>): Record<string, string> {
    const headers: Record<string, string> = {
      Origin: this.parsedUrl.origin,
      ...(extraHeaders || {}),
    };
    if (this.parsedUrl.token) {
      headers['Authorization'] = `token ${this.parsedUrl.token}`;
    }
    if (this.xsrfToken) {
      headers['X-XSRFToken'] = this.xsrfToken;
      headers['Cookie'] = `_xsrf=${this.xsrfToken}`;
    }
    return headers;
  }

  /**
   * Executes an authenticated HTTP request against the Jupyter Server with retry on 429.
   */
  async request(
    apiPath: string,
    init: RequestInit = {},
    timeoutMs = 30000
  ): Promise<Response> {
    const cleanEndpoint = apiPath.startsWith('/') ? apiPath : `/${apiPath}`;
    // Split path and query so we only normalize slashes in the path part
    const qIdx = cleanEndpoint.indexOf('?');
    const pathPart = qIdx >= 0 ? cleanEndpoint.slice(0, qIdx) : cleanEndpoint;
    const queryPart = qIdx >= 0 ? cleanEndpoint.slice(qIdx) : '';
    const normalizedPath = pathPart.replace(/\/+/g, '/');
    const fullUrl = `${this.parsedUrl.baseUrl}${normalizedPath}${queryPart}`;

    let attempt = 0;
    while (true) {
      attempt++;
      const controller = new AbortController();
      const timer = setTimeout(() => controller.abort(), timeoutMs);
      try {
        const headers = this.buildHeaders(
          init.headers ? (init.headers as Record<string, string>) : undefined
        );
        const resp = await this.limiter.schedule(() =>
          fetch(fullUrl, {
            ...init,
            headers,
            signal: init.signal || controller.signal,
          })
        );

        // Capture _xsrf cookie if returned by server
        const setCookie = resp.headers.get('set-cookie');
        if (setCookie) {
          const match = setCookie.match(/\b_xsrf=([^;,\s]+)/);
          if (match && match[1]) {
            this.xsrfToken = match[1];
          }
        }

        if (resp.status === 429 && attempt <= 4) {
          const backoffMs = Math.min(2000, 200 * Math.pow(2, attempt - 1));
          await new Promise((r) => setTimeout(r, backoffMs));
          continue;
        }

        if (resp.status === 401 || resp.status === 403) {
          const authErr = new JupyterAuthError(
            `Authentication failed (${resp.status}) at ${this.parsedUrl.baseUrl}. Token may be invalid or expired.`,
            resp.status
          );
          for (const cb of this.onAuthErrorCallbacks) {
            try {
              cb(authErr);
            } catch {
              // ignore callback error
            }
          }
          throw authErr;
        }

        return resp;
      } finally {
        clearTimeout(timer);
      }
    }
  }

  /**
   * Verifies connectivity & authentication against GET /api/contents?content=0.
   */
  async verifyConnection(): Promise<void> {
    const resp = await this.request('/api/contents?content=0', { method: 'GET' }, 15000);
    if (!resp.ok) {
      const text = await resp.text().catch(() => '');
      throw new Error(
        `Failed to connect to Jupyter Server at ${this.parsedUrl.baseUrl} (HTTP ${resp.status}): ${text}`
      );
    }
  }

  /**
   * Queries GET /api/kernelspecs.
   */
  async getKernelSpecs(): Promise<Record<string, any>> {
    const resp = await this.request('/api/kernelspecs', { method: 'GET' }, 15000);
    if (!resp.ok) {
      throw new Error(`GET /api/kernelspecs failed with HTTP ${resp.status}`);
    }
    return (await resp.json()) as Record<string, any>;
  }

  /**
   * Lists running kernels via GET /api/kernels.
   */
  async listKernels(): Promise<JupyterKernelModel[]> {
    const resp = await this.request('/api/kernels', { method: 'GET' }, 15000);
    if (!resp.ok) {
      throw new Error(`GET /api/kernels failed with HTTP ${resp.status}`);
    }
    return (await resp.json()) as JupyterKernelModel[];
  }

  /**
   * Lists active sessions via GET /api/sessions.
   */
  async listSessions(): Promise<JupyterSessionModel[]> {
    const resp = await this.request('/api/sessions', { method: 'GET' }, 15000);
    if (!resp.ok) {
      throw new Error(`GET /api/sessions failed with HTTP ${resp.status}`);
    }
    return (await resp.json()) as JupyterSessionModel[];
  }

  /**
   * Starts a new kernel via POST /api/kernels.
   */
  async startKernel(name = 'python3', cwdPath?: string): Promise<JupyterKernelModel> {
    const body: Record<string, any> = { name };
    if (cwdPath) {
      body.path = normalizeApiPath(cwdPath);
    }
    const resp = await this.request(
      '/api/kernels',
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      },
      30000
    );
    if (!resp.ok) {
      const text = await resp.text().catch(() => '');
      throw new Error(`POST /api/kernels failed (HTTP ${resp.status}): ${text}`);
    }
    return (await resp.json()) as JupyterKernelModel;
  }

  /**
   * Shuts down a kernel via DELETE /api/kernels/<id>.
   */
  async shutdownKernel(kernelId: string): Promise<void> {
    const resp = await this.request(`/api/kernels/${kernelId}`, { method: 'DELETE' }, 15000);
    if (resp.status !== 204 && resp.status !== 404 && !resp.ok) {
      throw new Error(`DELETE /api/kernels/${kernelId} failed (HTTP ${resp.status})`);
    }
  }

  /**
   * Restarts a kernel via POST /api/kernels/<id>/restart.
   */
  async restartKernel(kernelId: string): Promise<void> {
    const resp = await this.request(
      `/api/kernels/${kernelId}/restart`,
      { method: 'POST' },
      30000
    );
    if (!resp.ok) {
      const text = await resp.text().catch(() => '');
      throw new Error(`POST /api/kernels/${kernelId}/restart failed (HTTP ${resp.status}): ${text}`);
    }
  }

  /**
   * Fetches file or directory metadata/content via GET /api/contents/<path>.
   * Returns null if 404 Not Found.
   */
  async getContents(
    remotePath: string,
    includeContent = true
  ): Promise<JupyterContentsModel | null> {
    const clean = normalizeApiPath(remotePath);
    const endpoint = clean
      ? `/api/contents/${clean}?content=${includeContent ? '1' : '0'}`
      : `/api/contents?content=${includeContent ? '1' : '0'}`;
    const resp = await this.request(endpoint, { method: 'GET' }, 20000);
    if (resp.status === 404) {
      return null;
    }
    if (!resp.ok) {
      const text = await resp.text().catch(() => '');
      throw new Error(`GET ${endpoint} failed (HTTP ${resp.status}): ${text}`);
    }
    return (await resp.json()) as JupyterContentsModel;
  }

  /**
   * Checks whether a remote file or directory exists.
   */
  async fileExists(remotePath: string): Promise<boolean> {
    const model = await this.getContents(remotePath, false);
    return model !== null;
  }

  /**
   * Ensures a remote directory and all its parent directories exist.
   */
  async ensureDirectory(remoteDir: string): Promise<void> {
    const clean = normalizeApiPath(remoteDir);
    if (!clean || clean === '.' || this.createdDirs.has(clean)) {
      return;
    }

    const lastSlash = clean.lastIndexOf('/');
    if (lastSlash > 0) {
      const parent = clean.slice(0, lastSlash);
      await this.ensureDirectory(parent);
    }

    if (this.createdDirs.has(clean)) {
      return;
    }

    const existing = await this.getContents(clean, false);
    if (existing && existing.type === 'directory') {
      this.createdDirs.add(clean);
      return;
    }

    const resp = await this.request(
      `/api/contents/${clean}`,
      {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ type: 'directory' }),
      },
      20000
    );

    if (resp.ok || resp.status === 400 || resp.status === 409) {
      this.createdDirs.add(clean);
    } else {
      const text = await resp.text().catch(() => '');
      throw new Error(`Failed to create remote directory '${clean}' (HTTP ${resp.status}): ${text}`);
    }
  }

  /**
   * Uploads a single file to the remote Jupyter Server via PUT /api/contents/<path>.
   * Returns true if uploaded, or false if skipped (e.g., existing .ipynb when overwriteNotebooks=false).
   */
  async putFile(
    remotePath: string,
    rawContent: Buffer | string,
    options?: {
      overwriteNotebooks?: boolean;
      forceBase64?: boolean;
    }
  ): Promise<boolean> {
    const clean = normalizeApiPath(remotePath);
    const lastSlash = clean.lastIndexOf('/');
    if (lastSlash > 0) {
      await this.ensureDirectory(clean.slice(0, lastSlash));
    }

    const isNotebook = clean.toLowerCase().endsWith('.ipynb');
    const overwriteNotebooks = options?.overwriteNotebooks ?? false;

    if (isNotebook && !overwriteNotebooks) {
      const exists = await this.fileExists(clean);
      if (exists) {
        return false;
      }
    }

    let payload: Record<string, any>;
    const buf = typeof rawContent === 'string' ? Buffer.from(rawContent, 'utf-8') : rawContent;

    if (isNotebook && !options?.forceBase64) {
      try {
        const jsonContent = JSON.parse(buf.toString('utf-8'));
        payload = {
          type: 'notebook',
          format: 'json',
          content: jsonContent,
        };
      } catch {
        payload = {
          type: 'file',
          format: 'text',
          content: buf.toString('utf-8'),
        };
      }
    } else if (options?.forceBase64) {
      payload = {
        type: 'file',
        format: 'base64',
        content: buf.toString('base64'),
      };
    } else {
      // Check if valid UTF-8 text without null bytes
      const isBinary = buf.includes(0);
      if (!isBinary) {
        const text = buf.toString('utf-8');
        if (Buffer.from(text, 'utf-8').equals(buf)) {
          payload = {
            type: 'file',
            format: 'text',
            content: text,
          };
        } else {
          payload = {
            type: 'file',
            format: 'base64',
            content: buf.toString('base64'),
          };
        }
      } else {
        payload = {
          type: 'file',
          format: 'base64',
          content: buf.toString('base64'),
        };
      }
    }

    const resp = await this.request(
      `/api/contents/${clean}`,
      {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      },
      60000
    );

    if (!resp.ok) {
      const text = await resp.text().catch(() => '');
      throw new Error(`PUT /api/contents/${clean} failed (HTTP ${resp.status}): ${text}`);
    }
    return true;
  }

  /**
   * Deletes a remote file via DELETE /api/contents/<path>. Ignores 404.
   */
  async deleteFile(remotePath: string): Promise<void> {
    const clean = normalizeApiPath(remotePath);
    if (!clean) {
      return;
    }
    const resp = await this.request(`/api/contents/${clean}`, { method: 'DELETE' }, 20000);
    if (resp.status !== 204 && resp.status !== 404 && !resp.ok) {
      const text = await resp.text().catch(() => '');
      throw new Error(`DELETE /api/contents/${clean} failed (HTTP ${resp.status}): ${text}`);
    }
  }

  /**
   * Executes Python code on a specific remote kernel over WebSocket (/api/kernels/<id>/channels).
   */
  async executeCode(
    kernelId: string,
    code: string,
    options?: {
      silent?: boolean;
      storeHistory?: boolean;
      timeoutMs?: number;
      onStream?: (stream: 'stdout' | 'stderr', text: string) => void;
    }
  ): Promise<KernelExecutionResult> {
    const silent = options?.silent ?? false;
    const storeHistory = options?.storeHistory ?? !silent;
    const timeoutMs = options?.timeoutMs ?? 60000;
    const sessionId = `jsync-${crypto.randomUUID()}`;
    const msgId = `msg-${crypto.randomUUID()}`;

    const wsUrl = `${this.parsedUrl.wsBaseUrl}/api/kernels/${kernelId}/channels?session_id=${encodeURIComponent(
      sessionId
    )}`;

    return new Promise<KernelExecutionResult>((resolve, reject) => {
      const ws = new WebSocket(wsUrl, [V1_KERNEL_WS_PROTOCOL], {
        headers: this.buildHeaders(),
      });

      let stdout = '';
      let stderr = '';
      const results: string[] = [];
      let status: 'ok' | 'error' | 'abort' = 'ok';
      let ename: string | undefined;
      let evalue: string | undefined;
      let traceback: string[] | undefined;
      let executionCount: number | null | undefined;
      let gotExecuteReply = false;
      let gotIdle = false;
      let settled = false;

      const finish = (err?: Error) => {
        if (settled) {
          return;
        }
        settled = true;
        clearTimeout(timer);
        try {
          ws.close();
        } catch {
          // ignore
        }
        if (err) {
          reject(err);
        } else {
          resolve({
            status,
            stdout,
            stderr,
            results,
            ename,
            evalue,
            traceback,
            executionCount,
          });
        }
      };

      const timer = setTimeout(() => {
        finish(new Error(`Kernel execution timed out after ${timeoutMs}ms on kernel ${kernelId}`));
      }, timeoutMs);

      ws.on('open', () => {
        const executeRequest: JupyterWireMessage = {
          header: {
            msg_id: msgId,
            username: 'jupyter-sync',
            session: sessionId,
            date: new Date().toISOString(),
            msg_type: 'execute_request',
            version: '5.3',
          },
          parent_header: {},
          metadata: {},
          content: {
            code,
            silent,
            store_history: storeHistory,
            user_expressions: {},
            allow_stdin: false,
            stop_on_error: true,
          },
          buffers: [],
          channel: 'shell',
        };
        const wireData = encodeJupyterWsMessage(executeRequest, ws.protocol);
        ws.send(wireData);
      });

      ws.on('message', (data: WebSocket.RawData) => {
        const msg = decodeJupyterWsMessage(data, ws.protocol);
        if (!msg) {
          return;
        }

        if (msg?.parent_header?.msg_id !== msgId) {
          return;
        }

        const msgType = msg?.header?.msg_type;
        const content = msg?.content || {};

        if (msgType === 'stream') {
          const streamName = content.name === 'stderr' ? 'stderr' : 'stdout';
          const text = String(content.text || '');
          if (streamName === 'stderr') {
            stderr += text;
          } else {
            stdout += text;
          }
          options?.onStream?.(streamName, text);
        } else if (msgType === 'execute_result' || msgType === 'display_data') {
          const plain = content.data?.['text/plain'];
          if (plain !== undefined) {
            results.push(String(plain));
          }
        } else if (msgType === 'error') {
          status = 'error';
          ename = String(content.ename || 'Error');
          evalue = String(content.evalue || '');
          traceback = Array.isArray(content.traceback) ? content.traceback.map(String) : [];
        } else if (msgType === 'execute_reply') {
          gotExecuteReply = true;
          if (content.status === 'error') {
            status = 'error';
            ename = ename || String(content.ename || 'Error');
            evalue = evalue || String(content.evalue || '');
            traceback =
              traceback || (Array.isArray(content.traceback) ? content.traceback.map(String) : []);
          } else if (content.status === 'abort') {
            status = 'abort';
          }
          if (content.execution_count !== undefined) {
            executionCount = content.execution_count;
          }
        } else if (msgType === 'status') {
          if (content.execution_state === 'idle') {
            gotIdle = true;
          }
        }

        if (gotExecuteReply && gotIdle) {
          finish();
        }
      });

      ws.on('unexpected-response', (_req, res) => {
        if (res.statusCode === 401 || res.statusCode === 403) {
          const authErr = new JupyterAuthError(
            `WebSocket authentication failed (${res.statusCode})`,
            res.statusCode
          );
          for (const cb of this.onAuthErrorCallbacks) {
            try {
              cb(authErr);
            } catch {
              // ignore
            }
          }
          finish(authErr);
          return;
        }
        finish(new Error(`Unexpected WebSocket response: HTTP ${res.statusCode}`));
      });

      ws.on('error', (err) => {
        finish(err);
      });

      ws.on('close', () => {
        if (!settled) {
          if (gotExecuteReply) {
            finish();
          } else {
            finish(new Error(`Kernel WebSocket closed before execution completed`));
          }
        }
      });
    });
  }

  /**
   * Finds an idle kernel or starts a temporary kernel to run a callback.
   */
  async withIdleOrTempKernel<T>(fn: (kernelId: string) => Promise<T>): Promise<T> {
    const kernels = await this.listKernels();
    const idleKernel = kernels.find((k) => k.execution_state === 'idle') || kernels[0];
    if (idleKernel && idleKernel.execution_state === 'idle') {
      return await fn(idleKernel.id);
    }

    const tempKernel = await this.startKernel('python3');
    try {
      return await fn(tempKernel.id);
    } finally {
      await this.shutdownKernel(tempKernel.id).catch(() => {});
    }
  }

  /**
   * Executes a shell command on the remote server using a transient Jupyter Terminal WebSocket
   * (/api/terminals -> /terminals/websocket/<name>), falling back to a kernel subprocess if
   * terminals are disabled on the server.
   */
  async executeShellCommand(
    command: string,
    options?: {
      cwd?: string;
      timeoutMs?: number;
      onOutput?: (chunk: string) => void;
      preferredKernelId?: string;
    }
  ): Promise<ShellExecutionResult> {
    const timeoutMs = options?.timeoutMs ?? 60000;
    try {
      return await this.executeViaTerminalWebSocket(command, options?.cwd, timeoutMs, options?.onOutput);
    } catch (termErr) {
      if (termErr instanceof JupyterAuthError) {
        throw termErr;
      }
      // Fallback: execute via Python subprocess on an idle or temporary kernel
      return await this.executeShellViaKernel(
        command,
        options?.cwd,
        timeoutMs,
        options?.onOutput,
        options?.preferredKernelId
      );
    }
  }

  private async executeViaTerminalWebSocket(
    command: string,
    relCwd: string | undefined,
    timeoutMs: number,
    onOutput?: (chunk: string) => void
  ): Promise<ShellExecutionResult> {
    const createResp = await this.request(
      '/api/terminals',
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({}),
      },
      15000
    );
    if (!createResp.ok) {
      throw new Error(`POST /api/terminals returned HTTP ${createResp.status}`);
    }
    const termModel = (await createResp.json()) as { name: string };
    const termName = termModel.name;

    const runId = crypto.randomBytes(6).toString('hex');
    const startSentinel = `__JSYNC_START_${runId}__`;
    const exitPrefix = `__JSYNC_EXIT_${runId}__:`;
    const exitRegex = new RegExp(`__JSYNC_EXIT_${runId}__:(\\d+)__`);

    const wsUrl = `${this.parsedUrl.wsBaseUrl}/terminals/websocket/${encodeURIComponent(termName)}`;

    try {
      return await new Promise<ShellExecutionResult>((resolve, reject) => {
        const ws = new WebSocket(wsUrl, {
          headers: this.buildHeaders(),
        });

        let rawBuffer = '';
        let startedCapture = false;
        let streamedIndex = 0;
        let settled = false;

        const finish = (err?: Error, result?: ShellExecutionResult) => {
          if (settled) {
            return;
          }
          settled = true;
          clearTimeout(timer);
          try {
            ws.close();
          } catch {
            // ignore
          }
          if (err) {
            reject(err);
          } else if (result) {
            resolve(result);
          }
        };

        const timer = setTimeout(() => {
          finish(new Error(`Remote terminal command timed out after ${timeoutMs}ms`));
        }, timeoutMs);

        ws.on('open', async () => {
          const cleanCwd = relCwd ? normalizeApiPath(relCwd) : '';
          const cdCmd = cleanCwd
            ? `cd ${JSON.stringify(cleanCwd)} 2>/dev/null || cd "$HOME"/${JSON.stringify(cleanCwd)} 2>/dev/null\n`
            : '';
          const scriptBody = `${cdCmd}${command}\n`;
          const rawB64Lines = Buffer.from(scriptBody, 'utf-8').toString('base64').match(/.{1,76}/g) || [''];
          const tmpScript = `/tmp/.jsync_${runId}.sh`;
          const eofMarker = `__JSYNC_EOF_${runId}__`;

          // Group base64 lines into <= 1KB chunks and pace writes so we never overflow
          // the Linux PTY canonical line buffer (MAX_CANON) or ring buffer (N_TTY_BUF_SIZE = 4096)
          const chunksToSend: string[] = [
            ` stty -echo 2>/dev/null\r`,
            `cat << '${eofMarker}' | base64 -d > ${tmpScript}\r`,
          ];
          for (let i = 0; i < rawB64Lines.length; i += 12) {
            chunksToSend.push(rawB64Lines.slice(i, i + 12).join('\r') + '\r');
          }
          chunksToSend.push(`${eofMarker}\r`);
          chunksToSend.push(
            `printf '%s\\n' '${startSentinel}'; sh ${tmpScript} 2>&1; _ec=$?; rm -f ${tmpScript}; printf '\\n${exitPrefix}%d__\\n' $_ec\r`
          );

          for (const chunk of chunksToSend) {
            if (settled || ws.readyState !== WebSocket.OPEN) {
              break;
            }
            ws.send(JSON.stringify(['stdin', chunk]));
            if (chunksToSend.length > 4) {
              await new Promise((r) => setTimeout(r, 10));
            }
          }
        });

        ws.on('message', (data: WebSocket.RawData) => {
          let msg: any;
          try {
            const text = typeof data === 'string' ? data : data.toString('utf-8');
            msg = JSON.parse(text);
          } catch {
            return;
          }

          if (!Array.isArray(msg) || msg[0] !== 'stdout') {
            return;
          }

          const chunk = String(msg[1] || '');
          rawBuffer += chunk;

          if (!startedCapture) {
            const startIdx = rawBuffer.indexOf(startSentinel + '\n');
            const startIdxCrLf = rawBuffer.indexOf(startSentinel + '\r\n');
            if (startIdxCrLf !== -1) {
              rawBuffer = rawBuffer.slice(startIdxCrLf + startSentinel.length + 2);
              startedCapture = true;
            } else if (startIdx !== -1) {
              rawBuffer = rawBuffer.slice(startIdx + startSentinel.length + 1);
              startedCapture = true;
            } else {
              return;
            }
          }

          const match = exitRegex.exec(rawBuffer);
          if (match) {
            const outputPart = rawBuffer.slice(0, match.index).replace(/\r?\n$/, '');
            if (onOutput && outputPart.length > streamedIndex) {
              onOutput(outputPart.slice(streamedIndex));
              streamedIndex = outputPart.length;
            }
            const exitCode = parseInt(match[1], 10);
            finish(undefined, {
              exitCode,
              stdout: outputPart,
              stderr: '',
            });
          } else if (onOutput) {
            // Stream safe prefix before potential partial sentinel
            const safeEnd = Math.max(0, rawBuffer.length - 40);
            if (safeEnd > streamedIndex) {
              onOutput(rawBuffer.slice(streamedIndex, safeEnd));
              streamedIndex = safeEnd;
            }
          }
        });

        ws.on('unexpected-response', (_req, res) => {
          finish(new Error(`Terminal WebSocket HTTP ${res.statusCode}`));
        });

        ws.on('error', (err) => {
          finish(err);
        });

        ws.on('close', () => {
          if (!settled) {
            finish(new Error('Terminal WebSocket closed before command finished'));
          }
        });
      });
    } finally {
      await this.request(`/api/terminals/${encodeURIComponent(termName)}`, { method: 'DELETE' }, 10000).catch(
        () => {}
      );
    }
  }

  private async executeShellViaKernel(
    command: string,
    relCwd: string | undefined,
    timeoutMs: number,
    onOutput?: (chunk: string) => void,
    preferredKernelId?: string
  ): Promise<ShellExecutionResult> {
    const pyCode = `
import os, subprocess, sys, json
_root = getattr(sys, "_jupyter_sync_server_root", None) or os.environ.get("JUPYTER_SERVER_ROOT") or os.path.expanduser("~")
_rel = ${JSON.stringify(relCwd ? normalizeApiPath(relCwd) : '')}
_cwd = os.path.join(_root, _rel) if _rel else _root
if not os.path.isdir(_cwd):
    _cwd = os.getcwd()
_proc = subprocess.run(${JSON.stringify(command)}, shell=True, cwd=_cwd, capture_output=True, text=True)
if _proc.stdout:
    sys.stdout.write(_proc.stdout)
if _proc.stderr:
    sys.stderr.write(_proc.stderr)
print("__JSYNC_KERNEL_EXIT__:" + str(_proc.returncode))
`;
    const runOnKernel = async (kId: string) => {
      let stdoutAcc = '';
      const res = await this.executeCode(kId, pyCode, {
        silent: false,
        storeHistory: false,
        timeoutMs,
        onStream: (_stream, text) => {
          const cleanText = text.replace(/__JSYNC_KERNEL_EXIT__:\d+\r?\n?/, '');
          if (cleanText) {
            stdoutAcc += cleanText;
            onOutput?.(cleanText);
          }
        },
      });
      const exitMatch = res.stdout.match(/__JSYNC_KERNEL_EXIT__:(-?\d+)/);
      const exitCode = exitMatch ? parseInt(exitMatch[1], 10) : res.status === 'ok' ? 0 : 1;
      const cleanStdout = res.stdout.replace(/\r?\n?__JSYNC_KERNEL_EXIT__:-?\d+\r?\n?$/, '');
      return {
        exitCode,
        stdout: cleanStdout || stdoutAcc,
        stderr: res.stderr,
      };
    };

    if (preferredKernelId) {
      return await runOnKernel(preferredKernelId);
    }
    return await this.withIdleOrTempKernel(runOnKernel);
  }
}
