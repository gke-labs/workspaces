# Integrating Kubeflow Notebooks v2 with Ray (KubeRay) on GKE

This guide walks you through integrating **Kubeflow Notebooks v2** (`Workspace` & `WorkspaceKind`) with **Ray (KubeRay)** on Google Kubernetes Engine (GKE).

It covers both the **Platform Administrator Setup** (one-time cluster configuration) and the **Data Scientist / Notebook User Workflow** (pure Python, no Kubernetes experience required).

---

## What is this and why use it? (TL;DR)

* **Cost-Effective Development**: Your Kubeflow Notebook runs as a lightweight, low-cost pod (e.g. `0.1 CPU`, `128 MiB RAM`) for writing code, exploratory analysis, and plotting.
* **On-Demand Elastic Compute**: When you need heavy computing power (multi-node data processing, hyperparameter sweeps, or distributed model training), you spin up a dedicated `RayCluster` directly from Python.
* **Pure Python SDK Control**: Data scientists do **not** need to write Kubernetes YAML manifests or run `kubectl` commands. You can create, discover, monitor, connect to, and tear down Ray clusters entirely through the **Ray SDK** and the **KubeRay Python Client SDK** (`pip install`).
* **Zero Credential Plumbing**: The notebook's ServiceAccount is automatically granted namespace-scoped permissions (`ray-edit`), so cluster management works securely out-of-the-box.

---

## Directory Contents

| File | Description |
|------|-------------|
| [`ray_example.ipynb`](./ray_example.ipynb) | End-to-end interactive Jupyter Notebook demonstrating cluster creation via KubeRay Python SDK, interactive computing with Ray Client (`:10001`), cluster discovery (`list_ray_clusters`), batch job submission (`:8265`), in-workspace Ray Dashboard viewing, and cleanup. |
| [`manifests/ray-clusterrole.yaml`](./manifests/ray-clusterrole.yaml) | `ClusterRole` definitions (`ray-edit` and `ray-view`) granting permissions to create, inspect, update, and delete `RayCluster`, `RayJob`, and `RayService` resources. |
| [`manifests/raycluster.yaml`](./manifests/raycluster.yaml) | Sample declarative manifest for a shared team cluster (`raycluster-shared`) with 3 batch worker nodes, deployed by administrators or CI/CD pipelines. |

---

## Architecture & Mental Model

```
  ┌─────────────────────────────────────────────────────────────────┐
  │       Kubeflow Notebook v2 Pod (Tiny CPU: 0.1 CPU, 128Mi)       │
  │     Interactive Development, Notebook UI, Pure Python Code      │
  │         ServiceAccount: ws-<workspace-name> (ray-edit)          │
  └───────────────┬─────────────────────────────────┬───────────────┘
                  │ 1. Launch / List / Delete       │ 2. Submit Tasks / Jobs
                  ▼ (KubeRay Python Client SDK)     ▼ (Ray Client :10001 / Jobs API :8265)
  ┌───────────────────────────────┐ ┌───────────────────────────────────────────────┐
  │    Kubernetes API Server      │ │           RayCluster (ray.io/v1)              │
  │     + KubeRay Operator        │ │  ┌─────────────────┐   ┌───────────────────┐  │
  │ (Automated Cluster Manager)   │ │  │  Ray Head Pod   │   │  Ray Worker Pods  │  │
  └───────────────────────────────┘ │  │ (Dashboard, GCS │──▶│  (Distributed     │  │
                                    │  │  Client Server) │   │   Task Execution) │  │
                                    │  └─────────────────┘   └───────────────────┘  │
                                    └───────────────────────────────────────────────┘
```

### Jargon Buster (For Non-K8s Users)
* **Tenant Namespace**: Your team's isolated workspace in the cluster (e.g. `team-a` or `kubeflow-user`). All Ray clusters created by your notebook stay inside your tenant namespace.
* **Head Node**: The coordinator of your Ray cluster. It hosts the web dashboard, manages job scheduling, and coordinates distributed worker tasks.
* **Worker Nodes**: The worker instances running your distributed tasks in parallel across CPU and GPU pools.
* **Ray Client (`ray://...:10001`)**: An interactive connection allowing Python code running in your notebook to transparently execute on the remote Ray workers.
* **Ray Jobs API (`http://...:8265`)**: A background job runner. You submit a Python script, and it continues running on the Ray cluster even if you close the notebook or shut down your laptop.

