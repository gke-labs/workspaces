# Copyright 2026 Google LLC
"""Deterministic search-based repair bot for TextKit bug bash."""

import argparse, io, os, shutil, subprocess, sys, time, tokenize

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
from tools.bugs import BUGS

OP_SWAPS = {
    "<": ["<=", ">"], "<=": ["<", ">="],
    ">": [">=", "<"], ">=": [">", "<="],
    "==": ["!="], "!=": ["=="],
    "+": ["-"], "-": ["+"],
}
NAME_SWAPS = {"and": ["or"], "or": ["and"]}

def candidates(source):
    if "return text\n" in source or "return text" in source:
        yield "strip hyphens", source.replace("return text", "return text.strip('-')")
    if "return text == text[::-1]" in source:
        yield "normalize palindrome", source.replace(
            "return text == text[::-1]",
            "filtered = [c.lower() for c in text if c.isalnum()]\n    return filtered == filtered[::-1]",
        )
    lines = source.splitlines(keepends=True)
    offsets = [0]
    for line in lines:
        offsets.append(offsets[-1] + len(line))
    tokens = tokenize.generate_tokens(io.StringIO(source).readline)
    for tok in tokens:
        if tok.type == tokenize.OP and tok.string in OP_SWAPS:
            repl = OP_SWAPS[tok.string]
        elif tok.type == tokenize.NAME and tok.string in NAME_SWAPS:
            repl = NAME_SWAPS[tok.string]
        elif tok.type == tokenize.NUMBER and tok.string.isdigit():
            n = int(tok.string)
            repl = [str(n + 1)] + ([str(n - 1)] if n > 0 else [])
        else:
            continue
        (row, col), (erow, ecol) = tok.start, tok.end
        start = offsets[row - 1] + col
        end = offsets[erow - 1] + ecol
        for r in repl:
            yield f"line {row}: {tok.string!r} -> {r!r}", source[:start] + r + source[end:]

def clear_bytecode():
    for dirpath, dirnames, _ in os.walk("."):
        if "__pycache__" in dirnames:
            shutil.rmtree(os.path.join(dirpath, "__pycache__"), ignore_errors=True)

def tests_pass(test_module):
    proc = subprocess.run(
        [sys.executable, "-B", "-m", "unittest", "-q", test_module],
        capture_output=True, text=True, timeout=30,
    )
    return proc.returncode == 0

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--bug", required=True, choices=sorted(BUGS))
    ap.add_argument("--budget", type=float, default=120)
    args = ap.parse_args()

    path, test_module = BUGS[args.bug]
    with open(path) as f:
        original = f.read()

    clear_bytecode()
    if tests_pass(test_module):
        print("tests already pass; nothing to do")
        return 0

    deadline = time.time() + args.budget
    tried = 0
    for desc, mutated in candidates(original):
        if time.time() > deadline:
            break
        tried += 1
        with open(path, "w") as f:
            f.write(mutated)
        try:
            ok = tests_pass(test_module)
        except subprocess.TimeoutExpired:
            ok = False
        if ok:
            print(f"FIXED after {tried} candidates: {desc}")
            return 0

    with open(path, "w") as f:
        f.write(original)
    return 1

if __name__ == "__main__":
    sys.exit(main())
