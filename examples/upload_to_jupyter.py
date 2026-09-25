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

"""Uploads local .py and .yaml files to a remote Jupyter workspace preserving directory tree structure.

Designed for workflows where you develop locally in VS Code and execute against
a remote Jupyter kernel without needing `kubectl` access. Communicates directly
with the Jupyter Server's Contents REST API.

Features:
  - Automatically extracts token and base URL from full JupyterLab/Workspace links:
      https://connect.136.82.5.113.sslip.io/workspace/connect/kubeflow-user/ws-cpu/jupyterlab/?token=...
  - Defaults remote destination to the user's home directory (Jupyter root).
  - Preserves directory tree structure (e.g. jobs/train.py -> jobs/train.py).
  - Automatically creates intermediate remote directories.
  - Optional `--watch` mode to auto-sync files on save.

Usage:
    # One-time upload (using full URL with token):
    python examples/upload_to_jupyter.py "https://<host>/workspace/connect/.../?token=<token>" --dir examples/distributed

    # Continuous auto-sync on file save:
    python examples/upload_to_jupyter.py "https://<host>/workspace/connect/.../?token=<token>" --dir examples/distributed --watch

    # Custom local directory or target remote subdirectory:
    python examples/upload_to_jupyter.py "https://<host>/.../?token=..." --dir examples/distributed --remote-dir distributed
"""

import argparse
import os
import sys
import time
import urllib.parse
from pathlib import Path

try:
    import requests
except ImportError:
    print(
        "Error: 'requests' package is required. Install it with: pip install requests",
        file=sys.stderr,
    )
    sys.exit(1)

DEFAULT_EXTENSIONS = {".py", ".yaml", ".yml"}


def parse_jupyter_url(raw_url: str, explicit_token: str | None = None) -> tuple[str, str]:
    """Extract clean Jupyter base API URL and authentication token."""
    parsed = urllib.parse.urlparse(raw_url.strip())
    if not parsed.scheme or not parsed.netloc:
        raise ValueError(
            f"Invalid URL: '{raw_url}'. Expected a full URL like 'https://host/path?token=...'"
        )

    qs = urllib.parse.parse_qs(parsed.query)
    token = explicit_token or qs.get("token", [None])[0] or os.environ.get("JUPYTER_TOKEN", "")

    # Strip /lab, /tree, or /lab/tree... from the path if pasted from a browser tab
    path = parsed.path
    for suffix in ["/lab/tree", "/lab", "/tree"]:
        if path.endswith(suffix):
            path = path[:-len(suffix)]
            break
    path = path.rstrip("/")

    base_url = f"{parsed.scheme}://{parsed.netloc}{path}"
    return base_url, token


def create_session(base_url: str, token: str) -> requests.Session:
    """Create an authenticated requests session for the Jupyter REST API."""
    session = requests.Session()
    if token:
        # GKE access-proxy and Jupyter require 'token <token>' in Authorization header
        session.headers["Authorization"] = f"token {token}"

    # Initialize connection and capture any CSRF cookies
    try:
        resp = session.get(f"{base_url}/api/contents", timeout=15)
        if resp.status_code == 401:
            raise PermissionError(
                f"Unauthorized (401) at {base_url}. Check that the token is valid and not expired."
            )
        resp.raise_for_status()

        if "_xsrf" in session.cookies:
            session.headers["X-XSRFToken"] = session.cookies["_xsrf"]
    except requests.exceptions.RequestException as e:
        raise ConnectionError(f"Could not connect to Jupyter server at {base_url}: {e}") from e

    return session


