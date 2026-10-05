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

import functools
import logging
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

logger = logging.getLogger(__name__)


def _maybe_init_distributed_on_gke(
    slicebuilder_first_worker_port: int = 10000,
    environ: dict[str, str] | None = None,
) -> bool:
    """Initializes environment variables for distributed TPU training on GKE.

    This function configures environment variables required by SliceBuilder
    when running torch_tpu on GKE, based on the provided environment
    variables set by the Kubernetes setup. It adjusts bounds and addresses
    to match the expected configuration for distributed TPU workers.

    Args:
        slicebuilder_first_worker_port: The starting port number for SliceBuilder
            worker communication.
        environ: A dictionary of environment variables. Defaults to `os.environ`.

    Returns:
        True if the environment variables were successfully initialized for
        distributed TPU training on GKE, False otherwise.
    """
    if environ is None:
        environ = os.environ

    if (
        "TPU_WORKER_HOSTNAMES" not in environ
        or "TPU_CHIPS_PER_HOST_BOUNDS" not in environ
        or "TPU_HOST_BOUNDS" not in environ
        or "WORLD_SIZE" not in environ
        or "LOCAL_RANK" not in environ
        or "RANK" not in environ
    ):
        return False

    logger.info("Trying to initialize distributed TPU training on GKE.")
    world_size = int(environ["WORLD_SIZE"])
    local_rank = int(environ["LOCAL_RANK"])
    rank = int(environ["RANK"])
    is_v7 = (
        "TPU_ACCELERATOR_TYPE" in environ
        and environ["TPU_ACCELERATOR_TYPE"].startswith("tpu7x")
    )

    tpu_chips_per_host_bounds = list(
        map(int, environ["TPU_CHIPS_PER_HOST_BOUNDS"].split(","))
    )
    tpu_chips_per_host_bounds_product = functools.reduce(
        lambda x, y: x * y, tpu_chips_per_host_bounds
    )

    if tpu_chips_per_host_bounds_product == 1:
        return False  # either single host or environment is set manually.

    tpu_host_bounds = list(map(int, environ["TPU_HOST_BOUNDS"].split(",")))
    tpu_host_bounds_product = functools.reduce(
        lambda x, y: x * y, tpu_host_bounds
    )

    tpu_worker_hostnames = environ["TPU_WORKER_HOSTNAMES"].split(",")

    if tpu_host_bounds_product != len(tpu_worker_hostnames):
        return False  # environment is likely to set incorrectly.

    if is_v7:
        tpu_chips_per_host_bounds_product *= 2

    if tpu_chips_per_host_bounds_product * tpu_host_bounds_product != world_size:
        return False  # environment is likely to set incorrectly.

    environ["TPU_CHIPS_PER_HOST_BOUNDS"] = ",".join(
        ["1"] * (4 if is_v7 else 3)
    )
    for i in range(len(tpu_host_bounds)):
        tpu_host_bounds[i] = tpu_host_bounds[i] * tpu_chips_per_host_bounds[i]
    environ["TPU_HOST_BOUNDS"] = ",".join(map(str, tpu_host_bounds))
    if is_v7:
        environ["TPU_HOST_BOUNDS"] += ",2"

    environ["TPU_VISIBLE_CHIPS"] = str(local_rank)
    environ["CLOUD_TPU_TASK_ID"] = str(rank)
    ports = list(
        range(
            slicebuilder_first_worker_port,
            slicebuilder_first_worker_port + tpu_chips_per_host_bounds_product,
        )
    )
    environ["TPU_PROCESS_PORT"] = str(ports[local_rank])
    tpu_worker_addresses = []
    for tpu_worker_hostname in tpu_worker_hostnames:
        for port in ports:
            tpu_worker_addresses.append(tpu_worker_hostname + ":" + str(port))
    tpu_worker_addresses_str = ",".join(tpu_worker_addresses)
    environ["TPU_PROCESS_ADDRESSES"] = tpu_worker_addresses_str
    environ["TORCH_TPU_SLICEBUILDER_ADDRESSES"] = tpu_worker_addresses_str
    environ["TORCH_TPU_TOPOLOGY"] = environ["TPU_HOST_BOUNDS"]
    return True


def main():
    # 1. Initialize GKE multi-host TPU environment and process group
    _maybe_init_distributed_on_gke()
    if not dist.is_initialized():
        dist.init_process_group(backend="tpu_dist")
    device = torch.device("tpu")

    rank = dist.get_rank()
    world_size = dist.get_world_size()
    host_id = os.environ.get("TPU_WORKER_ID", os.environ.get("NODE_RANK", "0"))
    local_rank = os.environ.get("LOCAL_RANK", "0")

    print(
        f"[Host {host_id} | LocalRank {local_rank} | GlobalRank {rank}/{world_size}] "
        f"Initialized TorchTPU DDP worker on device: {device} "
        f"(TORCH_TPU_TOPOLOGY={os.environ.get('TORCH_TPU_TOPOLOGY')})",
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
