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

"""Render images/workspacekinds/*.yaml for a fresh deployment without custom images.

deploy_standalone.sh uses this to register the example WorkspaceKinds right after the
platform comes up, before anyone has run images/build.sh:

  * The custom `<name>-cpu` / `<name>-gpu` / `<name>-tpu` image options are pointed at
    the public upstream Kubeflow base images they are built FROM. There is no TPU
    variant upstream, so the TPU option uses the CPU base image (no libtpu / jax[tpu];
    `pip install "jax[tpu]"` inside the Workspace if you need it). The imageConfig ids
    are kept unchanged, so re-applying the templates later with custom images (see
    images/README.md) is an in-place upgrade for existing Workspaces.
  * GPU / TPU podConfig options keep their `cloud.google.com/compute-class` node
    selectors, which work on both Autopilot and Standard clusters. The ComputeClasses
    themselves come from examples/compute-classes/ (deploy_standalone.sh applies them).

Usage:
  render_workspacekinds.py TEMPLATE [TEMPLATE...] > workspacekinds.json

Environment:
  REGION, PROJECT_ID, REPO_NAME, GCS_BUCKET   substituted into the templates
  Base images per template, see BASE_IMAGES below; overridable
    via JUPYTERLAB_CPU_BASE_IMAGE, JUPYTERLAB_GPU_BASE_IMAGE,
    CODESERVER_CPU_BASE_IMAGE, CODESERVER_GPU_BASE_IMAGE
"""

import json
import os
import string
import sys

import yaml

UPSTREAM = "ghcr.io/kubeflow/kubeflow/notebook-servers"

# Same base images (and digests) as images/<name>/Dockerfile{,.gpu,.tpu}.
BASE_IMAGES = {
    "jupyterlab": {
        "cpu": os.environ.get(
            "JUPYTERLAB_CPU_BASE_IMAGE",
            f"{UPSTREAM}/jupyter-scipy:v1.11.0"
            "@sha256:8f8a000b7eece82c7e874924dc45a3922e9197137eefe67f55c711cc3ce068fd",
        ),
        "gpu": os.environ.get(
            "JUPYTERLAB_GPU_BASE_IMAGE",
            f"{UPSTREAM}/jupyter-pytorch-cuda-full:v1.11.0"
            "@sha256:55f6abbe52112317570b9cd10967bc8e4dc724eaff7030bc92d97a2e097dce84",
        ),
    },
    "codeserver": {
        "cpu": os.environ.get(
            "CODESERVER_CPU_BASE_IMAGE", f"{UPSTREAM}/codeserver-python:v1.11.0"
        ),
        # Upstream has no CUDA code-server image; the GPU driver libraries are still
        # mounted from the node via LD_LIBRARY_PATH=/usr/local/nvidia/lib64.
        "gpu": os.environ.get(
            "CODESERVER_GPU_BASE_IMAGE", f"{UPSTREAM}/codeserver-python:v1.11.0"
        ),
    },
}


def render(path):
    with open(path, encoding="utf-8") as f:
        text = f.read()

    # The image coordinates are replaced below, so feed placeholders for them.
    values = {k: os.environ[k] for k in ("REGION", "PROJECT_ID", "REPO_NAME", "GCS_BUCKET")}
    values.update(
        IMAGE_NAME="placeholder",
        CPU_IMAGE_TAG="placeholder",
        GPU_IMAGE_TAG="placeholder",
        TPU_IMAGE_TAG="placeholder",
    )
    # substitute() (not safe_substitute) so an unknown ${VAR} fails loudly instead of
    # rendering an empty string the way envsubst would.
    doc = yaml.safe_load(string.Template(text).substitute(values))

    kind_name = doc["metadata"]["name"]
    if kind_name not in BASE_IMAGES:
        sys.exit(f"{path}: no base images configured for WorkspaceKind '{kind_name}'")
    base = BASE_IMAGES[kind_name]
    # TPU: no upstream TPU image, use the CPU base image.
    base_for = {"cpu": base["cpu"], "gpu": base["gpu"], "tpu": base["cpu"]}

    options = doc["spec"]["podTemplate"]["options"]

    for image in options["imageConfig"]["values"]:
        spec = image["spec"]
        if not spec["image"].endswith(":placeholder"):
            continue  # already a public image (the "(Upstream)" entries)
        accel = next(
            l["value"] for l in image["spawner"]["labels"] if l["key"] == "accelerator"
        )
        base_image = base_for[accel]
        spec["image"] = base_image
        short = base_image.rsplit("/", 1)[-1].split("@", 1)[0]
        spawner = image["spawner"]
        spawner["displayName"] = f"{short} ({accel.upper()})"
        spawner["description"] = f"Upstream Kubeflow base image {short}" + (
            " (CPU image: no libtpu/jax[tpu] preinstalled)" if accel == "tpu" else ""
        )
        if accel == "gpu":
            # Version labels describe the custom build; keep only what still holds.
            spawner["labels"] = [l for l in spawner["labels"] if l["key"] == "accelerator"]

    return doc


def main():
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    items = [render(p) for p in sys.argv[1:]]
    json.dump({"apiVersion": "v1", "kind": "List", "items": items}, sys.stdout, indent=2)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