def ensure_remote_dir(
    session: requests.Session, base_url: str, remote_dir: str, created_dirs: set[str]
):
    """Ensure that the remote directory and all its parent directories exist."""
    clean_dir = remote_dir.strip("/")
    if not clean_dir or clean_dir == "." or clean_dir in created_dirs:
        return

    # Ensure parent directory exists first
    parent = os.path.dirname(clean_dir)
    if parent and parent != ".":
        ensure_remote_dir(session, base_url, parent, created_dirs)

    endpoint = f"{base_url}/api/contents/{clean_dir}"
    try:
        resp = session.put(endpoint, json={"type": "directory"}, timeout=15)
        # 200/201: created; 400/409: already exists
        if resp.status_code in (200, 201, 400, 409):
            created_dirs.add(clean_dir)
        else:
            resp.raise_for_status()
    except requests.exceptions.RequestException as e:
        if getattr(e.response, "status_code", None) not in (400, 409):
            print(f"  [WARN] Failed to create remote directory '{clean_dir}': {e}", file=sys.stderr)
        created_dirs.add(clean_dir)


def upload_single_file(
    session: requests.Session, base_url: str, local_path: Path, remote_rel_path: str
):
    """Upload a single file to Jupyter Server via PUT /api/contents/<path>."""
    with open(local_path, "r", encoding="utf-8", errors="replace") as f:
        content = f.read()

    endpoint = f"{base_url}/api/contents/{remote_rel_path.strip('/')}"
    payload = {
        "content": content,
        "type": "file",
        "format": "text",
    }
    resp = session.put(endpoint, json=payload, timeout=20)
    resp.raise_for_status()
    print(f"  ✓ {remote_rel_path}")


def get_target_files(local_dir: Path, extensions: set[str]) -> list[Path]:
    """Find all files in local_dir matching the target extensions."""
    target_files = []
    for root, _, files in os.walk(local_dir):
        for file in sorted(files):
            p = Path(root) / file
            if p.suffix.lower() in extensions:
                target_files.append(p)
    return target_files


def sync_all(
    session: requests.Session,
    base_url: str,
    local_dir: Path,
    remote_base: str,
    extensions: set[str],
) -> int:
    """Sync all matching files from local_dir to remote_base maintaining directory tree."""
    created_dirs: set[str] = set()
    files = get_target_files(local_dir, extensions)

    destination_desc = f"~/{remote_base.strip('/')}" if remote_base.strip("/") else "~ (home directory)"
    print(f"\nUploading {len(files)} file(s) from '{local_dir}' to {destination_desc}:")

    count = 0
    for local_path in files:
        rel_path = local_path.relative_to(local_dir).as_posix()
        if remote_base.strip("/"):
            remote_rel_path = f"{remote_base.strip('/')}/{rel_path}"
        else:
            remote_rel_path = rel_path

        remote_parent_dir = os.path.dirname(remote_rel_path)
        if remote_parent_dir:
            ensure_remote_dir(session, base_url, remote_parent_dir, created_dirs)

        try:
            upload_single_file(session, base_url, local_path, remote_rel_path)
            count += 1
        except Exception as e:
            print(f"  ✗ {remote_rel_path}: {e}", file=sys.stderr)

    print(f"Successfully uploaded {count}/{len(files)} files.\n")
    return count


def watch_and_sync(
    session: requests.Session,
    base_url: str,
    local_dir: Path,
    remote_base: str,
    extensions: set[str],
    interval: float = 1.0,
):
    """Continuously monitor local_dir and upload files on change."""
    created_dirs: set[str] = set()
    mtimes: dict[Path, float] = {}

    # Perform initial sync
    sync_all(session, base_url, local_dir, remote_base, extensions)

    ext_list = ", ".join(sorted(extensions))
    print(f"Watching '{local_dir}' for changes ({ext_list})... (Press Ctrl+C to stop)")

    for p in get_target_files(local_dir, extensions):
        try:
            mtimes[p] = p.stat().st_mtime
        except OSError:
            pass

    while True:
        try:
            time.sleep(interval)
            current_files = get_target_files(local_dir, extensions)
            for local_path in current_files:
                try:
                    mtime = local_path.stat().st_mtime
                except OSError:
                    continue

                if local_path not in mtimes or mtime > mtimes[local_path]:
                    mtimes[local_path] = mtime
                    rel_path = local_path.relative_to(local_dir).as_posix()
                    if remote_base.strip("/"):
                        remote_rel_path = f"{remote_base.strip('/')}/{rel_path}"
                    else:
                        remote_rel_path = rel_path

                    remote_parent_dir = os.path.dirname(remote_rel_path)
                    if remote_parent_dir:
                        ensure_remote_dir(session, base_url, remote_parent_dir, created_dirs)

                    print(f"Change detected: {rel_path}")
                    try:
                        upload_single_file(session, base_url, local_path, remote_rel_path)
                    except Exception as e:
                        print(f"  ✗ Failed to upload {remote_rel_path}: {e}", file=sys.stderr)
        except KeyboardInterrupt:
            print("\nStopped watch mode.")
            break


