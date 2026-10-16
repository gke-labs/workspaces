#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# ==============================================================================
# Deploy Agent Substrate + AX next to Standalone Kubeflow Workspaces on GKE
# ==============================================================================
# Run from your workstation, after providers/gke/deploy_standalone.sh, against
# a cluster that serves the PodCertificateRequest and ClusterTrustBundle APIs
# (GKE >= 1.37, or 1.36 created with them enabled; see README.md step 1).
#
# This script provisions:
#   1. A dedicated node pool for Substrate worker pods (not a gVisor pool)
#   2. A GCS bucket for actor snapshots, with Workload Identity bindings
#   3. Agent Substrate (ate-system) from pre-built release images
#   4. A WorkerPool: the warm worker pods the agents are packed onto
#   5. AX (ax-system): Redis, ax-server, and the agent runtime image every
#      agent runs (AX's ax-task-runner + agent-runtime/ax-coding-agent)
#   6. The "Code Server + AX agents" Workspace image (Gemini CLI with the
#      agent-substrate extension, ax-agents, ax), offered as an image option
#      on the codeserver WorkspaceKind
#   7. fleet-helper (default egress policy + read-only actor view)
#   8. Access for the tenant: NetworkPolicies and the RBAC `ax ssh` needs
#   9. A smoke test: one AX task through create -> resume -> ssh -> delete
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=versions.env
source "${SCRIPT_DIR}/versions.env"

# ------------------------------------------------------------------------------
# Configuration. The first block matches providers/gke/deploy_standalone.sh:
# export the same values you deployed the platform with.
# ------------------------------------------------------------------------------
export PROJECT_ID="${PROJECT_ID:-${PROJECT:-$(gcloud config get-value project 2>/dev/null || true)}}"
export CLUSTER_NAME="${CLUSTER_NAME:-${CLUSTER:-kubeflow-notebooks}}"
export LOCATION="${LOCATION:-us-central1-c}"
export REGION="${REGION:-us-central1}"
export TENANT_NAMESPACE="${TENANT_NAMESPACE:-kubeflow-user}"
export REPO_NAME="${REPO_NAME:-${REPOSITORY:-kubeflow-repo}}"
export CONTEXT="${CONTEXT:-gke_${PROJECT_ID}_${LOCATION}_${CLUSTER_NAME}}"
if [[ -z "${REGISTRY:-}" || "${REGISTRY:-}" != *"${PROJECT_ID}"* ]]; then
  export REGISTRY="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}"
fi

# Example-specific settings.
export SUBSTRATE_VERSION
if [[ -n "${ATE_IMAGE_REPO:-}" ]]; then
  export ATE_IMAGE_REPO
else
  unset ATE_IMAGE_REPO || true
fi
export KO_DOCKER_REPO="${KO_DOCKER_REPO:-${REGISTRY}/substrate}"
export KO_DEFAULTPLATFORMS="${KO_DEFAULTPLATFORMS:-linux/amd64}"
export WORKER_IMAGE="${WORKER_IMAGE:-}"
export ATE_SNAPSHOT_BUCKET="${ATE_SNAPSHOT_BUCKET:-${PROJECT_ID}-ate-snapshots}"
export SUBSTRATE_NODE_POOL="${SUBSTRATE_NODE_POOL:-substrate-pool}"
export SUBSTRATE_MACHINE_TYPE="${SUBSTRATE_MACHINE_TYPE:-c3-standard-4}"
export SUBSTRATE_NODES="${SUBSTRATE_NODES:-2}"
export CREATE_NODE_POOL="${CREATE_NODE_POOL:-true}"
export WORKER_NAMESPACE="${WORKER_NAMESPACE:-ate-workers}"
export WORKER_REPLICAS="${WORKER_REPLICAS:-4}"
export WORKER_CPU="${WORKER_CPU:-2}"
export WORKER_MEMORY="${WORKER_MEMORY:-4Gi}"
# Hosts agents may reach (TLS passthrough on :443). The Gemini API is all the
# Antigravity agent needs; the scripted mode needs no egress at all.
export EGRESS_HOSTNAMES="${EGRESS_HOSTNAMES:-[\"generativelanguage.googleapis.com\"]}"
# The agents' model key. Stored as a Secret in ax-system; ax-server hands it to
# every agent sandbox. Defaults to the tenant's gemini-api-key Secret, which the
# codeserver WorkspaceKind also passes to Gemini CLI in the Workspace.
export GEMINI_API_KEY="${GEMINI_API_KEY:-}"
if [[ -z "${GEMINI_API_KEY}" ]]; then
  GEMINI_API_KEY="$(kubectl --context "${CONTEXT}" -n "${TENANT_NAMESPACE}" get secret gemini-api-key -o jsonpath='{.data.GEMINI_API_KEY}' 2>/dev/null | base64 -d 2>/dev/null || true)"
