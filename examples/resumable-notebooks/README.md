# Stateful Resumable Notebooks on GKE (CPU & GPU)

**Pause a notebook, walk away, come back tomorrow, and find every variable,
thread, and even GPU memory exactly where you left it — with zero cost while
paused.**

Normally, stopping a Kubernetes workload destroys it: your Python process dies,
your variables are gone, and restarting means re-running everything from the top.
This example demonstrates **stateful pause & resume**, where GKE takes a full
snapshot of the running container — CPU registers, the entire process tree, RAM,
open threads, and GPU VRAM — writes it to Cloud Storage, deletes the machine, and
later restores the *identical* process on a fresh machine.

| | Ordinary stop/start | Stateful pause/resume (this example) |
| :--- | :--- | :--- |
| Python variables | Lost | Preserved |
| Process ID | New | **Same PID** |
| Background threads | Killed | Still running |
| Loaded model in GPU VRAM | Must reload (~30 s) | **Bit-identical, instant** |
| Cost while paused | Zero | Zero (plus a few cents of Cloud Storage) |

Two self-contained verification notebooks are provided:

| Notebook | What it proves | Hardware |
| :--- | :--- | :--- |
| [`cpu_checkpoint_restore_example.ipynb`](cpu_checkpoint_restore_example.ipynb) | A ~160 MB NumPy array is bit-identical, the PID is unchanged, a 1 Hz ticker thread resumes, and wall-clock shows the freeze | CPU only |
| [`gpu_checkpoint_restore_example.ipynb`](gpu_checkpoint_restore_example.ipynb) | A 3-billion-parameter LLM (`Qwen/Qwen2.5-3B-Instruct`, ~6 GB fp16) stays resident in GPU VRAM with bit-identical weights and reproducible greedy decoding | 1 × NVIDIA T4 |

