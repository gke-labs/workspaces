# GKE ComputeClasses for GPU and TPU Workspaces

Several of the examples one level up in [`examples/`](../) need a GPU or a TPU.
This folder contains the GKE **ComputeClass** objects that make those
accelerators available to your cluster — *without* you having to create node
pools by hand.

> [!TIP]
> **Already completed the one-time platform setup?** Applying these manifests is **step 6** of the [one-time setup in Examples README](../README.md#6-apply-the-computeclasses-gpu--tpu-examples-only). If you already ran `kubectl apply -f examples/compute-classes/` during platform setup, they are already applied and you can jump straight to verifying them under [Apply them](#apply-them) or proceed to your GPU/TPU example.

---

## What is a ComputeClass, in plain terms?

When you ask Kubernetes to run something, it has to find a machine ("node") to
run it on. A normal GKE cluster only has the CPU machines you created up front,
so a Pod that asks for a GPU will just sit in `Pending` forever.

A [ComputeClass](https://cloud.google.com/kubernetes-engine/docs/concepts/about-custom-compute-classes)
is a named recipe that says *"when a Pod asks for me, go create a machine that
looks like this"*. Each recipe below has `nodePoolAutoCreation.enabled: true`,
which means GKE will automatically create (and later delete) the right node pool
on demand.

You use a ComputeClass by putting its name in a Pod's `nodeSelector`:

```yaml
nodeSelector:
  cloud.google.com/compute-class: "gpu-t4-spot"
```

The `WorkspaceKind` pod options shipped in
[`images/workspacekinds/`](../../images/workspacekinds/) already do this for you:
picking the **GPU T4 Spot** or **TPU v5 2x2** pod option in the Kubeflow UI
selects the matching ComputeClass.

### ComputeClass vs node pool vs machine type

Three terms that are easy to confuse, and that this page uses for three different
things:

| Term | What it is | Who creates it | Example |
| :--- | :--- | :--- | :--- |
| **ComputeClass** | The *recipe* you apply to the cluster. A Kubernetes object (`kind: ComputeClass`), and the only thing in this folder. | You, with `kubectl apply` | `gpu-t4-spot` |
| **Node pool** | A group of identical VMs in your GKE cluster. Because each recipe sets `nodePoolAutoCreation.enabled: true`, GKE creates and deletes these for you when a Pod matches the ComputeClass. | GKE, automatically | an auto-created pool backing `gpu-t4-spot` |
| **Machine type / accelerator** | The hardware the VMs in that pool actually are. The ComputeClass asks for the accelerator (`gpu.type`, `tpu.type`); GKE picks a compatible machine shape. | GKE, from the recipe | `nvidia-tesla-t4`, `tpu-v5-lite-podslice` |

You never name a node pool or a machine type in these manifests — you name the
ComputeClass in a Pod's `nodeSelector` and GKE works backwards to the hardware.

> [!IMPORTANT]
> All ComputeClasses here request **Spot** capacity. Spot VMs are 60–91% cheaper
> than on-demand, but Google can reclaim them at any time with 30 seconds'
> notice. That is fine for the examples; do not use them for production jobs you
> cannot afford to lose.

---

## What is in this folder

### [`gpu-compute-class.yaml`](gpu-compute-class.yaml)

| Name | Accelerator | GPUs per node | Used by |
| :--- | :--- | :--- | :--- |
| `gpu-l4-spot` | NVIDIA L4 (Spot) | 1 | General GPU workloads |
| `gpu-t4-spot` | NVIDIA T4 (Spot) | 1 | `gpu_t4_spot` pod option; [`resumable-notebooks`](../resumable-notebooks/) GPU example |

### [`tpu-compute-class.yaml`](tpu-compute-class.yaml)

In the table below, **Chips per node** is the literal `spec.priorities[].tpu.count`
field in the manifest — chips on *one* VM — and **Total chips** is what the whole
slice adds up to (chips per node × nodes), which is also the number in each
ComputeClass name.

| Name | TPU type | Chips per node (`count`) | Topology | Nodes (hosts) | Total chips | Used by |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| `tpu-v5-1-single-host` | `tpu-v5-lite-podslice` (Spot) | 1 | 1x1 | 1 | 1 | `tpu_1` pod option in the JupyterLab/VS Code WorkspaceKinds |
| `tpu-v5-4-single-host` | `tpu-v5-lite-podslice` (Spot) | 4 | 2x2 | 1 | 4 | `tpu` pod option in the JupyterLab/VS Code WorkspaceKinds |
| `tpu-v5-8-single-host` | `tpu-v5-lite-podslice` (Spot) | 8 | 2x4 | 1 | 8 | Larger single-host training |
| `tpu-v5-8-multi-host` | `tpu-v5-lite-podslice` (Spot) | 4 | 2x4 | 2 | 8 | [`distributed`](../distributed/) multi-host TPU training |
| `tpu-v5-32-multi-host` | `tpu-v5-lite-podslice` (Spot) | 4 | 4x8 | 8 | 32 | Large multi-host training |

---

## Prerequisites

> [!NOTE]
> The commands on this page use `${CLUSTER_NAME}`, `${LOCATION}`, `${REGION}` and
> `${PROJECT_ID}`. Nothing here sets them — export them yourself first, with the
> same values you used for the platform deployment (see
> [`../README.md`](../README.md#1-set-environment-variables--create-a-gke-cluster)):
>
> ```bash
> export PROJECT_ID="my-project"
> export CLUSTER_NAME="kubeflow-notebooks"
> export LOCATION="us-west1"   # the cluster's zone or region
> export REGION="us-west1"
> ```

1. **A GKE cluster with node auto-provisioning / Autopilot-style autoscaling.**
   ComputeClass node pool auto-creation requires GKE 1.30.3-gke.1451000 or later
   on a Standard cluster with node auto-provisioning enabled, or an Autopilot
   cluster. Check your version:

   ```bash
   gcloud container clusters describe "${CLUSTER_NAME}" \
     --location="${LOCATION}" --project="${PROJECT_ID}" \
     --format='value(currentMasterVersion)'
   ```

2. **Accelerator quota in your region.** This is the most common reason an
   example never starts. Check and request quota here:

   ```bash
   # GPU quota (example: T4 Spot in us-west1)
   gcloud compute regions describe "${REGION}" --project="${PROJECT_ID}" \
     --format="table(quotas.metric,quotas.limit,quotas.usage)" \
     | grep -i -E "preemptible|tpu|gpu"
   ```

   Request more at
   [IAM & Admin → Quotas](https://console.cloud.google.com/iam-admin/quotas).
   The relevant quota names are `PREEMPTIBLE_NVIDIA_T4_GPUS`,
   `PREEMPTIBLE_NVIDIA_L4_GPUS`, and `PREEMPTIBLE_TPU_V5_LITE_PODSLICE_CHIPS`.

3. **`kubectl` pointed at your cluster:**

   ```bash
   gcloud container clusters get-credentials "${CLUSTER_NAME}" \
     --location="${LOCATION}" --project="${PROJECT_ID}"
   ```

---

## Apply them

Run these from the **repository root** — the paths are relative to it — with
`kubectl` already pointed at the cluster (prerequisite 3). Apply them *before*
creating a Workspace that uses a GPU or TPU pod option; a Pod whose
`nodeSelector` names a ComputeClass that does not exist yet stays `Pending`.

```bash
# Everything:
kubectl apply -f examples/compute-classes/

# Or just what you need:
kubectl apply -f examples/compute-classes/gpu-compute-class.yaml
kubectl apply -f examples/compute-classes/tpu-compute-class.yaml
```

Verify:

```bash
kubectl get computeclasses
# NAME                    AGE
# gpu-l4-spot             5s
# gpu-t4-spot             5s
# tpu-v5-1-single-host    5s
# tpu-v5-32-multi-host    5s
# tpu-v5-4-single-host    5s
# tpu-v5-8-multi-host     5s
# tpu-v5-8-single-host    5s
```

Applying a ComputeClass costs nothing — no machines are created until a Pod
actually asks for one.

---

## Troubleshooting

| Symptom | Cause | Fix |
| :--- | :--- | :--- |
| `error: no matches for kind "ComputeClass"` | The `cloud.google.com/v1` ComputeClass CRD is not installed; your GKE version is too old. | Upgrade the cluster to 1.30.3-gke.1451000 or later. |
| Workspace Pod stuck in `Pending` for more than ~10 minutes | No capacity or no quota for the requested accelerator. | `kubectl describe pod <pod>` and read the `Events`. Look for `Insufficient nvidia.com/gpu`, `SchedulingFailed`, or quota errors. |
| Pod `Pending` with `0/N nodes are available: node(s) didn't match Pod's node affinity/selector` | The ComputeClass named in the `nodeSelector` does not exist. | `kubectl get computeclasses` and apply the manifests above. |
| Node appears, then disappears, and the Pod restarts | Spot VM was reclaimed by Google. | Retry, or edit the manifest and set `spot: false` (costs more). |
| Node pool creation takes a long time | Normal. GPU node pool creation takes 3–7 minutes; TPU slices can take 10+ minutes. | Watch with `kubectl get nodes -w`. |

---

## Cleanup

Stop any Workspaces and jobs that use these ComputeClasses **first** (see the
note below), then, from the repository root:

```bash
kubectl delete -f examples/compute-classes/
```

Deleting a ComputeClass does not delete node pools that are already running.
Delete any workloads using them first, and GKE will scale the auto-created node
pools back down to zero and remove them.
