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
  - [Agent Sandbox MCP server (`--mcp-server`)](#agent-sandbox-mcp-server---mcp-server)
- [Customizing Images](#customizing-images)
  - [Adding Python Dependencies](#adding-python-dependencies)
  - [Adding Sample Notebooks & Datasets](#adding-sample-notebooks--datasets)
  - [Adding VS Code Extensions](#adding-vs-code-extensions)
  - [Home Directory Persistence (`$HOME` & `$HOME_TMP`)](#home-directory-persistence-home--home_tmp)
- [Using Custom Images in Kubeflow WorkspaceKind](#using-custom-images-in-kubeflow-workspacekind)
  - [Ready-made WorkspaceKind templates](#ready-made-workspacekind-templates)
  - [Accelerator prerequisites (GPU / TPU pod options)](#accelerator-prerequisites-gpu--tpu-pod-options)

---

## Overview & Architecture

This repository builds four custom image targets:

1. **VS Code (`codeserver-python`)**:
   - Extends upstream Kubeflow `codeserver-python:v1.11.0`.
   - This is **code-server**: VS Code running inside the Pod and reached through a
     browser tab. It is not the VS Code application installed on your laptop.
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

4. **Agent Sandbox MCP Server (`agent-sandbox-mcp-server`)**:
   - The Model Context Protocol server that lets an AI agent provision and drive
     isolated Kubernetes sandbox pods. Consumed by
     [`examples/agent-sandbox/`](../examples/agent-sandbox/).
   - Built from a **pinned upstream tag** of
     [`kubernetes-sigs/agent-sandbox`](https://github.com/kubernetes-sigs/agent-sandbox)
     (`AGENT_SANDBOX_VERSION`, default `v1.0.3`), because upstream ships the
     source at `clients/integrations/mcp-server/` but does not publish a
     prebuilt image to `registry.k8s.io`.
   - **Not built by `--all`.** Opt in with `--mcp-server`. Single generic image —
     no CPU/GPU/TPU variants.

### Hardware Accelerator Variants

| Accelerator | Dockerfile | Environment & Libraries |
| :--- | :--- | :--- |
| **CPU** | `Dockerfile` | Lightweight CPU stack (`jax[cpu]`, `numpy`, `pandas`, `matplotlib`, `kfp`, `kubeflow[spark]`) |
| **CUDA GPU** | `Dockerfile.gpu` | CUDA 12, `NVIDIA_VISIBLE_DEVICES=all`, PyTorch (`torch`, `torchvision`), `jax[cuda12]` |
| **Cloud TPU** | `Dockerfile.tpu` | Cloud TPU driver (`libtpu`), `jax[tpu]`, Kubeflow SDKs |

### Bundled Samples

All files placed inside [`samples/`](samples/) are automatically baked into workspace container images under `/home/jovyan/samples`.

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

> [!IMPORTANT]
> Only `PROJECT_ID` is a placeholder you must replace; the other values are working
> defaults. Every later command in this document — `gcloud`, `build.sh`, and the
> `envsubst` templating — reads `PROJECT_ID`, `REGION`, `REPO_NAME` and `REGISTRY`
> from these exports, so run them first and stay in the same shell. Nothing sets
> them for you.

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

[`build.sh`](build.sh) is a cloud-agnostic build and push script. The user provides the container registry path in one of three interchangeable ways — the `--registry-path` flag (aliases `--registry`, `-r`), the `REGISTRY` or `REGISTRY_PATH` environment variable, or a positional argument. All three set the same value, referred to below as `${REGISTRY}`; the flag wins over the environment.

> [!IMPORTANT]
> Unless you pass `--no-push`, `build.sh` pushes as soon as each image finishes, so
> the destination repository must already exist and your Docker CLI must already be
> authenticated to it — steps
> [3](#3-create-artifact-registry-docker-repository) and
> [4](#4-configure-docker-authentication) above. `build.sh` does not create the
> repository.

Images produced:
- `${REGISTRY}/codeserver-python:<tag>`
- `${REGISTRY}/jupyterlab:<tag>`
- `${REGISTRY}/spark-py312:<tag>`
- `${REGISTRY}/agent-sandbox-mcp-server:<tag>` (only with `--mcp-server`)

### CLI Reference

*(Verbatim output of `./build.sh --help`.)*

```
Usage: ./build.sh [options] [registry-path]

Images and Hardware Variants:
  codeserver   VS Code (codeserver-python) [cpu, gpu, tpu]
  jupyterlab   JupyterLab (jupyterlab)     [cpu, gpu, tpu]
  spark        Apache Spark (spark-py312)  [generic/cpu]
  mcp-server   Agent Sandbox MCP server (agent-sandbox-mcp-server) [generic/cpu]

Target Selection Options:
  --image, -i <name>        Select target image: codeserver, jupyterlab, spark, mcp-server, or all
                            (default: all, which excludes mcp-server)
  --codeserver              Shorthand to build codeserver-python
                            (alias: --codeserver-python)
  --jupyterlab              Shorthand to build jupyterlab
  --spark                   Shorthand to build spark-py312
  --mcp-server              Shorthand to build the Agent Sandbox MCP server
                            (alias: --agent-sandbox-mcp-server)
                            (needed by examples/agent-sandbox; not built by --all)
  --all                     Build codeserver, jupyterlab and spark (default)

Hardware Accelerator Options:
  --hardware, --hw, -hw,    Select accelerator variant: cpu, gpu, tpu, or all (default: all)
  --variant, -v <variant>
  --cpu                     Shorthand for hardware variant 'cpu'
  --gpu                     Shorthand for hardware variant 'gpu'
  --tpu                     Shorthand for hardware variant 'tpu'

Registry & Tag Options:
  --registry-path, -r <path> Container registry path (aliases: --registry; or pass as a
                             positional argument, or set REGISTRY_PATH or REGISTRY).
                             The flag wins over the environment.
                             Example: us-central1-docker.pkg.dev/my-proj/notebooks
                             Images produced:
                               <registry-path>/codeserver-python:<tag>
                               <registry-path>/jupyterlab:<tag>
                               <registry-path>/spark-py312:<tag>
                               <registry-path>/agent-sandbox-mcp-server:<tag>
  --tag, -t <tag>            Image tag prefix (env var: IMAGE_TAG; default: timestamp,
                             e.g. v20260922-120000). The hardware suffix -cpu/-gpu/-tpu
                             is appended automatically; do not include one here.

Execution Options:
  --no-push                  Build locally only; do not push to container registry
  --push                     Push images to registry after build (default: true)
  --cloud-build              Use Google Cloud Build (gcloud builds submit) instead of local Docker
  --dry-run                  Print build plan and commands without executing
  -h, --help                 Show this help message
```

> [!NOTE]
> `--all` (the default) builds **codeserver, jupyterlab and spark only**. The
> Agent Sandbox MCP server is deliberately excluded because it is only needed by
> [`examples/agent-sandbox/`](../examples/agent-sandbox/); build it explicitly
> with `--mcp-server` (also accepted: `--agent-sandbox-mcp-server`, or
> `--image mcp-server`).

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

#### 4b. Build the Agent Sandbox MCP server
Required by [`examples/agent-sandbox/`](../examples/agent-sandbox/) and **not**
included in `--all`:
```bash
./build.sh --mcp-server --registry-path "${REGISTRY}"
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

In the list below, `<tag>` is the tag *prefix*: the value you pass to `--tag`/`-t`
(or the `IMAGE_TAG` environment variable), defaulting to a UTC build timestamp such
as `v20260922-120000`. `build.sh` appends the hardware suffix `-cpu`, `-gpu` or
`-tpu` itself — do not include a suffix in `--tag`.

- **CPU Variant**:
  - `${REGISTRY}/<image>:<tag>-cpu`
  - `${REGISTRY}/<image>:latest-cpu`
  - `${REGISTRY}/<image>:<tag>` (primary default tag)
  - `${REGISTRY}/<image>:latest` (primary latest tag)

- **CUDA GPU Variant**:
  - `${REGISTRY}/<image>:<tag>-gpu`
  - `${REGISTRY}/<image>:latest-gpu`

- **Cloud TPU Variant**:
  - `${REGISTRY}/<image>:<tag>-tpu`
  - `${REGISTRY}/<image>:latest-tpu`

- **Spark**:
  - `${REGISTRY}/spark-py312:<tag>`
  - `${REGISTRY}/spark-py312:latest`

- **Agent Sandbox MCP server**:
  - `${REGISTRY}/agent-sandbox-mcp-server:<tag>`
  - `${REGISTRY}/agent-sandbox-mcp-server:latest`

These `-cpu` / `-gpu` / `-tpu` tags are exactly what the `CPU_IMAGE_TAG`,
`GPU_IMAGE_TAG` and `TPU_IMAGE_TAG` placeholders in the
[`WorkspaceKind` templates](#ready-made-workspacekind-templates) expect.

---

### Agent Sandbox MCP server (`--mcp-server`)

[`agent-sandbox-mcp-server/Dockerfile`](agent-sandbox-mcp-server/Dockerfile)
builds the Model Context Protocol server used by
[`examples/agent-sandbox/`](../examples/agent-sandbox/). It exposes five core
tools to an AI agent — `create_sandbox`, `upload_file`, `execute_command`,
`download_file`, `delete_sandbox` — and serves streamable HTTP on port `8000`.

| Property | Value |
| :--- | :--- |
| Image name | `${REGISTRY}/agent-sandbox-mcp-server` |
| Built by `--all`? | **No.** Opt in with `--mcp-server` / `--agent-sandbox-mcp-server` / `--image mcp-server`. |
| Hardware variants | None. Single generic image; `--cpu`/`--gpu`/`--tpu` are ignored. |
| Base image | `python:3.12-slim`, runs as non-root `appuser`, `EXPOSE 8000` |
| Upstream pin | `ARG AGENT_SANDBOX_VERSION` (default `v1.0.3`) |

It is a two-stage build: stage one does a shallow
`git clone --branch ${AGENT_SANDBOX_VERSION}` of
[`kubernetes-sigs/agent-sandbox`](https://github.com/kubernetes-sigs/agent-sandbox);
stage two `pip install`s `clients/integrations/mcp-server` from it. No upstream
source is vendored into this repository.

> [!NOTE]
> This target exists because upstream publishes the **source** for the MCP
> server but **no prebuilt image** — as of `v1.0.3`,
> `registry.k8s.io/agent-sandbox/mcp-server` has no tags, the release assets
> contain only YAML, and `k8s-agent-sandbox-mcp-server` is not on PyPI. Only
> `agent-sandbox-controller` and `python-runtime-sandbox` are published upstream.

> [!TIP]
> `build.sh` does not pass `--build-arg`, so to target a different upstream
> release either edit the `ARG AGENT_SANDBOX_VERSION=` line in the Dockerfile or
> build directly:
> ```bash
> docker build --build-arg AGENT_SANDBOX_VERSION=v1.0.4 \
>   -t "${REGISTRY}/agent-sandbox-mcp-server:v1.0.4" agent-sandbox-mcp-server
> ```
> Keep it in step with `AGENT_SANDBOX_VERSION` in
> `examples/agent-sandbox/deploy_agent_sandbox.sh`, which pins the operator and
> the sandbox runtime image.

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

To ensure samples persist across PVC mounts, each Dockerfile synchronizes `$HOME` to `$HOME_TMP`.
`HOME`, `HOME_TMP`, `NB_USER` and `NB_GID` are all defined by the upstream Kubeflow
base image, not by this repository, so the snippet below works unchanged:
```dockerfile
COPY --chown=${NB_USER}:${NB_GID} samples/. ${HOME}/samples/
RUN cp -p -r -T "${HOME}" "${HOME_TMP}" \
 && chmod -R g=u "${HOME_TMP}"
```

---

## Using Custom Images in Kubeflow WorkspaceKind

A `WorkspaceKind` is the cluster-wide "template" that tells Kubeflow which images a
user may pick when they create a Workspace. Two ordering rules apply: the images
must already be pushed to your container registry (the `WorkspaceKind` only
*references* them, it does not build them), and the `WorkspaceKind` must be
registered in the cluster before anyone can create a Workspace from it.

Reference the full image URIs in your `WorkspaceKind` manifest:

```yaml
apiVersion: kubeflow.org/v1beta1
kind: WorkspaceKind
spec:
  options:
    imageConfig:
      values:
        - id: "codeserver-python-cpu"
          spec:
            image: "us-central1-docker.pkg.dev/my-project/notebooks/codeserver-python:latest-cpu"
            imagePullPolicy: "IfNotPresent"
```

> [!NOTE]
> "Image" means two different things here. `image:` is the container image URI in
> your registry; `id:` is the identifier of the *image option* the user picks from
> a drop-down when creating a Workspace. They are independent strings — nothing
> checks that an option called `…-cpu` points at a `-cpu` tag, so keep them in step
> yourself.

### Ready-made WorkspaceKind templates

Two complete `WorkspaceKind` templates ship with this directory. They expose the
CPU, GPU and TPU variants of each image as selectable options, and they are what
the [`examples/`](../examples/) walkthroughs expect to find in the cluster.

| Template | `WorkspaceKind` name | Image options (`id`) | Pod options (`id`) |
| :--- | :--- | :--- | :--- |
| [`workspacekinds/jupyterlab.yaml`](workspacekinds/jupyterlab.yaml) | `jupyterlab` | `jupyterlab-cpu`, `jupyterlab-gpu`, `jupyterlab-tpu`, `jupyter-scipy:v1.10.0` | `tiny_cpu`, `small_cpu`, `medium_cpu`, `gpu_t4_spot`, `tpu` |
| [`workspacekinds/codeserver-python.yaml`](workspacekinds/codeserver-python.yaml) | `codeserver` | `codeserver-python-cpu`, `codeserver-python-gpu`, `codeserver-python-tpu`, `codeserver-python:v1.11.0` | `small_cpu`, `medium_cpu`, `gpu_t4_spot`, `tpu` |

> [!IMPORTANT]
> The second template's file is `codeserver-python.yaml` and its image is
> `codeserver-python`, but the `WorkspaceKind` it creates is named **`codeserver`**
> — that is the name `kubectl get workspacekinds` shows and the name the
> [`examples/`](../examples/) refer to. There is no `codeserver-python`
> `WorkspaceKind`.

> [!WARNING]
> **Applying `workspacekinds/jupyterlab.yaml` replaces the sample WorkspaceKind, it does
> not sit alongside it.** `WorkspaceKind` is a cluster-scoped resource identified by
> `metadata.name`, and *two different manifests in this repo both use the name
> `jupyterlab`*:
>
> | Manifest | Registered by | Image option `id`s it offers |
> | :--- | :--- | :--- |
> | `workspaces/controller/manifests/kustomize/samples/jupyterlab_v1beta1_workspacekind.yaml` | `deploy_standalone.sh` automatically (`APPLY_SAMPLE_WORKSPACEKIND=true`) | `jupyter-scipy:v1.10.0`, … (public `ghcr.io/kubeflow` images) |
> | [`workspacekinds/jupyterlab.yaml`](workspacekinds/jupyterlab.yaml) (this one) | you, with the `envsubst` command below | `jupyterlab-cpu`, `jupyterlab-gpu`, `jupyterlab-tpu`, `jupyter-scipy:v1.10.0` |
>
> Applying this template overwrites the sample. That is usually what you want — the
> examples need these options — but existing Workspaces keep running on their current
> `imageConfig` and users will see a different option list from then on. Set
> `APPLY_SAMPLE_WORKSPACEKIND=false` when deploying if you would rather the script never
> registered the sample in the first place.

They are templates, not final manifests: the image URIs contain placeholders that
you fill in with `envsubst`. **Both templates require all eight variables below** —
neither has a default for any of them.

| Placeholder | Meaning | Who sets it | Example |
| :--- | :--- | :--- | :--- |
| `PROJECT_ID` | Google Cloud project that owns the registry | You — exported in [step 1](#1-set-environment-variables--construct-registry) | `my-project` |
| `REGION` | Artifact Registry region | You — exported in [step 1](#1-set-environment-variables--construct-registry) | `us-west1` |
| `REPO_NAME` | Artifact Registry repository name | You — exported in [step 1](#1-set-environment-variables--construct-registry) | `kubeflow-repo` |
| `IMAGE_NAME` | Image name inside the repository; differs per template | You — set inline on the `envsubst` command below | `jupyterlab` or `codeserver-python` |
| `CPU_IMAGE_TAG` / `GPU_IMAGE_TAG` / `TPU_IMAGE_TAG` | Tag per accelerator variant; must name tags `build.sh` actually pushed | You — set inline on the `envsubst` command below | `latest-cpu` / `latest-gpu` / `latest-tpu` |
| `GCS_BUCKET` | Bucket name injected into every Workspace Pod as the `$GCS_BUCKET` environment variable | You — exported in the command below. Nothing here creates the bucket | `kubeflow-user-bucket` |

> [!NOTE]
> Both templates also inject a `REGISTRY` environment variable into every Workspace
> Pod, built from `REGION`, `PROJECT_ID` and `REPO_NAME`. It resolves to the same
> path as the `REGISTRY` you exported for `build.sh`, but it is a *pod* environment
> variable, not a build input.

> [!WARNING]
> **A variable you forget will not produce an error.** `envsubst` replaces an unset
> variable with an empty string and exits 0, so a missing `CPU_IMAGE_TAG` renders
> `image: ".../jupyterlab:"` and a missing `REPO_NAME` renders
> `image: "us-west1-docker.pkg.dev/my-project//jupyterlab:latest-cpu"`. `kubectl apply`
> accepts both, and the WorkspaceKind looks healthy in `kubectl get workspacekinds`;
> the failure surfaces only later, when a user creates a Workspace and its Pod can
> never pull the image. Inspect the rendered image URIs before applying:
>
> ```bash
> IMAGE_NAME="jupyterlab" CPU_IMAGE_TAG="latest-cpu" GPU_IMAGE_TAG="latest-gpu" \
> TPU_IMAGE_TAG="latest-tpu" envsubst < workspacekinds/jupyterlab.yaml | grep 'image:'
> ```
>
> Every line must end in a non-empty tag and contain no `//` after the hostname.

Register the JupyterLab `WorkspaceKind`. The `export` lines repeat
[step 1](#1-set-environment-variables--construct-registry) so this block also works
in a fresh shell; adjust the values to your own:

```bash
export PROJECT_ID="my-project"
export REGION="us-west1"
export REPO_NAME="kubeflow-repo"
export GCS_BUCKET="kubeflow-user-bucket"

IMAGE_NAME="jupyterlab" \
CPU_IMAGE_TAG="latest-cpu" \
GPU_IMAGE_TAG="latest-gpu" \
TPU_IMAGE_TAG="latest-tpu" \
  envsubst < workspacekinds/jupyterlab.yaml | kubectl apply -f -
```

Register the VS Code `WorkspaceKind`. This command reuses `PROJECT_ID`, `REGION`,
`REPO_NAME` and `GCS_BUCKET` exported above, so run it in the same shell:

```bash
IMAGE_NAME="codeserver-python" \
CPU_IMAGE_TAG="latest-cpu" \
GPU_IMAGE_TAG="latest-gpu" \
TPU_IMAGE_TAG="latest-tpu" \
  envsubst < workspacekinds/codeserver-python.yaml | kubectl apply -f -
```

Both commands read the templates through relative paths, so run them from this
`images/` directory.

Verify:

```bash
kubectl get workspacekinds
# NAME          DISPLAY NAME             DEPRECATED   HIDDEN   AGE
# codeserver    VS Code (code-server)                         10s
# jupyterlab    JupyterLab Notebook                           30s
```

> [!TIP]
> Prefer the immutable `v<timestamp>-<variant>` tags over `latest-*` for anything
> you care about reproducing. `build.sh` prints the exact tags it pushed.

### Accelerator prerequisites (GPU / TPU pod options)

The `gpu_t4_spot` and `tpu` pod options select nodes through GKE
[ComputeClasses](https://cloud.google.com/kubernetes-engine/docs/concepts/about-custom-compute-classes).
If those ComputeClasses do not exist in the cluster, the Workspace Pod stays
`Pending` forever. Apply them before using a GPU/TPU option:

```bash
kubectl apply -f ../examples/compute-classes/
```

See [`examples/compute-classes/README.md`](../examples/compute-classes/README.md)
for what each ComputeClass provisions and the quota you need.