def resolve_default_dir() -> Path:
    """Find default local directory: examples/distributed if in root, or . if inside distributed."""
    cwd = Path.cwd()
    if (cwd / "examples" / "distributed").is_dir():
        return cwd / "examples" / "distributed"
    if (cwd / "jobs").is_dir() and (cwd / "distributed_tpu_example.ipynb").is_file():
        return cwd
    if (cwd / "distributed").is_dir():
        return cwd / "distributed"
    return cwd


def main():
    parser = argparse.ArgumentParser(
        description="Upload .py and .yaml files to remote Jupyter server preserving tree structure."
    )
    parser.add_argument(
        "url",
        nargs="?",
        default=os.environ.get("JUPYTER_URL"),
        help="Jupyter Server URL with token (e.g. 'https://host/path/?token=...') or set JUPYTER_URL.",
    )
    parser.add_argument(
        "--url",
        dest="url_opt",
        help="Explicit Jupyter Server URL.",
    )
    parser.add_argument(
        "--token",
        default="",
        help="Explicit Jupyter token (optional; inferred from URL by default).",
    )
    parser.add_argument(
        "--dir",
        "-d",
        required=True,
        help="Local directory to upload (required, e.g. --dir examples/distributed).",
    )
    parser.add_argument(
        "--remote-dir",
        default="",
        help="Destination directory on remote Jupyter server (default: '' = home directory).",
    )
    parser.add_argument(
        "--ext",
        nargs="+",
        default=[".py", ".yaml", ".yml"],
        help="File extensions to upload (default: .py .yaml .yml).",
    )
    parser.add_argument(
        "--watch",
        action="store_true",
        help="Watch directory and auto-sync modified files on save.",
    )
    parser.add_argument(
        "--poll-interval",
        type=float,
        default=1.0,
        help="Poll interval in seconds for --watch (default: 1.0).",
    )

    args = parser.parse_args()
    raw_url = args.url_opt or args.url

    if not raw_url:
        parser.error(
            "Missing Jupyter URL. Provide it as an argument or set JUPYTER_URL.\n"
            "Example: python examples/upload_to_jupyter.py 'https://<host>/.../?token=...' --dir examples/distributed"
        )

    base_url, token = parse_jupyter_url(raw_url, explicit_token=args.token or None)
    if not token:
        print(
            "Warning: No token found in URL or arguments. Requests may be unauthorized.",
            file=sys.stderr,
        )

    local_dir = Path(args.dir).resolve()
    if not local_dir.is_dir():
        parser.error(f"Local directory does not exist: {local_dir}")

    extensions = {ext if ext.startswith(".") else f".{ext}".lower() for ext in args.ext}

    print(f"Connecting to Jupyter: {base_url}")
    session = create_session(base_url, token)

    if args.watch:
        watch_and_sync(
            session=session,
            base_url=base_url,
            local_dir=local_dir,
            remote_base=args.remote_dir,
            extensions=extensions,
            interval=args.poll_interval,
        )
    else:
        sync_all(
            session=session,
            base_url=base_url,
            local_dir=local_dir,
            remote_base=args.remote_dir,
            extensions=extensions,
        )


if __name__ == "__main__":
    main()
