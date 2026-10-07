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

"""High-level abstraction for launching multi-host TorchTPU training jobs on GKE.

Encapsulates all Kubernetes infrastructure details (ConfigMaps, volume mounts,
node selectors, tolerations, and Kubeflow Trainer SDK patches) so data
scientists can launch distributed training jobs with a single Python function.
"""

import os
import pathlib
import re
import shutil
import time
from typing import Dict, Optional, Tuple, Union

from kubernetes import client as k8s_client, config as k8s_config
from kubeflow.common.types import KubernetesBackendConfig
from kubeflow.trainer import CustomTrainerContainer, TrainerClient
from kubeflow.trainer.options import (
    ContainerPatch,
    JobSetSpecPatch,
    JobSetTemplatePatch,
    JobSpecPatch,
    JobTemplatePatch,
    Name,
    PodSpecPatch,
    PodTemplatePatch,
    ReplicatedJobPatch,
    RuntimePatch,
    TrainerCommand,
    TrainingRuntimeSpecPatch,
)


def get_current_namespace() -> str:
    """Discovers the active Kubernetes namespace from environment or serviceaccount."""
    if os.environ.get("TENANT_NAMESPACE"):
        return os.environ["TENANT_NAMESPACE"]
    ns_path = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
    if os.path.exists(ns_path):
        with open(ns_path, encoding="utf-8") as f:
            return f.read().strip()
    return "default"


def _init_k8s_clients():
    """Initializes in-cluster or kube-config clients."""
    try:
        k8s_config.load_incluster_config()
    except Exception:
        k8s_config.load_kube_config()
    return k8s_client.CoreV1Api(), k8s_client.CustomObjectsApi()


def _expand_k8s_vars(expr: str, extra_vars: Optional[Dict[str, str]] = None) -> str:
    """Expands Kubernetes $(VAR) and shell ${VAR} expressions."""
    if not expr:
        return ""
    lookup = dict(os.environ)
    if extra_vars:
        lookup.update(extra_vars)
    expanded = re.sub(
        r"\$\(([^)]+)\)",
        lambda m: lookup.get(m.group(1), m.group(0)),
        expr,
    )
    return os.path.expandvars(expanded)


def resolve_shared_pvc_subpath(
    src_dir: Union[str, pathlib.Path],
    namespace: str,
    pvc_name: str = "shared-workspace-rwx",
    shared_mount_path: str = "/home/jovyan/shared",
) -> Tuple[str, str, pathlib.Path]:
    """Resolves the PVC claimName and subPath for a source directory on a shared RWX volume.

    If `src_dir` is already inside `shared_mount_path` (for example, when synced via
    `jupyter-workspace-sync` with `"jupyterSync.remoteBaseDir": "shared/${workspaceFolderBasename}"`),
    this function computes its exact `subPath` on the PVC without copying any files.
    If `src_dir` is outside `shared_mount_path` (e.g., on the local RWO home disk), it
    syncs `src_dir` into `shared_mount_path` so the TPU worker pods can mount it.

    Returns:
        Tuple of (pvc_claim_name, pvc_sub_path, effective_src_path).
    """
    src_path = pathlib.Path(src_dir).resolve()
    if not src_path.exists():
        raise FileNotFoundError(f"Source directory '{src_dir}' not found.")

    mount_root = pathlib.Path(shared_mount_path).resolve()
    base_sub_path = ""
    detected_pvc = pvc_name

    # Inspect current Workspace Pod mounts if running in-cluster
    pod_name = os.environ.get("HOSTNAME")
    if pod_name:
        try:
            core_v1, _ = _init_k8s_clients()
            pod = core_v1.read_namespaced_pod(name=pod_name, namespace=namespace)
            ws_name = (pod.metadata.labels or {}).get(
                "notebooks.kubeflow.org/workspace-name",
                os.environ.get("WORKSPACE_NAME", ""),
            )
            extra_vars = {"WORKSPACE_NAME": ws_name} if ws_name else {}

            vol_to_pvc = {
                v.name: v.persistent_volume_claim.claim_name
                for v in (pod.spec.volumes or [])
                if v.persistent_volume_claim and v.persistent_volume_claim.claim_name
            }

            best_score = (-1, -1)
            for container in pod.spec.containers or []:
                for vm in container.volume_mounts or []:
                    if vm.name not in vol_to_pvc:
                        continue
                    vm_path = pathlib.Path(vm.mount_path).resolve()
                    contains_src = src_path == vm_path or vm_path in src_path.parents
                    is_configured_root = vm_path == mount_root
                    # Match either a mount covering src_path or the configured shared_mount_path
                    if contains_src or is_configured_root:
                        score = (1 if contains_src else 0, len(str(vm_path)))
                        if score > best_score:
                            best_score = score
                            mount_root = vm_path
                            detected_pvc = vol_to_pvc[vm.name]
                            raw_sub = vm.sub_path or vm.sub_path_expr or ""
                            base_sub_path = _expand_k8s_vars(raw_sub, extra_vars)
        except Exception:
            pass

    if not base_sub_path and os.environ.get("WORKSPACE_NAME"):
        base_sub_path = f"workspaces/{os.environ['WORKSPACE_NAME']}"

    # If src_path is outside the shared mount root, stage it into the shared mount root
    if src_path != mount_root and mount_root not in src_path.parents:
        if not mount_root.exists():
            raise FileNotFoundError(
                f"Shared RWX mount path '{mount_root}' does not exist in this pod, "
                f"and '{src_path}' is not on a shared PVC."
            )
        staged_dir = mount_root / src_path.name
        shutil.copytree(src_path, staged_dir, dirs_exist_ok=True)
        print(
            f"[RWX] Staged '{src_path}' -> shared volume at '{staged_dir}'"
        )
        src_path = staged_dir

    rel_from_mount = src_path.relative_to(mount_root).as_posix()
    if rel_from_mount == ".":
        rel_from_mount = ""

    parts = [p.strip("/") for p in (base_sub_path, rel_from_mount) if p and p.strip("/")]
    full_sub_path = "/".join(parts)
    return detected_pvc, full_sub_path, src_path


