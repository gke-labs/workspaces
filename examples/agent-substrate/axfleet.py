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

"""Drive a fleet of AX agent Tasks on Agent Substrate from a GKE Workspace.

A thin, stdlib-only wrapper around the ``ax`` CLI (AX has no Python SDK) plus
the read-only actor view served by ``fleet-helper``. The walkthrough notebook
imports it; you can also use it from a terminal::

    from axfleet import Fleet
    fleet = Fleet()
    fleet.apply(fleet.render(["roman", "slugify"], mode="scripted"))
    fleet.resume_all()
    print(fleet.leaderboard())
"""

import concurrent.futures as cf
import difflib
import hashlib
import json
import os
import shutil
import stat
import subprocess
import time
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
PROJECT_DIR = os.path.join(HERE, "project")
NS_FILE = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

TASK_PREFIX = "fix-"
WORKSPACE_NAME = "textkit"
WORKSPACE_PATH = "/workspace/textkit"
RESULT_PATH = "/workspace/result.json"

GOAL_TEMPLATE = (
    "You are fixing one bug in the textkit Python library in this directory. "
    "Read AGENTS.md first and follow it. Your assigned test module is "
    "tests.{test}. Make `python3 -m unittest -q tests.{test}` pass by changing "
    "only the code in {module}. Do not modify tests/ or .pristine/. Finish by "
    "running the full suite with `python3 -m unittest discover -s tests -t .`."
)


def current_namespace():
    try:
        with open(NS_FILE) as f:
            return f.read().strip()
    except OSError:
        return os.environ.get("TENANT_NAMESPACE", "kubeflow-user")


def ensure_ax_cli(uri=None, dest=os.path.expanduser("~/.local/bin/ax")):
    """Make sure the ``ax`` CLI is on PATH, downloading it if needed.

    ``deploy_agent_substrate.sh`` uploads a Linux build to
    ``gs://$GCS_BUCKET/tools/ax``; Workspace pods can read that bucket through
    the namespace-wide Workload Identity binding the platform already sets up.
    Set ``AX_CLI_URI`` to use another location (``gs://`` or ``https://``).
    """
    found = shutil.which("ax")
    if found:
        return found
    if os.path.exists(dest):
        _add_to_path(os.path.dirname(dest))
        return dest
    uri = uri or os.environ.get("AX_CLI_URI")
    if not uri:
        if not os.environ.get("GCS_BUCKET"):
            raise RuntimeError(
                "ax CLI not found and $GCS_BUCKET is not set. Set AX_CLI_URI to where "
                "deploy_agent_substrate.sh uploaded it (gs://<bucket>/tools/ax).")
        uri = f"gs://{os.environ['GCS_BUCKET']}/tools/ax"
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    if uri.startswith("gs://"):
        from google.cloud import storage  # baked into the Workspace images

        bucket, _, blob = uri[len("gs://"):].partition("/")
        storage.Client().bucket(bucket).blob(blob).download_to_filename(dest)
    else:
        urllib.request.urlretrieve(uri, dest)
    os.chmod(dest, os.stat(dest).st_mode | stat.S_IXUSR | stat.S_IXGRP)
    _add_to_path(os.path.dirname(dest))
    return dest


def _add_to_path(d):
    if d not in os.environ.get("PATH", "").split(os.pathsep):
        os.environ["PATH"] = d + os.pathsep + os.environ.get("PATH", "")


def _bugs():
    import sys

    sys.path.insert(0, PROJECT_DIR)
    from tools.bugs import BUGS  # noqa: E402

    return BUGS


