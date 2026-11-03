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

"""Distributed PyTorch MiniGPT Training with Kubeflow Trainer.

Supports seamless hardware switching between:
  1. NVIDIA Tesla T4 GPUs (CUDA with NCCL collective backend)
  2. Cloud TPU v5e (2x2 slice with TorchTPU 'tpu_dist' collective backend)

Pipeline contract:
  Reads:  gs://<bucket>/processed/train/shard-*.npz
          gs://<bucket>/processed/test/test.npz
  Writes: gs://<bucket>/model/minigpt.pt
          gs://<bucket>/model/config.json
          gs://<bucket>/model/metrics.json
"""

import glob
import json
import math
import os
import sys
import time
import numpy as np
import torch
import torch.distributed as dist
import torch.nn as nn
import torch.optim as optim
from torch.nn.parallel import DistributedDataParallel as DDP
from torch.utils.data import DataLoader, DistributedSampler, TensorDataset

# Ensure local jobs modules (model.py) are discoverable
SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
if SCRIPT_DIR not in sys.path:
    sys.path.insert(0, SCRIPT_DIR)
from model import MiniGPT

try:
    from google.cloud import storage
except ImportError:
    import subprocess
    subprocess.check_call([sys.executable, "-m", "pip", "install", "--quiet", "google-cloud-storage"])
    from google.cloud import storage


def init_distributed(accelerator: str):
    """Initializes distributed runtime for either GPU (CUDA/NCCL) or TPU (TorchTPU/tpu_dist)."""
    if accelerator == "tpu":
        import torch_tpu  # noqa: F401

        if not dist.is_initialized():
            dist.init_process_group(backend="tpu_dist")
        device = torch.device("tpu")
        dtype = torch.bfloat16
        local_rank = int(os.environ.get("LOCAL_RANK", "0"))
        print(f"[TorchTPU] Rank {dist.get_rank()}/{dist.get_world_size()} initialized on TPU core {local_rank}", flush=True)
    elif accelerator == "gpu":
        local_rank = int(os.environ.get("LOCAL_RANK", "0"))
        torch.cuda.set_device(local_rank)
        if not dist.is_initialized():
            dist.init_process_group(backend="nccl")
        device = torch.device(f"cuda:{local_rank}")
        dtype = torch.float32
        print(f"[CUDA] Rank {dist.get_rank()}/{dist.get_world_size()} initialized on GPU {local_rank} ({torch.cuda.get_device_name(local_rank)})", flush=True)
    else:
        # CPU Fallback
        if not dist.is_initialized():
            dist.init_process_group(backend="gloo")
        device = torch.device("cpu")
        dtype = torch.float32
        local_rank = 0
        print(f"[CPU] Rank {dist.get_rank()}/{dist.get_world_size()} initialized on CPU", flush=True)

    return device, dtype, local_rank


def download_shards(bucket_name: str, cache_dir: str = "/tmp/data"):
    """Downloads training and test shards from GCS to local worker disk atomically."""
    os.makedirs(cache_dir, exist_ok=True)
    client = storage.Client()
    bucket = client.bucket(bucket_name)

    train_blobs = list(bucket.list_blobs(prefix="processed/train/"))
    npz_blobs = [b for b in train_blobs if b.name.endswith(".npz")]
    if not npz_blobs:
        raise RuntimeError(f"No training shards found under gs://{bucket_name}/processed/train/")

    train_files = []
    for blob in npz_blobs:
        local_path = os.path.join(cache_dir, os.path.basename(blob.name))
        tmp_path = f"{local_path}.tmp.{os.getpid()}"
        if not os.path.exists(local_path):
            blob.download_to_filename(tmp_path)
            os.replace(tmp_path, local_path)
        train_files.append(local_path)

    test_blob = bucket.get_blob("processed/test/test.npz")
    test_file = None
    if test_blob:
        test_file = os.path.join(cache_dir, "test.npz")
        tmp_path = f"{test_file}.tmp.{os.getpid()}"
        if not os.path.exists(test_file):
            test_blob.download_to_filename(tmp_path)
            os.replace(tmp_path, test_file)

    return sorted(train_files), test_file