def submit_multihost_training(
    image: str,
    src_dir: Union[str, pathlib.Path] = "src",
    main_script: str = "train.py",
    num_nodes: int = 2,
    tpus_per_node: int = 4,
    compute_class: str = "tpu-v5-8-multi-host",
    namespace: Optional[str] = None,
    job_name: Optional[str] = None,
    extra_env: Optional[Dict[str, str]] = None,
    pvc_name: Optional[str] = None,
    shared_mount_path: str = "/home/jovyan/shared",
) -> str:
    """Packages or mounts local training code and submits a multi-host TrainJob on GKE.

    Args:
        image: Container image containing PyTorch and TorchTPU.
        src_dir: Directory containing user training scripts and modules.
        main_script: Entrypoint script relative to src_dir (e.g. "train.py").
        num_nodes: Number of TPU host nodes in the slice (e.g. 2).
        tpus_per_node: Physical TPU chips per host node (e.g. 4).
        compute_class: GKE TPU ComputeClass (e.g. "tpu-v5-8-multi-host").
        namespace: Target Kubernetes namespace.
        job_name: Optional custom job name (defaults to timestamped name).
        extra_env: Optional dictionary of additional environment variables.
        pvc_name: Optional ReadWriteMany PVC name (e.g. "shared-workspace-rwx").
            When provided, mounts the shared PVC (and resolved subdirectory) directly
            at `/workspace` on all TPU worker pods instead of creating a ConfigMap.
        shared_mount_path: Mount path of the shared RWX PVC inside the Workspace pod
            (default: "/home/jovyan/shared").

    Returns:
        The submitted TrainJob name.
    """
    if not image:
        raise ValueError("An 'image' must be provided for the TPU training container.")
    namespace = namespace or get_current_namespace()
    job_name = job_name or f"torch-tpu-multihost-{int(time.time())}"
    core_v1, _ = _init_k8s_clients()

    src_path = pathlib.Path(src_dir)
    if not src_path.exists():
        raise FileNotFoundError(f"Source directory '{src_dir}' not found.")

    # 1. Configure code volume (Shared ReadWriteMany PVC vs. Kubernetes ConfigMap)
    if pvc_name:
        claim_name, sub_path, effective_src = resolve_shared_pvc_subpath(
            src_dir=src_path,
            namespace=namespace,
            pvc_name=pvc_name,
            shared_mount_path=shared_mount_path,
        )
        entrypoint_path = effective_src / main_script
        if not entrypoint_path.exists():
            raise ValueError(
                f"Main script '{main_script}' not found in '{effective_src}'."
            )
        code_volume = {
            "name": "code-volume",
            "persistentVolumeClaim": {
                "claimName": claim_name,
            },
        }
        code_volume_mount = {
            "name": "code-volume",
            "mountPath": "/workspace",
        }
        if sub_path:
            code_volume_mount["subPath"] = sub_path
        print(
            f"[K8s] Mounting shared RWX PVC '{claim_name}' "
            f"(subPath='{sub_path or '/'}') at /workspace on all TPU worker pods"
        )
    else:
        cm_name = f"{job_name}-code"
        cm_data = {
            file_path.name: file_path.read_text(encoding="utf-8")
            for file_path in sorted(src_path.glob("*.py"))
        }
        if not cm_data:
            raise ValueError(f"No Python files found in '{src_dir}'.")
        if main_script not in cm_data:
            raise ValueError(
                f"Main script '{main_script}' not found in '{src_dir}' ({list(cm_data.keys())})."
            )

        cm_body = k8s_client.V1ConfigMap(
            metadata=k8s_client.V1ObjectMeta(name=cm_name, namespace=namespace),
            data=cm_data,
        )
        core_v1.create_namespaced_config_map(namespace=namespace, body=cm_body)
        print(
            f"[K8s] Created ConfigMap '{cm_name}' with {len(cm_data)} files: {list(cm_data.keys())}"
        )
        code_volume = {
            "name": "code-volume",
            "configMap": {
                "name": cm_name,
            },
        }
        code_volume_mount = {
            "name": "code-volume",
            "mountPath": "/workspace",
        }

    # 2. Define Trainer container specification
    merged_env = {"PYTHONPATH": "/workspace"}
    if extra_env:
        merged_env.update(extra_env)

    trainer = CustomTrainerContainer(
        image=image,
        num_nodes=num_nodes,
        resources_per_node={"google.com/tpu": str(tpus_per_node)},
        env=merged_env,
    )

    # 3. Attach GKE accelerator patch and code volume mount
    tpu_patch = RuntimePatch(
        training_runtime_spec=TrainingRuntimeSpecPatch(
            template=JobSetTemplatePatch(
                spec=JobSetSpecPatch(
                    replicated_jobs=[
                        ReplicatedJobPatch(
                            name="node",
                            template=JobTemplatePatch(
                                spec=JobSpecPatch(
                                    template=PodTemplatePatch(
                                        spec=PodSpecPatch(
                                            node_selector={
                                                "cloud.google.com/compute-class": compute_class,
                                            },
                                            tolerations=[
                                                {
                                                    "key": "google.com/tpu",
                                                    "operator": "Exists",
                                                    "effect": "NoSchedule",
                                                },
                                                {
                                                    "key": "cloud.google.com/compute-class",
                                                    "operator": "Exists",
                                                    "effect": "NoSchedule",
                                                },
                                            ],
                                            volumes=[code_volume],
                                            containers=[
                                                ContainerPatch(
                                                    name="node",
                                                    volume_mounts=[code_volume_mount],
                                                )
                                            ],
                                        )
                                    )
                                )
                            )
                        )
                    ]
                )
            )
        )
    )

    # 4. Submit via Kubeflow TrainerClient
    # Note: Kubeflow Trainer's 'torch-distributed' runtime automatically injects
    # PET_NNODES, PET_NODE_RANK, PET_MASTER_ADDR, and PET_MASTER_PORT, which
    # torchrun reads natively.
    trainer_client = TrainerClient(
        backend_config=KubernetesBackendConfig(namespace=namespace)
    )
    submitted_name = trainer_client.train(
        runtime="torch-distributed",
        trainer=trainer,
        options=[
            Name(job_name),
            TrainerCommand(
                [
                    "torchrun",
                    f"--nproc_per_node={tpus_per_node}",
                    f"/workspace/{main_script}",
                ]
            ),
            tpu_patch,
        ],
    )
    print(f"Successfully submitted multi-host TrainJob '{submitted_name}' (hosts={num_nodes}, chips={num_nodes * tpus_per_node})")
    return submitted_name