fi
# Where sources are cloned and binaries built (outside the repository).
export WORK_DIR="${WORK_DIR:-${HOME}/.cache/gke-workspaces/agent-substrate}"
# Build the task-runner image with Cloud Build instead of local Docker.
export USE_CLOUD_BUILD="${USE_CLOUD_BUILD:-false}"
export SMOKE_TEST="${SMOKE_TEST:-true}"
# Build the "Code Server + AX agents" Workspace image and add it as an image
# option to these WorkspaceKinds (those that exist; others are skipped).
export BUILD_WORKSPACE_IMAGE="${BUILD_WORKSPACE_IMAGE:-true}"
export WORKSPACE_KINDS="${WORKSPACE_KINDS:-codeserver}"

export SUBSTRATE_SRC="${WORK_DIR}/substrate-${SUBSTRATE_VERSION}"
export AX_SRC="${WORK_DIR}/ax-${AX_REF//\//-}"
export AX_TASK_IMAGE_REPO="${AX_TASK_IMAGE_REPO:-${REGISTRY}/ax-task-runner}"
export AGENT_RUNTIME_IMAGE_REPO="${AGENT_RUNTIME_IMAGE_REPO:-${REGISTRY}/ax-agent-runtime}"
K=(kubectl --context "${CONTEXT}")

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
render() { envsubst "$1" < "$2"; }  # $1: the only variables to substitute

[[ -n "${PROJECT_ID}" ]] || die "PROJECT_ID is not set. Run: export PROJECT_ID=your-gcp-project-id"

cat <<EOF
==============================================================================
Deploying Agent Substrate ${SUBSTRATE_VERSION} + AX (${AX_REF}) for Kubeflow Workspaces
  Project / cluster:   ${PROJECT_ID} / ${CLUSTER_NAME} (${LOCATION})
  Tenant namespace:    ${TENANT_NAMESPACE}   (AX atespace: ${TENANT_NAMESPACE})
  Worker pool:         ${WORKER_REPLICAS} x (${WORKER_CPU} CPU, ${WORKER_MEMORY}) on node pool ${SUBSTRATE_NODE_POOL}
  Snapshot bucket:     gs://${ATE_SNAPSHOT_BUCKET}
  Artifact Registry:   ${REGISTRY}
  Gemini API key:      $([[ -n "${GEMINI_API_KEY}" ]] && echo found || echo "MISSING: agents cannot run without one (see README)")
==============================================================================
EOF

# ------------------------------------------------------------------------------
log "1/9 Preflight checks"
# ------------------------------------------------------------------------------
for tool in gcloud kubectl git go envsubst python3; do
  command -v "${tool}" >/dev/null || die "'${tool}' is required but not installed."
done
if [[ "${USE_CLOUD_BUILD}" != "true" ]]; then
  command -v docker >/dev/null || die "docker is required (or set USE_CLOUD_BUILD=true)."
fi
gcloud container clusters get-credentials "${CLUSTER_NAME}" \
  --location "${LOCATION}" --project "${PROJECT_ID}" >/dev/null
"${K[@]}" get namespace "${TENANT_NAMESPACE}" >/dev/null 2>&1 \
  || die "Tenant namespace ${TENANT_NAMESPACE} not found. Run providers/gke/deploy_standalone.sh first."