---

# Part 1: Platform Administrator Setup (One-Time Setup)

Platform administrators or DevOps engineers configure the cluster environment once so notebooks can launch Ray clusters.

### Step 0: Ensure Correct `kubectl` Context & Credentials

Before applying any manifests or patching resources, verify that your local `kubectl` is pointed at the target GKE cluster where Kubeflow Notebooks is deployed:

```bash
export PROJECT_ID="<your-project-id>"
export CLUSTER_NAME="<your-cluster-name>"
export LOCATION="<your-location>"  # GKE cluster zone or region, e.g. us-central1-c or us-west1

# 1. Fetch cluster credentials for kubectl
gcloud container clusters get-credentials "${CLUSTER_NAME}" \
  --location="${LOCATION}" \
  --project="${PROJECT_ID}"

# 2. Verify that the active kubectl context matches your target GKE cluster
kubectl config current-context
kubectl cluster-info
```

> **Tip:** If you manage multiple Kubernetes clusters, ensure your active context matches the format `gke_${PROJECT_ID}_${LOCATION}_${CLUSTER_NAME}` before executing subsequent commands.

### Step 1: Install the KubeRay Operator on GKE

You can enable KubeRay using either GKE's managed add-on or upstream Helm:

#### Option A: GKE Managed Ray Operator Add-on (Recommended for GKE)
```bash
gcloud container clusters update "${CLUSTER_NAME}" \
  --location="${LOCATION}" \
  --project="${PROJECT_ID}" \
  --update-addons=RayOperator=ENABLED
```

