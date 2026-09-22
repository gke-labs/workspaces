# Standalone Kubeflow Workspaces on GKE: Automated Codelab & Pilot Record

> [!NOTE]
> **How this Codelab relates to [USER_GUIDE.md](USER_GUIDE.md):**
> - **[USER_GUIDE.md](USER_GUIDE.md)**: The comprehensive, step-by-step manual deployment guide explaining every GKE API, Gateway, Certificate Manager, IAP OAuth, Kubeflow Trainer, Spark Operator, and Workload Identity command in detail.
> - **CODELAB.md (this document)**:
>   - **Part I (Quickstart Codelab)**: A streamlined, end-to-end walkthrough using the automation scripts ([`deploy_standalone.sh`](deploy_standalone.sh), [`build_jupyterlab.sh`](build_jupyterlab.sh), and [`cleanup_standalone.sh`](cleanup_standalone.sh)) to deploy Standalone Kubeflow Workspaces (Notebooks v2), Kubeflow Trainer (v2), and Kubeflow Spark Operator on GKE **without Istio**, and run [`examples/distributed_tpu_example.ipynb`](examples/distributed_tpu_example.ipynb).
>   - **Part II (Maintainer Pilot Acceptance Record)**: Dated verification evidence and acceptance test records from the live GKE pilot deployments.

---

# Part I: Automated End-to-End Codelab

This quickstart deploys the entire standalone stack (Kubeflow Workspaces + Kubeflow Trainer v2 + Kubeflow Spark Operator) on GKE without Istio in 5 steps.

## Step 1: Configure Your Environment

Run commands from the repository root (`gke-notebook/`). Set your project, cluster, and user email:

```bash
export PROJECT="your-gcp-project-id"
export CLUSTER="kubeflow-notebooks"
export LOCATION="us-central1-c"             # Cluster zone or region
export REGION="us-central1"                 # Artifact Registry & GCS region
export PILOT_USERS="user1@example.com,user2@example.com"  # Comma- or space-separated Google emails to grant IAP & RBAC access
export TENANT_NAMESPACE="team-a"            # Tenant namespace
export GCS_BUCKET="${TENANT_NAMESPACE}-bucket"              # Workload data bucket (Spark ETL & TPU training)
export SNAPSHOT_GCS_BUCKET="${TENANT_NAMESPACE}-snapshots-bucket" # Dedicated GKE Pod Snapshot bucket
```

### Optional Configuration Options
- **Option A — What if I don't have a domain? (Default: `sslip.io` zero-DNS setup)**
  Leave `NOTEBOOK_HOST` unset. `deploy_standalone.sh` automatically reserves a global external IP and configures `notebooks.<GLOBAL_EXTERNAL_IP>.sslip.io`.
- **Option B — Custom Domain (`notebooks.example.com`)**
  ```bash
  export NOTEBOOK_HOST="notebooks.example.com"
  ```
- **Option C — Google-Managed OAuth (Users within your GCP Organization)**
  Leave `OAUTH_FILE`, `IAP_CLIENT_ID`, and `IAP_SECRET_NAME` unset.
- **Option D — Custom OAuth Client (External `@gmail.com` or cross-org users)**
  Create a Web Application OAuth client in Google Auth Platform with redirect URI `https://iap.googleapis.com/v1/oauth/clientIds/CLIENT_ID:handleRedirect`, download its JSON file, and set:
  ```bash
  export OAUTH_FILE="/absolute/private/path/oauth-client.json"
  ```
- **Option E — Access Proxy Kubernetes API Rate-Limiter (`KUBE_CLIENT_QPS` & `KUBE_CLIENT_BURST`)**
  By default, `gke-access-proxy` configures its Kubernetes API client with `KUBE_CLIENT_QPS=100` (`kubeClientQPS`) and `KUBE_CLIENT_BURST=200` (`kubeClientBurst`) and caches resolved Workspace targets for 5 seconds so JupyterLab's concurrent kernel/status/autosave bursts never trigger `client-go` client-side throttling (`503 Service Unavailable`). Override if needed:
  ```bash
  export KUBE_CLIENT_QPS="100"
  export KUBE_CLIENT_BURST="200"
  ```

---

## Step 2: Build Custom JupyterLab (CPU/GPU/TPU) & Spark Images

Build and push the custom JupyterLab and Spark 4.0.1 images (which bundle `jax[tpu]`, `kubeflow[spark]`, `kubeflow-trainer`, `google-cloud-storage`, and [`examples/distributed_tpu_example.ipynb`](examples/distributed_tpu_example.ipynb)):

```bash
./gke/build_jupyterlab.sh
```
*(Or add `--cloud-build` to build remotely with Google Cloud Build).*

---

## Step 3: Run the Automated Standalone Deployment

