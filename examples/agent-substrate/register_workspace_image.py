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

"""Add (or remove) the "JupyterLab + AX" image option on existing WorkspaceKinds.

The platform's admission policy only allows Workspaces of a few WorkspaceKinds
(jupyterlab, jupyterlab-resumable, ...), so rather than registering a new kind,
this adds one more entry to an existing kind's image list. Users then pick
"JupyterLab + AX agents (CPU)" in the Workspace creation form.

Reads a WorkspaceKind as JSON on stdin and writes a JSON Patch on stdout:

    kubectl get workspacekind jupyterlab -o json \\
      | register_workspace_image.py --image REGISTRY/jupyterlab-ax:TAG \\
      | kubectl patch workspacekind jupyterlab --type=json --patch-file=/dev/stdin

With --remove, the patch drops the entry again. An empty output means there is
nothing to change.
"""

import argparse
import copy
import json
import sys

OPTION_ID = "jupyterlab-ax"
VALUES_PATH = "/spec/podTemplate/options/imageConfig/values"


def build_option(image, template):
    """The new imageConfig value, reusing the kind's own port ids."""
    ports = copy.deepcopy(template.get("spec", {}).get("ports") or [{"id": "jupyterlab", "port": 8888}])
    return {
        "id": OPTION_ID,
        "spawner": {
            "displayName": "JupyterLab + AX agents (CPU)",
            "description": "JupyterLab with the ax CLI and the agent-substrate walkthrough "
                           "(~/agent-substrate) preinstalled",
            # The kinds' POD_CONFIG filterRule only offers CPU pod sizes for
            # images labeled accelerator=cpu.
            "labels": [{"key": "accelerator", "value": "cpu"}],
        },
        "spec": {"image": image, "imagePullPolicy": "IfNotPresent", "ports": ports},
    }


def patch_for(kind, image=None, remove=False):
    values = (kind.get("spec", {}).get("podTemplate", {}).get("options", {})
              .get("imageConfig", {}).get("values"))
    if not values:
        raise SystemExit(f"WorkspaceKind {kind.get('metadata', {}).get('name')} has no imageConfig values")
    existing = [i for i, v in enumerate(values) if v.get("id") == OPTION_ID]
    if remove:
        # Highest index first so earlier removals don't shift later ones.
        return [{"op": "remove", "path": f"{VALUES_PATH}/{i}"} for i in sorted(existing, reverse=True)]
    option = build_option(image, values[0])
    if existing:
        if values[existing[0]] == option:
            return []
        return [{"op": "replace", "path": f"{VALUES_PATH}/{existing[0]}", "value": option}]
    return [{"op": "add", "path": f"{VALUES_PATH}/-", "value": option}]


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
