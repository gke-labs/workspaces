# Copyright 2026 Google LLC
"""Small statistics helpers."""

def median(values):
    if not values:
        raise ValueError("median() of empty sequence")
    s = sorted(values)
    mid = len(s) // 2
    if len(s) % 2 == 1:
        return s[mid]
    return (s[mid] + s[mid + 1]) / 2
