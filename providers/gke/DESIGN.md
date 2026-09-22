# Design: why this integration works the way it does

Status: pilot, 2026-09. Source:
[github.com/aojea/notebooks, branch `gke`, directory `gke/`](https://github.com/aojea/notebooks/tree/gke/gke).
The dated evidence lives in the
[codelab deployment record](https://github.com/aojea/notebooks/blob/gke/gke/CODELAB.md#deployment-record);
this document explains
the choices. Deeper analysis is in the `gateway_api` branch's
`istio-replacement-evaluation.md` and `design-gateway-api.md`
([kubeflow/notebooks#1301](https://github.com/kubeflow/notebooks/pull/1301)).

## The decision

Run unmodified upstream Kubeflow Notebooks on GKE behind the managed Gateway and
Identity-Aware Proxy, with one access proxy that this integration owns: it
verifies IAP's signed assertion, translates it into the identity header upstream
expects, authorizes workspace connections with a `SubjectAccessReview`, and
proxies HTTP and WebSockets. An optional second hostname serves standard Jupyter
connection tokens for desktop clients. Everything GKE-specific lives in this
directory; Kubernetes RBAC remains the single authorization source of truth.

## Why not the alternatives

| Alternative | Why not |
| --- | --- |
| Full Kubeflow distribution (Istio) | The goal is standalone Notebooks. Istio's guarantee needs the whole mesh: proxy-attached policy, mTLS-anchored identity, a sidecar on every pod. Sound, but not standalone, and heavy to operate for one application. |
| Portable Gateway API series (`gateway_api` branch) | The standard `ExternalAuth` filter (GEP-1494) is experimental, leaves the denial response and path convention unspecified, and the GKE Gateway does not implement it — the managed controller also cannot share a cluster with experimental-channel CRDs. A live deployment without the mesh's safety net exposed two real defects: forged identity headers were accepted, and anonymous browsers never reached a login page. |
| Envoy Gateway `SecurityPolicy` + Dex + oauth2-proxy | Validated, but the security half is implementation-specific CRDs — the lock-in a portable deployment tries to avoid — and it means operating an identity provider and session proxy ourselves. |
| Fork upstream with GKE logic | Rebase conflicts forever, and Google Cloud support for a platform component does not extend to forked application code. Overlays plus an adapter consume upstream releases unchanged. |

The decisive property of IAP over every header-based scheme: the application does
not *trust* an identity, it *verifies* one. IAP adds `x-goog-iap-jwt-assertion`,
an ES256 JWT checked in
[identity.go](https://github.com/aojea/notebooks/blob/gke/gke/internal/access/identity.go)
against
Google's JWKS for signature, issuer, exact audience, and lifetime, with no
unsigned-header fallback. Google documents this verification as the defence even
against IAP being disabled or bypassed from inside the project. Plain
`kubeflow-userid` headers offer nothing equivalent without a mesh.

## Pros and cons

Pros:

- Signed, verifiable end-user identity at the application, not a trusted header.
- Zero upstream changes: controller, backend, frontend, APIs and manifests are
  consumed pinned and unmodified, through overlays.
- Managed login, IAM gating, TLS, and load balancing; no self-hosted Dex,
  oauth2-proxy, or session store to operate or patch.
- One owned data-path component; per-tenant `NetworkPolicy` prevents going
  around it.
- Workspace access is decided by `SubjectAccessReview` against existing RBAC —
  IAP admission alone grants nothing, and no IAM-to-RBAC translation is invented.

Cons:

- GKE-only by construction: `GatewayClass`, `GCPBackendPolicy`, IAP, IAM
  identities. This is the accepted trade-off for keeping upstream portable.
- The access proxy re-implements connect-path routing semantics the upstream
  controller already has (port resolution, prefix stripping, header injection);
  upstream changes there must be tracked by compatibility tests.
- The namespace-list filter is a temporary application-API adapter (see the
  [removal plan](https://github.com/aojea/notebooks/blob/gke/gke/README.md#temporary-namespace-filtering-adapter)).
- A data-path proxy we own is a security- and availability-critical component.
- Pilot-grade today: single trusted user; production and multi-tenant gates —
  enrollment, culling verification, upgrades, scale — remain open.

## How the browser path works

DNS → reserved address → GKE Gateway (Certificate Manager TLS) → IAP (login,
IAM check, strips client `x-goog-*` headers) → access proxy → upstream.
The proxy, per request:

1. Verifies the IAP assertion
   ([identity.go](https://github.com/aojea/notebooks/blob/gke/gke/internal/access/identity.go)).
2. Strips inbound identity and group headers and forwards only the verified
   Google-account email as the upstream user
   ([proxy.go](https://github.com/aojea/notebooks/blob/gke/gke/internal/access/proxy.go)).
3. For `/workspace/connect/…`, resolves the upstream-owned Service and checks
   workspace access with a `SubjectAccessReview` before proxying, WebSockets
   included
   ([kubernetes.go](https://github.com/aojea/notebooks/blob/gke/gke/internal/access/kubernetes.go)).
4. Answers the namespace-list request with only configured tenant namespaces
   where the user may `list` workspaces, failing closed on authorization errors.

Deployment policies block direct pod access around the proxy while preserving
controller webhooks and probes; cert-manager (reused upstream component) owns the
webhook certificate.

## How the desktop (VS Code) path works

The unmodified Microsoft Jupyter extension cannot complete IAP's browser login —
tested and failed at authentication — so a second, dedicated hostname on the same
Gateway serves token-authenticated requests with IAP off *for that backend only*.
Browser access and token issuance stay behind IAP.

Why Kubernetes-minted tokens instead of a token database or static secrets: each
generated connection is a dedicated ServiceAccount in a protected namespace, with
no RBAC and no pod. The token comes from `TokenRequest` with a desktop-only
audience and an administrator-bounded lifetime; the ServiceAccount records the
user and exact workspace UID/port, never the token. Kubernetes is the issuer,
the validator (`TokenReview` on every request, plus an authoritative grant lookup
that defeats TokenReview caching), and the revocation mechanism (revoke = delete
the ServiceAccount). The dedicated audience cannot authenticate to the Kubernetes
API. Active WebSockets are revalidated every 15 seconds and closed on revocation,
expiry, or lost workspace access; the proxy strips the token and Referer before
forwarding and supplies Jupyter's XSRF pair itself.

Costs, accepted knowingly:

- Jupyter puts tokens in WebSocket URLs, so desktop access logging must stay off
  and users must treat the URL as a password.
- No automatic renewal; the tested cluster capped tokens at 48 hours regardless
  of the requested seven days. Long work needs replacement URLs; the kernel and
  its state survive replacement (verified), VS Code's reconnect ergonomics are a
  validation gate.
- Token expiry is not kernel lifetime: culling and idle policies act separately.

Validation lives in
[vscode_validation.ipynb](https://github.com/aojea/notebooks/blob/gke/gke/vscode_validation.ipynb)
and the
[user guide](https://github.com/aojea/notebooks/blob/gke/gke/USER_GUIDE.md#vs-code-jupyter-extension);
acceptance evidence in the
[codelab record](https://github.com/aojea/notebooks/blob/gke/gke/CODELAB.md#2026-09-12-kubernetes-tokens-and-vs-code).

## Future work

The access proxy is now the one place where authentication, authorization, and
workspace routing meet, standing in for the mesh — an integration point that
upstream does not have to know about. That is where GKE-only capabilities can
land without forking upstream:

- **Pod snapshot restore.** Stopping a workspace today preserves files but not
  process memory: kernels and their variables are lost. GKE pod snapshotting is
  the candidate for making pause/resume — and possibly node maintenance and
  preemption — restore running kernels, with the proxy already owning the
  connection lifecycle around it. Unverified; needs evaluation against notebook
  pods with attached storage.
- **Deeper TPU integration.** Surface TPU topologies, provisioning classes, and
  reservation/queueing (e.g. Dynamic Workload Scheduler) through WorkspaceKind
  pod configuration, validated by this integration's tests instead of hand-built
  pod specs. The first milestone stays secure access, not accelerators, so this
  follows the security gates.
- **One-touch enrollment on Google Groups.** Enrollment today touches two
  layers: an IAP member and a RoleBinding subject. Both layers already accept
  groups — IAP natively, and RoleBindings through GKE's Google Groups for RBAC —
  so the end state is a single group membership covering admission and
  authorization. The missing piece is the proxy: group resolution happens at
  authentication time, not in the RBAC authorizer, so a `SubjectAccessReview`
  carrying only the email never matches a `Group` subject. The proxy must
  resolve transitive membership (Cloud Identity `checkTransitiveMembership`),
  cached and fail-closed, and pass groups in the review. Existing GKE features
  over invented enrollment machinery.
- **Production and multi-tenant gates.** Tenant provisioning,
  activity/culling verification, upgrade and cleanup automation, desktop token
  renewal ergonomics, and removal of the temporary namespace-filter adapter once
  upstream ships equivalent filtering
  ([removal plan](https://github.com/aojea/notebooks/blob/gke/gke/README.md#temporary-namespace-filtering-adapter)).

Two requirements are standing, not future:

- **Independence of this folder.** Everything lands under `gke/`; upstream
  rebases must never conflict with the integration. Behavioral drift is caught
  by compatibility tests against pinned upstream revisions, not by textual merge
  conflicts.
- **Upstream must keep Istio optional, officially.** This integration exists
  because Notebooks can run without the mesh; that has to become a supported
  upstream guarantee (routing provider stays pluggable, no hard Istio dependency
  in backend or controller), pursued as an upstream ask rather than assumed.
