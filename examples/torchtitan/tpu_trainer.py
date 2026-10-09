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

"""Generic, project-agnostic utilities for single-host and multi-host TorchTPU training on GKE.

Encapsulates Kubernetes and Cloud TPU infrastructure details (TPU `/dev/vfio/*`
pre-flight lock checks, single-host SliceBuilder launch environment, ConfigMaps,
shared `ReadWriteMany` PVC mounts, node selectors, tolerations, and Kubeflow
Trainer `TrainJob` patches) so notebooks can run any PyTorch/TPU project with
a single function call.
"""

from __future__ import annotations

import os
import pathlib
import re
import shutil
import site
import subprocess
import sys
import time
from typing import Callable, Dict, List, Optional, Sequence, Tuple, Union

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

try:
    import tpu_lock
except ImportError:
    tpu_lock = None  # type: ignore[assignment]


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


def configure_runtime_env(
    user_base: Optional[Union[str, pathlib.Path]] = None,
    extra_pythonpath: Optional[Union[str, pathlib.Path]] = None,
) -> Dict[str, str]:
    """Configures LD_LIBRARY_PATH, PYTHONUSERBASE, and PYTHONPATH for TPU workloads.

    Ensures `/opt/conda/lib` precedes system libraries in `LD_LIBRARY_PATH` so
    Conda C++ extensions (such as `pyarrow`) and `libtpu` share Conda's
    `libstdc++.so.6` (`GLIBCXX_3.4.31+`).
    """
    ld_parts = ["/opt/conda/lib"]
    existing_ld = os.environ.get("LD_LIBRARY_PATH", "")
    for part in existing_ld.split(":"):
        if part and part not in ld_parts:
            ld_parts.append(part)
    os.environ["LD_LIBRARY_PATH"] = ":".join(ld_parts)

    py_ver = f"python{sys.version_info.major}.{sys.version_info.minor}"
    pypath_parts = [p for p in os.environ.get("PYTHONPATH", "").split(":") if p]

    if extra_pythonpath:
        extra_str = str(pathlib.Path(extra_pythonpath).resolve())
        if extra_str not in sys.path:
            sys.path.insert(0, extra_str)
        if extra_str not in pypath_parts:
            pypath_parts.insert(0, extra_str)

    if user_base:
        ub_path = pathlib.Path(user_base).resolve()
        os.environ["PYTHONUSERBASE"] = str(ub_path)
        bin_dir = str(ub_path / "bin")
        path_parts = os.environ.get("PATH", "").split(":")
        if bin_dir not in path_parts:
            os.environ["PATH"] = f"{bin_dir}:{os.environ.get('PATH', '')}"
        site_pkgs = ub_path / "lib" / py_ver / "site-packages"
        if site_pkgs.exists():
            site.addsitedir(str(site_pkgs))
            if str(site_pkgs) not in pypath_parts:
                pypath_parts.append(str(site_pkgs))

    if pypath_parts:
        os.environ["PYTHONPATH"] = ":".join(pypath_parts)

    return {
        "LD_LIBRARY_PATH": os.environ["LD_LIBRARY_PATH"],
        "PYTHONUSERBASE": os.environ.get("PYTHONUSERBASE", ""),
        "PYTHONPATH": os.environ.get("PYTHONPATH", ""),
    }