for res in clustertrustbundles podcertificaterequests; do
  "${K[@]}" api-resources --api-group=certificates.k8s.io -o name 2>/dev/null | grep -q "^${res}\." \
    || die "The cluster does not serve ${res}.certificates.k8s.io. Agent Substrate needs GKE >= 1.37, or 1.36 created with --enable-kubernetes-unstable-apis (see README.md, step 1). These APIs cannot be enabled on an existing 1.36 cluster."
done

WI_POOL="$(gcloud container clusters describe "${CLUSTER_NAME}" --location "${LOCATION}" \
  --project "${PROJECT_ID}" --format='value(workloadIdentityConfig.workloadPool)')"
[[ "${WI_POOL}" == "${PROJECT_ID}.svc.id.goog" ]] || die "Workload Identity is not enabled on the cluster."
AUTOPILOT="$(gcloud container clusters describe "${CLUSTER_NAME}" --location "${LOCATION}" \
  --project "${PROJECT_ID}" --format='value(autopilot.enabled)')"
[[ "${AUTOPILOT}" != "True" ]] || die "Agent Substrate needs a GKE Standard cluster (its atelet DaemonSet manages node-local snapshot state)."
PROJECT_NUMBER="$(gcloud projects describe "${PROJECT_ID}" --format='value(projectNumber)')"

# ko builds the Go images (ax-server, fleet-helper). Install it if missing.
export PATH="${WORK_DIR}/bin:${PATH}"
if ! command -v ko >/dev/null; then
  log "Installing ko into ${WORK_DIR}/bin"
  GOBIN="${WORK_DIR}/bin" go install github.com/google/ko@latest
fi
gcloud auth configure-docker "${REGION}-docker.pkg.dev" --quiet >/dev/null 2>&1

# ------------------------------------------------------------------------------
log "2/9 Node pool for Substrate workers (${SUBSTRATE_NODE_POOL})"
# ------------------------------------------------------------------------------
if gcloud container node-pools describe "${SUBSTRATE_NODE_POOL}" --cluster "${CLUSTER_NAME}" \
     --location "${LOCATION}" --project "${PROJECT_ID}" >/dev/null 2>&1; then
  echo "Node pool exists."
elif [[ "${CREATE_NODE_POOL}" == "true" ]]; then
  # A regular (runc) pool: Substrate runs gVisor itself inside the worker pods,
  # which cannot nest inside a GKE Sandbox (gVisor) node.
  gcloud container node-pools create "${SUBSTRATE_NODE_POOL}" \
    --cluster "${CLUSTER_NAME}" --location "${LOCATION}" --project "${PROJECT_ID}" \
    --machine-type "${SUBSTRATE_MACHINE_TYPE}" --num-nodes "${SUBSTRATE_NODES}" \
    --workload-metadata GKE_METADATA
else
  die "Node pool ${SUBSTRATE_NODE_POOL} does not exist and CREATE_NODE_POOL=false."
fi

# ------------------------------------------------------------------------------
log "3/9 Snapshot bucket gs://${ATE_SNAPSHOT_BUCKET} and Workload Identity bindings"
# ------------------------------------------------------------------------------
if ! gcloud storage buckets describe "gs://${ATE_SNAPSHOT_BUCKET}" --project "${PROJECT_ID}" >/dev/null 2>&1; then
  gcloud storage buckets create "gs://${ATE_SNAPSHOT_BUCKET}" --project "${PROJECT_ID}" \
    --location "${REGION}" --uniform-bucket-level-access
fi
# atelet (snapshot streaming) and ate-api-server (snapshot copy/GC) authenticate
# as their Kubernetes ServiceAccounts; no Google service account is involved.
WI="principal://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${PROJECT_ID}.svc.id.goog/subject/ns/ate-system/sa"
for member in "${WI}/atelet" "${WI}/ate-api-server"; do
  for role in roles/storage.objectAdmin roles/storage.bucketViewer; do
    gcloud storage buckets add-iam-policy-binding "gs://${ATE_SNAPSHOT_BUCKET}" \
      --member "${member}" --role "${role}" >/dev/null
  done