def wait_for_job_pods(
    job_name: str,
    num_nodes: int = 2,
    timeout: int = 600,
    namespace: Optional[str] = None,
) -> bool:
    """Monitors worker Pods until all reach Running or Succeeded status."""
    namespace = namespace or get_current_namespace()
    core_v1, _ = _init_k8s_clients()

    print(f"Waiting for worker pods of TrainJob '{job_name}' to become Running...")
    start_time = time.time()

    while time.time() - start_time < timeout:
        pods = core_v1.list_namespaced_pod(
            namespace=namespace,
            label_selector=f"jobset.sigs.k8s.io/jobset-name={job_name}",
        ).items
        if len(pods) >= num_nodes:
            status_str = ", ".join(
                f"{p.metadata.name.split('-')[-3]}-{p.metadata.name.split('-')[-2]}: {p.status.phase}"
                for p in pods
            )
            print(f"[{int(time.time() - start_time):3d}s] Pod statuses: {status_str}", flush=True)
            if all(p.status.phase in ("Running", "Succeeded") for p in pods):
                print(f"\nAll {num_nodes} worker pods are active and Running!", flush=True)
                return True
        else:
            print(f"[{int(time.time() - start_time):3d}s] Waiting for pods to be created...", flush=True)
        time.sleep(5)

    print("\nWarning: Not all pods reached Running phase within timeout.")
    return False