Run `deploy_standalone.sh` to automatically:
1. Enable required Google Cloud APIs (`container`, `compute`, `artifactregistry`, `certificatemanager`, `iap`).
2. Enable standard Gateway API on your GKE cluster (`--gateway-api=standard`) and wait for `gatewayclass/gke-l7-global-external-managed` to become `Accepted`.
3. Install `cert-manager` v1.21.2 for internal webhook certificates.
4. Build and push the 4 digest-pinned standalone Workspaces core images (`gke-access-proxy`, `gke-frontend`, `gke-controller`, `gke-backend`).
5. Reserve a global static IP, configure Google Certificate Manager (with `sslip.io` or your custom domain), and deploy the GKE Gateway and HTTPRoute.
6. Discover the exact GKE global backend service and configure the IAP audience (`IAP_AUDIENCE`).
7. Deploy **Kubeflow Trainer (v2)** (`jax-distributed`, `torch-distributed`, etc.) and **Kubeflow Spark Operator** without Istio.
8. Admit `${PILOT_USERS}` to IAP, configure tenant RBAC and baseline Pod Security in `${TENANT_NAMESPACE}`, register the `jupyterlab` `WorkspaceKind` and GPU/TPU `ComputeClasses`, and grant Workload Identity GCS access to `gs://${GCS_BUCKET}`.

```bash
./gke/deploy_standalone.sh
```

---

## Step 4: Connect to JupyterLab & Run `distributed_tpu_example.ipynb`

1. Open the public HTTPS URL printed at the end of `deploy_standalone.sh` (e.g. `https://notebooks.<IP>.sslip.io/workspaces/`) and sign in with one of the emails in `${PILOT_USERS}`.
   *(Note: If the Certificate Manager certificate is still `PROVISIONING`, wait 5–15 minutes for Load Balancer Authorization to reach `ACTIVE` status).*
2. Select namespace **`${TENANT_NAMESPACE}`** (e.g., `kubeflow-user` or `team-a`) and click **Create workspace**:
   - **Workspace Kind**: **JupyterLab Notebook** (`jupyterlab`)
   - **Image**: **jupyterlab (CPU)** (`jupyterlab-cpu`)
   - **Pod Configuration**: **Small CPU** (`small_cpu`)
   - **Home Volume**: Attach a new volume using StorageClass **`notebooks-gke-rwo`** (10 GiB)
3. Click **Create**, wait for **Running**, and click **Connect > JupyterLab**.
4. Inside JupyterLab, open `/home/jovyan/distributed_tpu_example.ipynb` (pre-installed in `jupyterlab:latest-cpu`) and run all cells:
   - **Stage 1 (Distributed Spark ETL)**: Submits a 1-driver + 4-executor Apache Spark job via `kubeflow.spark.SparkClient` (`SparkConnect`) that preprocesses 60k Fashion-MNIST images to `gs://${GCS_BUCKET}/processed/train/`.
   - **Stage 2 (Multi-Host TPU Training)**: Submits a 2-host Cloud TPU v5e `TrainJob` (8 TPU cores) via `kubeflow.trainer.TrainerClient` (`runtime="jax-distributed"`) that trains a data-parallel JAX MLP and writes weights/metrics to `gs://${GCS_BUCKET}/model/`.
   - **Stage 3 (Model Serving)**: Deploys the 2-replica `fashion-mnist-inference` CPU `Deployment` and `Service` and sends live `/predict` requests.

Monitor your distributed workloads from your terminal:
```bash
kubectl get workspaces,sparkconnects,trainjobs,jobsets,deployments,pods -n "${TENANT_NAMESPACE}"
```

---

## Step 5: Teardown & Cleanup

When finished, cleanly remove all deployed workloads and controllers:
```bash
./gke/cleanup_standalone.sh
```

---

# Part II: Maintainer Pilot Acceptance Record

## 2026-09-16 Stateful Workspace Pause & Resume via GKE Pod Snapshots (Zero-Core-Change Webhook & ReadinessGate Architecture)

- **Cluster**: `pilot-gke-dev` / `us-west1` / `kubeflow-notebooks`, GKE `1.35.7-gke.1222000`, gVisor Sandbox node pool (`gke-kubeflow-noteboo-cpu-autoscaling--26ba0686-hg8p`), GKE Pod Snapshot controller (`podsnapshot.gke.io/v1alpha1`).
- **Tenant Namespace**: `kubeflow-user`, Test Workspace: `ws-snapshot-test` (`Pod`: `ws-ws-snapshot-test-x2wbv-0`), Snapshot GCS Bucket: `gs://kubeflow-user-snapshots-bucket/kubeflow-notebooks`.
- **Architecture**: Implemented in `gke/` ([`internal/snapshot/`](internal/snapshot/), [`cmd/snapshot-addon/main.go`](cmd/snapshot-addon/main.go)) and shipped as the standalone `gke-workspace-snapshot-addon` Deployment, using two mutating webhooks (`POST /mutate-workspace` and `POST /mutate-pod`), a custom Pod `readinessGate` (`podsnapshot.gke.kubeflow.org/active`), and a watch-driven, leader-elected snapshot reconciler—requiring **zero code changes** to upstream `workspaces/controller`, `workspaces/backend`, or `workspaces/frontend`.