done
# atelet pulls the agent images (and sandbox runtime assets) from Artifact Registry.
gcloud artifacts repositories add-iam-policy-binding "${REPO_NAME}" --location "${REGION}" \
  --project "${PROJECT_ID}" --member "${WI}/atelet" --role roles/artifactregistry.reader >/dev/null

# ------------------------------------------------------------------------------
log "4/9 Agent Substrate ${SUBSTRATE_VERSION} (ate-system)"
# ------------------------------------------------------------------------------
mkdir -p "${WORK_DIR}"
if [[ ! -d "${SUBSTRATE_SRC}/.git" ]]; then
  git clone --quiet --depth 1 --branch "${SUBSTRATE_VERSION}" "${SUBSTRATE_REPO}" "${SUBSTRATE_SRC}"
fi
render '${CONTEXT} ${KO_DOCKER_REPO} ${KO_DEFAULTPLATFORMS} ${ATE_SNAPSHOT_BUCKET} ${PROJECT_ID} ${CLUSTER_NAME} ${LOCATION}' \
  "${SCRIPT_DIR}/manifests/substrate-install.yaml" > "${WORK_DIR}/substrate-install.yaml"
# The k8s.io credential provider is deployed for completeness (secret injection
# is how a production setup would hand agents API keys); this demo's policy
# uses TLS passthrough and does not depend on it.
(cd "${SUBSTRATE_SRC}" && ATE_CONFIG="${WORK_DIR}/substrate-install.yaml" \
  ./hack/install-ate.sh --deploy-ate-system --credential-provider='{"name":"k8s.io"}')

# ------------------------------------------------------------------------------
log "5/9 WorkerPool ax-agents (${WORKER_NAMESPACE})"
# ------------------------------------------------------------------------------
if [[ -z "${WORKER_IMAGE}" ]]; then
  log "Building ateworker-gvisor worker image with ko"
  WORKER_IMAGE="$(cd "${SUBSTRATE_SRC}" && KO_DOCKER_REPO="${KO_DOCKER_REPO}" ko build --bare --platform=linux/amd64 ./cmd/ateworker-gvisor)"
fi
render '${WORKER_NAMESPACE} ${WORKER_REPLICAS} ${WORKER_IMAGE} ${SUBSTRATE_NODE_POOL} ${WORKER_CPU} ${WORKER_MEMORY}' \
  "${SCRIPT_DIR}/manifests/workerpool.yaml" | "${K[@]}" apply -f -
echo "Waiting for ${WORKER_REPLICAS} worker pod(s)..."
for _ in $(seq 1 60); do
  ready="$("${K[@]}" -n "${WORKER_NAMESPACE}" get workerpool ax-agents -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
  [[ "${ready:-0}" -ge "${WORKER_REPLICAS}" ]] && break
  sleep 10
done
"${K[@]}" -n "${WORKER_NAMESPACE}" get workerpool ax-agents

# ------------------------------------------------------------------------------
log "6/9 AX (${AX_REF}): agent runtime image, ax-server, ax CLI"
# ------------------------------------------------------------------------------
if [[ ! -d "${AX_SRC}/.git" ]]; then
  git clone --quiet --branch "${AX_REF}" "${AX_REPO}" "${AX_SRC}"
else
  git -C "${AX_SRC}" fetch --quiet origin "${AX_REF}" && git -C "${AX_SRC}" checkout --quiet FETCH_HEAD
fi
AX_SHA="$(git -C "${AX_SRC}" rev-parse --short HEAD)"

