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

"""TPU Hardware Lock Management & Pre-Flight Diagnostics.

Cloud TPU hardware character devices (/dev/vfio/* and /dev/accel*) are opened
exclusively by libtpu/PJRT when PyTorch (TorchTPU) or JAX initializes the TPU runtime.
Because libtpu retains open file descriptors in the C++ runtime for the entire lifetime
of the process, standard Python garbage collection (del tensor, gc.collect) cannot
release the hardware locks.

This module provides utilities to:
1. Detect whether TPU hardware character devices are currently locked by other sessions.
2. Check if the active Python process / Jupyter kernel is holding TPU devices.
3. Terminate orphaned / zombie processes (e.g. from interrupted torchrun runs).
4. Restart or shut down the active Jupyter kernel to release TPU locks.
"""

from __future__ import annotations

import argparse
import os
import pathlib
import signal
import time
from typing import Any, Dict, List, Optional, Set, Tuple


def get_tpu_devices() -> List[str]:
    """Returns a sorted list of TPU character device paths on this host."""
    devices: Set[str] = set()

    # Cloud TPU v4, v5e, v5p, v6e allocate VFIO group device nodes (/dev/vfio/0, 1, ...)
    vfio_dir = pathlib.Path("/dev/vfio")
    if vfio_dir.exists():
        for p in vfio_dir.iterdir():
            if p.name != "vfio":  # Skip the VFIO container control node
                try:
                    devices.add(str(p.resolve()))
                except Exception:
                    pass

    # Some systems / newer kernels expose TPUs via /dev/accel*
    dev_dir = pathlib.Path("/dev")
    for p in dev_dir.glob("accel*"):
        try:
            devices.add(str(p.resolve()))
        except Exception:
            pass

    return sorted(list(devices))


def is_tpu_device(path_str: str) -> bool:
    """Returns True if the given path string corresponds to a TPU character device."""
    if path_str.startswith("/dev/vfio/") and path_str != "/dev/vfio/vfio":
        return True
    if path_str.startswith("/dev/accel"):
        return True
    return False


def find_tpu_processes(
    exclude_pids: Optional[Set[int]] = None,
) -> List[Dict[str, Any]]:
    """Scans /proc to find processes holding open file descriptors to TPU devices
    or running distributed worker scripts.
    """
    if exclude_pids is None:
        exclude_pids = set()

    tpu_procs: List[Dict[str, Any]] = []
    proc_root = pathlib.Path("/proc")
    if not proc_root.exists():
        return tpu_procs

    for pid_dir in proc_root.iterdir():
        if not pid_dir.name.isdigit():
            continue
        pid = int(pid_dir.name)
        if pid in exclude_pids:
            continue

        fd_dir = pid_dir / "fd"
        open_devices: Set[str] = set()
        if fd_dir.exists():
            try:
                for fd in fd_dir.iterdir():
                    try:
                        target = os.readlink(fd)
                        if is_tpu_device(target):
                            open_devices.add(target)
                    except (OSError, PermissionError):
                        continue
            except (OSError, PermissionError):
                continue

        # Check process command line
        cmdline = ""
        comm = ""
        try:
            cmdline_bytes = (pid_dir / "cmdline").read_bytes()
            cmdline = cmdline_bytes.replace(b"\x00", b" ").decode(errors="ignore").strip()
        except Exception:
            pass

        try:
            comm = (pid_dir / "comm").read_text().strip()
        except Exception:
            pass

        is_worker = (
            "train_worker.py" in cmdline
            or "torch.distributed.run" in cmdline
            or "/torchrun " in cmdline
            or cmdline.startswith("torchrun ")
        )

        if open_devices or is_worker:
            tpu_procs.append({
                "pid": pid,
                "comm": comm,
                "cmdline": cmdline,
                "devices": sorted(list(open_devices)),
                "is_worker": is_worker,
            })

    return tpu_procs


def is_current_kernel_holding_tpu() -> Tuple[bool, List[str]]:
    """Checks if the current Python process (Jupyter kernel) is holding any TPU devices."""
    current_pid = os.getpid()
    fd_dir = pathlib.Path(f"/proc/{current_pid}/fd")
    holding_devices: Set[str] = set()

    if fd_dir.exists():
        try:
            for fd in fd_dir.iterdir():
                try:
                    target = os.readlink(fd)
                    if is_tpu_device(target):
                        holding_devices.add(target)
                except (OSError, PermissionError):
                    continue
        except (OSError, PermissionError):
            pass

    return bool(holding_devices), sorted(list(holding_devices))


def check_tpu_locks(verbose: bool = True) -> List[Dict[str, Any]]:
    """Checks whether TPU hardware is locked by another process (excluding current process)."""
    current_pid = os.getpid()
    locked_procs = find_tpu_processes(exclude_pids={current_pid})

    if verbose:
        devices = get_tpu_devices()
        print(f"Hardware Discovery: Found {len(devices)} TPU device node(s) on host: {devices or 'None detected'}")
        if locked_procs:
            print(f"⚠️ TPU is currently locked by {len(locked_procs)} external process(es):")
            for p in locked_procs:
                devs = ", ".join(p["devices"]) if p["devices"] else "worker process"
                print(f"  • PID {p['pid']} [{p['comm']}]: {devs} | CMD: {p['cmdline'][:80]}")
        else:
            print("✅ TPU hardware is free — no conflicting processes found.")

    return locked_procs