### Observed End-to-End Stateful Pause/Resume Verification

| Lifecycle Phase | Observed Kubernetes & Runtime State |
| --- | --- |
| **1. Workspace Creation & Pod Mutation** | `mutate-pod` intercepted `ws-ws-snapshot-test-x2wbv-0`, injected `runtimeClassName: gvisor`, `readinessGates: [{conditionType: "podsnapshot.gke.kubeflow.org/active"}]`, and mounted `ConfigMap/jupyter-ipc-config`. Reconciler created `PodSnapshotStorageConfig/kubeflow-pod-snapshot-storage-config`, `PodSnapshotPolicy/ws-ws-snapshot-test-policy`, and set `READINESS GATES: 1/1` (`STATE: Running`). |
| **2. In-Memory Kernel State Initialization** | Created Python 3 kernel `50ccf96b-280f-4637-9bad-d2d05ae3146e` over Unix domain IPC (`"transport": "ipc"`, `/tmp/jupyter_runtime/kernel-50ccf96b-280f-4637-9bad-d2d05ae3146e-ipc`) and executed `a = 100; b = 200; c = a * b` (`VALUE_OF_C= 20000`). |
| **3. Pause Interception & Option 1 UI/Network Lockout** | Patched `Workspace/ws-snapshot-test` with `{"spec":{"paused":true}}`. Within `<1s`, `mutate-workspace` rewrote `spec.paused` to `false`, set `podsnapshot.gke.kubeflow.org/checkpoint-state: Checkpointing`, and flipped `podsnapshot.gke.kubeflow.org/active` and `PodReady` to `False` (`READINESS GATES: 0/1`). `Workspace.status.state` immediately exited `Running`, disabling UI **Connect**, hiding **Stop**, blocking premature **Start**, and draining open sockets. |
| **4. GKE Checkpoint & Scale-to-Zero** | After the 3s `SocketSettleGracePeriod`, the reconciler created `PodSnapshotManualTrigger/ws-ws-snapshot-test-trigger`. GKE produced `PodSnapshot/e22faec7-c1ed-4f19-8548-b1f148fdd575` (`AllSnapshotsAvailable`), and the reconciler patched `Workspace/ws-snapshot-test` with `last-checkpoint-name: e22faec7-c1ed-4f19-8548-b1f148fdd575`, `checkpoint-state: Ready`, and `spec.paused: true` (`STATE: Paused`), scaling the Pod down to `0`. |
| **5. Stateful Restore & Cleanup** | Patched `Workspace/ws-snapshot-test` with `{"spec":{"paused":false}}`. `mutate-pod` injected `podsnapshot.gke.io/ps-name: e22faec7-c1ed-4f19-8548-b1f148fdd575` on `ws-ws-snapshot-test-x2wbv-0`. Kubelet logged `Normal GKEPodSnapshotting: Successfully restored the pod from PodSnapshot kubeflow-user/e22faec7-c1ed-4f19-8548-b1f148fdd575`. Querying kernel `50ccf96b-280f-4637-9bad-d2d05ae3146e` returned `RESTORED IN-MEMORY VARIABLES: a=100, b=200, c=20000`, and consumed `PodSnapshot` / `PodSnapshotManualTrigger` CRs were deleted. |

### Hiccups Encountered & How They Were Resolved

1. **Cluster-Scoped vs. Namespace-Scoped GKE `PodSnapshot*` CRDs (`the server could not find the requested resource`)**:
   - **Hiccup**: Initially treated all four `podsnapshot.gke.io/v1alpha1` resources as namespace-scoped and invoked `c.dynamicClient.Resource(gvrStorageConfig).Namespace(ns)`. The Kubernetes API server returned `NotFound (the server could not find the requested resource)` when reconciling `PodSnapshotStorageConfig`.
   - **Root Cause**: Inspection of the GKE CRDs (`kubectl get crd -o custom-columns=NAME:.metadata.name,SCOPE:.spec.scope | grep podsnapshot`) revealed an asymmetric scope model:
     - `podsnapshotstorageconfigs.podsnapshot.gke.io` is **`Cluster`-scoped**.
     - `podsnapshotpolicies.podsnapshot.gke.io`, `podsnapshotmanualtriggers.podsnapshot.gke.io`, and `podsnapshots.podsnapshot.gke.io` are **`Namespaced`**.
   - **Fix**: Updated `ensureStorageConfig` in [`internal/snapshot/snapshot.go`](internal/snapshot/snapshot.go) to reconcile a single cluster-scoped `PodSnapshotStorageConfig` (`kubeflow-pod-snapshot-storage-config`) without `.Namespace(...)`, while reconciling per-workspace `PodSnapshotPolicy` and `PodSnapshotManualTrigger` objects inside the tenant namespace.

