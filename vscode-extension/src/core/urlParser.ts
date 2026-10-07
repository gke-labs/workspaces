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

export interface ParsedJupyterUrl {
  /** Deterministic 16-hex-char identifier derived from baseUrl. */
  id: string;
  /** Clean Jupyter Server API base URL without trailing slash or UI suffixes. */
  baseUrl: string;
  /** WebSocket base URL (ws:// or wss://) matching baseUrl. */
  wsBaseUrl: string;
  /** Origin (<scheme>://<host>) used for Origin header validation on proxies. */
  origin: string;
  /** Extracted authentication token (from query string, explicit arg, or env). */
  token: string;
  /** Human-readable server display label for UI and kernel picker. */
  label: string;
  /** Optional Kubernetes namespace if parsed from a Kubeflow/GKE Workspaces URL. */
  namespace?: string;
  /** Optional Workspace name if parsed from a Kubeflow/GKE Workspaces URL. */
  workspace?: string;
}

const UI_PATH_PATTERNS: RegExp[] = [
  /\/lab\/workspaces\/[^/]+\/tree(?:\/.*)?$/,
  /\/lab\/workspaces\/[^/]+$/,
  /\/lab\/tree(?:\/.*)?$/,
  /\/lab$/,
  /\/tree(?:\/.*)?$/,
  /\/notebooks(?:\/.*)?$/,
  /\/retro(?:\/.*)?$/,
];

/**
 * Parses any JupyterLab, Notebook, JupyterHub, or Kubeflow / GKE Workspaces URL
 * into a normalized API baseUrl, token, and human-readable label.
 */
export function parseJupyterUrl(rawUrl: string, explicitToken?: string): ParsedJupyterUrl {
  const trimmed = (rawUrl || '').trim();
  if (!trimmed) {
    throw new Error('Jupyter Server URL cannot be empty.');
  }

  let parsed: URL;
  try {
    parsed = new URL(trimmed);
  } catch {
    throw new Error(
      `Invalid URL: '${rawUrl}'. Expected a full URL like 'https://host/path/?token=...'`
    );
  }

  if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
    throw new Error(
      `Unsupported URL protocol '${parsed.protocol}'. Expected 'http:' or 'https:'.`
    );
  }

  const queryToken = parsed.searchParams.get('token') || undefined;
  const token = (
    explicitToken ??
    queryToken ??
    process.env.JUPYTER_TOKEN ??
    ''
  ).trim();

  // Normalize path and strip duplicate slashes
  let cleanPath = parsed.pathname.replace(/\/+/g, '/').replace(/\/+$/, '');

  for (const pattern of UI_PATH_PATTERNS) {
    if (pattern.test(cleanPath)) {
      cleanPath = cleanPath.replace(pattern, '');
      break;
    }
  }
  cleanPath = cleanPath.replace(/\/+$/, '');

  const origin = `${parsed.protocol}//${parsed.host}`;
  const baseUrl = `${origin}${cleanPath}`;
  const wsProtocol = parsed.protocol === 'https:' ? 'wss:' : 'ws:';
  const wsBaseUrl = `${wsProtocol}//${parsed.host}${cleanPath}`;
  const id = computeServerId(baseUrl);

  // Derive human-readable display label
  let label = parsed.host;
  let namespace: string | undefined;
  let workspace: string | undefined;

  // Match Kubeflow / GKE Workspaces pattern: /workspace/connect/<namespace>/<workspace>/<port>
  const gkeMatch = cleanPath.match(/^\/workspace\/connect\/([^/]+)\/([^/]+)(?:\/([^/]+))?$/);
  if (gkeMatch) {
    namespace = gkeMatch[1];
    workspace = gkeMatch[2];
    label = `${namespace}/${workspace}`;
  } else {
    // Match JupyterHub pattern: /user/<username>
    const hubMatch = cleanPath.match(/^\/user\/([^/]+)/);
    if (hubMatch) {
      label = `${hubMatch[1]}@${parsed.hostname}`;
    }
  }

  return {
    id,
    baseUrl,
    wsBaseUrl,
    origin,
    token,
    label,
    namespace,
    workspace,
  };
}

export function computeServerId(baseUrl: string): string {
  const normalized = baseUrl.trim().replace(/\/+$/, '');
  return crypto.createHash('sha256').update(normalized).digest('hex').slice(0, 16);
}
