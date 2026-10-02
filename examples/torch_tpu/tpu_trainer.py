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
import time
from typing import Dict, Optional, Union

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

DEFAULT_TPU_IMAGE = (
    "us-west1-docker.pkg.dev/sizhang-gke-dev/kubeflow-repo/jupyterlab:latest-tpu"
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


def submit_multihost_training(
    src_dir: Union[str, pathlib.Path] = "src",
    main_script: str = "train.py",
    num_nodes: int = 2,
    tpus_per_node: int = 4,
    compute_class: str = "tpu-v5-8-multi-host",
    torch_tpu_topology: str = "2,4,1",
    image: Optional[str] = None,
    namespace: Optional[str] = None,
    job_name: Optional[str] = None,
    extra_env: Optional[Dict[str, str]] = None,
) -> str:
    """Packages local training code and submits a multi-host TrainJob on GKE.

    Args:
        src_dir: Directory containing user training scripts and modules.
        main_script: Entrypoint script relative to src_dir (e.g. "train.py").
        num_nodes: Number of TPU host nodes in the slice (e.g. 2).
        tpus_per_node: Physical TPU chips per host node (e.g. 4).
        compute_class: GKE TPU ComputeClass (e.g. "tpu-v5-8-multi-host").
        torch_tpu_topology: TorchTPU slice topology string (e.g. "2,4,1").
        image: Container image containing PyTorch and TorchTPU.
        namespace: Target Kubernetes namespace.
        job_name: Optional custom job name (defaults to timestamped name).
        extra_env: Optional dictionary of additional environment variables.

    Returns:
        The submitted TrainJob name.
    """
    namespace = namespace or get_current_namespace()
    job_name = job_name or f"torch-tpu-multihost-{int(time.time())}"
    image = image or os.environ.get("TORCH_TPU_IMAGE", DEFAULT_TPU_IMAGE)
    core_v1, _ = _init_k8s_clients()

    # 1. Package source code directory into a Kubernetes ConfigMap
    src_path = pathlib.Path(src_dir)
    if not src_path.exists():
        raise FileNotFoundError(f"Source directory '{src_dir}' not found.")

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
    print(f"[K8s] Created ConfigMap '{cm_name}' with {len(cm_data)} files: {list(cm_data.keys())}")

    # 2. Build startup bash command to bootstrap TorchTPU and launch torchrun
    extra_env_str = ""
    if extra_env:
        extra_env_str = "\n".join(f'export {k}="{v}"' for k, v in extra_env.items()) + "\n"

    cmd_script = (
        "set -e\n"
        "echo '=== Host Startup ==='\n"
        "echo \"HOSTNAME: $(hostname)\"\n"
        "echo \"TPU_WORKER_HOSTNAMES: $TPU_WORKER_HOSTNAMES\"\n"
        "echo \"JOB_COMPLETION_INDEX: $JOB_COMPLETION_INDEX\"\n"
        "\n"
        "# 1. Bootstrap TorchTPU multi-host environment\n"
        "echo '=== Bootstrapping TorchTPU Environment ==='\n"
        "python3 -m torch.tpu.distributed.environment | grep -E '^[A-Z0-9_]+=' | sed 's/^/export /' > /tmp/torch_tpu_env.sh\n"
        "source /tmp/torch_tpu_env.sh\n"
        f'export TORCH_TPU_TOPOLOGY="{torch_tpu_topology}"\n'
        'export PYTHONPATH="/workspace:$PYTHONPATH"\n'
        f"{extra_env_str}"
        "echo '=== TorchTPU Environment Configured ==='\n"
        "cat /tmp/torch_tpu_env.sh\n"
        "echo \"TORCH_TPU_TOPOLOGY: $TORCH_TPU_TOPOLOGY\"\n"
        "echo \"TORCH_TPU_SLICEBUILDER_ADDRESSES: $TORCH_TPU_SLICEBUILDER_ADDRESSES\"\n"
        "echo \"NNODES: $NNODES | NODE_RANK: $NODE_RANK | MASTER: $MASTER_ADDR:$MASTER_PORT\"\n"
        "\n"
        "# 2. Launch multi-core DDP worker processes with modular library files\n"
        "echo '=== Workspace Mounted Files ==='\n"
        "ls -la /workspace\n"
        "echo '=== Launching torchrun ==='\n"
        "torchrun \\\n"
        '  --nnodes="$NNODES" \\\n'
        '  --node_rank="$NODE_RANK" \\\n'
        '  --master_addr="$MASTER_ADDR" \\\n'
        '  --master_port="$MASTER_PORT" \\\n'
        f"  --nproc_per_node={tpus_per_node} \\\n"
        f"  /workspace/{main_script}\n"
        "echo '=== Training Complete ==='\n"
    )

    # 3. Define Trainer container specification
    trainer = CustomTrainerContainer(
        image=image,
        num_nodes=num_nodes,
        resources_per_node={"google.com/tpu": str(tpus_per_node)},
    )

    # 4. Attach GKE accelerator patch and ConfigMap volume mount
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
                                            volumes=[
                                                {
                                                    "name": "code-volume",
                                                    "configMap": {
                                                        "name": cm_name,
                                                    },
                                                }
                                            ],
                                            containers=[
                                                ContainerPatch(
                                                    name="node",
                                                    volume_mounts=[
                                                        {
                                                            "name": "code-volume",
                                                            "mountPath": "/workspace",
                                                        }
                                                    ],
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

    # 5. Submit via Kubeflow TrainerClient
    trainer_client = TrainerClient(
        backend_config=KubernetesBackendConfig(namespace=namespace)
    )
    submitted_name = trainer_client.train(
        runtime="torch-distributed",
        trainer=trainer,
        options=[
            Name(job_name),
            TrainerCommand(["/bin/bash", "-c", cmd_script]),
            tpu_patch,
        ],
    )
    print(f"Successfully submitted multi-host TrainJob '{submitted_name}' (hosts={num_nodes}, chips={num_nodes * tpus_per_node})")
    return submitted_name


def wait_for_job_pods(
    job_name: str,
    num_nodes: int = 2,
    timeout: int = 360,
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