2. **Workload Identity GCS Permission Validation Failure on `_perm_check` (`403 AccessDenied` in `PodSnapshotPolicy`)**:
   - **Hiccup**: After creating `PodSnapshotStorageConfig` and `PodSnapshotPolicy`, the policy remained in `Ready=False` (`Reason: TriggerFailed`) with:
     `failed to validate GCS access in storage config "kubeflow-pod-snapshot-storage-config": storage.objects.create validation failed for path gs://kubeflow-user-snapshots-bucket/kubeflow-notebooks/_perm_check with error: googleapi: Error 403: Caller does not have storage.objects.create access to the Google Cloud Storage object., forbidden`.
   - **Root Cause**: The GKE Pod Snapshot controller validates GCS bucket access by impersonating the **tenant namespace's Workload Identity `principalSet`** (`principalSet://iam.googleapis.com/projects/<PROJECT_NUMBER>/locations/global/workloadIdentityPools/<PROJECT_ID>.svc.id.goog/namespace/<TENANT_NAMESPACE>`) and writing a probe object (`_perm_check`) before admitting the policy. Granting IAM only to the node service account or a single Kubernetes ServiceAccount is insufficient.
   - **Fix**: Bound both `roles/storage.objectUser` and `roles/storage.bucketViewer` on `gs://${SNAPSHOT_GCS_BUCKET}` to `principalSet://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${PROJECT}.svc.id.goog/namespace/${TENANT_NAMESPACE}` in [`deploy_standalone.sh`](deploy_standalone.sh).

3. **Dynamic StatefulSet & Pod Naming (`ws-<workspace-name>-<random5>-0` vs. `<workspace-name>-0`)**:
   - **Hiccup**: During the first pause test, `flipPodReadinessGate` and `PodSnapshotManualTrigger` attempted to look up Pod `<workspace-name>-0` (`ws-snapshot-test-0`) and failed with `pods "ws-snapshot-test-0" not found`.
   - **Root Cause**: Upstream `workspaces-controller` creates the backing `StatefulSet` using `metadata.generateName: ws-<workspace-name>-`, which produces a random 5-character hash suffix in the StatefulSet name (`ws-ws-snapshot-test-x2wbv`) and Pod name (`ws-ws-snapshot-test-x2wbv-0`).
   - **Fix**: Added `getWorkspacePod(ctx, namespace, wsName)` in [`internal/snapshot/snapshot.go`](internal/snapshot/snapshot.go) to discover the active Pod dynamically via label selector `notebooks.kubeflow.org/workspace-name=<wsName>` rather than assuming a deterministic Pod name.

4. **Pilot `ValidatingAdmissionPolicy` Blocking `Workspace.spec.podTemplate.podMetadata` vs. Direct `Pod` Webhook Injection**:
   - **Hiccup**: The security hardening `ValidatingAdmissionPolicy` (`notebooks-pilot-workspace`) explicitly forbids setting `.spec.podTemplate.podMetadata` on `Workspace` CRs (`object.spec.podTemplate.?podMetadata.orValue({}) == {}`) so untrusted users cannot inject arbitrary Pod annotations/labels via the `Workspace` spec.
   - **Fix**: By intercepting `Pod` `CREATE` directly in `POST /mutate-pod` (`HandlePodMutate`) after the `Workspace` CR has already passed admission and `workspaces-controller` has rendered the `Pod`, `gke-workspace-snapshot-addon` injects `runtimeClassName: gvisor`, `readinessGates`, `jupyter-ipc-config`, and `podsnapshot.gke.io/ps-name: <snapshot-name>` directly onto the `Pod` without touching `Workspace.spec.podTemplate.podMetadata` or relaxing `ValidatingAdmissionPolicy`.

5. **Dataplane V2 `NetworkPolicy` Blocking Control-Plane Webhook Calls to Port `9443`**:
   - **Hiccup**: Hosting the mutating webhooks inside `gke-access-proxy` on TLS port `:9443` initially risked admission timeouts because `NetworkPolicy/gke-access-proxy` only permitted ingress on TCP `8080` (HTTP proxy) and `8081` (metrics).
   - **Fix**: Exposed TCP port `9443` through `Service/gke-snapshot-webhook-service` ([`manifests/snapshot/webhook.yaml`](manifests/snapshot/webhook.yaml)) and `NetworkPolicy/gke-snapshot-webhook-ingress` ([`internal/deploy/render.go`](internal/deploy/render.go)) and provisioned its TLS certificate via `cert-manager` (`Certificate/gke-snapshot-webhook-cert` with `cert-manager.io/inject-ca-from` on `MutatingWebhookConfiguration/gke-workspace-snapshot-mutating-webhook`).

