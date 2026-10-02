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

"""Synthetic dataset generation and distributed DataLoader helpers."""

from typing import Tuple
import numpy as np
import torch
from torch.utils.data import DataLoader, DistributedSampler, TensorDataset


def generate_synthetic_data(
    num_samples: int = 16384,
    num_features: int = 784,
    num_classes: int = 10,
    seed: int = 42,
) -> Tuple[torch.Tensor, torch.Tensor]:
    """Generates synthetic clustered data for classification benchmarks."""
    np.random.seed(seed)
    prototypes = np.random.randn(num_classes, num_features).astype(np.float32)
    labels = np.random.randint(0, num_classes, size=(num_samples,))
    noise = np.random.randn(num_samples, num_features).astype(np.float32) * 0.5
    images = prototypes[labels] + noise
    return torch.tensor(images, dtype=torch.float32), torch.tensor(labels, dtype=torch.long)


def get_distributed_dataloader(
    batch_size: int = 128,
    num_samples: int = 16384,
    world_size: int = 1,
    rank: int = 0,
) -> Tuple[DataLoader, DistributedSampler]:
    """Builds a sharded DataLoader using DistributedSampler for DDP training."""
    images, labels = generate_synthetic_data(num_samples=num_samples, seed=42)
    dataset = TensorDataset(images, labels)
    sampler = DistributedSampler(dataset, num_replicas=world_size, rank=rank, shuffle=True)
    loader = DataLoader(dataset, batch_size=batch_size, sampler=sampler)
    return loader, sampler
