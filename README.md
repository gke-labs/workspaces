# workspaces

The Kubernetes-native interactive dev environment for Notebooks, IDEs, and AI Agents — standalone, lightweight, and optimized for GKE.

## Interactive R&D: Easy, Safe Access to Cluster & Distributed Compute

Workspaces bridges the gap between infrastructure complexity and developer velocity. It gives researchers, ML engineers, and data scientists **easy, self-service access to high-performance Kubernetes compute** (CPUs, GPUs, Cloud TPUs) and **distributed compute resources**, while giving platform administrators **enterprise-grade safety, security, and isolation**.

### 1. Interactive Cloud IDEs That Scale Easily to Distributed Compute

Workspaces is **cloud-first**: users get instant, zero-install interactive development environments directly in the browser—co-located with high-throughput cluster storage, accelerators, and network fabrics. Each workspace is also the launchpad for distributed training and inference: prototype interactively on a single "VM", then scale the exact same code across multi-host cluster resources.

![Interactive Dev & Scaling to Distributed Multi-Host Training](examples/tpu/demo_tpu_workspaces.gif)

- **In-Browser Cloud IDEs**: Launch fully-featured **JupyterLab** or browser-based **VS Code (`code-server`)** environments in one click. No local CUDA/TPU driver installations, no environment drift across team members, and no massive datasets downloaded to laptops.
- **Instant Hardware Sizing**: Spin up interactive dev environments tailored to any workload shape—from 1 CPU or 1 TPU to multi-chip topologies (such as TPU v5e 2x2 or multi-GPU instances)—without writing Kubernetes YAML manifests.
- **Unified Code from Dev to Distributed Scale**: Prototype and test models interactively (e.g. using `jax.pmap` or PyTorch DDP), then submit multi-host training jobs (via the Kubeflow Trainer Python SDK, Ray, or Spark Operator) that reuse the **exact same training functions** across distributed nodes.
- **Safe & Auditable by Default**: Access is protected end-to-end by Google Identity-Aware Proxy (IAP) and Kubernetes RBAC (`SubjectAccessReview`). No SSH keys, no bastion hosts, no open node ports, and no cluster-admin kubeconfig credentials are required on developer laptops.
- **Tenant Isolation**: Each team runs their workspaces in their own tenant namespace with strict `NetworkPolicy` enforcement, persistent volume storage, and automated lifecycle management.

### 2. Supplementary Local Dev Mode for Jupyter Notebooks

For developers who prefer writing notebooks in their local desktop editor, Workspaces provides supplementary support to connect desktop VS Code (via the Jupyter extension) directly to remote in-cluster Jupyter kernels—keeping the familiar local editing experience while code runs on cluster accelerators.

![Connecting Local VS Code to Remote Cluster Accelerators](examples/tpu/demo_vscode_remote_tpu.gif)

- **Same Security Guarantees**: Local connections go through the same IAP + Kubernetes RBAC path as the cloud IDEs—no SSH keys, tunnels, or kubeconfig credentials on the laptop.

---

## Overview