6. **gVisor `epoll` / Loopback TCP Socket Constraints During Snapshot & Restore**:
   - **Hiccup**: Under gVisor (`runtimeClassName: gvisor`), default Jupyter `pyzmq` TCP loopback sockets (`127.0.0.1`) and `epoll` selectors can hang across checkpoint/restore boundaries, and open external proxy WebSocket connections can interfere with clean checkpointing.
   - **Fix**:
     - `HandlePodMutate` automatically ensures and mounts `ConfigMap/jupyter-ipc-config` (`/etc/jupyter/jupyter_server_config.py`) in every tenant namespace, configuring `asyncio.DefaultEventLoopPolicy` to use `selectors.PollSelector()` and setting `c.KernelManager.transport = 'ipc'` (Unix domain sockets under `/tmp/jupyter_runtime`).
     - `HandleWorkspaceMutate` immediately flips the custom `readinessGate` (`podsnapshot.gke.kubeflow.org/active: False`) and `PodReady: False`, invalidates the proxy cache, and waits `SocketSettleGracePeriod` (`3s`) so `Endpoints`/`EndpointSlices` drop the Pod and open sockets drain before `PodSnapshotManualTrigger` is created.

7. **`PodSnapshot` Finalizer Stuck in `Deleting` (`403 Forbidden` on `storage.objects.list` for GKE Service Agent) & GCS Storage Accumulation**:
   - **Hiccup**: When the snapshot addon deleted the consumed `PodSnapshot` CR (`e22faec7-c1ed-4f19-8548-b1f148fdd575`) after restore, the `PodSnapshot` stayed stuck in `STATUS: Deleting` with `finalizers: [podsnapshot.gke.io/podsnapshot-finalizer]` and message:
     `Failed to delete the snapshot files in GCS: failed to iterate objects in GCS path ...: googleapi: Error 403: service-<PROJECT_NUMBER>@container-engine-robot.iam.gserviceaccount.com does not have storage.objects.list access to the Google Cloud Storage bucket.`
   - **Root Cause**: While the node-level checkpoint and restore agent accesses GCS using the tenant namespace's **Workload Identity `principalSet`**, the control-plane `PodSnapshot` garbage-collection finalizer (`podsnapshot.gke.io/podsnapshot-finalizer`) runs as the **GKE Service Agent** (`service-<PROJECT_NUMBER>@container-engine-robot.iam.gserviceaccount.com`). Without `roles/storage.objectUser` granted to the GKE Service Agent, `PodSnapshot` finalizers cannot list/delete GCS files and old snapshots accumulate in GCS indefinitely.
   - **Fix**:
     1. Granted `roles/storage.objectUser` on `gs://${SNAPSHOT_GCS_BUCKET}` to `serviceAccount:service-${PROJECT_NUMBER}@container-engine-robot.iam.gserviceaccount.com` in [`deploy_standalone.sh`](deploy_standalone.sh) (confirmed on live cluster: stuck `PodSnapshot` CRs immediately cleared and `gs://.../e22faec7-c1ed-4f19-8548-b1f148fdd575/` was deleted from GCS).
     2. Added `ownerReferences` (`Workspace/<name>`) to `PodSnapshot` CRs and `retentionConfig: {lastAccessTimeout: "7d"}` to `PodSnapshotPolicy` in [`internal/snapshot/snapshot.go`](internal/snapshot/snapshot.go), plus a 14-day GCS Object Lifecycle `Delete` rule in [`deploy_standalone.sh`](deploy_standalone.sh).

---

## 2026-09-15 E2E Distributed ML Verification (`distributed_tpu_example.ipynb` on Standalone GKE)

- Cluster: `pilot-gke-dev` / `us-west1` / `kubeflow-notebooks`, GKE `1.35.7-gke.1222000`, Dataplane V2, Gateway API Standard (`gke-l7-global-external-managed`), Workload Identity (`pilot-gke-dev.svc.id.goog`).
- Tenant Namespace: `kubeflow-user`, Pilot User: `pilot-user@example.com`, Artifact Registry: `us-west1-docker.pkg.dev/pilot-gke-dev/kubeflow-repo`, GCS Bucket: `gs://kubeflow-user-bucket`.
- Deployed via `./gke/deploy_standalone.sh` and `./gke/build_jupyterlab.sh` without Istio.
- Workspace pod `ws-test-workspace-md2wx-0` created in `kubeflow-user` with `WorkspaceKind/jupyterlab` (`jupyterlab-cpu`, `small_cpu`) and Bound 10 GiB PVC `workspace-pvc` (`StorageClass/notebooks-gke-rwo`).

Observed end-to-end execution results (`jupyter nbconvert --to notebook --execute /home/jovyan/distributed_tpu_example.ipynb`):

