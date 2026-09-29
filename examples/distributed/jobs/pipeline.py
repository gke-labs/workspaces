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

"""Pipeline orchestration helpers used by the demo notebook.

These functions submit each stage of the ML workflow to Kubernetes from inside
the Jupyter Workspace pod. Keeping the logic here (rather than inline in the
notebook) makes the notebook narrative clean and lets the same pipeline be
driven from a plain `python -m` invocation for CI.

Stages
------
1. run_data_processing()  -> Apache Spark ETL over a Spark Connect session
                             (`SparkConnect` custom resource, Kubeflow Spark Operator)
2. run_training()         -> multi-host TPU TrainJob (Kubeflow Trainer v2)
3. Inference is deployed by the notebook itself: it renders
   `inference-service.yaml` (Deployment + Service) and a ConfigMap built from
   `jobs/serve.py`, then applies both with `kubectl apply -f -`.

All stages exchange data through a single GCS bucket, accessed with the
`google-cloud-storage` Python SDK (object paths such as
`gs://$BUCKET/processed/train/shard-000.npz` and `gs://$BUCKET/model/params.npz`).
Nothing is mounted into the pods: there is no GCSFuse mount and no shared
PersistentVolume. Pods authenticate to GCS with GKE Workload Identity, so the
namespace needs `roles/storage.objectUser` on the bucket.
"""

import os
import sys
import time

from kubernetes import client, config
from kubeflow.common.types import KubernetesBackendConfig
from kubeflow.spark import (
    Driver,
    Executor,
    Name,
    SparkClient,
)
from kubeflow.trainer import CustomTrainer, TrainerClient
from kubeflow.trainer.options import kubernetes as k8s_options


def _get_current_namespace():
    """Return DEMO_NAMESPACE if set, else the pod's mounted serviceaccount namespace, else 'default'."""
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
    return "default"


NAMESPACE = _get_current_namespace()
BUCKET_NAME = os.environ.get("GCS_BUCKET", f"{NAMESPACE}-bucket")
DEMO_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# TPU slice selector — matches the `tpu-v5-8-multi-host` ComputeClass defined in
# examples/compute-classes/tpu-compute-class.yaml (apply it before Stage 2 with
# `kubectl apply -f examples/compute-classes/tpu-compute-class.yaml`).
TPU_NODE_SELECTOR = {"cloud.google.com/compute-class": "tpu-v5-8-multi-host"}
TPU_TOLERATIONS = [
    {"key": "google.com/tpu", "operator": "Exists", "effect": "NoSchedule"},
    {"key": "cloud.google.com/compute-class", "operator": "Exists", "effect": "NoSchedule"},
]

# Images
#
# REGISTRY has no default on purpose. A placeholder default (e.g.
# ".../my-project/kubeflow-repo") only moves the failure to an ImagePullBackOff
# several minutes later, which is much harder to diagnose than an immediate error.
REGISTRY = os.environ.get("REGISTRY", "").rstrip("/")
TAG = os.environ.get("TAG", "latest")
SPARK_IMAGE = f"{REGISTRY}/spark-py312:{TAG}" if REGISTRY else None
TPU_IMAGE = os.environ.get(
    "DEMO_TPU_IMAGE", "us-docker.pkg.dev/cloud-tpu-images/jax-ai-image/tpu:latest"
)

_REGISTRY_HELP = """\
The Spark image for Stage 1 cannot be resolved because the REGISTRY environment
variable is not set (and pipeline.SPARK_IMAGE was not set manually).

REGISTRY must point at the container registry you pushed the custom images to,
for example:

    us-west1-docker.pkg.dev/<your-project>/<your-repo>

Fix it in one of these ways:

  1. Launch the Workspace from the `jupyterlab` WorkspaceKind registered from
     images/workspacekinds/jupyterlab.yaml — it injects REGISTRY (and GCS_BUCKET)
     into the pod for you. Other kinds (e.g. jupyterlab-resumable) do not.
  2. Set it for this kernel before importing the pipeline:
         import os; os.environ["REGISTRY"] = "us-west1-docker.pkg.dev/proj/repo"
  3. Override the full image directly:
         pipeline.SPARK_IMAGE = "us-west1-docker.pkg.dev/proj/repo/spark-py312:latest"

The image itself is built and pushed by:

    ./images/build.sh --spark --registry-path "${REGISTRY}"
"""


def spark_image():
    """Return the Spark driver/executor image, failing loudly if it is unknown."""
    if SPARK_IMAGE:
        return SPARK_IMAGE
    raise RuntimeError(_REGISTRY_HELP)