# Build and push an image, then print its digest-pinned reference (Substrate
# only accepts digest-pinned images in an ActorTemplate).
# $1: build context, $2: tag, $3: Dockerfile (relative to $1), $4...: build args.
build_image() {
  local ctx="$1" tag="$2" dockerfile="$3"; shift 3
  local a
  if [[ "${USE_CLOUD_BUILD}" == "true" ]]; then
    local args=""
    for a in "$@"; do args+=", --build-arg, \"${a}\""; done
    printf 'steps:\n- name: gcr.io/cloud-builders/docker\n  args: [build, --platform, linux/amd64, -t, "%s", -f, "%s"%s, .]\nimages: ["%s"]\n' \
      "${tag}" "${dockerfile}" "${args}" "${tag}" > "${WORK_DIR}/cloudbuild-image.yaml"
    gcloud builds submit "${ctx}" --project "${PROJECT_ID}" --config "${WORK_DIR}/cloudbuild-image.yaml" >&2
  else
    local build_args=()
    for a in "$@"; do build_args+=(--build-arg "${a}"); done
    docker buildx build --platform linux/amd64 -t "${tag}" --push -f "${ctx}/${dockerfile}" \
      ${build_args[@]+"${build_args[@]}"} "${ctx}" >&2
  fi
  echo "${tag%:*}@$(gcloud artifacts docker images describe "${tag}" --format='value(image_summary.digest)')"
}