| Stage | Component & Workloads | Result |
| --- | --- | --- |
| **Cell 0 & Storage** | Workload Identity & Tenant RBAC | `can-i create` verified for `trainjobs.trainer.kubeflow.org`, `sparkconnects.sparkoperator.k8s.io`, and `deployments.apps`. `gs://kubeflow-user-bucket` verified accessible via Workload Identity (`roles/storage.objectUser`). |
| **Stage 1: Spark ETL** | `SparkConnect/fashion-mnist-etl` (1 driver + 4 executors) | Driver `fashion-mnist-etl-server` and 4 executors (`exec-1` through `exec-4`) launched in `kubeflow-user` using dual-Python (`3.11`/`3.12`) `spark-py312:latest`. Preprocessed 60,000 train examples across 4 shards (`shard-000.npz` to `shard-003.npz`) + `_SUCCESS` marker in `gs://kubeflow-user-bucket/processed/`. |
| **Stage 2: Multi-Host TPU** | `TrainJob/cc805800034c` (`jax-distributed`, `tpu-v5-8-multi-host`) | GKE Node Auto-Provisioning provisioned 2 `ct5lp-hightpu-4t` nodes (`gke-tpu-bdf9bfbf-4hgw`, `gke-tpu-bdf9bfbf-t4jh`). 2 worker pods (`node-0-0`, `node-0-1`, 4 TPU chips each = 8 global TPU cores) trained 5 epochs in 4.2s (`final_test_accuracy: 0.8289`) and wrote `model/metrics.json` + `model/params.npz` to `gs://kubeflow-user-bucket/model/`. |
| **Stage 3: Serving** | `Deployment/fashion-mnist-inference` & `Service` | Deployed 2-replica inference service in `kubeflow-user`. Sent 5 test images from `ws-test-workspace-md2wx-0` to `http://fashion-mnist-inference.kubeflow-user.svc.cluster.local/predict`; 5/5 predictions matched ground truth (`Ankle boot`, `Pullover`, `Trouser`, `Trouser`, `Shirt`). |

### Root Causes & Fixes for Intermittent `503 Service Unavailable` / `Server Connection Error` in JupyterLab
- **Symptoms**:
  - `File Save Error for distributed_tpu_example.ipynb 503`
  - `Server Connection Error: A connection to the Jupyter server could not be established. JupyterLab will continue trying to reconnect.`
- **Root Causes**:
  1. **`gke-access-proxy` `http.Server` `IdleTimeout: 60s` vs. Google Cloud Load Balancer `600s` Keep-Alive Timeout**:
     - Google Cloud External HTTP(S) Load Balancer (`gke-l7-global-external-managed`) maintains persistent HTTP/1.1 keep-alive connections to backend pods (`gke-access-proxy`) with a fixed **600-second (10-minute)** idle timeout.
     - Because `gke-access-proxy` (`cmd/access-proxy/main.go`) configured `http.Server.IdleTimeout: 60 * time.Second`, Go's HTTP server closed idle keep-alive connections every 60 seconds (`FIN`/`RST`).
     - When JupyterLab sent periodic requests (such as autosave every 120 seconds or background status polling), Google Cloud Load Balancer reused a 60s-old keep-alive connection right as `gke-access-proxy` closed it, received `EOF` / `connection reset by peer` (`backend_connection_closed_before_data_sent_to_client`), and returned **`502` / `503` directly from the load balancer** before `gke-access-proxy` ever read the request.
  2. **Over-Strict Manual IAP JWT Clock Skew & Lifetime Bounds (`identity.go`)**:
     - `IAPAuthenticator.Authenticate()` enforced `claims.IssuedAt <= now+30` and `claims.ExpiresAt - claims.IssuedAt <= 660`. Whenever a Google Front End (GFE) had >30s clock skew or issued/cached an `X-Goog-Iap-Jwt-Assertion` JWT with lifetime >660s, `Authenticate()` rejected the valid cryptographically verified JWT with `401 Unauthorized`, triggering JupyterLab's `Server Connection Error` modal.
  3. **`client-go` Default Client-Side Rate Limiting (`5 QPS` / `10 Burst`) & Cache Stampede**:
     - On every incoming HTTP request to `/workspace/connect/{namespace}/{workspace}/{port}/...`, `KubernetesAccess.Resolve()` executed 4 sequential Kubernetes API calls (`SubjectAccessReview` POST + `Workspace` GET + `WorkspaceKind` GET + `Service` LIST) using `client-go`'s default `QPS = 5.0` and `Burst = 10`.
  4. **Multi-Replica `gke-access-proxy` Load Balancing Without Session Affinity**:
     - Because `gke-access-proxy` runs 2 replicas behind the Google Cloud Global External Load Balancer (`GCPBackendPolicy/notebooks-iap`), without `sessionAffinity: GENERATED_COOKIE`, the load balancer distributed a single browser tab's HTTP requests and WebSocket reconnects across different `gke-access-proxy` pods.
  5. **GCE Enforcer Deleting Untagged `gkegw1-*` Gateway Firewall Rule Every 5 Minutes (`503 failed_to_pick_backend`)**:
     - In Google-internal GCP projects (`google.com` org), **GCE Enforcer** (`gceenforcer-enforcer@system.gserviceaccount.com`) sweeps VPC firewall rules every 5 minutes and 10 seconds. Per GCE Enforcer's exemption logic (`SingleProjectAddRuleHandler.Callback`), `gke-*`/`gkegw1-*` rules are only preserved if they specify non-empty `targetTags` or RFC1918-only `sourceRanges`.
     - The GKE Gateway controller creates `gkegw1-silh-l7-default-global` allowing GCLB health check and GFE proxy source ranges (`35.191.0.0/16` and `130.211.0.0/22`) to TCP `0-65535` **without any `targetTags`**. Consequently, GCE Enforcer deleted `gkegw1-silh-l7-default-global` every 5 minutes (`20:24`, `20:29`, `20:34`, `20:39`, `20:44`, `20:50`).
     - Every time the firewall rule was deleted, Google Cloud Load Balancer health probes to `gke-access-proxy:8080/healthz` timed out, both NEGs transitioned to `UNHEALTHY`, and the load balancer returned **`503 Service Unavailable (failed_to_pick_backend)`** for 1–2 minutes until the GKE Gateway controller reconciled and recreated the firewall rule.
