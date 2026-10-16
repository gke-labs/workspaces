# Copyright 2026 Google LLC
"""Splitting sequences into fixed-size pieces."""

def chunk(items, size):
    if size <= 0:
        raise ValueError("size must be positive")
    items = list(items)
    return [items[i:i + size] for i in range(0, len(items) - 1, size)]