class Fleet:
    """One atespace's worth of AX Tasks, as seen from a Workspace."""

    def __init__(self, atespace=None, server=None, helper_url=None, ax_namespace=None):
        self.atespace = atespace or current_namespace()
        ax_ns = ax_namespace or os.environ.get("AX_SYSTEM_NAMESPACE", "ax-system")
        self.server = server or os.environ.get("AX_SERVER") or f"ax-server.{ax_ns}.svc.cluster.local:8080"
        self.helper_url = helper_url or os.environ.get(
            "FLEET_HELPER_URL", f"http://fleet-helper.{ax_ns}.svc.cluster.local:8080")
        self.bugs = _bugs()

    # ------------------------------------------------------------------ ax CLI
    def ax(self, *args, check=True, timeout=600, stdin_data=None):
        cmd = ["ax", "--server", self.server, "-a", self.atespace, *args]
        proc = subprocess.run(
            cmd, input=stdin_data, capture_output=True, text=True, timeout=timeout,
            stdin=None if stdin_data is not None else subprocess.DEVNULL,
        )
        if check and proc.returncode != 0:
            raise RuntimeError(f"{' '.join(cmd)} failed ({proc.returncode}):\n{proc.stderr or proc.stdout}")
        return proc

    def version(self):
        return self.ax("version").stdout.strip()

    # --------------------------------------------------------------- manifests
    def render(self, bugs=None, mode="scripted", image=None, cpu="1", memory="1Gi"):
        """Render the Workspace and one Task per bug as a multi-document YAML.

        The project files travel inside the Workspace's ``files`` list, so the
        agents need no Git access and no egress to fetch code.
        """
        bugs = list(bugs or self.bugs)
        files = []
        for root, dirs, names in os.walk(PROJECT_DIR):
            dirs[:] = [d for d in dirs if d != "__pycache__"]
            for name in sorted(names):
                full = os.path.join(root, name)
                rel = os.path.relpath(full, PROJECT_DIR)
                with open(full) as f:
                    content = f.read()
                files.append({"path": rel, "content": content})

        docs = [{
            "apiVersion": "ax.io/v1alpha1",
            "kind": "Workspace",
            "metadata": {"name": WORKSPACE_NAME, "atespace": self.atespace},
            "spec": {"files": files},
        }]
        for bug in bugs:
            module, test = self.bugs[bug]
            binding = {"name": WORKSPACE_NAME, "path": WORKSPACE_PATH}
            if mode == "gemini":
                binding["goal"] = GOAL_TEMPLATE.format(test=test.split(".", 1)[1], module=module)
            spec = {
                "command": ["sh", "run_task.sh", bug, mode],
                "resources": {
                    "requests": {"cpu": "250m", "memory": "512Mi"},
                    "limits": {"cpu": cpu, "memory": memory},
                },
                "workspaces": [binding],
                "debug": True,  # serve guest services so `ax ssh` works
            }
            if image:
                spec["image"] = image
            docs.append({
                "apiVersion": "ax.io/v1alpha1",
                "kind": "Task",
                "metadata": {"name": TASK_PREFIX + bug, "atespace": self.atespace},
                "spec": spec,
            })
        # JSON is valid YAML, which keeps this module free of a PyYAML dependency.
        return "\n---\n".join(json.dumps(d, indent=1) for d in docs) + "\n"

    def apply(self, manifest):
        return self.ax("apply", "-f", "-", stdin_data=manifest).stdout

    # --------------------------------------------------------------- lifecycle
    def tasks(self):
        """{name: phase} for this fleet's tasks (parsed from `ax get tasks`)."""
        out = self.ax("get", "tasks").stdout.splitlines()
        result = {}
        for line in out[1:]:
            cols = line.split()
            if len(cols) >= 3 and cols[0].startswith(TASK_PREFIX):
                result[cols[0]] = cols[2]
        return result

    def _parallel(self, verb, names, workers=8):
        def one(name):
            t0 = time.time()
            proc = self.ax(verb, "task", name, check=False)
            return name, proc.returncode == 0, round(time.time() - t0, 1), (proc.stderr or proc.stdout).strip()

        with cf.ThreadPoolExecutor(max_workers=workers) as pool:
            return list(pool.map(one, names))

    def resume(self, names):
        return self._parallel("resume", names)

    def suspend(self, names):
        return self._parallel("suspend", names)

    def resume_all(self):
        return self.resume(sorted(self.tasks()))

    def suspend_all(self):
        return self.suspend(sorted(n for n, p in self.tasks().items() if p == "Running"))

    def delete_all(self):
        names = sorted(self.tasks())
        out = self._parallel("delete", names)
        self.ax("delete", "workspace", WORKSPACE_NAME, check=False)
        return out

    # ------------------------------------------------------------- inspection
    def ssh(self, task, *command, timeout=120):
        return self.ax("ssh", task, "--", *command, check=False, timeout=timeout)

    def result(self, task):
        """The task's result.json, or None while the agent is still working."""
        proc = self.ssh(task, "cat", RESULT_PATH)
        if proc.returncode != 0:
            return None
        try:
            return json.loads(proc.stdout)
        except json.JSONDecodeError:
            return None

    def actors(self):
        """Actor state and worker placement, from fleet-helper."""
        url = f"{self.helper_url}/v1/actors?atespace={self.atespace}"
        with urllib.request.urlopen(url, timeout=10) as resp:
            return json.load(resp)

    def egress_ready(self, names):
        have = {a["name"]: a["egressPolicy"] for a in self.actors().get("actors") or []}
        return all(have.get(n) for n in names)

    def collect(self, names=None):
        """{task: result-or-None} for running tasks, fetched in parallel."""
        phases = self.tasks()
        names = names or sorted(phases)
        running = [n for n in names if phases.get(n) == "Running"]
        with cf.ThreadPoolExecutor(max_workers=8) as pool:
            fetched = dict(zip(running, pool.map(self.result, running)))
        return {n: fetched.get(n) for n in names}

    # ----------------------------------------------------------- evaluation
    def pristine(self, subdir):
        """{relative path: content} of the local, untouched project files."""
        out = {}
        for root, dirs, names in os.walk(os.path.join(PROJECT_DIR, subdir)):
            dirs[:] = [d for d in dirs if d != "__pycache__"]
            for name in sorted(names):
                if name.endswith(".py"):
                    full = os.path.join(root, name)
                    with open(full) as f:
                        out[os.path.relpath(full, PROJECT_DIR)] = f.read()
        return out

    def patch(self, result):
        """Unified diff between the pristine library and the task's sources."""
        if not result:
            return ""
        before = self.pristine("textkit")
        after = result.get("sources") or {}
        chunks = []
        for rel in sorted(set(before) | set(after)):
            chunks.extend(difflib.unified_diff(
                before.get(rel, "").splitlines(keepends=True),
                after.get(rel, "").splitlines(keepends=True),
                f"a/{rel}", f"b/{rel}"))
        return "".join(chunks)

    def tests_modified(self, result):
        want = {rel: hashlib.sha256(src.encode()).hexdigest()
                for rel, src in self.pristine("tests").items()}
        return (result or {}).get("testHashes", want) != want

    def leaderboard(self, results=None):
        """A pandas DataFrame summarizing every task's outcome."""
        import pandas as pd

        results = results if results is not None else self.collect()
        phases = self.tasks()
        rows = []
        for name in sorted(results):
            r = results[name]
            suite = (r or {}).get("suite") or {}
            patch = self.patch(r)
            tampered = self.tests_modified(r) if r else None
            rows.append({
                "task": name,
                "phase": phases.get(name, "?"),
                "mode": (r or {}).get("mode", ""),
                "fixed": (r.get("targetTestsPass") and not tampered) if r else None,
                "tests_modified": tampered,
                "modules_passing": f"{sum(suite.values())}/{len(suite)}" if suite else "",
                "patch_lines": len([ln for ln in patch.splitlines()
                                    if ln[:1] in "+-" and ln[:3] not in ("+++", "---")]),
                "seconds": (r or {}).get("seconds"),
            })
        return pd.DataFrame(rows)

    def wait_for_results(self, names, timeout=900, poll=10, on_poll=None):
        deadline = time.time() + timeout
        results = {}
        while time.time() < deadline:
            results = self.collect(names)
            if on_poll:
                on_poll(results)
            if all(results.get(n) for n in names):
                return results
            time.sleep(poll)
        return results
