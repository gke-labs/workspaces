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

import * as assert from 'assert';
import * as childProcess from 'child_process';
import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';
import {
  AgentBridgeServer,
  callAgentBridge,
  loadCliSession,
  removeCliSession,
  saveCliSession,
} from '../../agentBridge';
import { isProxySafePath, normalizeApiPath } from '../../core/jupyterClient';
import {
  computeSyncDiff,
  createTarGzBuffer,
  DEFAULT_EXCLUDE_GLOBS,
  LocalFileEntry,
  matchesAnyGlob,
  partitionIntoBatches,
  SyncManifest,
} from '../../core/syncEngine';
import { parseJupyterUrl } from '../../core/urlParser';
import {
  buildKernelInitSnippet,
  decodeJupyterWsMessage,
  encodeJupyterWsMessage,
  resolveNotebookRemotePaths,
  V1_KERNEL_WS_PROTOCOL,
} from '../../kernelInitializer';

interface TestCase {
  name: string;
  fn: () => void | Promise<void>;
}

const tests: TestCase[] = [];
function test(name: string, fn: () => void | Promise<void>) {
  tests.push({ name, fn });
}

// 1. URL Parser Tests
test('urlParser: parses GKE Workspaces desktop connection URL', () => {
  const raw =
    'https://connect.35.201.99.43.sslip.io/workspace/connect/kubeflow-user/jupyter-tpu-v4fi/jupyterlab/?token=my-secret-jwt';
  const parsed = parseJupyterUrl(raw);
  assert.strictEqual(
    parsed.baseUrl,
    'https://connect.35.201.99.43.sslip.io/workspace/connect/kubeflow-user/jupyter-tpu-v4fi/jupyterlab'
  );
  assert.strictEqual(
    parsed.wsBaseUrl,
    'wss://connect.35.201.99.43.sslip.io/workspace/connect/kubeflow-user/jupyter-tpu-v4fi/jupyterlab'
  );
  assert.strictEqual(parsed.origin, 'https://connect.35.201.99.43.sslip.io');
  assert.strictEqual(parsed.token, 'my-secret-jwt');
  assert.strictEqual(parsed.label, 'kubeflow-user/jupyter-tpu-v4fi');
  assert.strictEqual(parsed.namespace, 'kubeflow-user');
  assert.strictEqual(parsed.workspace, 'jupyter-tpu-v4fi');
  assert.strictEqual(parsed.id.length, 16);
});

test('urlParser: strips /lab/tree/... suffix from JupyterHub URL', () => {
  const raw = 'https://jupyter.example.com/user/alice/lab/tree/work/notebook.ipynb?token=tok123';
  const parsed = parseJupyterUrl(raw);
  assert.strictEqual(parsed.baseUrl, 'https://jupyter.example.com/user/alice');
  assert.strictEqual(parsed.wsBaseUrl, 'wss://jupyter.example.com/user/alice');
  assert.strictEqual(parsed.token, 'tok123');
  assert.strictEqual(parsed.label, 'alice@jupyter.example.com');
});

test('urlParser: parses localhost URL and produces deterministic server ID', () => {
  const p1 = parseJupyterUrl('http://localhost:8888/lab?token=t1');
  const p2 = parseJupyterUrl('http://localhost:8888/tree/?token=t2');
  assert.strictEqual(p1.baseUrl, 'http://localhost:8888');
  assert.strictEqual(p1.wsBaseUrl, 'ws://localhost:8888');
  assert.strictEqual(p1.id, p2.id);
});

// 2. Path Normalization & Proxy Safety Tests
test('jupyterClient: normalizeApiPath and isProxySafePath', () => {
  assert.strictEqual(normalizeApiPath('//gke-workspaces//examples/torch_tpu/'), 'gke-workspaces/examples/torch_tpu');
  assert.strictEqual(isProxySafePath('examples/torch_tpu/tpu_lock.py'), true);
  assert.strictEqual(isProxySafePath('examples/file with spaces.py'), false);
  assert.strictEqual(isProxySafePath('examples/../secret.py'), false);
});