# AX's task image: Python 3.12, git, the Antigravity SDK and ax-task-runner as PID 1.
(cd "${AX_SRC}" && mkdir -p bin/linux_amd64 && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" -o bin/linux_amd64/ax-task-runner ./cmd/ax-task-runner)
AX_TASK_IMAGE="$(build_image "${AX_SRC}" "${AX_TASK_IMAGE_REPO}:${AX_SHA}" Dockerfile.task-runner)"
echo "AX task image:  ${AX_TASK_IMAGE}"
# The agent runtime adds the coding agent (agent-runtime/ax-coding-agent) on top.
RUNTIME_SHA="$(cat "${SCRIPT_DIR}/agent-runtime/Dockerfile" "${SCRIPT_DIR}/agent-runtime/ax-coding-agent" | sha256sum | cut -c1-10)"
AX_AGENT_IMAGE="$(build_image "${SCRIPT_DIR}/agent-runtime" \
  "${AGENT_RUNTIME_IMAGE_REPO}:${AX_SHA}-${RUNTIME_SHA}" Dockerfile "BASE_IMAGE=${AX_TASK_IMAGE}")"
export AX_AGENT_IMAGE
echo "Agent runtime:  ${AX_AGENT_IMAGE}"

"${K[@]}" apply -f "${AX_SRC}/deploy/redis.yaml"
(cd "${AX_SRC}" && KO_DOCKER_REPO="${REGISTRY}/ax" ko resolve --base-import-paths -f deploy/ax-server.yaml) \
  | "${K[@]}" apply -f -
if [[ -n "${GEMINI_API_KEY}" ]]; then
  "${K[@]}" -n ax-system create secret generic gemini-api-secret \
    --from-literal=GEMINI_API_KEY="${GEMINI_API_KEY}" --dry-run=client -o yaml | "${K[@]}" apply -f -
  "${K[@]}" -n ax-system set env deployment/ax-server --from=secret/gemini-api-secret >/dev/null
fi
# Tasks that do not name an image (all of them, with ax-agents) run the agent runtime.
"${K[@]}" -n ax-system set env deployment/ax-server \
  AX_SNAPSHOTS_BUCKET="gs://${ATE_SNAPSHOT_BUCKET}/ax/" \
  AX_DEFAULT_TASK_IMAGE="${AX_AGENT_IMAGE}" >/dev/null
"${K[@]}" -n ax-system rollout status deployment/ax-redis --timeout=180s
"${K[@]}" -n ax-system rollout status deployment/ax-server --timeout=300s

# The ax CLI: for the Workspace image (linux/amd64) and for this workstation.
(cd "${AX_SRC}" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "${WORK_DIR}/ax-linux-amd64" ./cmd/ax \
  && go build -trimpath -o "${WORK_DIR}/bin/ax" ./cmd/ax)

# ------------------------------------------------------------------------------
log "7/9 Workspace image: Code Server + AX agents"
# ------------------------------------------------------------------------------
if [[ "${BUILD_WORKSPACE_IMAGE}" == "true" ]]; then
  WS_IMAGE_TAG="v$(date -u +%Y%m%d-%H%M%S)-ax-${AX_SHA}"
  BUILD_FLAGS=(--codeserver-ax --registry-path "${REGISTRY}" --tag "${WS_IMAGE_TAG}"
               --ax-repo "${AX_REPO}" --ax-ref "${AX_REF}")
  [[ "${USE_CLOUD_BUILD}" == "true" ]] && BUILD_FLAGS+=(--cloud-build)
  "${REPO_ROOT}/images/build.sh" "${BUILD_FLAGS[@]}"
  export AX_WORKSPACE_IMAGE="${REGISTRY}/codeserver-ax:${WS_IMAGE_TAG}"
  for kind in ${WORKSPACE_KINDS}; do
    if ! "${K[@]}" get workspacekind "${kind}" >/dev/null 2>&1; then
      echo "WorkspaceKind ${kind} not found; skipping."
      continue
    fi
    patch="$("${K[@]}" get workspacekind "${kind}" -o json \
      | python3 "${SCRIPT_DIR}/register_workspace_image.py" --image "${AX_WORKSPACE_IMAGE}")"
    if [[ -n "${patch}" ]]; then
      "${K[@]}" patch workspacekind "${kind}" --type=json -p "${patch}"
    fi
    echo "WorkspaceKind ${kind}: image option \"Code Server + AX agents (CPU)\" -> ${AX_WORKSPACE_IMAGE}"
  done
else
  echo "Skipped (BUILD_WORKSPACE_IMAGE=false)."
fi

# ------------------------------------------------------------------------------
log "8/9 fleet-helper and tenant access (${TENANT_NAMESPACE})"
# ------------------------------------------------------------------------------
FLEET_HELPER_IMAGE="$(cd "${SCRIPT_DIR}/fleet-helper" && KO_DOCKER_REPO="${REGISTRY}" ko build --base-import-paths .)"
export FLEET_HELPER_IMAGE
render '${TENANT_NAMESPACE} ${EGRESS_HOSTNAMES} ${FLEET_HELPER_IMAGE}' \
  "${SCRIPT_DIR}/manifests/fleet-helper.yaml" | "${K[@]}" apply -f -
render '${TENANT_NAMESPACE}' "${SCRIPT_DIR}/manifests/ax-access.yaml" | "${K[@]}" apply -f -
"${K[@]}" -n ax-system rollout status deployment/fleet-helper --timeout=180s

# ------------------------------------------------------------------------------
log "9/9 Smoke test"
# ------------------------------------------------------------------------------
if [[ "${SMOKE_TEST}" == "true" ]]; then
  AX=("${WORK_DIR}/bin/ax" --context "${CONTEXT}" -a "${TENANT_NAMESPACE}")
  "${AX[@]}" apply -f - <<EOF
apiVersion: ax.io/v1alpha1
kind: Task
metadata: {name: ax-smoke, atespace: ${TENANT_NAMESPACE}}
spec:
  command: ["sh", "-c", "echo ok > /workspace/smoke.txt"]
  debug: true
EOF
  "${AX[@]}" resume task ax-smoke
  out="$("${AX[@]}" ssh ax-smoke -- cat /workspace/smoke.txt || true)"
  "${AX[@]}" delete task ax-smoke >/dev/null || true
  [[ "${out}" == *ok* ]] || die "Smoke test failed: could not read /workspace/smoke.txt from the task (got: ${out})"
  echo "Smoke test passed: a task ran on Substrate and answered over ax ssh."
fi

cat <<EOF

==============================================================================
Done. Next: create a Workspace in ${TENANT_NAMESPACE} with kind "codeserver"
and image "Code Server + AX agents (CPU)", open a terminal, and run: gemini
Try: "The tests in ~/textkit fail. Send one background agent per failing test
module to fix them, then merge the fixes."

From this workstation:
  ${WORK_DIR}/bin/ax --context ${CONTEXT} -a ${TENANT_NAMESPACE} get tasks
  (cd ${SUBSTRATE_SRC} && go run ./cmd/kubectl-ate get workers)
==============================================================================
EOF
