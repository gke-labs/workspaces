# Standalone Kubeflow Notebooks on GKE (No Istio)

This directory contains the standalone deployment and access integration for **Kubeflow Workspaces (Notebooks v2)**, **Kubeflow Trainer (v2)**, and **Kubeflow Spark Operator** on Google Kubernetes Engine (GKE) **without depending on Istio**.

## Documentation & Quickstart Guide

| Document | Purpose |
| --- | --- |
| **[USER_GUIDE.md](USER_GUIDE.md)** | **Step-by-Step Deployment Guide**: Complete manual & automated instructions with configuration options (custom domain vs. automatic `sslip.io` zero-DNS setup, Google-managed OAuth vs. custom OAuth), Kubeflow Trainer + Spark Operator installation, and running [`examples/distributed_tpu_example.ipynb`](examples/distributed_tpu_example.ipynb). |
| **[CODELAB.md](CODELAB.md)** | **Automated Quickstart & Pilot Record**: 5-step quickstart using the automation scripts (`deploy_standalone.sh`, `build_jupyterlab.sh`, `cleanup_standalone.sh`) plus the historical maintainer acceptance test record. |
| **[DESIGN.md](DESIGN.md)** | **Architecture & Design**: Explains why this Istio-free approach was chosen, how the access proxy and signed IAP assertions work, and how browser and VS Code desktop endpoints are isolated. |

### Streamlined Automation Scripts
- **[`deploy_standalone.sh`](deploy_standalone.sh)**: End-to-end deployment script that enables GKE APIs and standard Gateway API, installs `cert-manager`, builds/pushes core images, configures Google Certificate Manager (with automatic `sslip.io` fallback when no domain is provided), discovers the IAP backend audience, deploys Kubeflow Trainer (v2) and Kubeflow Spark Operator, configures tenant RBAC and baseline Pod Security, and sets up GCS Workload Identity IAM bindings.
- **[`build_jupyterlab.sh`](build_jupyterlab.sh)**: Builds and pushes custom JupyterLab (CPU, GPU, TPU) and Spark 4.0.1 images (pre-bundled with `examples/distributed_tpu_example.ipynb`, `jax[tpu]`, `kubeflow[spark]`, and `google-cloud-storage`) and registers the `jupyterlab` `WorkspaceKind` and GPU/TPU `ComputeClasses`.
- **[`cleanup_standalone.sh`](cleanup_standalone.sh)**: Tears down deployed tenant workloads, controllers, and optional edge resources.