// 3. Glob Matching Tests
test('syncEngine: matchesAnyGlob filters excluded paths accurately', () => {
  assert.strictEqual(matchesAnyGlob('.git/config', DEFAULT_EXCLUDE_GLOBS), true);
  assert.strictEqual(matchesAnyGlob('examples/torch_tpu/__pycache__/tpu_lock.cpython-311.pyc', DEFAULT_EXCLUDE_GLOBS), true);
  assert.strictEqual(matchesAnyGlob('vscode-extension/node_modules/ws/index.js', DEFAULT_EXCLUDE_GLOBS), true);
  assert.strictEqual(matchesAnyGlob('vscode-extension/dist/extension.js', DEFAULT_EXCLUDE_GLOBS), true);
  assert.strictEqual(matchesAnyGlob('vscode-extension/jupyter-workspace-sync.vsix', DEFAULT_EXCLUDE_GLOBS), true);
  assert.strictEqual(matchesAnyGlob('examples/distributed/demo.mp4', DEFAULT_EXCLUDE_GLOBS), true);
  assert.strictEqual(matchesAnyGlob('.workspace-sync-manifest.json', DEFAULT_EXCLUDE_GLOBS), true);
  assert.strictEqual(matchesAnyGlob('examples/torch_tpu/tpu_lock.py', DEFAULT_EXCLUDE_GLOBS), false);
  assert.strictEqual(matchesAnyGlob('examples/distributed/jobs/pipeline.py', DEFAULT_EXCLUDE_GLOBS), false);
});

// 4. 3-Way Manifest Diff & Notebook Protection Tests
test('syncEngine: computeSyncDiff handles 3-way diff and protects tracked .ipynb files', () => {
  const localFiles: LocalFileEntry[] = [
    { relPath: 'jobs/unchanged.py', absPath: '/tmp/unchanged.py', sha256: 'aaa', size: 100, mtimeMs: 1 },
    { relPath: 'jobs/modified.py', absPath: '/tmp/modified.py', sha256: 'bbb_new', size: 200, mtimeMs: 2 },
    { relPath: 'jobs/added.py', absPath: '/tmp/added.py', sha256: 'ccc', size: 300, mtimeMs: 3 },
    { relPath: 'notebook.ipynb', absPath: '/tmp/notebook.ipynb', sha256: 'nb_local_modified', size: 500, mtimeMs: 4 },
    { relPath: 'new_notebook.ipynb', absPath: '/tmp/new_notebook.ipynb', sha256: 'nb_new', size: 400, mtimeMs: 5 },
  ];

  const remoteManifest: SyncManifest = {
    version: 1,
    repoName: 'test-repo',
    updatedAt: '2026-10-06T00:00:00Z',
    files: {
      'jobs/unchanged.py': { sha256: 'aaa', size: 100 },
      'jobs/modified.py': { sha256: 'bbb_old', size: 180 },
      'jobs/deleted_locally.py': { sha256: 'ddd', size: 150 },
      'notebook.ipynb': { sha256: 'nb_seeded_originally', size: 450 },
    },
  };

  const diff = computeSyncDiff(localFiles, remoteManifest, { overwriteNotebooks: false });

  assert.deepStrictEqual(
    diff.toUpload.map((f) => f.relPath),
    ['jobs/modified.py', 'jobs/added.py', 'new_notebook.ipynb']
  );
  assert.deepStrictEqual(diff.toDelete, ['jobs/deleted_locally.py']);
  assert.deepStrictEqual(
    diff.unchanged.map((f) => f.relPath),
    ['jobs/unchanged.py', 'notebook.ipynb']
  );
});

// 5. Batch Partitioning Tests
test('syncEngine: partitionIntoBatches splits by byte size and file count', () => {
  const files: LocalFileEntry[] = [
    { relPath: 'f1.py', absPath: '/f1.py', sha256: '1', size: 3 * 1024 * 1024, mtimeMs: 1 },
    { relPath: 'f2.py', absPath: '/f2.py', sha256: '2', size: 3 * 1024 * 1024, mtimeMs: 1 },
    { relPath: 'f3.py', absPath: '/f3.py', sha256: '3', size: 1 * 1024 * 1024, mtimeMs: 1 },
  ];
  const batches = partitionIntoBatches(files, 5 * 1024 * 1024, 100);
  assert.strictEqual(batches.length, 2);
  assert.deepStrictEqual(batches[0].map((f) => f.relPath), ['f1.py']);
  assert.deepStrictEqual(batches[1].map((f) => f.relPath), ['f2.py', 'f3.py']);
});

