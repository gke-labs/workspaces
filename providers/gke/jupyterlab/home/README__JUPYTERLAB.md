# About the JupyterLab Images (CPU, GPU, TPU)

This file contains notes about the custom Kubeflow Notebooks _JupyterLab_ workspace images built with JAX, `numpy`, `pandas`, `google-cloud-storage`, and Kubeflow SDKs (`kubeflow[spark]`, `kfp`).

## Accelerator Support & Variants

This WorkspaceKind is available in three specialized variants:
- **CPU (`jupyterlab:latest-cpu`)**: Extends `jupyter-scipy:v1.10.0` with `jax[cpu]`, `google-cloud-storage`, and `kubeflow[spark]`.
- **CUDA GPU (`jupyterlab:latest-gpu`)**: Extends `jupyter-pytorch-cuda-full:v1.10.0` with PyTorch (CUDA 12.4), `jax[cuda12]`, `google-cloud-storage`, and `kubeflow[spark]`.
- **TPU (`jupyterlab:latest-tpu`)**: Extends `jupyter-scipy:v1.10.0` with Cloud TPU runtime library (`libtpu`), `jax[tpu]`, `google-cloud-storage`, and `kubeflow[spark]`.

### Verifying Hardware Acceleration

Open and run `jax_example.ipynb` or `distributed_tpu_example.ipynb` to verify your accelerator device:
- On CPU pods: `Device Kind: cpu`
- On GPU pods: `Device Kind: gpu` (`CudaDevice`)
- On TPU pods: `Device Kind: tpu` (`TpuDevice`)