Browser JupyterLab works out-of-the-box with Google IAP authentication. An optional separate desktop endpoint supports Kubernetes-minted connection tokens for the standard VS Code Jupyter extension; see [VS Code Jupyter Extension](USER_GUIDE.md#7-vs-code-jupyter-extension-desktop-endpoint).

## Integration boundary

This directory owns the GKE integration. Keep upstream controller, backend,
frontend, APIs, and manifests unchanged. Deployment overlays, build configuration,
automation, integration code, and tests belong here. Do not depend on the previous
`gateway_api` branch's routing provider or authorization endpoint.

The intended product is installed and operated by a team in its own Google Cloud
project. Prefer documented GKE capabilities and existing upstream extension points.
Google Cloud support for a platform component does not imply support for this
integration or its authorization logic.

Pin upstream revisions and image digests. Reuse upstream manifests through overlays
instead of copying them. Keeping changes here reduces textual rebase conflicts;
compatibility tests must still detect upstream API and behavior changes.

## What this integration adds

This is deployment and access integration around upstream Notebooks, not a fork of
its application behavior. Track every addition below and update its status as code
lands. The architecture describes the integration; only the dated acceptance
record establishes which behaviors have been verified live.

| Addition | Owner and purpose | Current status |
| --- | --- | --- |
| GKE Gateway, HTTPS, and IAP configuration | Google-managed ingress and login; this integration owns configuration | Pilot installed; Gateway Programmed, TLS ACTIVE, anonymous denial/login redirect and authenticated custom-OAuth login verified |
| Signed IAP assertion validation | Access proxy verifies signatures, issuer, exact audience, lifetime, and identity; no unsigned-header fallback | Implemented with unit tests in [identity.go](internal/access/identity.go) and [identity_test.go](internal/access/identity_test.go) |
| Identity-header translation | Access proxy strips caller identity/group headers and forwards the verified Google-account email as the upstream user | Implemented and locally tested in [proxy.go](internal/access/proxy.go) |
| Workspace connection routing and authorization | Access proxy resolves upstream-owned Services and checks workspace access with Kubernetes SubjectAccessReview before proxying HTTP/WebSockets | Unit tests plus live Jupyter HTTP, kernel/terminal WebSockets, cross-tenant denial, and pause/resume verified |
| Filtered tenant discovery | Temporary access-proxy adapter for the namespace-list endpoint; checks existing Kubernetes RBAC, with no separate membership database | Implemented in [kubernetes.go](internal/access/kubernetes.go); removal plan below |
| Ingress isolation | Deployment policies prevent direct access around the proxy and preserve required controller probes | API-server webhook connectivity verified; unrelated-pod direct access blocked in live checks; controller activity/culling still unverified |
| Single-user pilot policy and storage | Reviewed user RBAC, admission restrictions, tokenless notebook identity, quota, restricted Pod Security, and opted-in GKE CSI storage | Installed through [pilot overlay](manifests/pilot/kustomization.yaml); positive and negative admission/RBAC tests passed; not an enrollment or multi-tenant provisioning system |
| Packaging and lifecycle automation | Overlays and build configuration reuse unchanged upstream; automation owns installation, upgrades, validation, and safe resource cleanup | Four pinned core images and pinned notebook running; browser creation and pause/resume tested; deletion/recreation, upgrades, and cleanup pending |
| Admission-webhook certificates | cert-manager issues and rotates the upstream controller certificate and injects the webhook CA bundle | Upstream cert-manager component reused; a healthy cert-manager installation is a prerequisite |

Upstream retains Workspace/WorkspaceKind APIs and reconciliation, notebook lifecycle,
storage handling, the frontend, and authorization of normal backend API requests.
Kubernetes RBAC remains the authorization source of truth. IAP admission does not
grant workspace access, and this integration does not automatically translate
Google Cloud IAM permissions into Kubernetes permissions.

### Temporary namespace-filtering adapter

The selected behavior is to show only explicitly configured tenant namespaces where
the authenticated user is authorized to `list` `kubeflow.org/workspaces`. The proxy
will answer the namespace-list request using the unchanged upstream response schema.
It must not grant users cluster-wide `list namespaces`, query the backend under an
administrator identity, or use UI filtering as authorization. Normal backend
requests remain subject to upstream authorization independently of this list.

This is an explicit, temporary application-API compatibility surface, not a GKE
platform capability. Keep it separate from IAP validation and workspace routing so
it can be removed without changing either. Other backend APIs must pass through
without response rewriting. Denied checks omit the namespace; authorization-service
failures must fail closed rather than expose an unfiltered list.

Upstream may add namespace filtering. No upstream issue, release, or implementation
is verified here yet; do not rely on a delivery date or automatically enable a new
behavior based only on a version number. When upgrading the pinned upstream revision:

1. Inspect upstream's filtering and authorization semantics, including the operation
  used to determine membership and whether it restricts the configured tenant set.
2. Run compatibility tests for users with zero, one, and multiple tenants, denied
  access, RBAC revocation, authorization failures, and the frontend response schema.
  Confirm that users no longer need cluster-wide namespace-list permission.
3. If upstream satisfies the agreed visibility policy, route namespace discovery to
  the backend using the normal verified user identity. Remove the adapter, its
  adapter-only permissions/configuration, and implementation-specific tests; retain
  end-to-end tenant-visibility tests as an upgrade gate.
4. If semantics differ, document the difference and obtain a policy decision before
  changing visibility. Do not silently fall back to listing all namespaces.

Until then, the temporary endpoint, its tests, and this compatibility record belong
entirely under `gke/`; no upstream source changes are required.

## Local implementation

Run from this directory with Go 1.25 or later (validated using Go 1.26.6):

```sh
make test
make build
make render
kubectl kustomize manifests/tenant
```

The tests use signed local JWTs, a local JWKS endpoint, fake Kubernetes clients,
HTTP servers, and local `kubectl kustomize` rendering. They require `kubectl` on
PATH but do not require or change a cluster. `make render` only
prints manifests. The image can be built with `docker build -t gke-access-proxy .`.
The pilot proxy image is deployed and healthy on GKE; authenticated notebook
HTTP and WebSocket checks passed. Pin its base images by digest as part of release
packaging.

The executable is `bin/access-proxy`; its command-line flags also accept these
environment variables:

| Variable | Required value |
| --- | --- |
| `PUBLIC_URL` | Exact public HTTPS origin, for example `https://notebooks.example.com`; no path |
| `IAP_AUDIENCE` | `/projects/PROJECT_NUMBER/global/backendServices/SERVICE_ID`; both IDs numeric |
| `FRONTEND_URL` | Internal frontend HTTP(S) origin without a path |
| `BACKEND_URL` | Internal backend HTTP(S) origin without a path |
| `TENANT_NAMESPACES` | Explicit comma-separated namespace allowlist; no duplicates |
| `DESKTOP_URL` | Optional separate HTTPS origin for connection-token access |
| `CONNECTION_TOKEN_DEFAULT_SECONDS` | Default requested lifetime; defaults to 86400 (24 hours) |
| `CONNECTION_TOKEN_MAX_SECONDS` | Maximum requested lifetime; defaults to 604800 (7 days), configurable up to 2592000 (30 days) |

Connection lifetimes are bounded by both administrator policy and Kubernetes'
returned expiration. The tested GKE cluster shortened requests above two days to
48 hours. Existing grants are not extended by configuration changes. See
[lifetime configuration and kernel reconnection](USER_GUIDE.md#configure-connection-lifetimes)
before planning days- or weeks-long sessions.

Use in-cluster credentials by default, or `--kubeconfig` for local integration
testing. Startup rejects a missing audience or invalid upstream/tenant configuration.
The public URL's host must match incoming requests. `/healthz` is an unauthenticated
GET-only health endpoint; it does not expose application data or prove Kubernetes
and IAP readiness.

### HTTP contract

- `/` redirects authenticated users to `/workspaces/`.
- `/workspaces/api/v1/namespaces` is the temporary GET-only adapter. It returns
  `{"data":[{"name":"team-a"}]}` using per-request `list workspaces` checks,
  with no positive authorization cache and no user namespace-list grant.
- Other `/workspaces/api/` requests go to the upstream backend after removing
  `/workspaces`. The backend must use `kubeflow-userid` and `kubeflow-groups` as
  its identity header configuration. Only the verified email is injected; no
  group membership is asserted.
- `/workspace/connect/<namespace>/<workspace>/<port-id>/...` requires `get` on
  that Workspace. The proxy reads its current image option and WorkspaceKind,
  then resolves a ClusterIP Service controlled by that Workspace's current UID.
  There is no caller-selected destination host or numeric port. Declared
  `removePathPrefix` is supported; custom WorkspaceKind request-header operations
  are rejected rather than silently ignored.
- Remaining paths go to the frontend. [frontend.Dockerfile](frontend.Dockerfile)
  builds unchanged frontend source in `standalone` mode with public path
  `/workspaces/`. [frontend-nginx.conf](frontend-nginx.conf) serves assets under
  that prefix. Browser login, workspace creation, and pause/resume passed;
  remaining frontend compatibility issues are recorded in the codelab.

Caller-supplied Kubeflow, IAP, auth-request, impersonation, authorization, and
forwarded identity headers are removed before forwarding. IAP session cookies
are stripped; notebook cookies such as `_xsrf` are retained. Encoded separators,
double encoding containing percent signs, and dot-segment paths are rejected.
This conservative path policy needs real notebook file-operation coverage.

Mutating requests and WebSocket handshakes require `Origin` to equal `PUBLIC_URL`.
Non-browser clients must supply the same origin as well as a valid IAP assertion;
this initial version is browser-focused. An origin check is not authorization.

### Packaging and deployment gates

[Proxy manifests](manifests/proxy/kustomization.yaml) assume the
`kubeflow-workspaces` namespace exists. They deliberately reference an unconfigured
image and a required `gke-access-proxy` ConfigMap containing the environment values
above. No public Gateway or IAP policy is installed by these manifests.

[Snapshot addon manifests](manifests/snapshot/kustomization.yaml) package the
stateful pause/resume control plane (`gke-workspace-snapshot-addon`) as a **separate
deployment** from the proxy: the executable is `bin/snapshot-addon`, built from
[snapshot.Dockerfile](snapshot.Dockerfile). It serves the two mutating admission
webhooks on `9443` and runs a watch-driven, leader-elected reconciler; it holds the
write permissions on `workspaces`, `pods/status` and `podsnapshot.gke.io`
resources, none of which the proxy needs. Both webhooks use `failurePolicy: Ignore`,
so an addon outage degrades pause/resume to stateless behaviour instead of blocking
Workspace operations. Its ConfigMap accepts `TENANT_NAMESPACES`,
`SNAPSHOT_GCS_BUCKET`, `SNAPSHOT_WORKERS`, `SNAPSHOT_RESYNC`,
`SNAPSHOT_SETTLE_GRACE_PERIOD`, `LEADER_ELECTION`, `KUBE_CLIENT_QPS` and
`KUBE_CLIENT_BURST`. See [the design document](pod_snapshot_webhook_design.md) for
the deployment boundary and scalability model.

[Tenant manifests](manifests/tenant/kustomization.yaml) must be rendered with the
target tenant namespace and installed before any tenant workloads start. They grant
the proxy only `get workspaces` and `list services` in that namespace. They do not
grant users any roles. The tenant ingress policy selects all pods to avoid a
label-dependent creation window, allowing only proxy and controller pods from the
system namespace. Existing additive policies can still widen access.

Before exposing a deployment, complete these gates:

1. Package unchanged upstream controller/backend/frontend with pinned images and
  the required frontend build settings; configure controller webhooks and certificates.
2. Provision Gateway HTTPS and IAP, configure the exact backend-service audience,
  and grant IAP admission separately from explicit tenant user RoleBindings.
3. Verify NetworkPolicy enforcement and tenant RBAC/admission restrictions. Tenants
  must not create trusted system pods, alter policies, expose Services, or obtain
  privileged service-account credentials through notebook configuration.
4. Exercise Google login and actual JupyterLab HTTP, terminal, and kernel WebSockets.
  Kubernetes fake-client tests and an echo WebSocket are not substitutes for this.
5. Review shared-origin browser isolation. Notebook HTML/JavaScript runs on the same
  origin as the API, so same-origin checks do not isolate untrusted notebook content
  from a visiting user's API session. Do not claim hostile multi-tenant browser
  isolation; a separate-origin design may be required before deployment.
6. Decide long-lived connection policy. RBAC is checked at each request/handshake;
  established WebSockets are not reauthorized or terminated on revocation, and
  graceful HTTP shutdown does not drain hijacked WebSockets. Workspace deletion
  races and Service IP reuse still need live lifecycle testing.

The local commands do not apply manifests, mutate cloud resources, or perform
automated cleanup. The pilot was installed separately using the codelab procedure.
TPU and snapshot/restore work remains out of this milestone.

## Deployment planning

The renderer consumes the non-secret schema shown in
[deployment.example.json](deployment.example.json). The example deliberately has
invalid image placeholders: replace them with pushed, digest-pinned images, not
tags. Keep local configuration in the ignored `deployment.local.json` file.
Neither the configuration nor the generated plan contains private keys or OAuth
client secrets. Unknown JSON fields and invalid tenant/image values are rejected.

From `gke/`, generate a new plan directory:

```sh
make plan CONFIG=deployment.local.json OUTPUT=rendered/initial
```

[scripts/plan.sh](scripts/plan.sh) builds the renderer and produces four Kubernetes
Lists plus the source revision and configuration checksum. It refuses to overwrite
an existing directory and publishes the directory only after every stage renders.
It does not contact the Kubernetes API or Google Cloud. Plans are snapshots: generate
a new directory after any configuration or source change. The recorded Git revision
does not capture uncommitted integration changes; commit reviewed changes before
using a plan as a reproducible release artifact.

| Stage | Contents and ordering |
| --- | --- |
| `namespaces.json` | System and configured tenant namespaces; apply first |
| `isolation.json` | Upstream and integration NetworkPolicies, plus tenant-scoped proxy RBAC; apply and verify before workloads |
| `applications.json` | Unchanged upstream components through GKE overlays, cert-manager webhook resources, proxy and its configuration; all four application images pinned |
| `edge.json` | HTTPS-only GKE Gateway, one HTTPRoute to the proxy, IAP GCPBackendPolicy, and health-check policy |

The renderer uses the checked-out upstream manifests through
[manifests/upstream/kustomization.yaml](manifests/upstream/kustomization.yaml).
Regression tests check webhook DNS names and trust injection, disabled Istio,
backend authentication settings, image pinning, and offline plan generation.
It does not grant users roles, install demo WorkspaceKinds, or create notebooks.

### Certificates and platform prerequisites

Two independent TLS lifecycles are required:

- Browser to Gateway: a Google-managed Certificate Manager certificate with a
  certificate-map entry for `hostname`. `certificateMap` names that map, referenced
  through the Gateway's `networking.gke.io/certmap` annotation. No cert-manager
  public certificate and no Kubernetes listener TLS Secret are used.
- Kubernetes API server to controller: cert-manager's upstream self-signed Issuer
  and serving Certificate provide the internal Service DNS certificate and inject
  its CA into the validating-webhook configuration. Gateway TLS does not cover this
  connection. GKE managed workload identities are not a verified drop-in replacement.

Certificate readiness does not prove that the API server can reach the webhook.
The isolation stage permits TCP 9443 to controller pods from `controlPlaneCIDR`
and from `kube-system` pods labeled `k8s-app=konnectivity-agent`, with both selectors
required for that pod-based allowance. This GKE cluster uses those agents for
API-server connectivity; allowing only the direct control-plane IP caused real
admission timeouts. Validate with a server-side WorkspaceKind dry run rather than
disabling admission or broadly allowing the pod/node network.

The initial target is a dedicated, VPC-native GKE cluster with Dataplane V2,
HTTP load balancing, and the GKE Gateway controller enabled. Confirm that the
`gke-l7-global-external-managed` GatewayClass and the required Gateway/GKE policy
CRDs are present; do not blindly replace GKE-managed CRDs with an experimental bundle.

Before application, operators must provision or confirm:

1. A healthy, version-pinned cert-manager installation, including webhook and
  cainjector, without replacing a shared installation. The renderer does not install it.
2. The global external address named by `addressName`, appropriate DNS, and an
  active Certificate Manager certificate-map entry in the load balancer project.
3. An IAP OAuth configuration appropriate to the users' organization. Only users
  within the project's organization can use Google-managed OAuth with both
  `iapClientID` and `iapSecretName` empty. This external-user pilot requires a
  custom client ID and an existing Secret named by `iapSecretName` in
  `kubeflow-workspaces`, with the `client_secret` key. Use an approved
  secret-management workflow; never pass the secret through chat or commit it.
  See [OAuth setup](CODELAB.md#4-prepare-google-login).
4. Published images. Build the proxy from the `gke/` context and the frontend from
  the repository root with `docker build -f gke/frontend.Dockerfile ...`.
  Build controller and backend from unchanged upstream sources. Review base-image
  digests and scan all images before publishing a release.
5. Explicit IAP admission and least-privilege Kubernetes tenant bindings. Review
  user access to Secrets, pod metadata, service accounts, and shared infrastructure
  before admitting users; the renderer intentionally does not choose these grants.

### Fail-closed IAP bootstrap

The IAP audience contains the numeric Google backend-service ID, which is only
known after GKE creates that resource. The first plan can set `iapAudience` to
an empty string. This is not an authentication bypass: the proxy exits at startup,
has no ready endpoints, and the public route cannot serve application traffic.

After approving the documented security gates, the operator workflow is:

1. Select and verify the explicit project and Kubernetes context. Review the plan.
  Install namespaces, then isolation, then applications, then edge resources.
  The application stage includes CRDs; use server-side apply for large upstream
  CRDs and wait for CRD establishment. Do not force ownership conflicts.
2. Wait for GKE to create the backend service associated with the proxy Service's
  network endpoint groups. Verify that mapping; do not select an arbitrary service
  from the project. An unhealthy backend during bootstrap is expected.
3. Verify the IAP policy's attachment and enabled state, TLS certificate and Gateway
  status, and absence of alternate unprotected routes. Obtain the exact signed
  header audience from IAP's settings for that backend service.
4. Set `iapAudience` to `/projects/PROJECT_NUMBER/global/backendServices/SERVICE_ID`,
  render a new plan, and reapply the application stage. Restart the proxy Deployment
  because ConfigMap environment variables are read only when a pod starts. The
  same restart requirement applies when tenant configuration changes.
5. Verify webhook Certificate readiness, CA injection, all Deployment rollouts,
  and Gateway/HTTPRoute/policy status. Then execute the positive and negative
  browser and network-isolation acceptance tests before admitting real users.

Backend-service recreation requires repeating audience discovery; never loosen
audience validation to keep the deployment available. Do not treat this partially
validated bootstrap procedure as a production installer. No automatic apply, IAM mutation,
DNS update, or destructive cleanup is included yet. Preserve user PVCs and shared
cert-manager resources during any manual cleanup.

## Verified upstream constraints

At upstream revision `24ce51e5`:

- The controller can run with `USE_ISTIO=false`. This disables its Istio routing;
  it does not supply a replacement path to workspace Services.
- The backend authenticates configured identity headers. It does not verify IAP's
  signed assertion. Merely exposing it behind IAP is insufficient protection
  against requests that bypass the load balancer.
- The namespace-list endpoint requires cluster-scoped `list namespaces` permission
  and returns all namespaces. Tenant-filtered discovery is not provided here.
- The frontend has a build-time `standalone` mode for use without the Kubeflow
  dashboard. Image packaging must select it without editing upstream files.

References: [controller configuration](../workspaces/controller/cmd/main.go),
[backend authentication](../workspaces/backend/internal/auth/authentication.go),
[namespace endpoint](../workspaces/backend/api/namespaces_handler.go), and
[frontend configuration](../workspaces/frontend/config/dotenv.js).

## Architecture choices

| Option | Reuse | Integration ownership and unresolved risks |
| --- | --- | --- |
| Managed Cloud Service Mesh with Istio APIs | Upstream VirtualService generation and mesh security mechanisms | Validate every required API field, identity-header injection, and notebook WebSockets against the actual managed control plane. Operate mesh onboarding and policies. |
| GKE Gateway + IAP + one access proxy | Managed ingress/login, unchanged upstream with Istio disabled | Own assertion validation, identity translation, workspace authorization, dynamic routing, streaming, and isolation. One public backend limits per-workspace cloud resource churn. |
| GKE Gateway + IAP + per-workspace access proxies | Managed ingress/login and distributed enforcement | Own proxy lifecycle and per-workspace routes, policies, audiences, health checks, and reconciliation ordering. More moving parts per notebook. |
| Self-managed Istio | Closest to the existing deployment model | Own mesh upgrades and operations; not a Google-managed Istio offering. Retain as a compatibility reference rather than the default product. |

Managed Cloud Service Mesh is not categorically missing Istio CRDs: provisioning
installs them. Its documented support differs between control plane implementations.
For example, JWT copy-claim-to-header is not supported on the `TRAFFIC_DIRECTOR`
implementation, and the protocol section has WebSocket support caveats. Neither
CRD presence nor a successful smoke test alone establishes supported compatibility.
Check the chosen feature's release status rather than treating all CSM as preview.

### Selected architecture

Selected on 2026-09-11: GKE Gateway + IAP + one centralized access proxy, with
Google as the identity provider, email-based Kubernetes user bindings, and filtered
tenant discovery. We own the proxy's security-sensitive behavior. This decision
does not imply that the full architecture has been implemented or validated on GKE.

```text
Browser -> GKE HTTPS load balancer / IAP -> access proxy
                                           |-> upstream frontend
                                           |-> upstream backend -> Kubernetes RBAC checks
                                           |-> workspace Service (after workspace RBAC check)
```

IAP handles browser authentication and application admission. The proxy validates
`x-goog-iap-jwt-assertion` using a maintained JWT library, including signature,
algorithm, issuer, issued-at/expiry, and the exact expected backend-service audience.
Do not accept arbitrary audiences from the same project or fall back to unsigned
identity headers. Bootstrap must remain closed until that audience is configured.

The proxy removes caller-supplied identity and group headers, derives the configured
upstream user identity from the verified assertion, and does not invent group
membership. IAP admission is not workspace authorization: notebook connections
also require a Kubernetes SubjectAccessReview for the requested workspace. The
existing backend continues to authorize its own API requests.

Resolve workspace targets only from trusted Kubernetes resources, not caller-supplied
hosts or URLs. Validate the upstream URL/base-path contract before implementing
rewrites. Preserve notebook HTTP and WebSocket behavior, while keeping IAP assertions
and identity headers out of notebook containers that do not need them.

Backend and workspace ingress must not be reachable by untrusted pods around the
proxy. Establish NetworkPolicy before starting workloads, allow required controller
activity probes, and test direct pod/Service access. NetworkPolicy is additive and
does not establish cryptographic workload identity. Tenant RBAC must prevent users
from altering these policies, impersonating trusted pods, or exposing new Services.
Cluster administrators remain trusted. A dedicated cluster is the initial target;
shared-cluster support needs an explicit threat model.

## Deployment inputs and limits

Confirm project, cluster/location, hostname/TLS, IAP configuration, allowed users,
and tenant namespaces before deployment. Never store secrets in checked-in
configuration or require them to pass through chat.

Initially, administrators grant IAP application admission and bind verified Google
account emails to tenant roles in Kubernetes. A Google account alone grants neither
permission. Group synchronization and external identity providers are separate
capabilities, not implicit IAP features. Do not add them to the initial deployment.

## Deployment acceptance gates

The eventual automation should validate prerequisites, render a reviewable plan,
install idempotently, verify readiness and security, and report the usable URL.
Uninstall must distinguish integration-owned resources from shared infrastructure
and retain user storage unless deletion is explicitly requested.

- One browser flow: login, tenant selection, create workspace, open JupyterLab,
  run a cell, and use its terminal through WebSockets.
- Anonymous, invalid-token, expired-token, wrong-audience, forged-user/group-header,
  and cross-tenant requests are rejected. Health checks expose only health status.
- Direct backend and notebook access from an unrelated pod is blocked, including
  during creation, restart, and restore. Controller activity probes still work.
- Workspace deletion/recreation cannot route a stale authorized request to another
  tenant. Encoded paths, redirects, cookies, and streaming are tested explicitly.
- User removal, RBAC revocation, long-lived sessions, and WebSocket reconnection
  have documented and tested behavior. Existing connections need an explicit
  revocation policy; handshake authorization alone does not terminate them.
- Reapplying deployment configuration is safe; upgrades preserve PVCs; uninstall
  reports remaining cloud resources and costs. All repository changes stay here.

## Later integrations

TPU support should start with GKE provisioning plus upstream WorkspaceKind resource
and scheduling options, using Workload Identity Federation for workload access to
Google Cloud APIs. Verify accelerator topology, quota, image compatibility, and the
upstream pod-template contract before promising a configuration-only integration.

Treat persistent-volume snapshots and process-memory checkpoint/restore as different
features. A volume snapshot does not restore a running kernel, accelerator state,
or open connections. Evaluate the exact GKE snapshot/restore capability, release
status, privileges, and workload constraints separately. Any integration must
coordinate with the upstream controller's lifecycle without competing writes and
reapply isolation before restored workloads become reachable.

## Platform references

Reviewed 2026-09-11; documentation is not a substitute for live acceptance tests.

- [GKE Gateway policies and IAP](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/configure-gateway-resources#configure_iap)
- [IAP signed assertion validation](https://docs.cloud.google.com/iap/docs/signed-headers-howto)
- [Managed CSM support with Istio APIs](https://docs.cloud.google.com/service-mesh/docs/supported-features-managed)
