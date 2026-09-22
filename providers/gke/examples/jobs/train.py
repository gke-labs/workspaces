#!/usr/bin/env python3
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Distributed Model Training Stage (Stage 2 of the ML Demo).

Data-parallel JAX training across a multi-host Cloud TPU slice, launched by
Kubeflow Trainer as a `TrainJob`. Each TPU host runs this function; JAX's
`pmap` fans the computation out across the local TPU cores, and gradients are
averaged across *all* cores on *all* hosts with `jax.lax.pmean`.

Pipeline contract (shared ReadWriteMany volume mounted at DATA_ROOT):
  reads : /data/processed/train/shard-*.npz   (from Stage 1)
          /data/processed/test/test.npz
  writes: /data/model/params.npz              (final trained weights)
          /data/model/checkpoint-<step>.npz   (periodic checkpoints)
          /data/model/metrics.json            (final accuracy / loss)

The model is a small 2-layer MLP (784 -> 256 -> 10). It is intentionally small
so the demo trains in a couple of minutes while still exercising real
multi-host collective communication.
"""

import glob
import json
import os
import time


def train_fashion_mnist():
    import glob
    import json
    import os
    import time
    import subprocess
    import sys
    try:
        from google.cloud import storage
    except ImportError:
        subprocess.check_call([sys.executable, "-m", "pip", "install", "--quiet", "google-cloud-storage"])
        from google.cloud import storage
    import jax
    import jax.distributed as dist
    import jax.numpy as jnp
    import numpy as np
    from jax import grad, pmap
    from jax import random

    raw_pid = os.environ.get("JAX_PROCESS_ID", "")
    if not raw_pid.isdigit():
        raw_pid = os.environ.get("JOB_COMPLETION_INDEX", "0")
    pid_val = int(raw_pid) if raw_pid.isdigit() else 0

    # --- 1. Bring up the JAX distributed runtime across TPU hosts. ---
    dist.initialize(
        coordinator_address=os.environ["JAX_COORDINATOR_ADDRESS"],
        num_processes=int(os.environ["JAX_NUM_PROCESSES"]),
        process_id=pid_val,
    )

    process_id = jax.process_index()
    n_local = jax.local_device_count()
    n_global = jax.device_count()
    bucket_name = os.environ.get("BUCKET_NAME")
    if not bucket_name:
        raise ValueError("BUCKET_NAME environment variable must be set")
    epochs = int(os.environ.get("EPOCHS", "5"))
    lr = float(os.environ.get("LEARNING_RATE", "0.1"))
    global_batch = int(os.environ.get("GLOBAL_BATCH_SIZE", "1024"))

    print(f"[proc {process_id}] local TPU cores={n_local}, global cores={n_global}", flush=True)

    # --- 2. Load the preprocessed shards produced by Stage 1. ---
    client = storage.Client()
    bucket = client.bucket(bucket_name)
    
    blobs = list(bucket.list_blobs(prefix="processed/train/shard-"))
    if not blobs:
        raise FileNotFoundError(
            f"No processed shards under gs://{bucket_name}/processed/train — run Stage 1 first."
        )
    xs, ys = [], []
    for blob in sorted(blobs, key=lambda b: b.name):
        tmp_shard = f"/tmp/{os.path.basename(blob.name)}"
        blob.download_to_filename(tmp_shard)
        d = np.load(tmp_shard)
        xs.append(d["images"])
        ys.append(d["labels"])
        os.remove(tmp_shard)
    train_x = np.concatenate(xs).astype(np.float32)
    train_y = np.concatenate(ys).astype(np.float32)
    print(f"[proc {process_id}] loaded {train_x.shape[0]} train examples", flush=True)

    tmp_test = "/tmp/test.npz"
    bucket.blob("processed/test/test.npz").download_to_filename(tmp_test)
    test_d = np.load(tmp_test)
    test_x = np.asarray(test_d["images"], dtype=np.float32)
    test_y = np.asarray(test_d["labels"], dtype=np.float32)
    os.remove(tmp_test)

    # --- 3. Initialize model parameters and replicate across local cores. ---
    key = random.PRNGKey(0)
    k1, k2 = random.split(key)
    hidden = 256
    params = {
        "w1": random.normal(k1, (784, hidden)) * jnp.sqrt(2.0 / 784),
        "b1": jnp.zeros((hidden,)),
        "w2": random.normal(k2, (hidden, 10)) * jnp.sqrt(2.0 / hidden),
        "b2": jnp.zeros((10,)),
    }

    def replicate(tree):
        return jax.tree_util.tree_map(lambda x: jnp.broadcast_to(x, (n_local,) + x.shape), tree)

    def to_local_numpy(x):
        if hasattr(x, "addressable_shards") and len(x.addressable_shards) > 0:
            return np.asarray(x.addressable_shards[0].data)
        return np.asarray(x)

    def to_local_scalar(x):
        arr = to_local_numpy(x)
        return float(arr.flat[0])

    def unreplicate(tree):
        def _get_leaf(x):
            arr = to_local_numpy(x)
            return arr[0] if arr.ndim > 0 and arr.shape[0] == 1 else arr
        return jax.tree_util.tree_map(_get_leaf, tree)

    dev_params = replicate(params)

    def forward(p, x):
        h = jnp.maximum(x @ p["w1"] + p["b1"], 0.0)
        return h @ p["w2"] + p["b2"]

    def loss_fn(p, x, y):
        logits = forward(p, x)
        logp = logits - jax.scipy.special.logsumexp(logits, axis=-1, keepdims=True)
        return -jnp.mean(jnp.sum(y * logp, axis=-1))

    def eval_accuracy_fn(p, x, y):
        logits = forward(p, x)
        preds = jnp.argmax(logits, axis=-1)
        labels = jnp.argmax(y, axis=-1)
        return jnp.mean(preds == labels)

    eval_step_cpu = jax.jit(eval_accuracy_fn, backend="cpu")

    def train_step_fn(p, x, y):
        # Average gradients across ALL cores on ALL hosts, then apply SGD.
        g = grad(loss_fn)(p, x, y)
        g = jax.lax.pmean(g, axis_name="i")
        loss = jax.lax.pmean(loss_fn(p, x, y), axis_name="i")
        p = jax.tree_util.tree_map(lambda w, gw: w - lr * gw, p, g)
        return p, loss

    train_step = pmap(train_step_fn, axis_name="i")

    # --- 4. Training loop with data-parallel batches. ---
    # Each core gets global_batch / n_global examples per step.
    per_core = max(1, global_batch // n_global)
    step_batch = per_core * n_local  # examples consumed per host per step
    n = train_x.shape[0]
    rng = np.random.default_rng(process_id)

    ckpt_every = int(os.environ.get("CHECKPOINT_EVERY", "50"))
    global_step = 0
    t0 = time.time()

    for epoch in range(epochs):
        perm = rng.permutation(n)
        for i in range(0, n - step_batch + 1, step_batch):
            idx = perm[i:i + step_batch]
            xb = train_x[idx].reshape(n_local, per_core, 784)
            yb = train_y[idx].reshape(n_local, per_core, 10)
            dev_params, loss = train_step(dev_params, jnp.asarray(xb), jnp.asarray(yb))
            global_step += 1
            if global_step % 20 == 0 and process_id == 0:
                print(f"  epoch {epoch} step {global_step} loss={to_local_scalar(loss):.4f}", flush=True)
            # Rank 0 writes periodic checkpoints to GCS.
            if ckpt_every and global_step % ckpt_every == 0 and process_id == 0:
                tmp_ckpt = f"/tmp/checkpoint-{global_step}.npz"
                np.savez(tmp_ckpt, **unreplicate(dev_params))
                bucket.blob(f"model/checkpoint-{global_step}.npz").upload_from_filename(tmp_ckpt)
                os.remove(tmp_ckpt)

        # --- Evaluate on the held-out test set once per epoch (rank 0). ---
        if process_id == 0:
            host_params = unreplicate(dev_params)
            acc = eval_step_cpu(host_params, test_x, test_y)
            print(f"[proc 0] epoch {epoch} test_accuracy={float(acc):.4f}", flush=True)

    # --- 5. Persist the final model + metrics to GCS (rank 0). ---
    if process_id == 0:
        final = unreplicate(dev_params)
        tmp_params = "/tmp/params.npz"
        np.savez(tmp_params, **final)
        bucket.blob("model/params.npz").upload_from_filename(tmp_params)
        os.remove(tmp_params)
        
        final_acc = float(eval_step_cpu(final, test_x, test_y))
        metrics = {
            "final_test_accuracy": final_acc,
            "epochs": epochs,
            "global_batch_size": global_batch,
            "global_tpu_cores": int(n_global),
            "train_examples": int(train_x.shape[0]),
            "wall_time_seconds": round(time.time() - t0, 1),
        }
        tmp_metrics = "/tmp/metrics.json"
        with open(tmp_metrics, "w") as f:
            json.dump(metrics, f, indent=2)
        bucket.blob("model/metrics.json").upload_from_filename(tmp_metrics)
        os.remove(tmp_metrics)
        print(f"[proc 0] TRAINING COMPLETE: {json.dumps(metrics)}", flush=True)


if __name__ == "__main__":
    # Allows running the training function directly (e.g. single-host smoke test).
    train_fashion_mnist()
