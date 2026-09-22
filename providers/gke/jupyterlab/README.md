# Custom JupyterLab WorkspaceKind & Images (CPU, GPU, TPU, Spark)

This directory contains the configuration and Dockerfiles for the **JupyterLab** `WorkspaceKind` (`jupyterlab`) in Kubeflow Notebooks v2 on Google Kubernetes Engine (GKE).

## Design Overview

Each variant extends upstream JupyterLab images (`jupyter-scipy:v1.10.0` or `jupyter-pytorch-cuda-full:v1.10.0`) with JAX, `google-cloud-storage`, `kubeflow[spark]`, `kfp`, and the demo notebooks:
1. **`Dockerfile.fast` (CPU)**: Extends `jupyter-scipy:v1.10.0` with `jax[cpu]`, `google-cloud-storage`, and `kubeflow[spark]`.
2. **`Dockerfile.fast.gpu` (CUDA GPU)**: Extends `jupyter-pytorch-cuda-full:v1.10.0` with `jax[cuda12]`, `google-cloud-storage`, and `kubeflow[spark]`.
3. **`Dockerfile.fast.tpu` (TPU)**: Extends `jupyter-scipy:v1.10.0` with `jax[tpu]`, `libtpu`, `google-cloud-storage`, and `kubeflow[spark]`.
4. **`Dockerfile.spark` (Spark 4.0.1)**: Custom Spark image with Python 3.12 matching `jupyterlab` for distributed Spark Connect workloads.
5. **`workspacekind.yaml`**: Configures `WorkspaceKind/jupyterlab` with `filterRules` matching CPU/GPU/TPU images to CPU/GPU/TPU `podConfig` profiles, `kubeflow-edit` RBAC permissions, `NB_PREFIX`, `REGISTRY`, and `GCS_BUCKET`.
6. **`workspacekind-resumable.yaml`**: Configures `WorkspaceKind/jupyterlab-resumable` containing only CPU and GPU pod configurations, enabling stateful pause and resume (PodSnapshot) across all profiles.

## Building & Registering

Use `../build_jupyterlab.sh` from the `gke/` directory:
```bash
./gke/build_jupyterlab.sh --register-workspacekind
```