#### Option B: Upstream KubeRay Operator via Helm
Follow the [upstream KubeRay Operator Installation Guide](https://docs.ray.io/en/latest/cluster/kubernetes/getting-started/kuberay-operator-installation.html).

Verify operator installation:
```bash
kubectl get crd rayclusters.ray.io
kubectl get pods -A | grep -i kuberay
```

### Step 2: Ensure a Standard (Non-gVisor) Node Pool for Ray

If your GKE cluster uses **GKE Sandbox (gVisor)** (`--sandbox type=gvisor`) for Kubeflow Notebook pods (common when using the Pause & Resume / `PodSnapshot` feature), you must ensure a standard, non-gVisor node pool is available for Ray workloads.

#### Why Ray Cannot Run on gVisor Nodes:
* **Node Taint**: GKE automatically applies the taint `sandbox.gke.io/runtime=gvisor:NoSchedule` to gVisor node pools. Pods without `runtimeClassName: gvisor` cannot schedule on these nodes.
* **Kernel & Memory Requirements**: Ray relies on native Linux kernel capabilities—specifically POSIX shared memory (`/dev/shm` for the Plasma in-memory object store), high-frequency inter-process communication (IPC), and raw socket / epoll performance. Running Ray within a gVisor sandboxed kernel is unsupported and will degrade performance.

#### Create an Autoscaling Non-gVisor Node Pool:
If your cluster does not already have a standard (non-sandboxed) worker node pool, create one:

```bash
gcloud container node-pools create ray-worker-pool \
  --cluster="${CLUSTER_NAME}" \
  --location="${LOCATION}" \
  --project="${PROJECT_ID}" \
  --machine-type=e2-standard-4 \
  --num-nodes=1 \
  --enable-autoscaling \
  --min-nodes=0 \
  --max-nodes=5
```

> **Note on Scheduling:** Because GKE's gVisor nodes are tainted with `sandbox.gke.io/runtime=gvisor:NoSchedule`, standard Ray pods (which do not specify `runtimeClassName: gvisor` and do not tolerate the taint) will automatically avoid the gVisor node pool and schedule onto your standard node pool.

### Step 3: Configure RBAC for Workspace Pods

In Kubeflow Notebooks v2, each `Workspace` pod runs under a dedicated `ServiceAccount` named `ws-<workspace-name>`.

#### 3.1 Apply the `ray-edit` ClusterRole
```bash
kubectl apply -f examples/ray/manifests/ray-clusterrole.yaml
```

> **Note on Aggregation:** `manifests/ray-clusterrole.yaml` includes the label `rbac.authorization.kubeflow.org/aggregate-to-kubeflow-edit: "true"`. If your cluster uses aggregated `kubeflow-edit` ClusterRoles, `ray-edit` permissions are automatically aggregated into `kubeflow-edit`.

#### 3.2 Bind `ray-edit` to your `WorkspaceKind`
Patch your existing `jupyterlab` `WorkspaceKind` so all notebooks automatically receive `ray-edit` permissions via dynamic `RoleBinding`s:

```bash
kubectl patch workspacekind jupyterlab --type='merge' -p='{
  "spec": {
    "podTemplate": {
      "serviceAccount": {
        "clusterRoles": [
          {"name": "kubeflow-edit"},
          {"name": "ray-edit"}
        ]
      }
    }
  }
}'
```

#### 3.3 Verify Workspace ServiceAccount Permissions
```bash
export TENANT_NAMESPACE="team-a"  # or your user tenant namespace
export WORKSPACE_NAME="my-notebook"

kubectl auth can-i create rayclusters.ray.io \
  --as="system:serviceaccount:${TENANT_NAMESPACE}:ws-${WORKSPACE_NAME}" \
  -n "${TENANT_NAMESPACE}"
# Expected output: yes
```

### Step 4: Optional Pre-provisioned Shared Cluster (`manifests/raycluster.yaml`)

Platform administrators can deploy a shared, persistent Ray cluster (e.g. for batch training or team pipelines) alongside on-demand user clusters:

```bash
kubectl apply -f examples/ray/manifests/raycluster.yaml -n "${TENANT_NAMESPACE}"
```
This creates `raycluster-shared` with 3 dedicated batch workers (`2 CPU, 4Gi RAM` each, sized to fit comfortably on default `e2-standard-4` nodes alongside system pods), while individual data scientists can dynamically spin up their own smaller interactive clusters (`raycluster-sample` with 1 worker at `500m CPU, 1Gi RAM`).

### Architecture Note: Why Istio Sidecar Injection is Disabled (`sidecar.istio.io/inject: "false"`)

In Kubeflow tenant namespaces (`istio-injection=enabled`), Istio injects an `istio-proxy` sidecar and enforces namespace `AuthorizationPolicy` requiring mTLS peer identity.
- Ray Head and Worker pods communicate via direct Pod-IP-to-Pod-IP gRPC connections on dynamic ephemeral ports (e.g. `ray.rpc.NodeManagerService/GetResourceLoad`).
- Because ephemeral ports are not registered in a Kubernetes `Service`, Envoy routes them through `PassthroughCluster` without mTLS client certificates. Inbound gRPC health checks are denied (`RBAC: access denied`), causing worker nodes to fail health checks.
- Setting `sidecar.istio.io/inject: "false"` on Ray pods excludes them from Istio sidecars while allowing the Kubeflow Notebook pod (which has `istio-proxy` with auto-mTLS) to seamlessly connect to `ray://<head-svc>:10001` and `http://<head-svc>:8265`.

---

# Part 2: Data Scientist / Notebook User Workflow (Pure Python)

You can run this entire workflow directly inside [`ray_example.ipynb`](./ray_example.ipynb) without running `kubectl` or writing YAML files.

### 1. Install SDKs via `pip`

Inside your notebook environment, install the Ray Python SDK and the official KubeRay Python Client:

```bash
# Core Ray SDK (client & job submission)
pip install "ray[default,client]==2.41.0"

# KubeRay Python Client SDK (cluster lifecycle management)
pip install "git+https://github.com/ray-project/kuberay.git#subdirectory=clients/python-client"

# Or if working from a local repository checkout:
# pip install /path/to/kuberay/clients/python-client
```

> [!TIP]
> **Kernel Restart:** When installing `ray` for the first time in an active Jupyter notebook session, restart the kernel (**Kernel ➔ Restart Kernel...**) so Python loads the compiled C extensions (`_raylet.so`) cleanly into the runtime.

### 2. Provision an On-Demand `RayCluster` Programmatically

Use `kuberay_cluster_builder.ClusterBuilder` and `kuberay_cluster_api.RayClusterApi`:

```python
import sys
from python_client import kuberay_cluster_api
from python_client.utils import kuberay_cluster_builder

py_tag = f"py{sys.version_info.major}{sys.version_info.minor}"
IMAGE = f"rayproject/ray:2.41.0-{py_tag}"
TENANT_NAMESPACE = "team-a"  # Auto-detected in notebook
CLUSTER_NAME = "raycluster-sample"

# Build the Ray cluster definition
builder = kuberay_cluster_builder.ClusterBuilder()
cluster = (
    builder.build_meta(
        name=CLUSTER_NAME,
        k8s_namespace=TENANT_NAMESPACE,
        labels={"app.kubernetes.io/name": CLUSTER_NAME},
        ray_version="2.41.0",
    )
    .build_head(
        ray_image=IMAGE,
        service_type="ClusterIP",
        cpu_requests="250m",
        memory_requests="1Gi",
        cpu_limits="500m",
        memory_limits="2Gi",
        ray_start_params={"dashboard-host": "0.0.0.0", "num-cpus": "0"},
    )
    .build_worker(
        group_name="workers",
        ray_image=IMAGE,
        cpu_requests="500m",
        memory_requests="1Gi",
        cpu_limits="1",
        memory_limits="2Gi",
        replicas=1,
        min_replicas=1,
        max_replicas=2,
    )
    .get_cluster()
)

# Disable Istio sidecars on Ray pods for reliable internal communication
cluster["spec"]["headGroupSpec"]["template"].setdefault("metadata", {})["labels"] = {
    "ray.io/node-type": "head",
    "sidecar.istio.io/inject": "false",
}
for wg in cluster["spec"]["workerGroupSpecs"]:
    wg["template"].setdefault("metadata", {})["labels"] = {
        "ray.io/node-type": "worker",
        "sidecar.istio.io/inject": "false",
    }

# Submit and wait for readiness
cluster_api = kuberay_cluster_api.RayClusterApi()
cluster_api.create_ray_cluster(body=cluster, k8s_namespace=TENANT_NAMESPACE)
cluster_api.wait_until_ray_cluster_running(CLUSTER_NAME, k8s_namespace=TENANT_NAMESPACE, timeout=300)

HEAD_SVC = f"{CLUSTER_NAME}-head-svc.{TENANT_NAMESPACE}.svc.cluster.local"
RAY_CLIENT_URI = f"ray://{HEAD_SVC}:10001"
```

### 3. Interactive Computing with Ray Client (`ray.init`)

Connect your interactive session to the cluster. Any function or class with `@ray.remote` runs on the worker pods:

```python
import ray

ray.init(address=RAY_CLIENT_URI)

@ray.remote
def square(x: int) -> int:
    return x * x

# Run 10 tasks in parallel across worker nodes
futures = [square.remote(i) for i in range(10)]
print(ray.get(futures))  # [0, 1, 4, 9, 16, 25, 36, 49, 64, 81]

ray.shutdown()
```

### 4. Discover Clusters & Submit Batch Jobs (`JobSubmissionClient`)

Data scientists often have access to multiple clusters (e.g. their own interactive `raycluster-sample` and a team-wide `raycluster-shared`).

Using `list_ray_clusters()`, you can discover all active clusters in your tenant namespace and choose where to submit your batch job:

```python
from python_client import kuberay_cluster_api
from ray.job_submission import JobSubmissionClient

cluster_api = kuberay_cluster_api.RayClusterApi()
cluster_list = cluster_api.list_ray_clusters(k8s_namespace=TENANT_NAMESPACE)

print("Found Ray Clusters in tenant namespace:")
for item in cluster_list.get("items", []):
    name = item["metadata"]["name"]
    state = item.get("status", {}).get("state", "pending")
    workers = item.get("status", {}).get("availableWorkerReplicas", 0)
    print(f"  • {name:<20} | State: {state:<8} | Workers: {workers}")

# Select the desired cluster (e.g. 'raycluster-shared' or 'raycluster-sample')
selected_cluster = "raycluster-shared" if any(c["metadata"]["name"] == "raycluster-shared" for c in cluster_list.get("items", [])) else CLUSTER_NAME
selected_dashboard_uri = f"http://{selected_cluster}-head-svc.{TENANT_NAMESPACE}.svc.cluster.local:8265"

# Submit the asynchronous batch job
job_client = JobSubmissionClient(selected_dashboard_uri)
job_id = job_client.submit_job(
    entrypoint="python train_job.py",
    runtime_env={"working_dir": "./ray_job_src"},
)
print(f"Job submitted to {selected_cluster} (Job ID: {job_id})")
print(job_client.get_job_logs(job_id))
```

### 5. Accessing the Ray Dashboard Through Your Workspace (In-Browser)

Data scientists work solely through a web browser in JupyterLab and do not have `kubectl` or VPN access to the GKE network on their local machines.

Because your **Kubeflow Workspace Pod is already inside the cluster network**, the Ray Head service (`http://<cluster-name>-head-svc:8265`) is directly reachable from within your workspace container. To expose the dashboard to your browser through the workspace ingress (port 8888), **`jupyter-server-proxy`** intercepts requests under `.../jupyterlab/proxy/<port>/` and forwards them to `127.0.0.1:<port>`.

There are two ways to bridge the Ray Head port to localhost inside your workspace:

#### Option A: Pure Python Socket Forwarder (Inside the Notebook)
You can start a lightweight socket forwarder directly from Python in your notebook—no terminal commands needed:

```python
import socket, threading

_active_forwarders = {}

def stop_dashboard_port(local_port: int = 8265):
    """Stop an existing in-workspace port-forward."""
    if local_port in _active_forwarders:
        try:
            _active_forwarders[local_port].close()
        except Exception:
            pass
        del _active_forwarders[local_port]
        print(f"🛑 Closed port-forward on 127.0.0.1:{local_port}")

def forward_dashboard_port(cluster_name: str, local_port: int = 8265):
    if local_port in _active_forwarders:
        stop_dashboard_port(local_port)

    remote_host = f"{cluster_name}-head-svc.{TENANT_NAMESPACE}.svc.cluster.local"
    server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    server.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    try:
        server.bind(("127.0.0.1", local_port))
    except OSError as e:
        print(f"ℹ️ Port {local_port} already in use ({e}). Dashboard is accessible at /proxy/{local_port}/")
        return
    server.listen(5)
    _active_forwarders[local_port] = server

    def handle_client(client_sock):
        remote_sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        try:
            remote_sock.connect((remote_host, 8265))
        except Exception:
            client_sock.close()
            return
        def bridge(s1, s2):
            try:
                while True:
                    data = s1.recv(4096)
                    if not data: break
                    s2.sendall(data)
            except Exception:
                pass
            finally:
                s1.close()
                s2.close()
        threading.Thread(target=bridge, args=(client_sock, remote_sock), daemon=True).start()
        threading.Thread(target=bridge, args=(remote_sock, client_sock), daemon=True).start()

    def loop():
        while local_port in _active_forwarders:
            try:
                c, _ = server.accept()
                threading.Thread(target=handle_client, args=(c,), daemon=True).start()
            except OSError:
                break

    threading.Thread(target=loop, daemon=True).start()
    print(f"✅ Ray Dashboard forwarded on 127.0.0.1:{local_port}")

# Start the forwarder in background
forward_dashboard_port("raycluster-sample", local_port=8265)
```

Now you can view the Ray Dashboard in two ways:
1. **In Browser Tab**: Navigate to `https://<kubeflow-endpoint>/workspace/connect/<tenant-namespace>/<workspace-name>/jupyterlab/proxy/8265/`.
   *(Note: The `/jupyterlab/` path segment is required because `gke-access-proxy` routes to the notebook server, which uses `jupyter-server-proxy` to proxy port 8265.)*
2. **Inside Notebook Cell**:
   ```python
   import os
   from IPython.display import IFrame
   nb_prefix = os.environ.get("NB_PREFIX", "/").rstrip("/") + "/"
   IFrame(src=f"{nb_prefix}proxy/8265/", width="100%", height=600)
   ```

#### Option B: Forwarding via Workspace Terminal
If your workspace container image includes `kubectl`, you can alternatively run the port-forward in a **Terminal tab inside JupyterLab**:
```bash
kubectl port-forward svc/raycluster-sample-head-svc 8265:8265
```
Then open `https://<kubeflow-endpoint>/workspace/connect/<tenant-namespace>/<workspace-name>/jupyterlab/proxy/8265/` in your browser.

### 6. Clean Up & Tear Down Compute

#### 6.1 User Cleanup (Inside the Notebook)
When you finish your computation, stop the forwarder and delete the on-demand Ray cluster to stop compute billing:

```python
# 1. Stop the dashboard port-forward
stop_dashboard_port(8265)

# 2. Delete the on-demand Ray cluster
cluster_api.delete_ray_cluster(name=CLUSTER_NAME, k8s_namespace=TENANT_NAMESPACE)
print("RayCluster deleted successfully.")
```
*(Your Kubeflow Notebook pod remains running, preserving your code, notebooks, and files.)*

#### 6.2 Administrator Cleanup (Shared Cluster)
If you deployed the optional shared team cluster (`manifests/raycluster.yaml`) in Step 4, tear it down when testing is complete:

```bash
kubectl delete -f examples/ray/manifests/raycluster.yaml -n "${TENANT_NAMESPACE}"
```

---

## Troubleshooting

| Symptom | Cause | Fix |
| :--- | :--- | :--- |
| `access denied` (HTTP 403 Forbidden) when opening `.../workspace/connect/<tenant-namespace>/<workspace-name>/proxy/8265` | URL is missing the `/jupyterlab/` port segment. `gke-access-proxy` interprets `proxy` as a workspace port ID, which is not declared on `WorkspaceKind` (`jupyterlab-resumable` only declares `jupyterlab`). | Use `https://<endpoint>/workspace/connect/<tenant-namespace>/<workspace-name>/jupyterlab/proxy/8265/` (ensure both `/jupyterlab/` and the trailing `/` are present). In Python, construct it via `f"{os.environ.get('NB_PREFIX', '/')}proxy/8265/"`. |
| `404 Not Found` when opening `.../jupyterlab/proxy/8265/` | `jupyter-server-proxy` is not installed or enabled in the workspace JupyterLab environment. | Install and enable `jupyter-server-proxy` in the workspace. See [Resolving 404 Not Found / Why jupyter-server-proxy is Required](#resolving-404-not-found-on-ray-dashboard-why-jupyter-server-proxy-is-required) below. |
| `bind: address already in use` on port 8265 when running `kubectl port-forward` | The Jupyter Python kernel is already running the socket forwarder on `127.0.0.1:8265`, or a previous background `kubectl` process is running. | See [Resolving Port Conflicts](#resolving-port-conflicts-bind-address-already-in-use) below. |
| `ImportError`, symbol conflict, or crash when importing `ray` | `ray` was installed in an active Python kernel that had already loaded conflicting modules or uninitialized C extensions (`_raylet.so`). | Restart the notebook kernel (**Kernel ➔ Restart Kernel...**) to reload the Python environment. |
| Ray worker pods stuck in `Pending` (`0/N nodes are available: Insufficient cpu`) | Ray workers request more CPU (e.g., 4 CPU) than is allocatable on standard nodes (e.g. `e2-standard-4` has ~3.7 CPU allocatable after GKE system reservations). | Reduce worker CPU request to `2` in `manifests/raycluster.yaml` or the Python script, or provision a larger node pool (`e2-standard-8`). |
| Ray object store crashes or fails to initialize `/dev/shm` | Ray pods scheduled onto a gVisor-sandboxed node pool (`--sandbox type=gvisor`), which restricts shared memory IPC required by Ray's Plasma store. | Provision a standard non-gVisor node pool per [Part 1, Step 2](#step-2-ensure-a-standard-non-gvisor-node-pool-for-ray). |

### Resolving `404 Not Found` on Ray Dashboard / Why `jupyter-server-proxy` is Required

If you navigate to `https://<endpoint>/workspace/connect/<tenant-namespace>/<workspace-name>/jupyterlab/proxy/8265/` and receive `404 Not Found`:

#### Why this happens
1. **The Ingress Boundary**: The GKE Workspaces Gateway (`gke-access-proxy`) only routes external browser traffic into the workspace pod through port `8888` (the JupyterLab server).
2. **Localhost Isolation**: When you run `kubectl port-forward` (or the Python socket forwarder) inside the workspace pod, port `8265` is bound only to `127.0.0.1:8265` inside that isolated container. The outside world cannot connect to port 8265 directly.
3. **The Role of `jupyter-server-proxy`**: The only process open to the external gateway is JupyterLab on port 8888.
   * **Without `jupyter-server-proxy`**: JupyterLab only knows how to serve notebooks, kernels, and static files. It has no handler for `/proxy/<port>/` and immediately returns **`404 Not Found`**.
   * **With `jupyter-server-proxy`**: JupyterLab registers an extension handler that intercepts `/proxy/<port>/` and proxies HTTP and WebSocket traffic to `127.0.0.1:<port>`.

#### Fix
Install and enable `jupyter-server-proxy` in the workspace pod (via workspace terminal tab or by rebuilding the workspace image):
```bash
pip install jupyter-server-proxy
echo '{"ServerApp": {"jpserver_extensions": {"jupyter_server_proxy": true}}}' > /opt/conda/etc/jupyter/jupyter_server_config.d/jupyter_server_proxy.json
/package/admin/s6/command/s6-svc -r /run/service/jupyterlab
```
*(In images built from `images/jupyterlab/requirements.txt`, `jupyter-server-proxy` is installed by default).*

### Resolving Port Conflicts (`bind: address already in use`)

If you run `kubectl port-forward svc/<cluster-name>-head-svc 8265:8265` inside the notebook terminal and receive:
```
Unable to listen on port 8265: Listeners failed to create with the following errors:
[unable to create listener: Error listen tcp4 127.0.0.1:8265: bind: address already in use ...]
```

#### Why this happens
If you previously executed `forward_dashboard_port(...)` in a notebook cell, the Jupyter Python kernel process itself is already bound to `127.0.0.1:8265` and proxying to the Ray Dashboard. Your forwarder is already active and working!

#### Solutions

1. **Quick Workaround (Use Another Port)**:
   You don't need to terminate anything. Simply forward to another local port such as `8266:8265`:
   ```bash
   kubectl port-forward svc/raycluster-shared-head-svc 8266:8265
   ```
   Then navigate to `.../jupyterlab/proxy/8266/` in your browser.

2. **From the Notebook (Python / UI)**:
   * Run in a code cell:
     ```python
     stop_dashboard_port(8265)
     ```
   * Or in the JupyterLab top menu, click **Kernel** ➔ **Restart Kernel...**. This instantly closes all sockets held by the Python kernel and releases port 8265.

3. **From Workspace Terminal (Without `fuser` or `lsof`)**:
   Standard workspace container images do not have `fuser`, `lsof`, or `netstat` installed.
   * If a previous `kubectl port-forward` command is running in the background:
     ```bash
     pkill -f "kubectl port-forward"
     ```
   * To terminate whatever process is bound to port 8265 using pure Python (standard library):
     ```bash
     python3 -c '
     import os, glob, signal
     hex_port = f"{8265:04X}"
     inodes = {l.split()[9] for p in ["/proc/net/tcp", "/proc/net/tcp6"] if os.path.exists(p) for l in open(p) if len(l.split()) > 9 and l.split()[1].endswith(":" + hex_port)}
     for pid in [os.path.basename(d) for d in glob.glob("/proc/[0-9]*")]:
         try:
             if any(f"[{i}]" in os.readlink(f"/proc/{pid}/fd/{f}") for i in inodes for f in os.listdir(f"/proc/{pid}/fd")):
                 print(f"Terminating PID {pid}")
                 os.kill(int(pid), signal.SIGKILL)
         except Exception: pass
     '
     ```

4. **From Workspace Terminal (If `fuser` or `lsof` are installed)**:
   ```bash
   fuser -k 8265/tcp || lsof -ti :8265 | xargs kill -9
   ```

### Resolving `access denied` on Ray Dashboard URL

If you navigate to `https://<endpoint>/workspace/connect/<tenant-namespace>/<workspace-name>/proxy/8265` and see:
```
access denied
```

#### Why this happens
`gke-access-proxy` parses URLs with the format:
```
https://<domain>/workspace/connect/<tenant-namespace>/<workspace-name>/<port-id>/<subpath>
```
If you omit `jupyterlab/`, `gke-access-proxy` interprets `proxy` as `<port-id>`. It validates `<port-id>` against the ports declared in `WorkspaceKind` (`jupyterlab-resumable`), which only defines `id: jupyterlab` (port 8888). Because `proxy` is not a declared application port, `gke-access-proxy` rejects the request with HTTP 403 `access denied`.

#### Fix
Always include the `/jupyterlab/` port prefix and trailing slash `/`:
```
https://<endpoint>/workspace/connect/<tenant-namespace>/<workspace-name>/jupyterlab/proxy/8265/
```
In Python or within your notebooks, use the `$NB_PREFIX` environment variable (which expands to `/workspace/connect/<tenant-namespace>/<workspace-name>/jupyterlab/`):
```python
import os
dashboard_url = f"{os.environ.get('NB_PREFIX', '/').rstrip('/')}/proxy/8265/"
print(f"Ray Dashboard URL: {dashboard_url}")
```

