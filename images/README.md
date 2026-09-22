# Custom Kubeflow Workspace & Spark Image Builder

This directory is a dedicated, cloud-agnostic build workspace for creating custom container images for **Kubeflow Workspaces** and **Apache Spark** on Google Kubernetes Engine (GKE) or any standard Kubernetes cluster.

---

## Table of Contents

- [Overview & Architecture](#overview--architecture)
- [Setting Up Google Cloud Artifact Registry](#setting-up-google-cloud-artifact-registry)
  - [1. Set Environment Variables & Construct REGISTRY](#1-set-environment-variables--construct-registry)
  - [2. Enable Google Cloud APIs](#2-enable-google-cloud-apis)
  - [3. Create Artifact Registry Docker Repository](#3-create-artifact-registry-docker-repository)
  - [4. Configure Docker Authentication](#4-configure-docker-authentication)
  - [5. Grant Required IAM Roles](#5-grant-required-iam-roles)
- [Building Images with `build.sh`](#building-images-with-buildsh)
  - [CLI Reference](#cli-reference)
  - [Common Build Examples](#common-build-examples)
  - [Image Tagging Conventions](#image-tagging-conventions)
- [Customizing Images](#customizing-images)
  - [Adding Python Dependencies](#adding-python-dependencies)
  - [Adding Sample Notebooks & Datasets](#adding-sample-notebooks--datasets)
  - [Adding VS Code Extensions](#adding-vs-code-extensions)
  - [Home Directory Persistence (`$HOME` & `$HOME_TMP`)](#home-directory-persistence-home--home_tmp)
- [Using Custom Images in Kubeflow WorkspaceKind](#using-custom-images-in-kubeflow-workspacekind)

---

## Overview & Architecture

This repository builds three custom image targets:

1. **VS Code (`codeserver-python`)**:
   - Extends upstream Kubeflow `codeserver-python:v1.11.0`.
   - Pre-installed VS Code extensions:
     - **Gemini Code Assist** (`Google.geminicodeassist`) for AI code completion and chat
     - **Python** (`ms-python.python`) and **Jupyter** (`ms-toolsai.jupyter`)
   - Pre-installed Gemini CLI tool (`@google/gemini-cli`).
   - Supports **CPU**, **CUDA GPU**, and **Cloud TPU**.

2. **JupyterLab (`jupyterlab`)**:
   - Extends upstream Kubeflow `jupyter-scipy:v1.11.0` (CPU/TPU) and `jupyter-pytorch-cuda-full:v1.11.0` (GPU) pinned by immutable digests.
   - Pre-installed data science packages: JAX, PyTorch, Pandas, Matplotlib, Kubeflow SDKs (`kubeflow[spark]`, `kfp`).
   - Supports **CPU**, **CUDA GPU**, and **Cloud TPU**.

3. **Apache Spark (`spark-py312`)**:
   - Standalone Apache Spark 4.0.1 image built with Python 3.12, NumPy, Pandas, PyArrow, GCS connector, and PySpark Connect.
   - Compatible with Spark drivers in both VS Code and JupyterLab workspaces.

### Hardware Accelerator Variants

| Accelerator | Dockerfile | Environment & Libraries |
| :--- | :--- | :--- |
| **CPU** | `Dockerfile` | Lightweight CPU stack (`jax[cpu]`, `numpy`, `pandas`, `matplotlib`, `kfp`, `kubeflow[spark]`) |
| **CUDA GPU** | `Dockerfile.gpu` | CUDA 12, `NVIDIA_VISIBLE_DEVICES=all`, PyTorch (`torch`, `torchvision`), `jax[cuda12]` |
| **Cloud TPU** | `Dockerfile.tpu` | Cloud TPU driver (`libtpu`), `jax[tpu]`, Kubeflow SDKs |

### Bundled Samples

All files placed inside [`samples/`](samples/) are automatically baked into workspace container images under `/home/jovyan/samples` and symlinked to `/home/jovyan/` (`${HOME}`).

---

## Setting Up Google Cloud Artifact Registry

Google Artifact Registry is Google Cloud's recommended container registry. Follow these steps to configure your registry before building and pushing images.

### 1. Set Environment Variables & Construct REGISTRY

Define your Google Cloud parameters and construct the `REGISTRY` path from them:

```bash
# Your Google Cloud Project ID
export PROJECT_ID="your-gcp-project-id"

# Target Region (e.g. us-central1, us-east4, europe-west1)
export REGION="us-central1"

# Repository name
export REPO_NAME="notebooks"

# Construct REGISTRY path from the environment variables:
export REGISTRY="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}"

# Configure gcloud
gcloud config set project "${PROJECT_ID}"
```

### 2. Enable Google Cloud APIs

```bash
gcloud services enable \
  artifactregistry.googleapis.com \
  container.googleapis.com \
  cloudbuild.googleapis.com \
  --project="${PROJECT_ID}"
```

### 3. Create Artifact Registry Docker Repository

```bash
gcloud artifacts repositories create "${REPO_NAME}" \
  --repository-format=docker \
  --location="${REGION}" \
  --description="Kubeflow custom workspace and spark images" \
  --project="${PROJECT_ID}"
```

Verify that the repository is ready:

```bash
gcloud artifacts repositories describe "${REPO_NAME}" \
  --location="${REGION}" \
  --project="${PROJECT_ID}"
```

### 4. Configure Docker Authentication

Configure your local Docker CLI to authenticate with Google Artifact Registry:

```bash
gcloud auth configure-docker "${REGION}-docker.pkg.dev" --quiet
```

### 5. Grant Required IAM Roles

- **Image Builders (Developers / CI/CD)**:
  Requires write access to Artifact Registry:
  ```bash
  gcloud artifacts repositories add-iam-policy-binding "${REPO_NAME}" \
    --location="${REGION}" \
    --project="${PROJECT_ID}" \
    --member="user:your-email@example.com" \
    --role="roles/artifactregistry.writer"
  ```

- **GKE Nodes / Workload Identity (Image Pulling)**:
  Nodes in your GKE cluster need read access to pull images:
  ```bash
  GKE_SA="$(gcloud iam service-accounts list --filter="displayName:Compute Engine default service account" --format='value(email)')"

  gcloud artifacts repositories add-iam-policy-binding "${REPO_NAME}" \
    --location="${REGION}" \
    --project="${PROJECT_ID}" \
    --member="serviceAccount:${GKE_SA}" \
    --role="roles/artifactregistry.reader"
  ```

---

## Building Images with `build.sh`

[`build.sh`](build.sh) is a cloud-agnostic build and push script. The user provides the container registry path via `--registry-path` (or `REGISTRY` environment variable, or positional argument).

Images produced:
- `${REGISTRY}/codeserver-python:<tag>`
- `${REGISTRY}/jupyterlab:<tag>`
- `${REGISTRY}/spark-py312:<tag>`

### CLI Reference

```
Usage: ./build.sh [options] [registry-path]

Target Selection Options:
  --image, -i <name>         Select target: codeserver, jupyterlab, spark, or all (default: all)
  --codeserver               Shorthand to build codeserver-python
  --jupyterlab               Shorthand to build jupyterlab
  --spark                    Shorthand to build spark-py312
  --all                      Build all images (default)

Hardware Accelerator Options:
  --hardware, --hw,          Select accelerator variant: cpu, gpu, tpu, or all (default: all)
  --variant, -v <variant>
  --cpu                      Shorthand for hardware variant 'cpu'
  --gpu                      Shorthand for hardware variant 'gpu'
  --tpu                      Shorthand for hardware variant 'tpu'

Registry & Tag Options:
  --registry-path, -r <path> Container registry path (or pass as positional argument or REGISTRY env var)
                             Example: us-central1-docker.pkg.dev/my-proj/notebooks
  --tag, -t <tag>            Image tag prefix (default: timestamp, e.g. v20260922-120000)

Execution Options:
  --no-push                  Build locally only; do not push to container registry
  --push                     Push images to registry after build (default: true)
  --cloud-build              Use Google Cloud Build (gcloud builds submit) instead of local Docker
  --dry-run                  Print build plan and commands without executing
  -h, --help                 Show this help message
```

### Common Build Examples

#### 1. Build all images (CPU, GPU, TPU for codeserver & jupyterlab, plus spark)
```bash
./build.sh --all --registry-path "${REGISTRY}"

# Or since REGISTRY is in the environment:
./build.sh
```

#### 2. Build VS Code for NVIDIA GPU
```bash
./build.sh --codeserver --gpu --registry-path "${REGISTRY}"
```

#### 3. Build JupyterLab for Cloud TPU
```bash
# Pass registry as a positional argument:
./build.sh --jupyterlab --tpu "${REGISTRY}"
```

#### 4. Build Apache Spark
```bash
./build.sh --spark --registry-path "${REGISTRY}"
```

#### 5. Build locally without pushing (local testing)
```bash
./build.sh --codeserver --cpu --no-push my-local-repo
```

#### 6. Preview build plan (dry run)
```bash
./build.sh --all --registry-path "${REGISTRY}" --dry-run
```

#### 7. Build with Google Cloud Build (no local Docker required)
```bash
./build.sh --all --registry-path "${REGISTRY}" --cloud-build
```

#### 8. Build using other container registries
Because `build.sh` is cloud-agnostic, you can push to any registry:
```bash
# Docker Hub:
./build.sh --codeserver --gpu --registry-path docker.io/myusername

# GitHub Container Registry (ghcr.io):
./build.sh --all --registry-path ghcr.io/myorg
```

### Image Tagging Conventions

For each image variant built, the script generates both a pinned, immutable tag and a floating `:latest-*` tag:

- **CPU Variant**:
  - `${REGISTRY}/<image>:${TAG}-cpu`
  - `${REGISTRY}/<image>:latest-cpu`
  - `${REGISTRY}/<image>:${TAG}` (primary default tag)
  - `${REGISTRY}/<image>:latest` (primary latest tag)

- **CUDA GPU Variant**:
  - `${REGISTRY}/<image>:${TAG}-gpu`
  - `${REGISTRY}/<image>:latest-gpu`

- **Cloud TPU Variant**:
  - `${REGISTRY}/<image>:${TAG}-tpu`
  - `${REGISTRY}/<image>:latest-tpu`

- **Spark**:
  - `${REGISTRY}/spark-py312:${TAG}`
  - `${REGISTRY}/spark-py312:latest`

---

## Customizing Images

### Adding Python Dependencies

To add or modify Python packages, edit the corresponding requirements file:

- **CPU**: `codeserver-python/requirements.txt` or `jupyterlab/requirements.txt`
- **GPU**: `codeserver-python/requirements-gpu.txt` or `jupyterlab/requirements-gpu.txt`
- **TPU**: `codeserver-python/requirements-tpu.txt` or `jupyterlab/requirements-tpu.txt`

Then rebuild the affected image:
```bash
./build.sh --codeserver --gpu --registry-path "${REGISTRY}"
```

### Adding Sample Notebooks & Datasets

Any file placed inside [`samples/`](samples/) is automatically copied into the image at build time.

1. Add notebooks or data files into `images/samples/`.
2. Rebuild the image:
   ```bash
   ./build.sh --codeserver --cpu --registry-path "${REGISTRY}"
   ```
3. Inside the workspace container, your files will be accessible at:
   - `/home/jovyan/samples/`
   - `/home/jovyan/` (symlinked)

### Adding VS Code Extensions

To add VS Code extensions from [Open VSX](https://open-vsx.org/), edit `codeserver-python/Dockerfile`:

```dockerfile
RUN code-server --install-extension "<Publisher>.<ExtensionName>@<version>" --force
```

### Home Directory Persistence (`$HOME` & `$HOME_TMP`)

In Kubeflow Workspaces:
- A PersistentVolumeClaim (PVC) is mounted at `/home/jovyan` (`$HOME`).
- When a workspace is first created, the empty PVC hides any files written to `$HOME` during image build.
- Kubeflow's `s6` initialization script (`/etc/cont-init.d/01-copy-tmp-home`) copies everything from `$HOME_TMP` into `$HOME` if the mounted directory is empty.

To ensure samples persist across PVC mounts, each Dockerfile synchronizes `$HOME` to `$HOME_TMP`:
```dockerfile
COPY --chown=${NB_USER}:${NB_GID} samples/. ${HOME}/samples/
RUN ln -s -f ${HOME}/samples/* ${HOME}/ 2>/dev/null || true \
 && cp -p -r -T "${HOME}" "${HOME_TMP}" \
 && chmod -R g=u "${HOME_TMP}"
```

---

## Using Custom Images in Kubeflow WorkspaceKind

After pushing custom images to your container registry, reference the full image URIs in your `WorkspaceKind` manifest:

```yaml
apiVersion: kubeflow.org/v1beta1
kind: WorkspaceKind
spec:
  options:
    imageConfig:
      values:
        - id: "codeserver-python-cpu"
          spec:
            image: "us-central1-docker.pkg.dev/my-project/notebooks/codeserver-python:latest-gpu"
            imagePullPolicy: "IfNotPresent"
```