def _client():
    return TrainerClient(backend_config=KubernetesBackendConfig(namespace=NAMESPACE))


def _tpu_placement_patch(replicated_job_name="node"):
    """Return a RuntimePatch that pins pods to TPU nodes."""
    return k8s_options.RuntimePatch(
        training_runtime_spec=k8s_options.TrainingRuntimeSpecPatch(
            template=k8s_options.JobSetTemplatePatch(
                spec=k8s_options.JobSetSpecPatch(
                    replicated_jobs=[
                        k8s_options.ReplicatedJobPatch(
                            name=replicated_job_name,
                            template=k8s_options.JobTemplatePatch(
                                spec=k8s_options.JobSpecPatch(
                                    template=k8s_options.PodTemplatePatch(
                                        spec=k8s_options.PodSpecPatch(
                                            node_selector=TPU_NODE_SELECTOR,
                                            tolerations=TPU_TOLERATIONS,
                                        ),
                                    )
                                )
                            ),
                        )
                    ]
                )
            )
        )
    )


SPARK_APP_NAME = "fashion-mnist-etl"


def _get_k8s_core_api():
    """Initialize and return a Kubernetes CoreV1Api client."""
    try:
        config.load_incluster_config()
    except config.ConfigException:
        config.load_kube_config()
    return client.CoreV1Api()


def _configure_spark_connect(resource, backend):
    """Configure SparkConnect driver/executor templates with Python 3.12 environment."""
    from kubeflow_spark_api import models

    pyspark_env = [
        models.IoK8sApiCoreV1EnvVar(name="PYSPARK_PYTHON", value="/usr/bin/python3.12"),
        models.IoK8sApiCoreV1EnvVar(name="PYSPARK_DRIVER_PYTHON", value="/usr/bin/python3.12"),
    ]
    if isinstance(resource, models.SparkV1alpha1SparkConnect):
        for role_spec, container_name in [
            (resource.spec.server, "spark-connect-server"),
            (resource.spec.executor, "spark-kubernetes-executor"),
        ]:
            if role_spec.template is None:
                role_spec.template = models.IoK8sApiCoreV1PodTemplateSpec()
            container = models.IoK8sApiCoreV1Container(
                name=container_name,
                image_pull_policy="Always",
                env=pyspark_env,
            )
            if role_spec.template.spec is None:
                role_spec.template.spec = models.IoK8sApiCoreV1PodSpec(
                    containers=[container]
                )
            elif not role_spec.template.spec.containers:
                role_spec.template.spec.containers = [container]


def run_data_processing(num_executors=4, num_shards=4, wait=True):
    """Stage 1: run distributed preprocessing using the Kubeflow Spark SDK.

    Instead of submitting a YAML `SparkApplication`, this opens a **Spark
    Connect** session (`SparkClient.connect`), which makes the Spark Operator
    create a `SparkConnect` custom resource: one Spark Connect server ("driver")
    pod plus `num_executors` executor pods. The ETL then runs against that remote
    session as if Spark were local.

    Executors write their output shards directly to GCS with the
    `google-cloud-storage` SDK (`gs://$BUCKET/processed/...`) — no volume is
    mounted, and no GCSFuse is involved.

    Raises:
        RuntimeError: if the Spark image cannot be resolved (REGISTRY unset).
    """
    image = spark_image()  # fail fast before any pod is created
    from jobs.data_processing import run_etl

    # Package jobs directory as zip and send to Spark executors
    import shutil
    import tempfile

    tmp_dir = tempfile.gettempdir()
    zip_path = os.path.join(tmp_dir, "jobs")
    if os.path.exists(zip_path + ".zip"):
        os.remove(zip_path + ".zip")
    shutil.make_archive(zip_path, "zip", root_dir=DEMO_DIR, base_dir="jobs")
    zip_file = zip_path + ".zip"

    print(f"[pipeline] connecting to Spark ({num_executors} executors) in namespace '{NAMESPACE}' via GCS bucket {BUCKET_NAME}...")

    spark_client = SparkClient(backend_config=KubernetesBackendConfig(namespace=NAMESPACE))

    # Clean up any leftover session with the same name so re-running is idempotent
    try:
        spark_client.delete_session(SPARK_APP_NAME)
        v1 = _get_k8s_core_api()
        for _ in range(30):
            pods = v1.list_namespaced_pod(
                NAMESPACE,
                label_selector=f"sparkoperator.k8s.io/connect-name={SPARK_APP_NAME}",
            ).items
            if not pods:
                break
            time.sleep(2)
    except Exception:  # noqa: BLE001
        pass

    options = [
        Name(SPARK_APP_NAME),
        _configure_spark_connect,
    ]

    spark = spark_client.connect(
        num_executors=num_executors,
        driver=Driver(
            image=image,
            resources={"cpu": "1", "memory": "4Gi"},
        ),
        executor=Executor(
            num_instances=num_executors,
            resources_per_executor={"cpu": "1", "memory": "4Gi"},
        ),
        options=options,
        spark_conf={
            "spark.pyspark.python": "/usr/bin/python3.12",
            "spark.pyspark.driver.python": "/usr/bin/python3.12",
            "spark.executorEnv.PYSPARK_PYTHON": "/usr/bin/python3.12",
            "spark.kubernetes.container.image.pullPolicy": "Always",
        },
    )
    
    spark.addArtifacts(zip_file, pyfile=True)
    if os.path.exists(zip_file):
        os.remove(zip_file)

    try:
        print(f"[pipeline] running ETL logic ({num_shards} shards)...")
        run_etl(spark, bucket_name=BUCKET_NAME, num_shards=num_shards)
        print("[pipeline] Stage 1 complete.")
    finally:
        if wait:
            spark.stop()

    return SPARK_APP_NAME