// 6. Tarball Builder & Python Extraction Verification
test('syncEngine: createTarGzBuffer produces valid .tar.gz with short and long paths', () => {
  const longSubdir = 'deeply/nested/directory/structure/that/exceeds/one/hundred/characters/in/posix/ustar/header/format/easily';
  const longRelPath = `${longSubdir}/module_name.py`;
  const tarGz = createTarGzBuffer([
    { relPath: 'examples/torch_tpu/tpu_lock.py', content: Buffer.from('print("hello tpu")\n', 'utf-8') },
    { relPath: longRelPath, content: Buffer.from('X = 42\n', 'utf-8') },
  ]);

  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'jsync-tar-test-'));
  try {
    const tarPath = path.join(tmpDir, 'batch.tar.gz');
    fs.writeFileSync(tarPath, tarGz);
    childProcess.execFileSync(
      'python3',
      [
        '-c',
        `import tarfile, sys; tf = tarfile.open(sys.argv[1], 'r:gz'); tf.extractall(sys.argv[2])`,
        tarPath,
        tmpDir,
      ]
    );
    const extractedShort = fs.readFileSync(path.join(tmpDir, 'examples/torch_tpu/tpu_lock.py'), 'utf-8');
    const extractedLong = fs.readFileSync(path.join(tmpDir, longRelPath), 'utf-8');
    assert.strictEqual(extractedShort, 'print("hello tpu")\n');
    assert.strictEqual(extractedLong, 'X = 42\n');
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

// 7. Kernel Initializer Path & Snippet Tests
test('kernelInitializer: resolves nested notebook paths and generates valid Python snippet', () => {
  const paths = resolveNotebookRemotePaths(
    '/workspace/gke-workspaces',
    'gke-workspaces',
    '/workspace/gke-workspaces/examples/torch_tpu/torch_tpu_training.ipynb'
  );
  assert.strictEqual(paths.relRepoRoot, 'gke-workspaces');
  assert.strictEqual(paths.relNotebookDir, 'gke-workspaces/examples/torch_tpu');
  assert.strictEqual(paths.relNotebookFile, 'gke-workspaces/examples/torch_tpu/torch_tpu_training.ipynb');

  const snippet = buildKernelInitSnippet(paths.relRepoRoot, paths.relNotebookDir, {
    setKernelWorkingDirectory: true,
    enableAutoreload: true,
  });
  assert.ok(snippet.includes('gke-workspaces/examples/torch_tpu'));
  assert.ok(snippet.includes('run_line_magic("autoreload", "2")'));
});

// 8. Jupyter WebSocket Wire Protocol Codec Tests
test('kernelInitializer: encodes and decodes both JSON and v1.kernel.websocket.jupyter.org binary messages', () => {
  const sampleMsg = {
    channel: 'shell',
    header: {
      msg_id: 'sync-init-123',
      msg_type: 'execute_request',
      username: 'user',
      session: 'sess-1',
      date: '2026-10-06T00:00:00Z',
      version: '5.3',
    },
    parent_header: {},
    metadata: { vscode: { cellId: 'test' } },
    content: {
      code: 'import os',
      silent: true,
      store_history: false,
    },
    buffers: [],
  };

  // Default JSON protocol
  const encodedJson = encodeJupyterWsMessage(sampleMsg, '');
  const decodedJson = decodeJupyterWsMessage(encodedJson, '');
  assert.ok(decodedJson);
  assert.strictEqual(decodedJson!.header.msg_id, 'sync-init-123');
  assert.strictEqual(decodedJson!.content.code, 'import os');

  // Binary v1.kernel.websocket.jupyter.org protocol
  const encodedBin = encodeJupyterWsMessage(sampleMsg, V1_KERNEL_WS_PROTOCOL);
  const decodedBin = decodeJupyterWsMessage(encodedBin, V1_KERNEL_WS_PROTOCOL);
  assert.ok(decodedBin);
  assert.strictEqual(decodedBin!.channel, 'shell');
  assert.strictEqual(decodedBin!.header.msg_id, 'sync-init-123');
  assert.strictEqual(decodedBin!.content.code, 'import os');
  assert.strictEqual(decodedBin!.content.silent, true);
});

// 9. Workspace-Scoped CLI Session Inheritance Tests
test('agentBridge: loadCliSession inherits in subdirectories but never leaks across workspaces', () => {
  const tmpBase = fs.mkdtempSync(path.join(os.tmpdir(), 'jsync-cli-sess-'));
  const repoA = path.join(tmpBase, 'repo-a');
  const repoASubdir = path.join(repoA, 'examples', 'torch_tpu');
  const repoB = path.join(tmpBase, 'repo-b');
  fs.mkdirSync(repoASubdir, { recursive: true });
  fs.mkdirSync(repoB, { recursive: true });

  try {
    saveCliSession(repoA, {
      url: 'http://localhost:8888/?token=tok-a',
      remoteDir: 'shared/repo-a',
      serverLabel: 'local-a',
      baseUrl: 'http://localhost:8888',
      updatedAt: new Date().toISOString(),
    });

    // Subprocess in repoA or nested subdirectory inherits the session
    const loadedRoot = loadCliSession(repoA);
    const loadedSubdir = loadCliSession(repoASubdir);
    assert.ok(loadedRoot);
    assert.strictEqual(loadedRoot!.url, 'http://localhost:8888/?token=tok-a');
    assert.ok(loadedSubdir);
    assert.strictEqual(loadedSubdir!.url, 'http://localhost:8888/?token=tok-a');
    assert.strictEqual(loadedSubdir!.remoteDir, 'shared/repo-a');

    // Unrelated repoB must NOT inherit repoA's session
    const loadedRepoB = loadCliSession(repoB);
    assert.strictEqual(loadedRepoB, undefined);

    // Removing repoA session clears it
    removeCliSession(repoA);
    assert.strictEqual(loadCliSession(repoA), undefined);
  } finally {
    removeCliSession(repoA);
    fs.rmSync(tmpBase, { recursive: true, force: true });
  }
});

// 10. Lazy Bridge Activation Tests (No sync on status/ping/outputs; activates on agent sync/connect)
test('agentBridge: stays idle on status/outputs when disconnected and activates when agent initiates sync', async () => {
  const tmpWs = fs.mkdtempSync(path.join(os.tmpdir(), 'jsync-bridge-lazy-'));
  let connectCalledWith: string | null = null;
  let syncTriggered = 0;

  const sampleNb = path.join(tmpWs, 'test.ipynb');
  fs.writeFileSync(
    sampleNb,
    JSON.stringify({
      cells: [
        {
          cell_type: 'code',
          execution_count: 1,
          source: ['print("hi")\n'],
          outputs: [{ output_type: 'stream', name: 'stdout', text: ['hi\n'] }],
        },
      ],
    }),
    'utf-8'
  );

  saveCliSession(tmpWs, {
    url: 'http://localhost:9999/?token=lazy-tok',
    remoteDir: 'shared/lazy',
    serverLabel: 'lazy-server',
    baseUrl: 'http://localhost:9999',
    updatedAt: new Date().toISOString(),
  });

  const dummySummary = {
    uploadedFiles: 3,
    deletedFiles: 0,
    unchangedFiles: 10,
    skippedNotebooks: 0,
    skippedLargeFiles: [] as string[],
    batchesUsed: 1,
    totalBytesTransferred: 1024,
    durationMs: 25,
    transport: 'tarball' as const,
  };

  let isConnected = false;
  const mockClient: any = { label: 'lazy-server', baseUrl: 'http://localhost:9999' };
  const mockSyncEngine: any = {
    flushAndWait: async () => {},
  };

  const bridge = new AgentBridgeServer({
    workspaceRoot: tmpWs,
    getClient: () => (isConnected ? mockClient : null),
    getSyncEngine: () => (isConnected ? mockSyncEngine : null),
    getKernelInitializer: () => ({ invalidate: () => {} } as any),
    getRemoteBaseDir: () => 'shared/lazy',
    getSetKernelWorkingDirectory: () => true,
    getEnableAutoreload: () => true,
    getAutoSaveOutputs: () => true,
    triggerSyncNow: async () => {
      syncTriggered++;
      return dummySummary;
    },
    connectFromUrl: async (url: string) => {
      connectCalledWith = url;
      isConnected = true;
      syncTriggered++;
      return dummySummary;
    },
  });

  const sockPath = await bridge.start();
  try {
    // 1. `status` and `outputs` must NOT trigger connection or syncing
    const statusBefore = await callAgentBridge(sockPath, 'status');
    assert.strictEqual(statusBefore.connected, false);
    assert.strictEqual(connectCalledWith, null);
    assert.strictEqual(syncTriggered, 0);

    const outCells = await callAgentBridge(sockPath, 'outputs', { notebookPath: 'test.ipynb' });
    assert.strictEqual(outCells.length, 1);
    assert.strictEqual(outCells[0].stdout, 'hi\n');
    assert.strictEqual(connectCalledWith, null);
    assert.strictEqual(syncTriggered, 0);

    // 2. Agent-initiated `sync` activates connection using saved CLI session
    const syncRes = await callAgentBridge(sockPath, 'sync');
    assert.strictEqual(connectCalledWith, 'http://localhost:9999/?token=lazy-tok');
    assert.strictEqual(isConnected, true);
    assert.strictEqual(syncRes.uploadedFiles, 3);
  } finally {
    await bridge.stop();
    removeCliSession(tmpWs);
    fs.rmSync(tmpWs, { recursive: true, force: true });
  }
});

async function main() {
  console.log(`Running ${tests.length} unit tests...\n`);
  let passed = 0;
  for (const t of tests) {
    try {
      await t.fn();
      console.log(`  ✓ ${t.name}`);
      passed++;
    } catch (err) {
      console.error(`  ✗ ${t.name}`);
      console.error(err);
      process.exit(1);
    }
  }
  console.log(`\nAll ${passed}/${tests.length} unit tests passed!`);
}

main();