def clear_tpu_locks(verbose: bool = True) -> bool:
    """Terminates conflicting processes holding TPU devices (excluding current process).

    Returns True if TPU devices are successfully cleared.
    """
    current_pid = os.getpid()
    procs = find_tpu_processes(exclude_pids={current_pid})

    if not procs:
        if verbose:
            print("✅ TPU hardware is already free. No action needed.")
        return True

    if verbose:
        print(f"⚠️ Terminating {len(procs)} conflicting process(es)...")

    # Step 1: Send SIGTERM for graceful exit
    for p in procs:
        try:
            os.kill(p["pid"], signal.SIGTERM)
            if verbose:
                print(f"  Sent SIGTERM to PID {p['pid']} ({p['comm']})")
        except (ProcessLookupError, PermissionError):
            pass

    time.sleep(1.0)

    # Step 2: Send SIGKILL to any remaining processes
    remaining = find_tpu_processes(exclude_pids={current_pid})
    if remaining:
        if verbose:
            print(f"  {len(remaining)} process(es) still active. Sending SIGKILL...")
        for p in remaining:
            try:
                os.kill(p["pid"], signal.SIGKILL)
                if verbose:
                    print(f"  Force-killed PID {p['pid']}")
            except (ProcessLookupError, PermissionError):
                pass
        time.sleep(0.5)
        remaining = find_tpu_processes(exclude_pids={current_pid})

    if not remaining:
        if verbose:
            print("✅ All TPU locks successfully cleared!")
        return True
    else:
        if verbose:
            print(f"❌ Warning: Could not terminate {len(remaining)} process(es):")
            for p in remaining:
                print(f"  • PID {p['pid']}: {p['cmdline'][:80]}")
        return False


def release_current_kernel(restart: bool = True, verbose: bool = True) -> None:
    """Releases exclusive TPU hardware locks held by this Jupyter kernel by restarting
    or shutting down the kernel process.
    """
    if verbose:
        action = "Restarting" if restart else "Shutting down"
        print(f"🔄 {action} Python kernel (PID {os.getpid()}) to release TPU hardware locks...")

    try:
        from IPython import get_ipython

        ip = get_ipython()
        if ip and hasattr(ip, "kernel"):
            ip.kernel.do_shutdown(restart=restart)
            return
    except Exception:
        pass

    # Fallback: terminate process directly
    os._exit(0)


def preflight_check(is_distributed: bool = False, auto_clear: bool = False, verbose: bool = True) -> bool:
    """Comprehensive pre-flight check before running a TPU notebook session."""
    if verbose:
        print("🔍 Running TPU Hardware Pre-Flight Check...")

    locked_procs = check_tpu_locks(verbose=verbose)
    if locked_procs:
        if auto_clear:
            if verbose:
                print("Auto-clearing conflicting locks...")
            if not clear_tpu_locks(verbose=verbose):
                return False
        else:
            if verbose:
                print("❌ TPU hardware is busy. Call `tpu_lock.clear_tpu_locks()` to release it.")
            return False

    if is_distributed:
        holding, devs = is_current_kernel_holding_tpu()
        if holding:
            if verbose:
                print(
                    f"\n🚨 ALERT: This active Jupyter kernel (PID {os.getpid()}) is currently holding TPU device(s): {devs}"
                )
                print("Because libtpu retains exclusive hardware locks for the life of the process,")
                print("child worker processes spawned by `torchrun` will fail with 'Device or resource busy'.")
                print("\n👉 Solution: Restart this kernel now:")
                print("   Kernel -> Restart Kernel (or execute `tpu_lock.release_current_kernel(restart=True)`)")
                print("   Then re-run this notebook without initializing torch.device('tpu') in the kernel.\n")
            return False

    if verbose:
        print("✅ TPU hardware pre-flight check passed! Hardware is ready for execution.")
    return True


def main() -> None:
    parser = argparse.ArgumentParser(description="Cloud TPU Hardware Lock Management Utility")
    parser.add_argument("--status", action="store_true", help="Check TPU device nodes and lock status")
    parser.add_argument("--clear", action="store_true", help="Force clear external processes holding TPU locks")
    parser.add_argument(
        "--distributed", action="store_true", help="Validate environment for distributed torchrun execution"
    )

    args = parser.parse_args()

    if args.clear:
        clear_tpu_locks(verbose=True)
    elif args.distributed:
        preflight_check(is_distributed=True, verbose=True)
    else:
        check_tpu_locks(verbose=True)
        holding, devs = is_current_kernel_holding_tpu()
        if holding:
            print(f"Note: Current process (PID {os.getpid()}) is holding: {devs}")


if __name__ == "__main__":
    main()