> [!TIP]
> **Already completed the one-time platform setup?** If you already followed steps 1–6 in the [Examples README](../README.md#start-here-one-time-setup-the-order-things-have-to-happen-in), your cluster, storage, images, and ComputeClasses are already in place. You can skip the prerequisites and jump straight to [Registering the `jupyterlab-resumable` WorkspaceKind](#registering-the-jupyterlab-resumable-workspacekind) (or [Running the Examples](#running-the-examples) if already registered).

### What survives a pause (and what does not)

The checkpoint captures the container's **init process tree** — everything under
the workspace's supervisor, including `jupyter-lab` and every notebook kernel it
spawned.

| Started how | Survives a pause? |
| :--- | :--- |
| A notebook kernel (any cell you ran in JupyterLab) | ✅ Yes — same PID, all Python variables, **and GPU memory** |
| A process launched from a JupyterLab **terminal** | ✅ Yes — it is also a child of `jupyter-lab` |
| A process launched with `kubectl exec … &` from your laptop | ❌ **No** — silently gone after resume |

> [!NOTE]
> Verified on a T4: a kernel holding both a Python variable and a live CUDA
> tensor came back after pause/resume with the **same kernel PID**, the variable
> intact, and the GPU tensor's checksum unchanged — the GPU allocation itself is
> restored, not just recreated.

> [!WARNING]
> `kubectl exec` starts processes in a separate exec session that is **not** part
> of the checkpointed process tree, so they are terminated by a pause and never
> come back. This surprises people who script a long job with
> `kubectl exec … -- nohup python train.py &` and then pause. Start long-running
> work from a **notebook cell or a JupyterLab terminal** instead.

---

## Glossary (if you are new to Kubernetes)

| Term | What it means here |
| :--- | :--- |
| **Pod** | The smallest unit Kubernetes runs: one or more containers on one machine. Your notebook is a Pod. |
| **Node** | A virtual machine in the cluster that Pods run on. |
| **Namespace** | A folder that groups and isolates resources. Your workspaces live in a *tenant namespace* such as `kubeflow-user`. |
| **PVC (PersistentVolumeClaim)** | A network disk mounted at `/home/jovyan`. It survives a restart, but it only holds *files* — not running memory. |
| **WorkspaceKind** | A cluster-wide template listing which images and machine sizes a user may pick when creating a workspace. |
| **gVisor** | A user-space sandbox that GKE runs your container inside. It is what makes checkpointing the process possible. Pods get `runtimeClassName: gvisor` injected automatically. |
| **GKE Pod Snapshots** | The GKE feature that writes the sandboxed process state to a Cloud Storage bucket and restores it later. |
| **ComputeClass** | A named recipe telling GKE what kind of machine to auto-create when a Pod asks for it (used here for the T4 GPU). See [`../compute-classes/`](../compute-classes/). |
| **`PodSnapshotStorageConfig`** | A cluster-scoped object that says *which Cloud Storage bucket* snapshots go to. |
| **Workload Identity** | How Pods authenticate to Google Cloud APIs (such as Cloud Storage) without any key files. |

---

## Prerequisites

Work through this checklist before you start. Each item has a command that tells
you whether you already satisfy it.

### 1. A GKE cluster with Pod Snapshots and gVisor

The cluster must have been created with `--enable-pod-snapshots`, and the node
pool running your workspaces must use `--image-type=cos_containerd --sandbox type=gvisor`.
For the GPU notebook the node must also be a **T4** that supports gVisor
(GKE 1.29.2+ provides the required nvproxy support).

```bash
gcloud container clusters describe "${CLUSTER_NAME}" \
  --location="${LOCATION}" --project="${PROJECT_ID}" \
  --format='value(podAutoscaling,currentMasterVersion)'

# The Pod Snapshot CRDs must exist:
kubectl get crd | grep podsnapshot
# podsnapshotpolicies.podsnapshot.gke.io
# podsnapshots.podsnapshot.gke.io
# podsnapshotstorageconfigs.podsnapshot.gke.io
```

See [`../../providers/gke/USER_GUIDE.md`](../../providers/gke/USER_GUIDE.md) for
the full `gcloud container clusters create` command.

### 2. The standalone Kubeflow Workspaces platform

Deploy it with [`../../providers/gke/deploy_standalone.sh`](../../providers/gke/deploy_standalone.sh).
That script installs the `gke-workspace-snapshot-addon`, creates the snapshot
bucket, wires up the IAM bindings, and creates the default
`PodSnapshotStorageConfig`.

```bash
kubectl -n kubeflow-workspaces get deploy gke-workspace-snapshot-addon
kubectl get podsnapshotstorageconfigs
# NAME                                    READY
# kubeflow-pod-snapshot-storage-config    True
```

### 3. Workspace images

Build and push the JupyterLab CPU and GPU images with
[`../../images/build.sh`](../../images/build.sh) and note the tags it prints:

```bash
cd images
./build.sh --jupyterlab --cpu --gpu --registry-path "${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}"
```

See [`../../images/README.md`](../../images/README.md) for the full reference.

> [!NOTE]
> For the CPU notebook you may instead use the public upstream image
> `ghcr.io/kubeflow/kubeflow/notebook-servers/jupyter-scipy:v1.10.0`. The GPU
> notebook needs CUDA and `transformers`, so it requires the custom GPU image.

### 4. The GPU ComputeClass (GPU notebook only)

The `GPU T4 Spot` pod option selects nodes through the `gpu-t4-spot`
ComputeClass. Without it, the workspace Pod stays `Pending` forever.

```bash
kubectl apply -f ../compute-classes/gpu-compute-class.yaml
kubectl get computeclass gpu-t4-spot
```

You also need **T4 Spot quota** in your region. See
[`../compute-classes/README.md`](../compute-classes/README.md).

### 5. Internet access from the workspace (GPU notebook only)

The GPU notebook downloads `Qwen/Qwen2.5-3B-Instruct` (~6 GB) from Hugging Face
on first run into `~/.cache/huggingface` on the home PVC.

---

## Registering the `jupyterlab-resumable` WorkspaceKind

[`manifests/workspacekind-resumable.yaml`](manifests/workspacekind-resumable.yaml)
is the WorkspaceKind that carries the two annotations that switch snapshotting on:

```yaml
podsnapshot.gke.kubeflow.org/enabled: "true"
podsnapshot.gke.kubeflow.org/storage-config: "kubeflow-pod-snapshot-storage-config"
```

It contains three placeholders you must replace. Run this from the repository
root:

```bash
export PROJECT_ID="my-project"
export REGION="us-west1"
export REPO_NAME="kubeflow-repo"
export TENANT_NAMESPACE="kubeflow-user"
export GCS_BUCKET="${TENANT_NAMESPACE}-bucket"

export CPU_IMAGE="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}/jupyterlab:latest-cpu"
export GPU_IMAGE="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}/jupyterlab:latest-gpu"

sed -e "s|<YOUR_CPU_IMAGE>|${CPU_IMAGE}|g" \
    -e "s|<YOUR_GPU_IMAGE>|${GPU_IMAGE}|g" \
    -e "s|<YOUR_GCS_BUCKET>|${GCS_BUCKET}|g" \
    examples/resumable-notebooks/manifests/workspacekind-resumable.yaml \
  | kubectl apply -f -
```

> [!WARNING]
> Do not run a bare `kubectl apply -f examples/resumable-notebooks/manifests`.
> The unsubstituted `<YOUR_CPU_IMAGE>` placeholder is not a valid image
> reference, and every Workspace created from the kind will fail to start with
> `InvalidImageName`.

Verify:

```bash
kubectl get workspacekind jupyterlab-resumable
kubectl get workspacekind jupyterlab-resumable \
  -o jsonpath='{.spec.podTemplate.options.imageConfig.values[*].spec.image}{"\n"}'
```

---

## Running the Examples

The verification workflow is self-contained within each notebook. Upload the file
to your workspace and follow the instructions inside the notebook:

1. **CPU Example**:
   - In Kubeflow Workspaces, create or open a **CPU workspace** using `jupyterlab-resumable` (or any workspace annotated with `podsnapshot.gke.kubeflow.org/enabled: "true"`), pod config `small_cpu` or `medium_cpu`.
   - Upload [`cpu_checkpoint_restore_example.ipynb`](cpu_checkpoint_restore_example.ipynb) using the upload button in the JupyterLab file browser.
   - Run **Steps 1 & 2** in the notebook to set up the in-memory state and ticker thread.
   - Pause the workspace using the **Stop** / **Pause** button in the Kubeflow Workspaces UI.
   - Once paused, resume the workspace using the **Start** / **Resume** button in the UI.
   - Run **Step 3** to verify that variables, process PID, array memory, and compute resumed intact.

2. **GPU Example**:
   - Create or open a **GPU workspace** using the `GPU T4 Spot` pod config and the GPU image.
   - Upload [`gpu_checkpoint_restore_example.ipynb`](gpu_checkpoint_restore_example.ipynb).
   - Run **Steps 1 & 2** to load the model into VRAM and compute initial tokens and weight fingerprint.
   - Pause the workspace using the **Stop** / **Pause** button in the UI, then resume it using the **Start** / **Resume** button.
   - Run **Step 3** to verify that model weights in VRAM remain bit-identical and generation continues without reloading.

> [!IMPORTANT]
> Only the container's own process tree is checkpointed. Anything you started
> through `kubectl exec` is killed when the workspace pauses. Run everything you
> want preserved from inside the notebook.

### What to expect

| Phase | CPU workspace | GPU workspace (T4, 3B model fp16) |
| :--- | :--- | :--- |
| Checkpoint (pause) | 12–25 s, ~0.5–1.2 GB to Cloud Storage | 115–127 s, ~8.3 GiB to Cloud Storage |
| Restore (resume) | 4–8 s | 11–12 s |

> [!CAUTION]
> Every pause writes a full memory image to Cloud Storage — 8.3 GiB for the GPU
> example. The deployment sets a 14-day object lifecycle rule on the snapshot
> bucket as a billing backstop; see
> [Changing the Controller's Default Bucket](#optional-changing-the-controllers-default-bucket-cluster-wide).

### Watching it happen

```bash
# The addon that drives snapshotting:
kubectl -n kubeflow-workspaces logs -l app=gke-workspace-snapshot-addon -f

# The snapshot objects themselves:
kubectl -n "${TENANT_NAMESPACE}" get podsnapshots,podsnapshotpolicies

# Confirm gVisor was injected into your workspace pod:
kubectl -n "${TENANT_NAMESPACE}" get pod -l notebooks.kubeflow.org/workspace-name \
  -o jsonpath='{.items[*].spec.runtimeClassName}{"\n"}'
# gvisor
```

---

## Creating a Custom `PodSnapshotStorageConfig` from a Bucket

If you do **not** want to use the default bucket supplied when deploying the controller, you do **not need to reconfigure or restart the controller**. You can define a custom `PodSnapshotStorageConfig` pointing to any bucket and configure individual workspaces (or custom WorkspaceKinds) to use it.

This allows different teams, tenants, or workspaces to use dedicated GCS buckets independently.

### Step 1: Prepare the Custom GCS Bucket & IAM Permissions

Ensure the custom bucket exists and has the required IAM bindings for both identities:

```bash
export REGION="us-central1"
export PROJECT_ID="my-gcp-project"
export TENANT_NAMESPACE="kubeflow-user"
export CUSTOM_SNAPSHOT_BUCKET="my-team-snapshots"

PROJECT_NUMBER=$(gcloud projects describe "${PROJECT_ID}" --format='value(projectNumber)')

# 1. Create the bucket
gcloud storage buckets create "gs://${CUSTOM_SNAPSHOT_BUCKET}" \
  --location="${REGION}" \
  --project="${PROJECT_ID}" \
  --uniform-bucket-level-access || true

# 2. Grant node-level Workload Identity access (checkpoint & restore)
gcloud storage buckets add-iam-policy-binding "gs://${CUSTOM_SNAPSHOT_BUCKET}" \
  --member="principalSet://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${PROJECT_ID}.svc.id.goog/namespace/${TENANT_NAMESPACE}" \
  --role="roles/storage.objectUser"

gcloud storage buckets add-iam-policy-binding "gs://${CUSTOM_SNAPSHOT_BUCKET}" \
  --member="principalSet://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${PROJECT_ID}.svc.id.goog/namespace/${TENANT_NAMESPACE}" \
  --role="roles/storage.bucketViewer"

# 3. Grant GKE Service Agent robot access (automatic snapshot deletion & cleanup)
gcloud storage buckets add-iam-policy-binding "gs://${CUSTOM_SNAPSHOT_BUCKET}" \
  --member="serviceAccount:service-${PROJECT_NUMBER}@container-engine-robot.iam.gserviceaccount.com" \
  --role="roles/storage.objectUser"

# 4. Set a 14-day lifecycle delete rule as a billing backstop
cat <<EOF > /tmp/snapshot-lifecycle.json
{
  "rule": [{"action": {"type": "Delete"}, "condition": {"age": 14}}]
}
EOF
gcloud storage buckets update "gs://${CUSTOM_SNAPSHOT_BUCKET}" \
  --lifecycle-file=/tmp/snapshot-lifecycle.json --project="${PROJECT_ID}"
rm -f /tmp/snapshot-lifecycle.json
```

### Step 2: Create the `PodSnapshotStorageConfig` Custom Resource

Create a cluster-scoped `PodSnapshotStorageConfig` defining your custom bucket and snapshot path:

```yaml
# custom-storage-config.yaml
apiVersion: podsnapshot.gke.io/v1
kind: PodSnapshotStorageConfig
metadata:
  name: my-team-storage-config
spec:
  snapshotStorageConfig:
    gcs:
      bucket: "my-team-snapshots"
      path: "kubeflow-notebooks"
```

Apply it to the cluster:

```bash
kubectl apply -f custom-storage-config.yaml
```

Verify that GKE acknowledges the storage config:

```bash
kubectl get podsnapshotstorageconfigs
# NAME                     READY
# my-team-storage-config   True
```

### Step 3: Direct Workspaces to Use the Custom Storage Config

To route a workspace's snapshots to `my-team-storage-config`, set the `podsnapshot.gke.kubeflow.org/storage-config` annotation.

#### On an Individual Workspace
Annotate the `Workspace` resource:

```yaml
apiVersion: kubeflow.org/v1beta1
kind: Workspace
metadata:
  name: my-workspace
  namespace: kubeflow-user
  annotations:
    podsnapshot.gke.kubeflow.org/enabled: "true"
    podsnapshot.gke.kubeflow.org/storage-config: "my-team-storage-config"
spec:
  ...
```

Or patch an existing workspace from the CLI:

```bash
kubectl -n kubeflow-user patch workspace my-workspace --type=merge -p '{
  "metadata": {
    "annotations": {
      "podsnapshot.gke.kubeflow.org/enabled": "true",
      "podsnapshot.gke.kubeflow.org/storage-config": "my-team-storage-config"
    }
  }
}'
```

#### On a Custom `WorkspaceKind`
If you are creating a dedicated `WorkspaceKind` for your team, annotate the kind:

```yaml
apiVersion: kubeflow.org/v1beta1
kind: WorkspaceKind
metadata:
  name: jupyterlab-resumable
  annotations:
    podsnapshot.gke.kubeflow.org/enabled: "true"
    podsnapshot.gke.kubeflow.org/storage-config: "my-team-storage-config"
```

All workspaces spawned from this kind will automatically store their snapshots in `gs://my-team-snapshots/kubeflow-notebooks/`.

### How the Controller Handles Custom Storage Configs

When a workspace pauses, `gke-workspace-snapshot-addon`:
1. Reads the `podsnapshot.gke.kubeflow.org/storage-config` annotation (falling back to the `WorkspaceKind` annotation, and then to `kubeflow-pod-snapshot-storage-config`).
2. Creates the per-workspace `PodSnapshotPolicy` with `spec.storageConfigName: "my-team-storage-config"`.
3. GKE attaches the snapshot trigger to the policy, streaming the pod's checkpoint directly to `gs://my-team-snapshots/`.

---

## (Optional) Changing the Controller's Default Bucket Cluster-Wide

If cluster administrators want to change the fallback default bucket used by `gke-workspace-snapshot-addon` across the entire cluster:

- **Before Deployment**: Set `export SNAPSHOT_GCS_BUCKET="my-org-snapshots"` before running `deploy_standalone.sh`.
- **On a Running Cluster**:
  ```bash
  # 1. Update the addon ConfigMap and restart the deployment
  kubectl -n kubeflow-workspaces patch configmap gke-workspace-snapshot-addon \
    --type merge -p '{"data":{"SNAPSHOT_GCS_BUCKET":"my-org-snapshots"}}'
  kubectl -n kubeflow-workspaces rollout restart deployment/gke-workspace-snapshot-addon

  # 2. Update the default PodSnapshotStorageConfig
  kubectl patch podsnapshotstorageconfig kubeflow-pod-snapshot-storage-config \
    --type merge -p '{"spec":{"snapshotStorageConfig":{"gcs":{"bucket":"my-org-snapshots"}}}}'
  ```

---

## Troubleshooting

| Symptom | Cause | Fix |
| :--- | :--- | :--- |
| Workspace shows **`Error`** (not `Pending`) within a minute of creation, with `stateMessage: Workspace Pod is unschedulable: 0/N nodes are available…` | **Usually normal.** No GPU node exists yet, so the scheduler reports the pod unschedulable and the controller surfaces that as `Error` — *while* GKE provisions one in the background. A cold T4 Spot node typically takes 5–10 minutes. | Confirm a node really is coming: `kubectl describe pod <pod> -n "${TENANT_NAMESPACE}"` and look for a `TriggeredScaleUp` event (e.g. `nap-n1-standard-16-spot-gpu1-… 0->1`). If you see `TriggeredScaleUp`, just wait — the state flips to `Running` on its own. Only if there is **no** `TriggeredScaleUp`, or you see `FailedScaleUp`, is something actually wrong: see the next row. |
| Workspace Pod stuck `Pending` for >10 min (GPU) | The `gpu-t4-spot` ComputeClass is missing, or you have no T4 Spot quota in the region. | `kubectl apply -f ../compute-classes/gpu-compute-class.yaml`; check quota per [`../compute-classes/README.md`](../compute-classes/README.md). `kubectl describe pod <pod>` shows the real reason under `Events`. |
| Pod starts, but `runtimeClassName` is empty | The `gke-workspace-snapshot-addon` mutating webhook did not fire. | Check the addon is running (`kubectl -n kubeflow-workspaces get deploy gke-workspace-snapshot-addon`) and that the WorkspaceKind or Workspace carries `podsnapshot.gke.kubeflow.org/enabled: "true"`. |
| Workspace fails with `InvalidImageName` | The `<YOUR_CPU_IMAGE>` / `<YOUR_GPU_IMAGE>` placeholders were never substituted. | Re-apply the WorkspaceKind through the `sed` pipeline above. |
| Pause hangs, or the workspace comes back with a fresh PID | The checkpoint failed and GKE fell back to a cold start. | `kubectl -n kubeflow-workspaces logs -l app=gke-workspace-snapshot-addon` and `kubectl -n "${TENANT_NAMESPACE}" describe podsnapshot <name>`. |
| Snapshot errors with a Cloud Storage `403` | Workload Identity or the GKE service agent binding is missing on the snapshot bucket. | Re-run the IAM bindings from [Step 1](#step-1-prepare-the-custom-gcs-bucket--iam-permissions), including the `service-<PROJECT_NUMBER>@container-engine-robot.iam.gserviceaccount.com` grant. |
| Variables survive, but a terminal you opened is gone | Expected. Only the container's own process tree is checkpointed; `kubectl exec` sessions are not. | Run long-lived work from inside the notebook. |
| GPU notebook OOMs while pausing | Snapshotting copies GPU state through Pod memory; the node needs headroom. | The `gpu_t4_spot` pod config requests 12 CPU / 40 Gi for exactly this reason. Do not shrink it. |
| Restore fails after switching machine types | A snapshot cannot be restored onto a different GPU model. | Resume on the same ComputeClass you paused on. |

> [!NOTE]
> Known constraints: fp16 only on Turing (T4), multi-GPU only on L4, MIG is not
> supported, and you cannot restore a snapshot onto a different GPU type.

---

## Cleanup

```bash
# Delete the workspaces (this also releases their PVCs if the kind is configured to).
kubectl -n "${TENANT_NAMESPACE}" delete workspaces --all

# Remove leftover snapshot objects.
kubectl -n "${TENANT_NAMESPACE}" delete podsnapshots,podsnapshotpolicies,podsnapshotmanualtriggers --all

# Remove the WorkspaceKind.
kubectl delete workspacekind jupyterlab-resumable

# Optional: remove the GPU ComputeClass (auto-created node pools scale to zero and are removed).
kubectl delete -f ../compute-classes/gpu-compute-class.yaml
```

Optional — delete the checkpoint data from Cloud Storage.

`SNAPSHOT_GCS_BUCKET` is the snapshot bucket created by
[`deploy_standalone.sh`](../../providers/gke/deploy_standalone.sh); it defaults to
`<tenant-namespace>-snapshots-bucket`. Read the value the cluster is actually using
rather than guessing:

```bash
SNAPSHOT_GCS_BUCKET=$(kubectl -n kubeflow-workspaces get configmap \
  gke-workspace-snapshot-addon -o jsonpath='{.data.SNAPSHOT_GCS_BUCKET}')
echo "${SNAPSHOT_GCS_BUCKET}"

gcloud storage rm --recursive "gs://${SNAPSHOT_GCS_BUCKET}/**"
```

> [!NOTE]
> Deleting a Workspace does **not** always delete its home PVC — that depends on the
> WorkspaceKind. Check for leftovers with
> `kubectl -n "${TENANT_NAMESPACE}" get pvc`; unused PVCs keep billing.

[`../../providers/gke/cleanup_standalone.sh`](../../providers/gke/cleanup_standalone.sh)
performs all of the in-cluster steps above as part of a full platform teardown.