This project is derived from upstream [Kubeflow Workspaces (Notebooks V2)](https://www.kubeflow.org/docs/components/workspaces/), the next-generation controller, backend, and web interface for interactive development environments on Kubernetes.

### Standalone Deployment (No Istio Required)

In the standard Kubeflow Community Distribution, Workspaces is coupled with a heavy platform stack requiring an Istio service mesh, sidecar injection, Dex authentication, and dozens of cluster-wide components.

This repository provides a **standalone deployment of Kubeflow Workspaces** with the option **not to depend on Istio**:
- **Istio-Free Architecture**: Replaces the Istio service mesh, ingress gateways, and sidecars with native cloud and Kubernetes primitives — on GKE, utilizing the GKE Gateway API (`gke-l7-global-external-managed`), Google Certificate Manager, Google Identity-Aware Proxy (IAP), an authenticated access proxy (`gke-access-proxy`) enforcing Kubernetes RBAC via `SubjectAccessReview`, and native Kubernetes `NetworkPolicy` rules.
- **A Gateway to Kubernetes**: Acts as a streamlined, self-service gateway to Kubernetes for data scientists, ML engineers, and AI developers without the operational overhead of the full, heavy Kubeflow community distribution. Users get containerized interactive environments (JupyterLab, browser-based VS Code via `codeserver`, desktop VS Code connected to remote kernels, or AI agent sandboxes) with direct access to cluster resources, accelerators (GPUs and TPUs), and distributed compute frameworks (such as Spark Operator and Kubeflow Trainer v2).

> [!NOTE]
> **Terminology: Workspaces and Notebooks**
> In this repository and throughout Kubeflow, the terms **workspaces** and **notebooks** are often used interchangeably (reflecting the evolution from "Kubeflow Notebooks" to "Kubeflow Workspaces / Notebooks v2"). You will encounter "notebooks" in legacy defaults, Go package paths, resource names, and Kubernetes label keys (such as `notebooks.kubeflow.org/workspace-name`).
>
> Importantly, **Workspaces is not limited to Jupyter notebooks** — it is a general-purpose, containerized interactive environment supporting diverse IDEs and runtimes including browser-based VS Code (`codeserver`), desktop VS Code connecting to remote kernels, AI agent sandboxes, and distributed compute (Spark Operator, Kubeflow Trainer v2) across CPU, GPU, and TPU environments.

## Repository layout

| Path | What it is |
| :--- | :--- |
| [`providers/gke/`](providers/gke/) | Everything needed to deploy Kubeflow Workspaces in **standalone mode on GKE** (no Istio): the IAP-authenticated access proxy, the Pod snapshot add-on, deployment manifests, and automation scripts (`deploy_standalone.sh` / `cleanup_standalone.sh`). Start with [`providers/gke/USER_GUIDE.md`](providers/gke/USER_GUIDE.md). |
| [`images/`](images/) | Cloud-agnostic build utilities for custom workspace images (VS Code, JupyterLab, Spark) across CPU/GPU/TPU, plus ready-made `WorkspaceKind` templates. See [`images/README.md`](images/README.md). |
| [`examples/`](examples/) | End-to-end examples written for people who have never used Kubernetes: stateful pause & resume, distributed Spark + TPU training, AI agent sandboxes, and elastic Ray clusters. See [`examples/README.md`](examples/README.md). |
| [`workspaces/`](workspaces/) | The upstream Kubeflow Workspaces controller, backend, and frontend. |

## Quick start

To deploy the standalone platform on an existing GKE cluster:

```bash
export PROJECT_ID="my-project"              # required
export CLUSTER_NAME="kubeflow-notebooks"    # must already exist
export LOCATION="us-west1"                  # the cluster's zone or region
export REGION="us-west1"                    # region for Artifact Registry and GCS
export PILOT_USERS="you@example.com"        # required; comma-separated
export TENANT_NAMESPACE="kubeflow-user"
export REPO_NAME="kubeflow-repo"            # Artifact Registry repository name

./providers/gke/deploy_standalone.sh
```

Every configuration variable is listed in [`providers/gke/USER_GUIDE.md`](providers/gke/USER_GUIDE.md).

For the complete, step-by-step walkthrough — from cluster creation and GCS data bucket setup to building custom images, registering `WorkspaceKind` templates, and running end-to-end examples — see [**`examples/README.md`**](examples/README.md).

## Contributing

This project is licensed under the [Apache 2.0 License](LICENSE).

We welcome contributions! Please see [docs/contributing.md](docs/contributing.md) for more information.

We follow [Google's Open Source Community Guidelines](https://opensource.google.com/conduct/).

## Disclaimer

This is not an officially supported Google product.

This project is not eligible for the Google Open Source Software Vulnerability Rewards Program.
