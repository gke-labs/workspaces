# Providers

This directory contains custom extensions, additive components, and deployment configurations for specific cloud providers or environments.

## Architecture

The core logic of Kubeflow Notebooks (located in the `workspaces/` directory) is designed to be provider-agnostic. Any custom logic, specialized proxies, or environment-specific manifests should be placed here to keep the core codebase clean and easy to sync with upstream.

## Directory Structure

Each provider should have its own subdirectory:

```text
providers/
├── gke/         # Google Kubernetes Engine specialized components
├── istio-oss/   # Open Source Istio integration example
└── <provider>/  # Future provider implementations
```

## Guidelines for New Providers

1. **Additive Customization**: Provider-specific code should ideally be additive and not require modifying upstream files in `workspaces/`.
2. **Decoupling**: Ensure that core features can still function without the provider-specific components.
3. **Licensing**: New files added to this directory must include appropriate copyright headers (e.g., Google LLC for Google-authored code).