- **Fixes**:
  1. **Match GCLB Backend Keep-Alive Timeout (`IdleTimeout: 650s`)**: Increased `http.Server.IdleTimeout` from `60s` to `650 * time.Second` (`> 600s` GCLB keep-alive timeout) in [`gke/cmd/access-proxy/main.go`](cmd/access-proxy/main.go).
  2. **Verified IAP JWT In-Memory Cache & Relaxed Bounds (`identity.go`)**: Added an in-memory cache of cryptographically verified IAP JWT assertions (`assertion -> {identity, expiresAt}`) and updated bounds to allow 300s clock skew (`now+300`) and up to 24h token lifetime (`86400s`) in [`gke/internal/access/identity.go`](internal/access/identity.go).
  3. **Configurable Kubernetes Client Rate-Limiter (`KUBE_CLIENT_QPS` & `KUBE_CLIENT_BURST`)**: Added `KUBE_CLIENT_QPS` (`kubeClientQPS`, default `100`) and `KUBE_CLIENT_BURST` (`kubeClientBurst`, default `200`) to `Config`, `gke-access-proxy` ConfigMap, CLI flags (`--kube-qps`, `--kube-burst`), and `deploy_standalone.sh`.
  4. **Stale-While-Revalidate + Singleflight Deduplication**: Upgraded `KubernetesAccess.Resolve()` with a 30s fresh TTL, 5-minute stale-while-revalidate window with background async refresh, and singleflight request deduplication (`resolveCall` with `sync.WaitGroup`).
  5. **`GENERATED_COOKIE` Session Affinity (`cookieTtlSec: 86400`) & LB Logging**: Added `sessionAffinity: {type: GENERATED_COOKIE, cookieTtlSec: 86400}` and `logging: {enabled: true, sampleRate: 1000000}` to `GCPBackendPolicy/notebooks-iap` in [`gke/internal/deploy/render.go`](internal/deploy/render.go) and patched the live cluster.
  6. **Upstream HTTP Keep-Alive Connection Pooling**: Configured `MaxIdleConns: 256`, `MaxIdleConnsPerHost: 64`, `IdleConnTimeout: 300 * time.Second`, and `ResponseHeaderTimeout: 120 * time.Second` on `http.Transport` in [`gke/internal/access/proxy.go`](internal/access/proxy.go).
  7. **GKE-Node-Tagged Firewall Rule (`gke-kubeflow-notebooks-db3216d9-gclb-hc`) Exempt from GCE Enforcer**: Created a persistent firewall rule prefixed with `gke-` and explicitly tagged with `--target-tags=gke-kubeflow-notebooks-db3216d9-node` allowing `--source-ranges=35.191.0.0/16,130.211.0.0/22` on `--allow=tcp:8080,tcp:8081` (and added automatic creation to [`gke/deploy_standalone.sh`](deploy_standalone.sh)). GCE Enforcer recognizes `gke-*` rules with `targetTags` as GKE-managed and never deletes them, keeping all `gke-access-proxy` NEGs `HEALTHY` continuously without 5-minute `503 failed_to_pick_backend` drops.

## 2026-09-12 working single-user pilot