def stream_job_logs(
    job_name: str,
    num_nodes: int = 2,
    max_iterations: int = 60,
    namespace: Optional[str] = None,
):
    """Streams and filters distributed logs from all worker pods."""
    namespace = namespace or get_current_namespace()
    core_v1, _ = _init_k8s_clients()

    print(f"Streaming distributed training logs for TrainJob '{job_name}'...\n")
    completed_phases = {"Succeeded", "Failed"}
    logged_lines = {}

    for _ in range(max_iterations):
        pods = core_v1.list_namespaced_pod(
            namespace=namespace,
            label_selector=f"jobset.sigs.k8s.io/jobset-name={job_name}",
        ).items

        all_completed = len(pods) >= num_nodes and all(p.status.phase in completed_phases for p in pods)

        for p in sorted(pods, key=lambda x: x.metadata.name):
            pod_name = p.metadata.name
            if pod_name not in logged_lines:
                logged_lines[pod_name] = 0
            try:
                raw_log = core_v1.read_namespaced_pod_log(
                    name=pod_name,
                    namespace=namespace,
                    container="node",
                )
                lines = raw_log.splitlines()
                if len(lines) > logged_lines[pod_name]:
                    new_lines = lines[logged_lines[pod_name]:]
                    logged_lines[pod_name] = len(lines)
                    for line in new_lines:
                        if any(skip in line for skip in ["cachednslookup", "compilation_cache", "OMP_NUM_THREADS", "TcMallocWarning"]):
                            continue
                        prefix = f"[{pod_name.split('-')[-3]}-{pod_name.split('-')[-2]}]"
                        print(f"{prefix:12s} {line}", flush=True)
            except Exception:
                pass

        if all_completed:
            print(f"\nAll {num_nodes} worker pods have successfully finished execution!", flush=True)
            break

        time.sleep(3)


def delete_training_job(
    job_name: str,
    namespace: Optional[str] = None,
    delete_configmap: bool = True,
):
    """Deletes the TrainJob and associated code ConfigMap."""
    namespace = namespace or get_current_namespace()
    core_v1, _ = _init_k8s_clients()
    trainer_client = TrainerClient(
        backend_config=KubernetesBackendConfig(namespace=namespace)
    )

    try:
        trainer_client.delete_job(job_name)
        print(f"Deleted TrainJob: {job_name}")
    except Exception as e:
        print(f"Error deleting TrainJob: {e}")

    if delete_configmap:
        cm_name = f"{job_name}-code"
        try:
            core_v1.delete_namespaced_config_map(name=cm_name, namespace=namespace)
            print(f"Deleted ConfigMap: {cm_name}")
        except Exception:
            pass
