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

"""Pipeline Orchestration Helpers for MiniGPT Workflow.

Automates the complete lifecycle:
  1. Distributed data processing with Spark on Kubernetes
  2. Switchable distributed training (4x Tesla T4 GPU vs 2x2 Cloud TPU slice) via Kubeflow Trainer
  3. High-availability 2-replica GPU inference deployment on NVIDIA T4 hardware
  4. Real-time inference generation and cross-hardware validation
"""

import datetime
import json
import os
import pathlib
import shutil
import tempfile
import time
from typing import Dict, List, Optional, Tuple

from kubernetes import client, config
from kubeflow.common.types import KubernetesBackendConfig
from kubeflow.spark import Driver, Executor, SparkClient, Name, NodeSelector
from kubeflow.trainer import CustomTrainerContainer, TrainerClient
from kubeflow.trainer.options import (
    ContainerPatch,
    JobSetSpecPatch,
    JobSetTemplatePatch,
    JobSpecPatch,
    JobTemplatePatch,
    Name as TrainerJobName,
    PodSpecPatch,
    PodTemplatePatch,
    ReplicatedJobPatch,
    RuntimePatch,
    TrainerCommand,
    TrainingRuntimeSpecPatch,
)

# Environment configuration & validation
def _get_current_namespace() -> str:
    """Return TENANT_NAMESPACE or DEMO_NAMESPACE if set, else the pod's serviceaccount namespace, else 'kubeflow-user'."""
    if os.environ.get("TENANT_NAMESPACE"):
        return os.environ["TENANT_NAMESPACE"]
    if os.environ.get("DEMO_NAMESPACE"):
        return os.environ["DEMO_NAMESPACE"]
    ns_path = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
    if os.path.exists(ns_path):
        try:
            with open(ns_path) as f:
                ns = f.read().strip()
                if ns:
                    return ns
        except OSError:
            pass
    return "kubeflow-user"


def get_bucket_name() -> str:
    """Returns the GCS bucket name, checking that the environment variable exists and is non-empty."""
    bucket = os.environ.get("GCS_BUCKET") or os.environ.get("DEMO_BUCKET")
    if not bucket:
        raise ValueError(
            "GCS bucket environment variable 'GCS_BUCKET' (or 'DEMO_BUCKET') is not set.\n"
            "Please configure your GCS bucket, for example:\n"
            "    export GCS_BUCKET='<your-project>-<your-namespace>-bucket'"
        )
    return bucket


def get_registry() -> str:
    """Returns the container registry URL, defaulting to the prebuilt GHCR repository."""
    return os.environ.get("REGISTRY", "ghcr.io/gke-labs/workspaces").rstrip("/")


def get_spark_image() -> str:
    tag = os.environ.get("TAG", "latest")
    return f"{get_registry()}/spark-py312:{tag}"


def get_gpu_image() -> str:
    tag = os.environ.get("TAG", "latest-gpu")
    return f"{get_registry()}/jupyterlab:{tag}"


def get_tpu_image() -> str:
    tag = os.environ.get("TAG", "latest-tpu")
    return f"{get_registry()}/jupyterlab:{tag}"


NAMESPACE = _get_current_namespace()
JOBS_DIR = pathlib.Path(__file__).parent.resolve()


def __getattr__(name: str):
    if name == "BUCKET_NAME":
        return get_bucket_name()
    if name == "REGISTRY":
        return get_registry()
    if name == "SPARK_IMAGE":
        return get_spark_image()
    if name == "GPU_IMAGE":
        return get_gpu_image()
    if name == "TPU_IMAGE":
        return get_tpu_image()
    raise AttributeError(f"module '{__name__}' has no attribute '{name}'")


def _get_k8s_clients():
    """Initializes in-cluster or kubeconfig Kubernetes API clients."""
    try:
        config.load_incluster_config()
    except config.ConfigException:
        config.load_kube_config()
    return client.CoreV1Api(), client.AppsV1Api()


def _trainer_client():
    return TrainerClient(backend_config=KubernetesBackendConfig(namespace=NAMESPACE))


