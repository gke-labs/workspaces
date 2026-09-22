# Istio OSS Provider Example

This directory serves as an example of how to integrate Kubeflow Workspaces with Open Source Istio.

## Overview

Kubeflow Workspaces core is decoupled from Istio, allowing it to run on various ingress and service mesh solutions. This example demonstrates how to use the built-in Istio support via additive components and overlays.

## Prerequisites

- A Kubernetes cluster.
- `kubectl` installed and configured.
- `istioctl` installed (or use the setup script).

## Installation

You can use the scripts located in `developing/scripts/` or `testing/scripts/` as a reference for setting up Istio in your environment.

For example, to setup Istio as done in testing:

```bash
./testing/scripts/setup-istio.sh
```

## Configuration

To enable Istio support in Kubeflow Workspaces, you can use the Istio overlay located in `workspaces/controller/manifests/kustomize/overlays/istio`.

This overlay enables the Istio component which injects the necessary environment variables and sidecar configurations.

## Customization

You can customize the Istio installation by modifying the `IstioOperator` configuration files (e.g., `testing/istio-cni.yaml` or `developing/istio-developing.yaml`).