- Cluster: `ipv6-project-379110` / `us-central1-c` / `kubeflow-notebooks`, GKE `1.35.7-gke.1222000`, Dataplane V2, VPC-native networking, Workload Identity enabled.
- Edge: Global static IP `8.233.28.206`, hostname `notebooks.8.233.28.206.sslip.io`, Certificate Manager certificate map `notebooks-gke` (`ACTIVE`).
- Custom OAuth client ID `628944397724-a2uj76f0gd8g04asj0dtmq0qeod2kdn6.apps.googleusercontent.com` configured on GCPBackendPolicy.
- Google login as `aojea@google.com` reached the frontend successfully. Tenant discovery returned HTTP 200 with only `team-a`. Backend workspace audit data records that verified email as the creator.
- Browser creation produced Workspace `team-a/gke-pilot` and Bound 10 GiB PVC `gke-pilot-home`.

Observed acceptance results:

| Check | Result |
| --- | --- |
| Public TLS and unauthenticated access | Valid TLS; anonymous HTTPS returns 302 to Google login |
| Notebook HTTP | Public Jupyter status and kernelspec APIs return 200; JupyterLab renders |
| Kernel WebSocket | Executed a notebook cell through JupyterLab; output was `GKE kernel result: 42` |
| File operations | Wrote and read `gke-pilot-validation.txt`; saved executed `gke-pilot-validation.ipynb` and read its output back |
| Terminal WebSocket | Received `GKE_TERMINAL_OK` and UID `1000` over the public terminal WebSocket |
| Pause/resume | UI Stop removed the pod while the PVC stayed Bound; UI Start created a replacement pod on the same PVC/disk; both saved files and notebook output survived |
| Cross-tenant authorization | Authenticated API and notebook requests to `default` returned 403 |
| Forged identity headers | Cross-tenant API request with forged Kubeflow user/group and impersonation headers still returned 403 |
| Pilot RBAC | Required workspace/PVC actions allowed; tested Secret, pod, Service, policy, role, WorkspaceKind creation and cross-tenant actions denied |
| Admission | Allowed notebook dry run passed; Secret mounts, pod metadata, and an unreviewed kind were each rejected by the pilot admission policy |
| Direct ingress bypass | Tokenless probe in `default` reached public HTTPS (302), while direct backend/frontend/proxy Services and notebook Service/pod connections all timed out; probe was deleted afterward |
| Pod hardening | Actual notebook runs as UID 1000, without privilege escalation, with all capabilities dropped and RuntimeDefault seccomp |

## 2026-09-12 Kubernetes tokens and VS Code

Implemented optional `desktopHostname` support entirely under `gke/`. The IAP-protected page `/workspaces/connections` lists the signed-in user's grants, issues a connection URL for an authorized running workspace, and revokes grants. Each grant is a dedicated no-RBAC ServiceAccount in `notebooks-connections`, with TokenRequest audience `https://connect.8.233.28.206.sslip.io/desktop`.

Verification:
- `scripts/test-connection-tokens.sh` confirmed dedicated audience acceptance, wrong-audience rejection, and rejection of ordinary Kubernetes API authentication.
- Public unauthenticated discovery returned 401 over verified TLS. An issued token discovered `python3`, created a kernel (201), and executed `DESKTOP_KERNEL_OK 42` over WebSocket. Revocation returned 204, the active socket closed, and later requests with that token returned 403.
- The operator generated a fresh URL through the page, selected its remote Python kernel in the standard Microsoft Jupyter extension 2025.9.1, and Cell 2 of `vscode_validation.ipynb` executed through VS Code (`ws-gke-pilot-2qdwp-0`, Linux, `/opt/conda/bin/python`, UID 1000 assertions, result 42).

## 2026-09-12 configurable token lifetimes

Replaced the initial fixed one-hour lifetime with administrator-configurable `connectionTokenDefaultSeconds` (default 86400) and `connectionTokenMaxSeconds` (default 604800). The common policy requires `600 <= default <= maximum <= 2592000` seconds.

Live GKE TokenRequest measurements:

| Requested | Granted |
| --- | --- |
| 86400 seconds (1 day) | 86400 seconds, within one-second measurement overhead |
| 604800 seconds (7 days) | 172800 seconds (2 days) |
| 2592000 seconds (30 days) | 172800 seconds (2 days) |

A public token-based kernel test assigned `lifetime_value = 42`, disconnected, and revoked the original grant. A replacement grant accessed the same kernel ID and executed `print(lifetime_value)`, yielding 42. The original token returned 403.

## 2026-09-12 shutdown recovery snapshot

- Repository branch `gke`, HEAD `24ce51e5b19adcef194aa989e9df6bbee3b3c1f9`.
- Latest deployed image and complete plan are the configurable-lifetime versions recorded above: `rendered/desktop-lifetimes-v1`, image digest `sha256:2470150446b3e87644d851fe1b2817db5db1f56a5d1738a13632ddbc8d0c5e32`.
