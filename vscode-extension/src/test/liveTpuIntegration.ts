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
import * as fs from 'fs';
import * as path from 'path';
import { AgentBridgeServer, callAgentBridge } from '../agentBridge';
import { JupyterClient } from '../core/jupyterClient';
import { MANIFEST_FILENAME, SyncEngine, SyncProgressEvent } from '../core/syncEngine';
import { KernelInitializer } from '../kernelInitializer';

const TEST_URL = process.env.JUPYTER_TEST_URL || process.env.JUPYTER_URL;
if (!TEST_URL) {
  console.error('Error: Set JUPYTER_TEST_URL environment variable to run live TPU integration tests.');
  process.exit(1);
}

const WORKSPACE_ROOT = path.resolve(__dirname, '../../..');
const TEST_REMOTE_DIR = 'gke-workspaces-test';

async function main() {
  console.log('================================================================');
  console.log('  Jupyter Workspace Sync — Live Remote TPU Integration Suite');
  console.log('================================================================\n');

  const client = new JupyterClient(TEST_URL!);
  const syncEngine = new SyncEngine(client);
  const kernelInitializer = new KernelInitializer();
  let createdTempKernelId: string | null = null;
  let testKernelId: string | null = null;

  try {
    // ------------------------------------------------------------------
    // TC-1: Proxy Auth, XSRF & Rate-Limit Handshake
    // ------------------------------------------------------------------
    console.log('▶ TC-1: Proxy Auth, XSRF & Rate-Limit Handshake...');
    await client.verifyConnection();
    const specs = await client.getKernelSpecs();
    assert.ok(specs && specs.kernelspecs, 'Expected kernelspecs from remote server');
    const existingKernels = await client.listKernels();
    console.log(
      `  ✓ Connected to ${client.label} (${client.baseUrl}) — default kernelspec: ${specs.default}, running kernels: ${existingKernels.length}\n`
    );

    // Clean any leftover test directory before starting
    await client.executeShellCommand(`rm -rf ${JSON.stringify(TEST_REMOTE_DIR)}`);

    // ------------------------------------------------------------------
    // TC-2: Initial Whole-Repo Chunked Sync & Progress
    // ------------------------------------------------------------------
    console.log('▶ TC-2: Initial Whole-Repo Chunked Sync & Progress...');
    const progressEvents: SyncProgressEvent[] = [];
    const initialSummary = await syncEngine.syncWorkspace({
      localRoot: WORKSPACE_ROOT,
      remoteBaseDir: TEST_REMOTE_DIR,
      onProgress: (ev) => progressEvents.push(ev),
      onLog: (msg) => console.log(`    ${msg}`),
    });

    assert.ok(initialSummary.uploadedFiles > 50, 'Expected whole-repo initial upload (> 50 files)');
    assert.ok(progressEvents.length >= 3, 'Expected progress callbacks during initial sync');
    assert.strictEqual(
      progressEvents[progressEvents.length - 1].percent,
      100,
      'Expected final progress event to reach 100%'
    );

    const manifestExists = await client.fileExists(`${TEST_REMOTE_DIR}/${MANIFEST_FILENAME}`);
    const tpuLockExists = await client.fileExists(
      `${TEST_REMOTE_DIR}/examples/torch_tpu/tpu_lock.py`
    );
    const tpuTrainerExists = await client.fileExists(
      `${TEST_REMOTE_DIR}/examples/torch_tpu/tpu_trainer.py`
    );
    const pipelineExists = await client.fileExists(
      `${TEST_REMOTE_DIR}/examples/distributed/jobs/pipeline.py`
    );
    const gitConfigExists = await client.fileExists(`${TEST_REMOTE_DIR}/.git/config`);

    assert.strictEqual(manifestExists, true, 'Remote manifest must exist');
    assert.strictEqual(tpuLockExists, true, 'examples/torch_tpu/tpu_lock.py must exist');
    assert.strictEqual(tpuTrainerExists, true, 'examples/torch_tpu/tpu_trainer.py must exist');
    assert.strictEqual(pipelineExists, true, 'examples/distributed/jobs/pipeline.py must exist');
    assert.strictEqual(gitConfigExists, false, '.git/config must be excluded');
    console.log(
      `  ✓ TC-2 Passed: ${initialSummary.uploadedFiles} files synced across ${initialSummary.batchesUsed} batch(es) in ${initialSummary.durationMs}ms\n`
    );

    // ------------------------------------------------------------------
    // TC-3: Reconnect Differential Sync & Remote Artifact Preservation
    // ------------------------------------------------------------------
    console.log('▶ TC-3: Reconnect Differential Sync & Remote Artifact Preservation...');
    const mockCheckpointPath = `${TEST_REMOTE_DIR}/examples/torch_tpu/_mock_tpu_checkpoint.pt`;
    await client.putFile(mockCheckpointPath, Buffer.from('MOCK_TPU_WEIGHTS_V1', 'utf-8'));

    // 3a: No-op reconnect sync
    const noopSummary = await syncEngine.syncWorkspace({
      localRoot: WORKSPACE_ROOT,
      remoteBaseDir: TEST_REMOTE_DIR,
    });
    assert.strictEqual(noopSummary.uploadedFiles, 0, 'No-op reconnect must upload 0 files');
    assert.strictEqual(noopSummary.deletedFiles, 0, 'No-op reconnect must delete 0 files');
    assert.strictEqual(noopSummary.transport, 'noop');

    // 3b: Create temporary local file, sync, then delete it locally and sync again
    const tempLocalFile = path.join(WORKSPACE_ROOT, 'examples/torch_tpu/_temp_sync_helper.py');
    try {
      fs.writeFileSync(tempLocalFile, 'TEMP_VAL = "hello_from_laptop"\n', 'utf-8');
      const addSummary = await syncEngine.syncWorkspace({
        localRoot: WORKSPACE_ROOT,
        remoteBaseDir: TEST_REMOTE_DIR,
      });
      assert.strictEqual(addSummary.uploadedFiles, 1, 'Expected 1 newly added file uploaded');
      assert.strictEqual(
        await client.fileExists(`${TEST_REMOTE_DIR}/examples/torch_tpu/_temp_sync_helper.py`),
        true
      );
    } finally {
      if (fs.existsSync(tempLocalFile)) {
        fs.unlinkSync(tempLocalFile);
      }
    }

    const delSummary = await syncEngine.syncWorkspace({
      localRoot: WORKSPACE_ROOT,
      remoteBaseDir: TEST_REMOTE_DIR,
    });
    assert.strictEqual(delSummary.deletedFiles, 1, 'Expected 1 deleted file removed from remote');
    assert.strictEqual(
      await client.fileExists(`${TEST_REMOTE_DIR}/examples/torch_tpu/_temp_sync_helper.py`),
      false
    );
    assert.strictEqual(
      await client.fileExists(mockCheckpointPath),
      true,
      'Untracked remote checkpoint must remain untouched!'
    );
    console.log(
      `  ✓ TC-3 Passed: 0-file reconnect (${noopSummary.durationMs}ms), delta add/delete verified, untracked checkpoint preserved\n`
    );

    // ------------------------------------------------------------------
    // TC-4: Cancelled Sync & Resumability
    // ------------------------------------------------------------------
    console.log('▶ TC-4: Cancelled Sync & Resumability...');
    await client.deleteFile(`${TEST_REMOTE_DIR}/${MANIFEST_FILENAME}`);
    const abortController = new AbortController();
    let abortedAfterBatch1 = false;

    await assert.rejects(
      async () => {
        await syncEngine.syncWorkspace({
          localRoot: WORKSPACE_ROOT,
          remoteBaseDir: TEST_REMOTE_DIR,
          maxBatchFiles: 40,
          signal: abortController.signal,
          onProgress: (ev) => {
            if (ev.phase === 'transfer' && ev.batchIndex === 1 && ev.filesCompleted >= 40) {
              abortedAfterBatch1 = true;
              abortController.abort();
            }
          },
        });
      },
      /Sync cancelled/
    );
    assert.strictEqual(abortedAfterBatch1, true, 'Should have aborted after Batch 1 completed');

    const partialManifest = await syncEngine.fetchRemoteManifest(TEST_REMOTE_DIR);
    assert.ok(partialManifest, 'Partial manifest after Batch 1 must be saved on remote');
    const batch1RecordedCount = Object.keys(partialManifest!.files).length;
    assert.strictEqual(batch1RecordedCount, 40, 'Exactly 40 files from Batch 1 should be recorded');

    const resumeSummary = await syncEngine.syncWorkspace({
      localRoot: WORKSPACE_ROOT,
      remoteBaseDir: TEST_REMOTE_DIR,
      maxBatchFiles: 100,
    });
    assert.strictEqual(
      resumeSummary.unchangedFiles,
      40,
      'Resumed sync must skip the 40 already-synced Batch 1 files'
    );
    console.log(
      `  ✓ TC-4 Passed: Cancelled after Batch 1 (${batch1RecordedCount} files saved), resumed sync skipped Batch 1 and uploaded remaining ${resumeSummary.uploadedFiles} files\n`
    );

    // ------------------------------------------------------------------
    // TC-5: Nested Notebook cwd, sys.path & Relative Imports on TPU Kernel
    // ------------------------------------------------------------------
    console.log('▶ TC-5: Nested Notebook cwd, sys.path & Relative Imports on TPU Kernel...');
    const kernels = await client.listKernels();
    const idleExisting = kernels.find((k) => k.execution_state === 'idle');
    if (idleExisting) {
      testKernelId = idleExisting.id;
    } else {
      const started = await client.startKernel('python3');
      createdTempKernelId = started.id;
      testKernelId = started.id;
    }

    const targetNb = path.join(WORKSPACE_ROOT, 'examples/torch_tpu/torch_tpu_training.ipynb');
    const initRes = await kernelInitializer.initializeKernelDirect(
      client,
      testKernelId,
      WORKSPACE_ROOT,
      TEST_REMOTE_DIR,
      targetNb,
      { setKernelWorkingDirectory: true, enableAutoreload: true }
    );
    assert.strictEqual(initRes.status, 'ok', `Kernel init failed: ${initRes.evalue}`);

    const verifyImportsCode = `
import os, sys, json
import tpu_lock
import tpu_trainer
from examples.distributed.jobs import pipeline

with open("../distributed/inference-service.yaml", "r") as f:
    yaml_head = f.readline().strip()

print("__TC5_JSON__:" + json.dumps({
    "cwd": os.getcwd(),
    "sys_path_0": sys.path[0],
    "sys_path_1": sys.path[1],
    "tpu_lock_file": os.path.abspath(tpu_lock.__file__),
    "pipeline_file": os.path.abspath(pipeline.__file__),
    "yaml_head": yaml_head,
    "pid": os.getpid(),
}))
`;
    const tc5Exec = await client.executeCode(testKernelId, verifyImportsCode);
    assert.strictEqual(
      tc5Exec.status,
      'ok',
      `TC-5 execution failed: ${tc5Exec.ename}: ${tc5Exec.evalue}\n${(tc5Exec.traceback || []).join(
        '\n'
      )}`
    );
    const tc5Match = tc5Exec.stdout.match(/__TC5_JSON__:(\{.*\})/);
    assert.ok(tc5Match, `Expected __TC5_JSON__ in stdout, got: ${tc5Exec.stdout}`);
    const tc5Data = JSON.parse(tc5Match![1]);
    assert.ok(
      tc5Data.cwd.endsWith(`/${TEST_REMOTE_DIR}/examples/torch_tpu`),
      `Unexpected cwd: ${tc5Data.cwd}`
    );
    assert.strictEqual(tc5Data.sys_path_0, tc5Data.cwd);
    assert.ok(tc5Data.sys_path_1.endsWith(`/${TEST_REMOTE_DIR}`));
    console.log(
      `  ✓ TC-5 Passed: cwd=${tc5Data.cwd}, imported tpu_lock, tpu_trainer, examples.distributed.jobs.pipeline (PID ${tc5Data.pid})\n`
    );

    // ------------------------------------------------------------------
    // TC-6: Live .py Edit + %autoreload 2 Without Releasing TPU / Restarting Kernel
    // ------------------------------------------------------------------
    console.log('▶ TC-6: Live .py Edit + %autoreload 2 Without Restarting Kernel...');
    const remoteProbeRel = `${TEST_REMOTE_DIR}/examples/torch_tpu/_autoreload_probe.py`;
    await client.putFile(
      remoteProbeRel,
      'def get_version():\n    return "v1"\n',
      { overwriteNotebooks: true }
    );

    const v1Exec = await client.executeCode(
      testKernelId,
      'import os, _autoreload_probe\nprint(f"__V1__:{_autoreload_probe.get_version()}:{os.getpid()}")'
    );
    assert.strictEqual(v1Exec.status, 'ok');
    assert.ok(v1Exec.stdout.includes('__V1__:v1:'), `Unexpected v1 output: ${v1Exec.stdout}`);

    // Wait 1.1s so filesystem mtime advances by at least 1s for IPython %autoreload mtime check
    await new Promise((r) => setTimeout(r, 1100));
    await client.putFile(
      remoteProbeRel,
      'def get_version():\n    return "v2_hot_reloaded"\n',
      { overwriteNotebooks: true }
    );

    const v2Exec = await client.executeCode(
      testKernelId,
      'import os\nprint(f"__V2__:{_autoreload_probe.get_version()}:{os.getpid()}")'
    );
    assert.strictEqual(v2Exec.status, 'ok');
    assert.ok(
      v2Exec.stdout.includes(`__V2__:v2_hot_reloaded:${tc5Data.pid}`),
      `Expected hot-reloaded v2 in same PID ${tc5Data.pid}, got: ${v2Exec.stdout}`
    );
    await client.deleteFile(remoteProbeRel);
    console.log(
      `  ✓ TC-6 Passed: Live module updated from "v1" -> "v2_hot_reloaded" via %autoreload 2 in same kernel PID ${tc5Data.pid}\n`
    );

    // ------------------------------------------------------------------
    // TC-7: Notebook (.ipynb) Output Protection
    // ------------------------------------------------------------------
    console.log('▶ TC-7: Notebook (.ipynb) Output Protection...');
    const remoteNbPath = `${TEST_REMOTE_DIR}/examples/torch_tpu/torch_tpu_training.ipynb`;
    const nbModel = await client.getContents(remoteNbPath, true);
    assert.ok(nbModel && nbModel.content, 'Remote notebook must exist');
    const nbJson =
      typeof nbModel!.content === 'string' ? JSON.parse(nbModel!.content) : nbModel!.content;
    nbJson.metadata = nbJson.metadata || {};
    nbJson.metadata._remote_executed_marker = 'PROTECTED_OUTPUT_123';
    await client.putFile(remoteNbPath, JSON.stringify(nbJson), { overwriteNotebooks: true });

    await syncEngine.syncWorkspace({
      localRoot: WORKSPACE_ROOT,
      remoteBaseDir: TEST_REMOTE_DIR,
      overwriteNotebooks: false,
    });

    const nbAfter = await client.getContents(remoteNbPath, true);
    const nbAfterJson =
      typeof nbAfter!.content === 'string' ? JSON.parse(nbAfter!.content) : nbAfter!.content;
    assert.strictEqual(
      nbAfterJson.metadata?._remote_executed_marker,
      'PROTECTED_OUTPUT_123',
      'Remote executed .ipynb must not be overwritten when overwriteNotebooks=false'
    );
    console.log('  ✓ TC-7 Passed: Remote .ipynb outputs & metadata preserved across sync\n');

    // ------------------------------------------------------------------
    // TC-8: Local Coding Agent CLI Bridge (`jupyter-sync`)
    // ------------------------------------------------------------------
    console.log('▶ TC-8: Local Coding Agent IPC Bridge (`jupyter-sync`)...');
    let barrierFlushed = false;
    syncEngine.setPendingFlushHook(async () => {
      barrierFlushed = true;
    });

    const bridge = new AgentBridgeServer({
      workspaceRoot: WORKSPACE_ROOT,
      getClient: () => client,
      getSyncEngine: () => syncEngine,
      getKernelInitializer: () => kernelInitializer,
      getRemoteBaseDir: () => TEST_REMOTE_DIR,
      getSetKernelWorkingDirectory: () => true,
      getEnableAutoreload: () => true,
      getAutoSaveOutputs: () => false,
      getKernelForNotebook: () => testKernelId!,
      triggerSyncNow: async () =>
        await syncEngine.syncWorkspace({
          localRoot: WORKSPACE_ROOT,
          remoteBaseDir: TEST_REMOTE_DIR,
        }),
    });

    const sockPath = await bridge.start();
    try {
      const statusRes = await callAgentBridge(sockPath, 'status');
      assert.strictEqual(statusRes.connected, true);

      const execRes = await callAgentBridge(sockPath, 'exec', {
        code: 'import tpu_lock; print("BRIDGE_TPU_LOCK_OK")',
        notebookPath: 'examples/torch_tpu/torch_tpu_training.ipynb',
      });
      assert.strictEqual(barrierFlushed, true, 'Pre-execution sync barrier must have flushed');
      assert.strictEqual(execRes.status, 'ok');
      assert.ok(execRes.stdout.includes('BRIDGE_TPU_LOCK_OK'));

      const shRes = await callAgentBridge(sockPath, 'sh', {
        command: 'python3 tpu_lock.py --status',
        cwd: 'examples/torch_tpu',
      });
      assert.strictEqual(shRes.exitCode, 0, `tpu_lock.py --status failed: ${shRes.stdout}`);
      console.log(
        `  ✓ TC-8 Passed: IPC Bridge exec & remote shell ('python3 tpu_lock.py --status') succeeded:\n    ${shRes.stdout
          .trim()
          .split('\n')
          .join('\n    ')}\n`
      );
    } finally {
      await bridge.stop();
    }
  } finally {
    // ------------------------------------------------------------------
    // TC-9: Teardown & Cleanup
    // ------------------------------------------------------------------
    console.log('▶ TC-9: Teardown & Cleanup...');
    if (createdTempKernelId) {
      await client.shutdownKernel(createdTempKernelId).catch(() => {});
    } else if (testKernelId) {
      await client
        .executeCode(
          testKernelId,
          'import os, sys; os.chdir(getattr(sys, "_jupyter_sync_server_root", os.path.expanduser("~")))',
          { silent: true, storeHistory: false, timeoutMs: 10000 }
        )
        .catch(() => {});
    }
    await client.executeShellCommand(`rm -rf ${JSON.stringify(TEST_REMOTE_DIR)}`).catch(() => {});
    const stillExists = await client.fileExists(TEST_REMOTE_DIR);
    assert.strictEqual(stillExists, false, 'Temporary test directory must be cleaned up');
    console.log(`  ✓ TC-9 Passed: Cleaned up <jupyter-root>/${TEST_REMOTE_DIR}\n`);
  }

  console.log('================================================================');
  console.log('  ALL 9 LIVE TPU INTEGRATION TEST CASES PASSED!');
  console.log('================================================================');
}

main().catch((err) => {
  console.error('\n✗ Live TPU Integration Test Failed:', err);
  process.exit(1);
});
