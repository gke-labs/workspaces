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

"""Multi-host distributed PyTorch DDP training entrypoint for TorchTPU."""

import os
import time
import torch
import torch.distributed as dist
import torch.nn as nn
import torch.optim as optim
from torch.nn.parallel import DistributedDataParallel as DDP

# Import modular libraries mounted via Kubernetes ConfigMap
from dataset import get_distributed_dataloader
from models import MLPClassifier

# Ensure torch_tpu backend is registered
try:
    import torch_tpu  # noqa: F401
except ImportError:
    pass


def main():
    # 1. Initialize process group with tpu_dist backend
    if not dist.is_initialized():
        dist.init_process_group(backend="tpu_dist")
    device = torch.device("tpu")

    rank = dist.get_rank()
    world_size = dist.get_world_size()
    host_id = os.environ.get("TPU_WORKER_ID", os.environ.get("NODE_RANK", "0"))
    local_rank = os.environ.get("LOCAL_RANK", "0")

    print(
        f"[Host {host_id} | LocalRank {local_rank} | GlobalRank {rank}/{world_size}] "
        f"Initialized TorchTPU DDP worker on device: {device}",
        flush=True,
    )

    # 2. Synchronize random seeds for model weights across all ranks
    torch.manual_seed(42)

    # 3. Model setup on TPU wrapped in DDP
    model = MLPClassifier().to(device=device, dtype=torch.bfloat16)
    ddp_model = DDP(model)

    # 4. Distributed DataLoader sharded across all ranks
    loader, sampler = get_distributed_dataloader(
        batch_size=128,
        num_samples=16384,
        world_size=world_size,
        rank=rank,
    )

    loss_fn = nn.CrossEntropyLoss()
    optimizer = optim.SGD(ddp_model.parameters(), lr=0.05)

    epochs = 5
    total_start = time.time()

    for epoch in range(1, epochs + 1):
        sampler.set_epoch(epoch)
        epoch_start = time.time()
        running_loss = 0.0
        total_samples = 0

        for batch_images, batch_labels in loader:
            batch_images = batch_images.to(device=device, dtype=torch.bfloat16)
            batch_labels = batch_labels.to(device=device)

            optimizer.zero_grad()
            outputs = ddp_model(batch_images)
            loss = loss_fn(outputs, batch_labels)
            loss.backward()
            optimizer.step()

            # Execute .item() synchronously across all ranks to prevent divergence
            current_loss = loss.detach().item()
            running_loss += current_loss * batch_images.size(0)
            total_samples += batch_images.size(0)

        epoch_loss = running_loss / total_samples
        duration = time.time() - epoch_start
        throughput = (total_samples * world_size) / duration

        if rank == 0:
            print(
                f"Epoch {epoch:2d}/{epochs:2d} | Avg Loss: {epoch_loss:.4f} | "
                f"Throughput: {throughput:.0f} samples/s ({duration:.2f}s)",
                flush=True,
            )

    if rank == 0:
        print(
            f"\n[Rank 0] Multi-host distributed training successfully finished "
            f"in {time.time() - total_start:.2f}s across {world_size} TPU cores!",
            flush=True,
        )

    dist.destroy_process_group()


if __name__ == "__main__":
    main()
