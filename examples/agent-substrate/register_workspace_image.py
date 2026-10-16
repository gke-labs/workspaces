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

"""Add (or remove) the "Code Server + AX agents" image option on a WorkspaceKind.

The platform's admission policy only allows Workspaces of a few WorkspaceKinds
(codeserver, jupyterlab, ...), so rather than registering a new kind, this adds
one more entry to the existing `codeserver` kind's image list. Users then pick
"Code Server + AX agents (CPU)" in the Workspace creation form.

It also makes sure the kind passes GEMINI_API_KEY from the namespace's optional
`gemini-api-key` Secret into Workspaces, the same way the `jupyterlab` kind
does, so Gemini CLI works without any setup.

Reads a WorkspaceKind as JSON on stdin and writes a JSON Patch on stdout:

    kubectl get workspacekind codeserver -o json \\
      | register_workspace_image.py --image REGISTRY/codeserver-ax:TAG \\
      | kubectl patch workspacekind codeserver --type=json --patch-file=/dev/stdin

With --remove, the patch drops the image options again (the environment
variable is left alone). An empty output means there is nothing to change.

The Workspaces API does not allow changing the image of an option that a
Workspace uses, so every image gets its own option id (codeserver-ax-<tag>).
Registering a new image adds a new option and hides the older ones, with a
redirect that moves their Workspaces to the new image on their next restart.
"""

import argparse
import copy
import hashlib
import json
import re
import sys

OPTION_PREFIX = "codeserver-ax"
VALUES_PATH = "/spec/podTemplate/options/imageConfig/values"
ENV_PATH = "/spec/podTemplate/extraEnv"
GEMINI_ENV = {
    "name": "GEMINI_API_KEY",
    "valueFrom": {"secretKeyRef": {"name": "gemini-api-key", "key": "GEMINI_API_KEY", "optional": True}},
}


def option_id(image):
    """codeserver-ax-<tag> (or a digest prefix), unique per image reference."""
    ref = image.rsplit("/", 1)[-1]
    if "@" in ref:
        suffix = ref.split("@", 1)[1].split(":")[-1][:12]
    elif ":" in ref:
        suffix = ref.split(":", 1)[1]
    else:
        suffix = hashlib.sha256(image.encode()).hexdigest()[:12]
    suffix = re.sub(r"[^a-z0-9-]+", "-", suffix.lower()).strip("-")
    return f"{OPTION_PREFIX}-{suffix}"[:256]


def is_ours(value):
    return value.get("id") == OPTION_PREFIX or value.get("id", "").startswith(OPTION_PREFIX + "-")


def build_option(image, template):
    """The new imageConfig value, reusing the kind's own port ids."""
    ports = copy.deepcopy(template.get("spec", {}).get("ports") or [{"id": "codeserver", "port": 8888}])
    return {
        "id": option_id(image),
        "spawner": {
            "displayName": "Code Server + AX agents (CPU)",
            "description": "VS Code with Gemini CLI and the agent-substrate extension: delegate coding "
                           "tasks to background agents on Agent Substrate",
            # The kind's POD_CONFIG filterRule only offers CPU pod sizes for
            # images labeled accelerator=cpu.
            "labels": [{"key": "python_version", "value": "3.12"}, {"key": "accelerator", "value": "cpu"}],
            "hidden": False,  # the API server's default; set so re-runs see no diff
        },
        "spec": {"image": image, "imagePullPolicy": "IfNotPresent", "ports": ports},
    }


def patch_for(kind, image=None, remove=False):
    pod_template = kind.get("spec", {}).get("podTemplate", {})
    values = pod_template.get("options", {}).get("imageConfig", {}).get("values")
    if not values:
        raise SystemExit(f"WorkspaceKind {kind.get('metadata', {}).get('name')} has no imageConfig values")
    ours = [i for i, v in enumerate(values) if is_ours(v)]
    if remove:
        # Highest index first so earlier removals don't shift later ones.
        return [{"op": "remove", "path": f"{VALUES_PATH}/{i}"} for i in sorted(ours, reverse=True)]

    patch = []
    option = build_option(image, next((v for v in values if not is_ours(v)), values[0]))
    current = [i for i in ours if values[i]["id"] == option["id"]]
    if current:
        # Only spawner info may change on an option in use; the image is the same.
        if values[current[0]]["spawner"] != option["spawner"]:
            patch.append({"op": "replace", "path": f"{VALUES_PATH}/{current[0]}/spawner", "value": option["spawner"]})
    else:
        patch.append({"op": "add", "path": f"{VALUES_PATH}/-", "value": option})
    # Hide older images and send their Workspaces to this one on restart.
    redirect = {"to": option["id"], "message": {
        "level": "Info", "text": "A newer Code Server + AX agents image is available; it is used on the next restart."}}
    for i in ours:
        old = values[i]
        if old["id"] == option["id"]:
            continue
        if not old["spawner"].get("hidden"):
            patch.append({"op": "add", "path": f"{VALUES_PATH}/{i}/spawner/hidden", "value": True})
        if old.get("redirect") != redirect:
            patch.append({"op": "add", "path": f"{VALUES_PATH}/{i}/redirect", "value": redirect})

    env = pod_template.get("extraEnv")
    if env is None:
        patch.append({"op": "add", "path": ENV_PATH, "value": [GEMINI_ENV]})
    elif not any(e.get("name") == GEMINI_ENV["name"] for e in env):
        patch.append({"op": "add", "path": f"{ENV_PATH}/-", "value": GEMINI_ENV})
    return patch


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--image", help="image reference for the option (required unless --remove)")
    ap.add_argument("--remove", action="store_true", help="remove the option instead")
    args = ap.parse_args()
    if not args.remove and not args.image:
        ap.error("--image is required")
    patch = patch_for(json.load(sys.stdin), args.image, args.remove)
    if patch:
        json.dump(patch, sys.stdout)


if __name__ == "__main__":
    main()