# ------------------------------------------------------------------------------
# Stage 1: Spark Data Processing
# ------------------------------------------------------------------------------
def run_data_processing(
    num_executors: int = 2,
    num_shards: int = 4,
    num_records: int = 16000,
    block_size: int = 128,
    wait: bool = True,
    bucket_name: Optional[str] = None,
) -> str:
    """Executes distributed data preprocessing with Apache Spark on Kubernetes."""
    from jobs.data_processing import run_etl

    bucket = bucket_name or get_bucket_name()
    spark_image = get_spark_image()

    app_name = f"minigpt-spark-etl-{int(time.time())}"
    tmp_dir = tempfile.gettempdir()
    zip_path = os.path.join(tmp_dir, "jobs")
    if os.path.exists(zip_path + ".zip"):
        os.remove(zip_path + ".zip")
    shutil.make_archive(zip_path, "zip", root_dir=JOBS_DIR.parent, base_dir="jobs")
    zip_file = zip_path + ".zip"

    print(f"[Spark] Connecting to Spark ({num_executors} executors) with GCS bucket gs://{bucket}...")

    spark_client = SparkClient(backend_config=KubernetesBackendConfig(namespace=NAMESPACE))
    spark = spark_client.connect(
        num_executors=num_executors,
        driver=Driver(image=spark_image, resources={"cpu": "1", "memory": "2Gi"}),
        executor=Executor(
            num_instances=num_executors,
            resources_per_executor={"cpu": "1", "memory": "2Gi"},
        ),
        options=[
            Name(app_name),
        ],
        spark_conf={
            "spark.kubernetes.driver.label.sidecar.istio.io/inject": "false",
            "spark.kubernetes.executor.label.sidecar.istio.io/inject": "false",
        },
    )

    spark.addArtifacts(zip_file, pyfile=True)
    if os.path.exists(zip_file):
        os.remove(zip_file)

    try:
        print(f"[Spark] Running ETL logic across {num_executors} worker pods...")
        run_etl(
            spark,
            bucket_name=bucket,
            num_shards=num_shards,
            num_records=num_records,
            block_size=block_size,
        )
        print("[Spark] Stage 1 ETL complete!")
    finally:
        if wait:
            spark.stop()
            try:
                spark_client.delete_session(app_name)
            except Exception:
                pass

    return app_name


# ------------------------------------------------------------------------------
# Stage 2: Distributed Training with Kubeflow Trainer
# ------------------------------------------------------------------------------
def _create_code_configmap(job_name: str, core_v1: client.CoreV1Api) -> str:
    """Creates a ConfigMap containing user training scripts."""
    cm_name = f"{job_name}-code"
    py_files = ["model.py", "train.py"]
    cm_data = {}
    for fn in py_files:
        p = JOBS_DIR / fn
        if p.exists():
            cm_data[fn] = p.read_text(encoding="utf-8")
        else:
            raise FileNotFoundError(f"Required script {fn} not found in {JOBS_DIR}")

    cm_body = client.V1ConfigMap(
        metadata=client.V1ObjectMeta(
            name=cm_name,
            namespace=NAMESPACE,
            labels={"app.kubernetes.io/part-of": "minigpt-scaling-demo"},
        ),
        data=cm_data,
    )
    core_v1.create_namespaced_config_map(namespace=NAMESPACE, body=cm_body)
    return cm_name


