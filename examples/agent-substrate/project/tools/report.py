# Copyright 2026 Google LLC
"""Write the task result: test pass status and source files."""

import argparse, hashlib, json, os, subprocess, sys, time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, ROOT)
from tools.bugs import BUGS

def run_tests(test_module):
    proc = subprocess.run(
        [sys.executable, "-B", "-m", "unittest", "-q", test_module],
        cwd=ROOT, capture_output=True, text=True, timeout=60,
    )
    return proc.returncode == 0, proc.stderr[-2000:]

def read_tree(subdir, hashes=False):
    out = {}
    for dirpath, dirs, files in os.walk(os.path.join(ROOT, subdir)):
        dirs[:] = [d for d in dirs if d != "__pycache__"]
        for name in sorted(files):
            if name.endswith(".py"):
                full = os.path.join(dirpath, name)
                rel = os.path.relpath(full, ROOT)
                with open(full, "rb") as f:
                    data = f.read()
                out[rel] = hashlib.sha256(data).hexdigest() if hashes else data.decode()
    return out

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--bug", required=True, choices=sorted(BUGS))
    ap.add_argument("--mode", default="scripted")
    ap.add_argument("--started", type=float, default=time.time())
    ap.add_argument("--out", default="/workspace/result.json")
    args = ap.parse_args()

    target_ok, target_log = run_tests(BUGS[args.bug][1])
    suite = {bug: run_tests(mod)[0] for bug, (_, mod) in sorted(BUGS.items())}

    result = {
        "bug": args.bug,
        "mode": args.mode,
        "targetTestsPass": target_ok,
        "suite": suite,
        "sources": read_tree("textkit"),
        "testHashes": read_tree("tests", hashes=True),
        "targetLog": "" if target_ok else target_log,
        "seconds": round(time.time() - args.started, 1),
        "finishedAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    }
    tmp = args.out + ".tmp"
    with open(tmp, "w") as f:
        json.dump(result, f, indent=2)
    os.replace(tmp, args.out)
    print(json.dumps({k: v for k, v in result.items() if k not in ("sources", "testHashes")}))

if __name__ == "__main__":
    main()