def resolve_shared_pvc_subpath(
    src_dir: Union[str, pathlib.Path],
    namespace: str,
    pvc_name: str = "shared-workspace-rwx",
    shared_mount_path: str = "/home/jovyan/shared",
) -> Tuple[str, str, pathlib.Path]:
    """Resolves the PVC claimName and subPath for a directory on a shared RWX volume.

    If `src_dir` is already inside `shared_mount_path` (for example, when synced via
    `jupyter-workspace-sync` with `"jupyterSync.remoteBaseDir": "shared/${workspaceFolderBasename}"`),
    this function computes its exact `subPath` on the PVC without copying any files.
    If `src_dir` is outside `shared_mount_path` (e.g., on the local RWO home disk), it
    stages `src_dir` into `shared_mount_path` so the TPU worker pods can mount it.

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
        shutil.copytree(
            src_path,
            staged_dir,
            dirs_exist_ok=True,
            ignore=shutil.ignore_patterns(".git", "__pycache__", "*.pyc"),
        )
        print(f"[RWX] Staged '{src_path}' -> shared volume at '{staged_dir}'")
        src_path = staged_dir

    rel_from_mount = src_path.relative_to(mount_root).as_posix()
    if rel_from_mount == ".":
        rel_from_mount = ""

    parts = [p.strip("/") for p in (base_sub_path, rel_from_mount) if p and p.strip("/")]
    full_sub_path = "/".join(parts)
    return detected_pvc, full_sub_path, src_path


def run_singlehost_training(
    main_script: str = "train.py",
    args: Optional[Sequence[str]] = None,
    src_dir: Optional[Union[str, pathlib.Path]] = None,
    module: bool = False,
    nproc_per_node: Optional[int] = None,
    extra_env: Optional[Dict[str, str]] = None,
    user_base: Optional[Union[str, pathlib.Path]] = None,
    auto_clear_locks: bool = True,
    log_filter: Optional[Callable[[str], Optional[str]]] = None,
) -> int:
    """Runs a single-host `torchrun` job on the local Workspace TPU.

    Args:
        main_script: Script path (when `module=False`) or Python module name
            (when `module=True`, passed as `torchrun -m <main_script>`).
        args: Optional list of CLI arguments passed to `main_script`.
        src_dir: Working directory and `PYTHONPATH` root for `torchrun`.
        module: If True, invokes `torchrun -m <main_script>` instead of a script path.
        nproc_per_node: Number of processes/chips to use. Defaults to all attached
            TPU devices discovered in `/dev/vfio/*`.
        extra_env: Optional extra environment variables for `torchrun`.
        user_base: Optional `PYTHONUSERBASE` directory (e.g., `/home/jovyan/shared/.pydeps`).
        auto_clear_locks: If True, automatically clears lingering `/dev/vfio/*` locks.
        log_filter: Optional callable `fn(line) -> str | None` to filter/transform
            stdout lines (returning `None` suppresses the line).

    Returns:
        Process exit code (0 on success).
    """
    if tpu_lock is not None:
        if not tpu_lock.preflight_check(is_distributed=True, auto_clear=auto_clear_locks):
            raise RuntimeError("TPU hardware pre-flight check failed. Free TPU locks before running.")
        detected_devices = tpu_lock.get_tpu_devices()
    else:
        detected_devices = []

    if nproc_per_node is None:
        nproc_per_node = max(1, len(detected_devices))

    workdir = pathlib.Path(src_dir).resolve() if src_dir else pathlib.Path.cwd()
    if user_base is None:
        default_ub = pathlib.Path("/home/jovyan/shared/.pydeps")
        if default_ub.exists():
            user_base = default_ub

    configure_runtime_env(user_base=user_base, extra_pythonpath=workdir)

    try:
        from torch.tpu.distributed import environment as tpu_env

        tpu_env.set_tpu_launch_env(nproc_per_node=nproc_per_node)
    except Exception:
        pass

    run_env = dict(os.environ)
    for stale_key in (
        "TORCH_DEVICE_BACKEND_AUTOLOAD",
        "RANK",
        "LOCAL_RANK",
        "WORLD_SIZE",
        "LOCAL_WORLD_SIZE",
    ):
        run_env.pop(stale_key, None)
    if extra_env:
        run_env.update(extra_env)

    cmd: List[str] = ["torchrun", f"--nproc_per_node={nproc_per_node}"]
    if module:
        cmd.extend(["-m", main_script])
    else:
        cmd.append(main_script)
    if args:
        cmd.extend(args)

    print(f"Launching single-host torchrun on {nproc_per_node} TPU chip(s):")
    print(f"  Working dir : {workdir}")
    if run_env.get("PYTHONUSERBASE"):
        print(f"  Userbase    : {run_env['PYTHONUSERBASE']}")
    print(f"  Command     : {' '.join(cmd)}\n", flush=True)

    process = subprocess.Popen(
        cmd,
        cwd=str(workdir),
        env=run_env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        bufsize=1,
    )

    try:
        assert process.stdout is not None
        for line in process.stdout:
            if log_filter is not None:
                filtered = log_filter(line)
                if filtered is None or filtered is False:
                    continue
                if isinstance(filtered, str):
                    line = filtered
            print(line, end="", flush=True)
        return_code = process.wait()
    except KeyboardInterrupt:
        print("\nInterrupted! Terminating torchrun workers...", flush=True)
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
        if tpu_lock is not None:
            tpu_lock.clear_tpu_locks(verbose=True)
        raise

    if return_code != 0:
        raise RuntimeError(f"Single-host torchrun exited with code {return_code}")
    print("\nSingle-host training completed successfully!")
    return return_code


def submit_multihost_training(
    image: str,
    src_dir: Union[str, pathlib.Path] = "src",
    main_script: str = "train.py",
    args: Optional[Sequence[str]] = None,
    module: bool = False,
    num_nodes: int = 2,
    tpus_per_node: int = 4,
    compute_class: str = "tpu-v5-8-multi-host",
    namespace: Optional[str] = None,
    job_name: Optional[str] = None,
    extra_env: Optional[Dict[str, str]] = None,
    pvc_name: Optional[str] = None,
    shared_mount_path: str = "/home/jovyan/shared",
    user_base: Optional[Union[str, pathlib.Path]] = None,
) -> str:
    """Packages or mounts local training code and submits a multi-host TrainJob on GKE.

    Args:
        image: Container image containing PyTorch and TorchTPU.
        src_dir: Directory containing user training scripts or repository root.
        main_script: Entrypoint script relative to `src_dir` (e.g. `"train.py"`)
            or Python module name when `module=True` (e.g. `"pkg.train"`).
        args: Optional list of CLI arguments passed to `main_script`.
        module: If True, runs `torchrun -m <main_script>` with `/workspace` as
            the working directory.
        num_nodes: Number of TPU host nodes in the slice (e.g. 2).
        tpus_per_node: Physical TPU chips per host node (e.g. 4).
        compute_class: GKE TPU ComputeClass (e.g. `"tpu-v5-8-multi-host"`).
        namespace: Target Kubernetes namespace.
        job_name: Optional custom job name (defaults to timestamped name).
        extra_env: Optional dictionary of additional environment variables.
        pvc_name: Optional ReadWriteMany PVC name (e.g. `"shared-workspace-rwx"`).
            When provided, mounts the shared PVC (and resolved subdirectory) directly
            at `/workspace` on all TPU worker pods instead of creating a ConfigMap.
        shared_mount_path: Mount path of the shared RWX PVC inside the Workspace pod
            (default: `"/home/jovyan/shared"`).
        user_base: Optional shared `PYTHONUSERBASE` directory on `shared_mount_path`
            (e.g. `"/home/jovyan/shared/.pydeps"`). When present, mounts it at
            `/workspace-deps` on all TPU worker pods so packages installed via
            `pip install --user` in the Workspace are available without rebuilding
            the Docker image.

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

    volumes: List[Dict] = []
    volume_mounts: List[Dict] = []
    py_ver = f"python{sys.version_info.major}.{sys.version_info.minor}"
    pypath_parts = ["/workspace"]

    # 1. Configure code volume (Shared ReadWriteMany PVC vs. Kubernetes ConfigMap)
    if pvc_name:
        claim_name, sub_path, effective_src = resolve_shared_pvc_subpath(
            src_dir=src_path,
            namespace=namespace,
            pvc_name=pvc_name,
            shared_mount_path=shared_mount_path,
        )
        if not module:
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
        code_volume_mount: Dict[str, str] = {
            "name": "code-volume",
            "mountPath": "/workspace",
        }
        if sub_path:
            code_volume_mount["subPath"] = sub_path
        volumes.append(code_volume)
        volume_mounts.append(code_volume_mount)
        print(
            f"[K8s] Mounting shared RWX PVC '{claim_name}' "
            f"(subPath='{sub_path or '/'}') at /workspace on all TPU worker pods"
        )

        # Optionally mount shared PYTHONUSERBASE (.pydeps) if present
        if user_base is None:
            default_ub = pathlib.Path(shared_mount_path) / ".pydeps"
            if default_ub.exists():
                user_base = default_ub
        if user_base and pathlib.Path(user_base).exists():
            _, deps_sub_path, _ = resolve_shared_pvc_subpath(
                src_dir=user_base,
                namespace=namespace,
                pvc_name=pvc_name,
                shared_mount_path=shared_mount_path,
            )
            deps_volume = {
                "name": "deps-volume",
                "persistentVolumeClaim": {
                    "claimName": claim_name,
                },
            }
            deps_volume_mount: Dict[str, str] = {
                "name": "deps-volume",
                "mountPath": "/workspace-deps",
            }
            if deps_sub_path:
                deps_volume_mount["subPath"] = deps_sub_path
            volumes.append(deps_volume)
            volume_mounts.append(deps_volume_mount)
            pypath_parts.append(f"/workspace-deps/lib/{py_ver}/site-packages")
            print(
                f"[K8s] Mounting shared Python userbase '{claim_name}' "
                f"(subPath='{deps_sub_path or '/'}') at /workspace-deps on all TPU worker pods"
            )
    else:
        cm_name = f"{job_name}-code"
        cm_data = {
            file_path.name: file_path.read_text(encoding="utf-8")
            for file_path in sorted(src_path.glob("*.py"))
        }
        if not cm_data:
            raise ValueError(f"No Python files found in '{src_dir}'.")
        if not module and main_script not in cm_data:
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
        volumes.append(
            {
                "name": "code-volume",
                "configMap": {
                    "name": cm_name,
                },
            }
        )
        volume_mounts.append(
            {
                "name": "code-volume",
                "mountPath": "/workspace",
            }
        )

    # 2. Define Trainer container specification
    merged_env = {
        "PYTHONPATH": ":".join(pypath_parts),
        "LD_LIBRARY_PATH": "/opt/conda/lib:/usr/local/nvidia/lib64",
    }
    if any(vm["mountPath"] == "/workspace-deps" for vm in volume_mounts):
        merged_env["PYTHONUSERBASE"] = "/workspace-deps"
        merged_env["PATH"] = (
            "/workspace-deps/bin:/home/jovyan/.local/bin:/usr/local/nvidia/bin:"
            "/usr/local/cuda/bin:/opt/conda/bin:/usr/local/sbin:/usr/local/bin:"
            "/usr/sbin:/usr/bin:/sbin:/bin"
        )
    if extra_env:
        merged_env.update(extra_env)

    trainer = CustomTrainerContainer(
        image=image,
        num_nodes=num_nodes,
        resources_per_node={"google.com/tpu": str(tpus_per_node)},
        env=merged_env,
    )

    # 3. Attach GKE accelerator patch and volume mounts
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
                                            volumes=volumes,
                                            containers=[
                                                ContainerPatch(
                                                    name="node",
                                                    volume_mounts=volume_mounts,
                                                )
                                            ],
                                        )
                                    )
                                )
                            ),
                        )
                    ]
                )
            )
        )
    )

    # 4. Build TrainerCommand and submit via Kubeflow TrainerClient
    if module:
        cmd_parts = [
            "torchrun",
            f"--nproc_per_node={tpus_per_node}",
            "-m",
            main_script,
        ]
        if args:
            cmd_parts.extend(args)
        trainer_cmd = ["bash", "-c", f"cd /workspace && exec {' '.join(cmd_parts)}"]
    else:
        trainer_cmd = [
            "torchrun",
            f"--nproc_per_node={tpus_per_node}",
            f"/workspace/{main_script}",
        ]
        if args:
            trainer_cmd.extend(args)

    trainer_client = TrainerClient(
        backend_config=KubernetesBackendConfig(namespace=namespace)
    )
    submitted_name = trainer_client.train(
        runtime="torch-distributed",
        trainer=trainer,
        options=[
            Name(job_name),
            TrainerCommand(trainer_cmd),
            tpu_patch,
        ],
    )
    print(
        f"Successfully submitted multi-host TrainJob '{submitted_name}' "
        f"(hosts={num_nodes}, chips={num_nodes * tpus_per_node})"
    )
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
                for p in sorted(pods, key=lambda x: x.metadata.name)
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
    max_iterations: int = 120,
    namespace: Optional[str] = None,
    log_filter: Optional[Callable[[str, str], Optional[str]]] = None,
):
    """Streams and filters distributed logs from all worker pods."""
    namespace = namespace or get_current_namespace()
    core_v1, _ = _init_k8s_clients()

    print(f"Streaming distributed training logs for TrainJob '{job_name}'...\n")
    completed_phases = {"Succeeded", "Failed"}
    logged_lines: Dict[str, int] = {}

    default_skip = (
        "cachednslookup",
        "compilation_cache",
        "OMP_NUM_THREADS",
        "TcMallocWarning",
    )

    for _ in range(max_iterations):
        pods = core_v1.list_namespaced_pod(
            namespace=namespace,
            label_selector=f"jobset.sigs.k8s.io/jobset-name={job_name}",
        ).items

        all_completed = len(pods) >= num_nodes and all(
            p.status.phase in completed_phases for p in pods
        )

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
                    new_lines = lines[logged_lines[pod_name] :]
                    logged_lines[pod_name] = len(lines)
                    prefix = f"[{pod_name.split('-')[-3]}-{pod_name.split('-')[-2]}]"
                    for line in new_lines:
                        if any(skip in line for skip in default_skip):
                            continue
                        if log_filter is not None:
                            filtered = log_filter(pod_name, line)
                            if filtered is None or filtered is False:
                                continue
                            if isinstance(filtered, str):
                                line = filtered
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
