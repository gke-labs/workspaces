# About the Code-Server Python Image (with Gemini Code Assist)

This file contains notes about the custom Kubeflow Notebooks _Code-Server Python_ image built with **Gemini Code Assist**, `numpy`, `pandas`, and Kubeflow SDKs.

## Pre-installed VS Code Extensions

This image comes with the following extensions pre-installed:
- **Gemini Code Assist** (`Google.geminicodeassist`) — AI coding assistant powered by Gemini
- **Python** (`ms-python.python`) — Python language support and debugging
- **Jupyter** (`ms-toolsai.jupyter`) — Jupyter Notebook support inside VS Code

## Getting Started with Gemini Code Assist in VS Code

1. Click the **Gemini Code Assist** icon in the Activity Bar on the left sidebar to open the extension panel.
2. Sign in with your Google account to activate AI code completion and chat.

## Accelerator Support & Variants

This image is available in three specialized variants:
- **CPU**: Standard lightweight image with CPU-optimized libraries (`jax[cpu]`, `numpy`, `pandas`, `matplotlib`).
- **CUDA GPU**: Configured with NVIDIA Container Toolkit runtime (`NVIDIA_VISIBLE_DEVICES=all`), PyTorch (`torch`, `torchvision`), and CUDA-enabled JAX (`jax[cuda12]`).
- **TPU**: Configured with Cloud TPU runtime library (`libtpu`) and TPU-enabled JAX (`jax[tpu]`).

### Verifying Hardware Acceleration

Open and run `jax_example.ipynb` to verify your accelerator device:
- On CPU pods: `Device Kind: cpu`
- On GPU pods: `Device Kind: gpu` (`CudaDevice`)
- On TPU pods: `Device Kind: tpu` (`TpuDevice`)