def print_spark_logs():
    """Print the Spark driver logs for the ETL job."""
    v1 = _get_k8s_core_api()
    for label_selector in [
        f"sparkoperator.k8s.io/connect-name={SPARK_APP_NAME},spark-role=connect-server",
        f"kubeflow.org/spark-connect-name={SPARK_APP_NAME},spark-role=driver",
        "app.kubernetes.io/name=spark-connect",
    ]:
        pods = v1.list_namespaced_pod(NAMESPACE, label_selector=label_selector).items
        if pods:
            break

    if not pods:
        print("[pipeline] no Spark driver pod found.")
        return

    driver_pod = pods[0].metadata.name
    try:
        logs = v1.read_namespaced_pod_log(driver_pod, NAMESPACE, tail_lines=40)
        print(logs)
    except client.ApiException as e:
        print(f"[pipeline] error fetching logs for {driver_pod}: {e}")


def run_training(num_hosts=2, epochs=5, global_batch_size=1024, wait=True, timeout=1800):
    """Stage 2: launch a multi-host TPU TrainJob for data-parallel training.

    Reads `gs://$BUCKET_NAME/processed/train/shard-*.npz` and
    `processed/test/test.npz`, and writes `model/params.npz`,
    `model/checkpoint-<step>.npz`, and `model/metrics.json` back to the same
    bucket with the `google-cloud-storage` SDK.

    Requires the `jax-distributed` ClusterTrainingRuntime (installed by the
    standalone deploy script with INSTALL_TRAINER=true) and the
    `tpu-v5-8-multi-host` ComputeClass from
    examples/compute-classes/tpu-compute-class.yaml.
    """
    from jobs.train import train_fashion_mnist  # local module

    client = _client()
    print(f"[pipeline] submitting TPU TrainJob ({num_hosts} hosts, {epochs} epochs)...")
    job_id = client.train(
        runtime="jax-distributed",
        trainer=CustomTrainer(
            func=train_fashion_mnist,
            image=TPU_IMAGE,
            num_nodes=num_hosts,
            resources_per_node={"google.com/tpu": 4},
            env={
                "JAX_PLATFORMS": "tpu,cpu",
                "ENABLE_PJRT_COMPATIBILITY": "true",
                "BUCKET_NAME": BUCKET_NAME,
                "EPOCHS": str(epochs),
                "GLOBAL_BATCH_SIZE": str(global_batch_size),
            },
        ),
        options=[_tpu_placement_patch()],
    )
    print(f"[pipeline] created training job: {job_id}")
    if wait:
        client.wait_for_job_status(job_id, timeout=timeout, polling_interval=10)
    return job_id


def print_logs(job_id):
    """Print logs for a TPU TrainJob across all multi-host worker pods."""
    v1 = _get_k8s_core_api()
    pods = v1.list_namespaced_pod(
        NAMESPACE, label_selector=f"jobset.sigs.k8s.io/jobset-name={job_id}"
    ).items
    if not pods:
        client_sdk = _client()
        logs = client_sdk.get_job_logs(job_id)
        for item in logs:
            print(item)
        return

    for pod in sorted(pods, key=lambda p: p.metadata.name):
        pod_name = pod.metadata.name
        print(f"\n--- {pod_name} ---")
        try:
            log = v1.read_namespaced_pod_log(pod_name, NAMESPACE)
            print(log)
        except client.ApiException as e:
            print(f"[pipeline] error fetching logs for {pod_name}: {e}")