def load_dataset(file_paths):
    """Loads and concatenates .npz shards into PyTorch tensors."""
    all_inputs, all_targets = [], []
    for p in file_paths:
        data = np.load(p)
        # Support both 'inputs'/'targets' and 'images'/'labels' key naming
        inputs_key = "inputs" if "inputs" in data else "images"
        targets_key = "targets" if "targets" in data else "labels"
        all_inputs.append(torch.tensor(data[inputs_key], dtype=torch.long))
        all_targets.append(torch.tensor(data[targets_key], dtype=torch.long))

    x = torch.cat(all_inputs, dim=0)
    y = torch.cat(all_targets, dim=0)
    return TensorDataset(x, y)


def main():
    accelerator = os.environ.get("ACCELERATOR", "gpu").lower()
    bucket_name = os.environ.get("BUCKET_NAME") or os.environ.get("GCS_BUCKET")
    if not bucket_name:
        raise ValueError("BUCKET_NAME or GCS_BUCKET environment variable must be set")

    epochs = int(os.environ.get("EPOCHS", "2"))
    batch_size = int(os.environ.get("BATCH_SIZE", "32"))
    lr = float(os.environ.get("LEARNING_RATE", "3e-4"))
    block_size = int(os.environ.get("BLOCK_SIZE", "128"))
    vocab_size = int(os.environ.get("VOCAB_SIZE", "50257"))
    n_layer = int(os.environ.get("N_LAYER", "4"))
    n_head = int(os.environ.get("N_HEAD", "4"))
    n_embd = int(os.environ.get("N_EMBD", "128"))

    # 1. Initialize distributed device and process group
    device, dtype, local_rank = init_distributed(accelerator)
    rank = dist.get_rank()
    world_size = dist.get_world_size()

    # 2. Download data shards from GCS
    # Multi-worker synchronization: local_rank 0 on each node downloads the shards, others wait.
    cache_dir = "/tmp/data"
    if local_rank == 0:
        if rank == 0:
            print(f"[Rank {rank}] Downloading data shards from gs://{bucket_name}/processed/...", flush=True)
        download_shards(bucket_name, cache_dir=cache_dir)

    if world_size > 1:
        dist.barrier()

    train_files = sorted(glob.glob(os.path.join(cache_dir, "shard-*.npz")))
    if not train_files:
        raise RuntimeError(f"No training shards found under {cache_dir} on rank {rank}")
    test_file = os.path.join(cache_dir, "test.npz")
    if not os.path.exists(test_file):
        test_file = None

    train_dataset = load_dataset(train_files)

    sampler = DistributedSampler(train_dataset, num_replicas=world_size, rank=rank, shuffle=True)
    train_loader = DataLoader(train_dataset, batch_size=batch_size, sampler=sampler, pin_memory=(accelerator == "gpu"))

    test_loader = None
    if test_file and os.path.exists(test_file):
        test_dataset = load_dataset([test_file])
        test_loader = DataLoader(test_dataset, batch_size=batch_size, shuffle=False)

    if rank == 0:
        print(f"[Rank {rank}] Loaded {len(train_dataset)} training examples across {len(train_files)} shards", flush=True)

    # 3. Instantiate model and wrap in DDP
    model = MiniGPT(
        vocab_size=vocab_size,
        block_size=block_size,
        n_layer=n_layer,
        n_head=n_head,
        n_embd=n_embd,
    ).to(device=device, dtype=dtype)

    if accelerator == "gpu":
        ddp_model = DDP(model, device_ids=[local_rank])
    else:
        # For TPU, device_ids is None
        ddp_model = DDP(model)

    optimizer = optim.AdamW(ddp_model.parameters(), lr=lr, weight_decay=0.01)

    # 4. Training loop
    if rank == 0:
        print(f"\n--- Starting {epochs} Epochs on {world_size} {accelerator.upper()} workers ---", flush=True)

    start_time = time.time()
    step_count = 0
    total_tokens = 0

    for epoch in range(1, epochs + 1):
        sampler.set_epoch(epoch)
        ddp_model.train()
        epoch_loss = 0.0
        num_batches = 0

        for x, y in train_loader:
            x, y = x.to(device), y.to(device)
            optimizer.zero_grad()
            _, loss = ddp_model(x, targets=y)
            loss.backward()
            torch.nn.utils.clip_grad_norm_(ddp_model.parameters(), max_norm=1.0)
            optimizer.step()

            batch_loss = loss.item()
            epoch_loss += batch_loss
            num_batches += 1
            step_count += 1
            total_tokens += x.size(0) * x.size(1) * world_size

            if rank == 0 and (step_count % 10 == 0 or step_count == 1):
                elapsed = time.time() - start_time
                tok_per_sec = total_tokens / max(elapsed, 1e-5)
                print(f"[Epoch {epoch}/{epochs} | Step {step_count}] Loss: {batch_loss:.4f} | Throughput: {tok_per_sec:.0f} tokens/sec", flush=True)

        avg_train_loss = epoch_loss / max(num_batches, 1)
        if rank == 0:
            print(f"[Epoch {epoch} Complete] Average Train Loss: {avg_train_loss:.4f}", flush=True)

    elapsed_time = time.time() - start_time
    if rank == 0:
        print(f"\nTraining completed in {elapsed_time:.2f} seconds ({step_count} total steps)", flush=True)

    # 5. Distributed Evaluation pass
    eval_loss = None
    if test_loader:
        ddp_model.eval()
        total_eval_loss = torch.tensor(0.0, device=device)
        eval_batches = torch.tensor(0.0, device=device)
        with torch.no_grad():
            for x, y in test_loader:
                x, y = x.to(device), y.to(device)
                _, l = ddp_model(x, targets=y)
                total_eval_loss += l
                eval_batches += 1.0
        if world_size > 1:
            dist.all_reduce(total_eval_loss, op=dist.ReduceOp.SUM)
            dist.all_reduce(eval_batches, op=dist.ReduceOp.SUM)
        eval_loss = (total_eval_loss / max(eval_batches.item(), 1.0)).item()
        if rank == 0:
            perplexity = math.exp(min(eval_loss, 20.0))
            print(f"[Evaluation] Held-out Cross-Entropy Loss: {eval_loss:.4f} | Perplexity: {perplexity:.2f}", flush=True)

    # 6. Save model weights to GCS
    # Extract model and convert state dict to CPU Float32 on all ranks
    raw_model = ddp_model.module if hasattr(ddp_model, "module") else ddp_model
    cpu_state_dict = {k: v.cpu().float() for k, v in raw_model.state_dict().items()}

    if world_size > 1:
        dist.barrier()

    if rank == 0:
        print(f"[Rank 0] Saving model weights and metadata to gs://{bucket_name}/model/...", flush=True)
        tmp_model_path = "/tmp/minigpt.pt"
        torch.save(cpu_state_dict, tmp_model_path)

        config_data = {
            "vocab_size": vocab_size,
            "block_size": block_size,
            "n_layer": n_layer,
            "n_head": n_head,
            "n_embd": n_embd,
            "trained_on": accelerator,
            "training_time_seconds": round(elapsed_time, 2),
            "eval_loss": round(eval_loss, 4) if eval_loss else None,
            "epochs": epochs,
            "world_size": world_size,
        }
        tmp_config_path = "/tmp/config.json"
        with open(tmp_config_path, "w") as f:
            json.dump(config_data, f, indent=2)

        client = storage.Client()
        bucket = client.bucket(bucket_name)

        print(f"[Rank 0] Uploading model weights to gs://{bucket_name}/model/minigpt.pt...", flush=True)
        bucket.blob("model/minigpt.pt").upload_from_filename(tmp_model_path)
        bucket.blob("model/config.json").upload_from_filename(tmp_config_path)

        metrics_data = {
            "train_loss": round(avg_train_loss, 4),
            "eval_loss": round(eval_loss, 4) if eval_loss else None,
            "throughput_tokens_per_sec": round(total_tokens / max(elapsed_time, 1e-5), 2),
        }
        tmp_metrics_path = "/tmp/metrics.json"
        with open(tmp_metrics_path, "w") as f:
            json.dump(metrics_data, f, indent=2)
        bucket.blob("model/metrics.json").upload_from_filename(tmp_metrics_path)

        print(f"[Rank 0] Successfully uploaded model artifacts to gs://{bucket_name}/model/", flush=True)

    if world_size > 1:
        dist.barrier()
    dist.destroy_process_group()
    print(f"[Rank {rank}] Finished and exited cleanly.", flush=True)


if __name__ == "__main__":
    main()