def run_training(
    accelerator: str = "tpu",
    epochs: int = 2,
    batch_size: int = 32,
    lr: float = 3e-4,
    block_size: int = 128,
    wait: bool = True,
    timeout: int = 1800,
    bucket_name: Optional[str] = None,
) -> str:
    """Launches distributed training on either 4x Tesla T4 GPUs or 2x2 Cloud TPU slice.

    Args:
        accelerator: "gpu" (4 Tesla T4 GPUs) or "tpu" (2x2 TPU slice, 4 chips).
        epochs: Number of training epochs.
        batch_size: Per-worker batch size.
        lr: Learning rate for AdamW optimizer.
        block_size: Sequence context window length.
        wait: Whether to block until training succeeds.
        timeout: Maximum seconds to wait.
        bucket_name: GCS bucket name (defaults to GCS_BUCKET / DEMO_BUCKET env var).

    Returns:
        The submitted Kubeflow TrainJob name.
    """
    accelerator = accelerator.lower()
    assert accelerator in ("gpu", "tpu"), f"Unsupported accelerator '{accelerator}', choose 'gpu' or 'tpu'."

    bucket = bucket_name or get_bucket_name()
    core_v1, _ = _get_k8s_clients()
    job_name = f"minigpt-train-{accelerator}-{int(time.time())}"

    # 1. Package code in ConfigMap
    cm_name = _create_code_configmap(job_name, core_v1)
    code_volume = {"name": "code-volume", "configMap": {"name": cm_name}}
    code_volume_mount = {"name": "code-volume", "mountPath": "/workspace"}

    # 2. Hardware-specific configuration
    if accelerator == "tpu":
        # 2x2 TPU slice (4 chips on 1 host)
        compute_class = "tpu-v5-4-single-host"
        image = get_tpu_image()
        num_nodes = 1
        tpus_per_node = 4
        resources_per_node = {"google.com/tpu": "4"}
        tolerations = [
            {"key": "google.com/tpu", "operator": "Exists", "effect": "NoSchedule"},
            {"key": "cloud.google.com/compute-class", "operator": "Exists", "effect": "NoSchedule"},
        ]
        torchrun_cmd = [
            "python3",
            "-c",
            "import subprocess, sys; from torch.tpu.distributed import environment; environment.set_tpu_launch_env(nproc_per_node=4); sys.exit(subprocess.call(['torchrun', '--nproc_per_node=4', '/workspace/train.py']))",
        ]
        extra_env = {
            "ACCELERATOR": "tpu",
            "LOCAL_WORLD_SIZE": "4",
            "BUCKET_NAME": bucket,
            "EPOCHS": str(epochs),
            "BATCH_SIZE": str(batch_size),
            "LEARNING_RATE": str(lr),
            "BLOCK_SIZE": str(block_size),
            "PYTHONPATH": "/workspace",
        }
        print(f"[Trainer] Configuring Cloud TPU v5e (2x2 slice, 4 chips) via ComputeClass '{compute_class}'...")

    else:  # accelerator == "gpu"
        # 4 Tesla T4 GPUs (4 distributed worker nodes, 1 GPU per node)
        compute_class = "gpu-t4-spot"
        image = get_gpu_image()
        num_nodes = 4
        resources_per_node = {"nvidia.com/gpu": "1", "cpu": "2", "memory": "8Gi"}
        tolerations = [
            {"key": "nvidia.com/gpu", "operator": "Exists", "effect": "NoSchedule"},
            {"key": "cloud.google.com/compute-class", "operator": "Exists", "effect": "NoSchedule"},
        ]
        torchrun_cmd = [
            "bash",
            "-c",
            "export PATH=/opt/conda/bin:/usr/local/nvidia/bin:/usr/local/cuda/bin:$PATH; "
            "export LD_LIBRARY_PATH=/usr/local/nvidia/lib:/usr/local/nvidia/lib64:$LD_LIBRARY_PATH; "
            "exec torchrun "
            "--nnodes=${PET_NNODES:-${WORLD_SIZE:-1}} "
            "--node_rank=${PET_NODE_RANK:-${RANK:-0}} "
            "--master_addr=${PET_MASTER_ADDR:-${MASTER_ADDR:-127.0.0.1}} "
            "--master_port=${PET_MASTER_PORT:-${MASTER_PORT:-29500}} "
            "--nproc_per_node=1 "
            "/workspace/train.py",
        ]
        extra_env = {
            "ACCELERATOR": "gpu",
            "LOCAL_WORLD_SIZE": "1",
            "BUCKET_NAME": bucket,
            "EPOCHS": str(epochs),
            "BATCH_SIZE": str(batch_size),
            "LEARNING_RATE": str(lr),
            "BLOCK_SIZE": str(block_size),
            "PYTHONPATH": "/workspace",
            "LD_LIBRARY_PATH": "/usr/local/nvidia/lib:/usr/local/nvidia/lib64",
            "PATH": "/opt/conda/bin:/usr/local/nvidia/bin:/usr/local/cuda/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
            "PYTHONUNBUFFERED": "1",
        }
        print(f"[Trainer] Configuring 4x Tesla T4 GPUs across {num_nodes} worker nodes via ComputeClass '{compute_class}'...")

    # 3. Build Kubeflow Trainer runtime patch
    accelerator_patch = RuntimePatch(
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
                                            node_selector={"cloud.google.com/compute-class": compute_class},
                                            tolerations=tolerations,
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

    trainer = CustomTrainerContainer(
        image=image,
        num_nodes=num_nodes,
        resources_per_node=resources_per_node,
        env=extra_env,
    )

    # 4. Submit TrainJob
    tc = _trainer_client()
    submitted_name = tc.train(
        runtime="torch-distributed",
        trainer=trainer,
        options=[
            TrainerJobName(job_name),
            TrainerCommand(torchrun_cmd),
            accelerator_patch,
        ],
    )
    print(f"[Trainer] Successfully submitted TrainJob '{submitted_name}'!")

    if wait:
        print(f"[Trainer] Streaming logs for '{submitted_name}'...")
        try:
            for line in tc.get_job_logs(name=submitted_name, follow=True):
                print(line, end="", flush=True)
        except Exception:
            pass
        tc.wait_for_job_status(submitted_name, timeout=timeout, polling_interval=10)
        print(f"[Trainer] TrainJob '{submitted_name}' finished successfully!")

    return submitted_name


# ------------------------------------------------------------------------------
# Stage 3: High-Availability GPU Inference Serving (2 Replicas on T4)
# ------------------------------------------------------------------------------
def deploy_inference(
    replicas: int = 2,
    service_name: str = "minigpt-inference",
    wait: bool = True,
    timeout: int = 600,
    bucket_name: Optional[str] = None,
) -> str:
    """Deploys a 2-replica GPU inference service on NVIDIA Tesla T4 hardware."""
    bucket = bucket_name or get_bucket_name()
    gpu_image = get_gpu_image()
    core_v1, apps_v1 = _get_k8s_clients()
    print(f"[Serving] Deploying high-availability GPU inference ({replicas} replicas on T4 GPUs)...")

    # 1. ConfigMap with serve.py and model.py
    cm_name = f"{service_name}-code"
    cm_data = {
        "serve.py": (JOBS_DIR / "serve.py").read_text(encoding="utf-8"),
        "model.py": (JOBS_DIR / "model.py").read_text(encoding="utf-8"),
    }
    cm_body = client.V1ConfigMap(
        metadata=client.V1ObjectMeta(
            name=cm_name,
            namespace=NAMESPACE,
            labels={"app.kubernetes.io/part-of": "minigpt-scaling-demo"},
        ),
        data=cm_data,
    )
    try:
        core_v1.read_namespaced_config_map(name=cm_name, namespace=NAMESPACE)
        core_v1.replace_namespaced_config_map(name=cm_name, namespace=NAMESPACE, body=cm_body)
    except client.ApiException as e:
        if e.status == 404:
            core_v1.create_namespaced_config_map(namespace=NAMESPACE, body=cm_body)
        else:
            raise

    # 2. Deployment
    deployment = client.V1Deployment(
        metadata=client.V1ObjectMeta(
            name=service_name,
            namespace=NAMESPACE,
            labels={
                "app": service_name,
                "app.kubernetes.io/part-of": "minigpt-scaling-demo",
            },
        ),
        spec=client.V1DeploymentSpec(
            replicas=replicas,
            selector=client.V1LabelSelector(match_labels={"app": service_name}),
            strategy=client.V1DeploymentStrategy(
                type="RollingUpdate",
                rolling_update=client.V1RollingUpdateDeployment(
                    max_surge=0,
                    max_unavailable=1,
                ),
            ),
            template=client.V1PodTemplateSpec(
                metadata=client.V1ObjectMeta(
                    labels={
                        "app": service_name,
                        "app.kubernetes.io/part-of": "minigpt-scaling-demo",
                    },
                    annotations={
                        "kubectl.kubernetes.io/restartedAt": datetime.datetime.now(datetime.timezone.utc).isoformat()
                    },
                ),
                spec=client.V1PodSpec(
                    node_selector={"cloud.google.com/compute-class": "gpu-t4-spot"},
                    tolerations=[
                        client.V1Toleration(key="nvidia.com/gpu", operator="Exists", effect="NoSchedule"),
                        client.V1Toleration(key="cloud.google.com/compute-class", operator="Exists", effect="NoSchedule"),
                    ],
                    containers=[
                        client.V1Container(
                            name="server",
                            image=gpu_image,
                            command=["/opt/conda/bin/python", "/app/serve.py"],
                            env=[
                                client.V1EnvVar(name="BUCKET_NAME", value=bucket),
                                client.V1EnvVar(name="PORT", value="8080"),
                                client.V1EnvVar(name="PYTHONPATH", value="/app"),
                                client.V1EnvVar(name="PYTHONUNBUFFERED", value="1"),
                                client.V1EnvVar(name="LD_LIBRARY_PATH", value="/usr/local/nvidia/lib:/usr/local/nvidia/lib64"),
                                client.V1EnvVar(name="PATH", value="/opt/conda/bin:/usr/local/nvidia/bin:/usr/local/cuda/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"),
                            ],
                            ports=[client.V1ContainerPort(container_port=8080)],
                            volume_mounts=[client.V1VolumeMount(name="serve-code", mount_path="/app")],
                            readiness_probe=client.V1Probe(
                                http_get=client.V1HTTPGetAction(path="/healthz", port=8080),
                                initial_delay_seconds=30,
                                period_seconds=5,
                            ),
                            liveness_probe=client.V1Probe(
                                http_get=client.V1HTTPGetAction(path="/healthz", port=8080),
                                initial_delay_seconds=300,
                                period_seconds=15,
                            ),
                            resources=client.V1ResourceRequirements(
                                requests={"cpu": "2", "memory": "8Gi", "nvidia.com/gpu": "1"},
                                limits={"cpu": "2", "memory": "8Gi", "nvidia.com/gpu": "1"},
                            ),
                        )
                    ],
                    volumes=[
                        client.V1Volume(
                            name="serve-code",
                            config_map=client.V1ConfigMapVolumeSource(name=cm_name),
                        )
                    ],
                ),
            ),
        ),
    )

    try:
        apps_v1.read_namespaced_deployment(name=service_name, namespace=NAMESPACE)
        apps_v1.replace_namespaced_deployment(name=service_name, namespace=NAMESPACE, body=deployment)
    except client.ApiException as e:
        if e.status == 404:
            apps_v1.create_namespaced_deployment(namespace=NAMESPACE, body=deployment)
        else:
            raise

    # 3. Service
    service = client.V1Service(
        metadata=client.V1ObjectMeta(
            name=service_name,
            namespace=NAMESPACE,
            labels={"app": service_name},
        ),
        spec=client.V1ServiceSpec(
            selector={"app": service_name},
            ports=[client.V1ServicePort(name="http", port=80, target_port=8080)],
        ),
    )

    try:
        core_v1.read_namespaced_service(name=service_name, namespace=NAMESPACE)
        core_v1.replace_namespaced_service(name=service_name, namespace=NAMESPACE, body=service)
    except client.ApiException as e:
        if e.status == 404:
            core_v1.create_namespaced_service(namespace=NAMESPACE, body=service)
        else:
            raise

    print(f"[Serving] Deployed Service '{service_name}' in namespace '{NAMESPACE}'.")

    if wait:
        print(f"[Serving] Waiting for {replicas} GPU replicas to become Ready...")
        start_t = time.time()
        while time.time() - start_t < timeout:
            dep = apps_v1.read_namespaced_deployment_status(name=service_name, namespace=NAMESPACE)
            ready_replicas = dep.status.ready_replicas or 0
            if ready_replicas >= replicas:
                print(f"[Serving] All {ready_replicas}/{replicas} GPU replicas are 1/1 Ready and serving traffic!")
                return service_name
            time.sleep(10)
        print(f"[Serving] Warning: Timed out waiting for replicas ({ready_replicas}/{replicas} ready).")

    return service_name


# ------------------------------------------------------------------------------
# Stage 4: Test Inference Endpoint
# ------------------------------------------------------------------------------
def query_inference(
    service_name: str = "minigpt-inference",
    prompts: Optional[List[str]] = None,
    max_new_tokens: int = 50,
):
    """Sends prompts to the GPU inference cluster and displays responses."""
    import urllib.request

    if prompts is None:
        from jobs import PROMPTS
        prompts = PROMPTS

    service_url = f"http://{service_name}.{NAMESPACE}.svc.cluster.local:80/generate"
    print(f"\n=== Querying 2-Replica GPU Inference Service ({service_url}) ===\n")

    for prompt in prompts:
        payload = json.dumps({
            "prompt": prompt,
            "max_new_tokens": max_new_tokens,
            "temperature": 0.8,
            "top_k": 40,
        }).encode("utf-8")

        req = urllib.request.Request(
            service_url,
            data=payload,
            headers={"Content-Type": "application/json"},
        )

        try:
            with urllib.request.urlopen(req, timeout=30) as resp:
                data = json.loads(resp.read().decode("utf-8"))
                print(f"Prompt:     {data['prompt']}")
                print(f"Generated:  {data['text']}")
                print(f"Hardware:   {data.get('device_name', data.get('device'))} (Trained on: {data.get('trained_on', 'unknown').upper()}) | Latency: {data.get('latency_ms')} ms")
                print("-" * 70)
        except Exception as e:
            print(f"Failed querying prompt '{prompt}': {e}")
