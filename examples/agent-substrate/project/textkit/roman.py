# Copyright 2026 Google LLC
"""Roman numerals."""

VALUES = {"I": 1, "V": 5, "X": 10, "L": 50, "C": 100, "D": 500, "M": 1000}

def roman_to_int(numeral):
    total = 0
    prev = 0
    for ch in reversed(numeral.upper()):
        value = VALUES[ch]
        if value > prev:
            total -= value
        else:
            total += value
        prev = value
    return total
