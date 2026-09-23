# workspaces

The Kubernetes-native interactive dev environment for Notebooks, IDEs, and AI Agents — standalone, lightweight, and optimized for GKE.

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
| [`examples/`](examples/) | End-to-end examples written for people who have never used Kubernetes: stateful pause & resume, distributed Spark + TPU training, and AI agent sandboxes. See [`examples/README.md`](examples/README.md). |
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
