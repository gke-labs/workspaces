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
# Deploy Standalone Kubeflow Workspaces (Notebooks v2), Kubeflow Trainer (v2),
# and Kubeflow Spark Operator on GKE WITHOUT Istio
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
DIST_DIR="${DIST_DIR:-/tmp/kubeflow-community-distribution}"

# ==============================================================================
# 1. Configuration & Environment Variables
# ==============================================================================
export PROJECT_ID="${PROJECT_ID:-${PROJECT:-$(gcloud config get-value project 2>/dev/null || true)}}"
export CLUSTER_NAME="${CLUSTER_NAME:-${CLUSTER:-kubeflow-notebooks}}"
export LOCATION="${LOCATION:-us-central1-c}"
export REGION="${REGION:-us-central1}"
# Comma- or space-separated list of Google account emails to grant IAP & RBAC access
export PILOT_USERS="${PILOT_USERS:-${PILOT_USER:-}}"
export TENANT_NAMESPACE="${TENANT_NAMESPACE:-team-a}"
export REPO_NAME="${REPO_NAME:-${REPOSITORY:-notebooks}}"
export ADDRESS_NAME="${ADDRESS_NAME:-notebooks-gke-global}"
export CERTIFICATE_NAME="${CERTIFICATE_NAME:-notebooks-gke}"
export CERTIFICATE_MAP="${CERTIFICATE_MAP:-notebooks-gke}"
export CONTEXT="${CONTEXT:-gke_${PROJECT_ID}_${LOCATION}_${CLUSTER_NAME}}"
export REGISTRY="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}"
if [[ -z "${TAG:-}" ]]; then
  if [[ "${BUILD_IMAGES:-true}" == "false" ]]; then
    TAG=$(gcloud artifacts docker tags list "${REGISTRY}/gke-access-proxy" \
      --project="${PROJECT_ID}" --format='value(tag)' --limit=1 2>/dev/null | head -n1)
  fi
  export TAG="${TAG:-pilot-$(date -u +%Y%m%d%H%M%S)}"
fi

# Optional: Custom domain (e.g., "notebooks.example.com" or "workspaces.example.com").
# If unset or empty, defaults automatically to "notebooks.<GLOBAL_EXTERNAL_IP>.sslip.io" (zero DNS setup required).
export WORKSPACES_HOST="${WORKSPACES_HOST:-${NOTEBOOK_HOST:-}}"
export DESKTOP_HOST="${DESKTOP_HOST:-}"

# Optional: OAuth configuration
# Leave IAP_CLIENT_ID and IAP_SECRET_NAME empty for Google-managed OAuth (internal organization users).
# For custom OAuth (external users), set OAUTH_FILE=/path/to/oauth-client.json or set IAP_CLIENT_ID and IAP_SECRET_NAME.
export OAUTH_FILE="${OAUTH_FILE:-}"
export IAP_CLIENT_ID="${IAP_CLIENT_ID:-}"
export IAP_SECRET_NAME="${IAP_SECRET_NAME:-}"

# Optional: Deploy Kubeflow Trainer (v2) and Kubeflow Spark Operator
export INSTALL_TRAINER="${INSTALL_TRAINER:-true}"
export INSTALL_SPARK_OPERATOR="${INSTALL_SPARK_OPERATOR:-true}"

# Optional: Build standalone core images (access-proxy, snapshot-addon, frontend, controller, backend).
# Custom workspace images (JupyterLab / VS Code / Spark) are NOT built here; see ../../images/build.sh.
export BUILD_IMAGES="${BUILD_IMAGES:-true}"

# Optional: Register the upstream sample JupyterLab WorkspaceKind so users have
# something to launch immediately after the deployment finishes.
export APPLY_SAMPLE_WORKSPACEKIND="${APPLY_SAMPLE_WORKSPACEKIND:-true}"
export SAMPLE_WORKSPACEKIND="${SAMPLE_WORKSPACEKIND:-${REPO_ROOT}/workspaces/controller/manifests/kustomize/samples/jupyterlab_v1beta1_workspacekind.yaml}"

# Optional: Kubernetes client QPS & Burst for gke-access-proxy
export KUBE_CLIENT_QPS="${KUBE_CLIENT_QPS:-100}"
export KUBE_CLIENT_BURST="${KUBE_CLIENT_BURST:-200}"

# Dedicated GCS Bucket for GKE Pod Snapshots (stateful Workspace Pause & Resume)
export SNAPSHOT_GCS_BUCKET="${SNAPSHOT_GCS_BUCKET:-${TENANT_NAMESPACE}-snapshots-bucket}"

# Optional: Skip organization policy check for external load balancer types
export SKIP_ORG_POLICY_CHECK="${SKIP_ORG_POLICY_CHECK:-false}"

if [[ -z "${PROJECT_ID}" ]]; then
  echo "ERROR: PROJECT_ID is not set. Please run: export PROJECT_ID=your-gcp-project-id" >&2
  exit 1
fi

if [[ -z "${PILOT_USERS}" ]]; then
  echo "ERROR: PILOT_USERS is not set. Please run: export PILOT_USERS=\"user1@example.com,user2@example.com\"" >&2
  exit 1
fi

# Pre-flight check: verify that project allows external HTTP/HTTPS load balancers
if [[ "${SKIP_ORG_POLICY_CHECK}" != "true" ]]; then
  echo "Checking organization policy constraints on project '${PROJECT_ID}'..."
  EFFECTIVE_LB_POLICY=$(gcloud resource-manager org-policies describe compute.restrictLoadBalancerCreationForTypes \
    --project="${PROJECT_ID}" --effective --format=json 2>/dev/null || true)
  if [[ -n "${EFFECTIVE_LB_POLICY}" ]]; then
    ALL_VALUES=$(jq -r '.listPolicy.allValues // empty' <<< "${EFFECTIVE_LB_POLICY}")
    LB_ALLOWED=true
    if [[ "${ALL_VALUES}" == "DENY" ]]; then
      LB_ALLOWED=false
    elif [[ "${ALL_VALUES}" != "ALLOW" ]]; then
      if jq -e '.listPolicy.allowedValues' <<< "${EFFECTIVE_LB_POLICY}" >/dev/null 2>&1; then
        if ! jq -e '.listPolicy.allowedValues | index("GLOBAL_EXTERNAL_MANAGED_HTTP_HTTPS")' <<< "${EFFECTIVE_LB_POLICY}" >/dev/null 2>&1; then
          LB_ALLOWED=false
        fi
      fi
      if jq -e '.listPolicy.deniedValues' <<< "${EFFECTIVE_LB_POLICY}" >/dev/null 2>&1; then
        if jq -e '.listPolicy.deniedValues | index("GLOBAL_EXTERNAL_MANAGED_HTTP_HTTPS")' <<< "${EFFECTIVE_LB_POLICY}" >/dev/null 2>&1; then
          LB_ALLOWED=false
        fi
      fi
    fi

    if [[ "${LB_ALLOWED}" == "false" ]]; then
      echo "ERROR: Organization policy constraint 'constraints/compute.restrictLoadBalancerCreationForTypes' on project '${PROJECT_ID}' does not allow GLOBAL_EXTERNAL_MANAGED_HTTP_HTTPS." >&2
      echo "The GKE Gateway controller requires this load balancer type for the 'gke-l7-global-external-managed' GatewayClass." >&2
      echo "To fix this:" >&2
      echo "  1. Request an org policy exemption for project '${PROJECT_ID}' allowing GLOBAL_EXTERNAL_MANAGED_HTTP_HTTPS (e.g. via go/gcp-control-gclb or go/overground-quickstart#project-level)." >&2
      echo "  2. Or deploy into an already-exempted project/folder (e.g. projects under teams/gke/dev/dev_projects)." >&2
      echo "  (To bypass this check, re-run with: export SKIP_ORG_POLICY_CHECK=true)" >&2
      exit 1
    fi
  fi
fi

echo "=================================================================="
echo "Standalone Kubeflow Workspaces on GKE (No Istio) Deployment"
echo "=================================================================="
echo "  PROJECT_ID:          ${PROJECT_ID}"
echo "  CLUSTER_NAME:        ${CLUSTER_NAME} (${LOCATION})"
echo "  REGION:              ${REGION}"
echo "  PILOT_USERS:         ${PILOT_USERS}"
echo "  TENANT_NAMESPACE:    ${TENANT_NAMESPACE}"
echo "  REPO_NAME:           ${REPO_NAME}"
echo "  REGISTRY:            ${REGISTRY}"
echo "  SNAPSHOT_GCS_BUCKET: ${SNAPSHOT_GCS_BUCKET}"
echo "  BUILD_IMAGES:        ${BUILD_IMAGES}"
echo "  INSTALL_TRAINER:     ${INSTALL_TRAINER}"
echo "  INSTALL_SPARK:       ${INSTALL_SPARK_OPERATOR}"
echo "  SAMPLE_WORKSPACEKIND:${APPLY_SAMPLE_WORKSPACEKIND}"
echo "=================================================================="

# ==============================================================================
# Step 1: Enable GCP Platform APIs & GKE Gateway Controller
# ==============================================================================
echo "=================================================================="
echo "Step 1: Enabling GCP APIs & GKE Gateway API Controller..."
echo "=================================================================="
gcloud services enable \
  container.googleapis.com \
  compute.googleapis.com \
  artifactregistry.googleapis.com \
  certificatemanager.googleapis.com \
  iap.googleapis.com \
  --project="${PROJECT_ID}"

gcloud container clusters get-credentials "${CLUSTER_NAME}" \
  --location="${LOCATION}" \
  --project="${PROJECT_ID}"

if kubectl --context="${CONTEXT}" get gatewayclass/gke-l7-global-external-managed -o jsonpath='{.status.conditions[?(@.type=="Accepted")].status}' 2>/dev/null | grep -q "True"; then
  echo "GatewayClass 'gke-l7-global-external-managed' is already Accepted on cluster '${CLUSTER_NAME}'; skipping cluster update."
else
  echo "Enabling standard Gateway API on GKE cluster '${CLUSTER_NAME}'..."
  gcloud container clusters update "${CLUSTER_NAME}" \
    --location="${LOCATION}" \
    --project="${PROJECT_ID}" \
    --gateway-api=standard \
    --quiet

  echo "Waiting for GatewayClass 'gke-l7-global-external-managed' to become Accepted..."
  kubectl --context="${CONTEXT}" wait gatewayclass/gke-l7-global-external-managed \
    --for=condition=Accepted --timeout=10m
fi

kubectl --context="${CONTEXT}" get crd \
  gateways.gateway.networking.k8s.io \
  httproutes.gateway.networking.k8s.io \
  gcpbackendpolicies.networking.gke.io \
  healthcheckpolicies.networking.gke.io

# Ensure Artifact Registry repository exists and GKE nodes can pull images
if ! gcloud artifacts repositories describe "${REPO_NAME}" \
    --location="${REGION}" \
    --project="${PROJECT_ID}" >/dev/null 2>&1; then
  echo "Creating Artifact Registry repository '${REPO_NAME}' in ${REGION}..."
  gcloud artifacts repositories create "${REPO_NAME}" \
    --repository-format=docker \
    --location="${REGION}" \
    --project="${PROJECT_ID}"
fi

GKE_SA="$(gcloud iam service-accounts list --project="${PROJECT_ID}" --filter="displayName:Compute Engine default service account" --format='value(email)' 2>/dev/null | head -n1 || true)"
if [[ -n "${GKE_SA}" ]]; then
  gcloud artifacts repositories add-iam-policy-binding "${REPO_NAME}" \
    --location="${REGION}" \
    --project="${PROJECT_ID}" \
    --member="serviceAccount:${GKE_SA}" \
    --role="roles/artifactregistry.reader" >/dev/null 2>&1 || true
fi

# ==============================================================================
# Step 2: Install Cert-Manager (v1.21.2) for Internal Webhook TLS
# ==============================================================================
echo "=================================================================="
echo "Step 2: Installing Cert-Manager (v1.21.2)..."
echo "=================================================================="
if ! kubectl --context="${CONTEXT}" get namespace cert-manager >/dev/null 2>&1; then
  mkdir -p "${SCRIPT_DIR}/bin"
  curl -fsSL https://github.com/cert-manager/cert-manager/releases/download/v1.21.2/cert-manager.yaml \
    -o "${SCRIPT_DIR}/bin/cert-manager-v1.21.2.yaml"
  printf '%s  %s\n' e03b668ec8675214af6b0a671699d088f2601fa3878e0dbe1b41d3feafd1879f \
    "${SCRIPT_DIR}/bin/cert-manager-v1.21.2.yaml" | sha256sum --check
  kubectl --context="${CONTEXT}" apply --server-side \
    --field-manager=notebooks-gke-platform -f "${SCRIPT_DIR}/bin/cert-manager-v1.21.2.yaml"
else
  echo "cert-manager namespace already exists; skipping install."
fi

for component in cert-manager cert-manager-webhook cert-manager-cainjector; do
  kubectl --context="${CONTEXT}" -n cert-manager rollout status \
    deployment/"${component}" --timeout=5m
done

# ==============================================================================
# Step 3: Build & Push Application Images
# ==============================================================================
if [[ "${BUILD_IMAGES}" == "true" ]]; then
  echo "=================================================================="
  echo "Step 3: Building & Pushing Standalone Workspaces Core Images..."
  echo "=================================================================="
  gcloud auth configure-docker "${REGION}-docker.pkg.dev" --quiet
  docker build --platform=linux/amd64 -t "${REGISTRY}/gke-access-proxy:${TAG}" "${SCRIPT_DIR}"
  docker build --platform=linux/amd64 -f "${SCRIPT_DIR}/snapshot.Dockerfile" \
    -t "${REGISTRY}/gke-snapshot-addon:${TAG}" "${SCRIPT_DIR}"
  docker build --platform=linux/amd64 -f "${SCRIPT_DIR}/frontend.Dockerfile" \
    -t "${REGISTRY}/gke-frontend:${TAG}" "${REPO_ROOT}"
  docker build --platform=linux/amd64 -f "${REPO_ROOT}/workspaces/controller/Dockerfile" \
    -t "${REGISTRY}/gke-controller:${TAG}" "${REPO_ROOT}/workspaces/controller"
  docker build --platform=linux/amd64 -f "${REPO_ROOT}/workspaces/backend/Dockerfile" \
    -t "${REGISTRY}/gke-backend:${TAG}" "${REPO_ROOT}/workspaces"

  for component in access-proxy snapshot-addon frontend controller backend; do
    docker push "${REGISTRY}/gke-${component}:${TAG}"
  done
fi

# ==============================================================================
# Step 4: Reserve Global External IP, Configure Domain & Certificate Manager
# ==============================================================================
echo "=================================================================="
echo "Step 4: Configuring Global External IP, Domain & Certificate Manager..."
echo "=================================================================="
if ! gcloud compute addresses describe "${ADDRESS_NAME}" --global --project="${PROJECT_ID}" >/dev/null 2>&1; then
  gcloud compute addresses create "${ADDRESS_NAME}" --global --ip-version=IPV4 \
    --network-tier=PREMIUM --project="${PROJECT_ID}"
fi

export ADDRESS=$(gcloud compute addresses describe "${ADDRESS_NAME}" \
  --global --project="${PROJECT_ID}" --format='value(address)')
echo "Global External IP (${ADDRESS_NAME}): ${ADDRESS}"

if [[ -z "${WORKSPACES_HOST}" ]]; then
  export WORKSPACES_HOST="notebooks.${ADDRESS}.sslip.io"
  echo "No WORKSPACES_HOST specified. Automatically using sslip.io domain: ${WORKSPACES_HOST}"
else
  echo "Using custom WORKSPACES_HOST: ${WORKSPACES_HOST}"
  echo "Ensure your DNS A record maps ${WORKSPACES_HOST} -> ${ADDRESS}"
fi

if [[ -z "${DESKTOP_HOST}" ]]; then
  export DESKTOP_HOST="connect.${ADDRESS}.sslip.io"
  echo "No DESKTOP_HOST specified. Automatically using sslip.io domain: ${DESKTOP_HOST}"
else
  echo "Using custom DESKTOP_HOST: ${DESKTOP_HOST}"
  echo "Ensure your DNS A record maps ${DESKTOP_HOST} -> ${ADDRESS}"
fi

if ! gcloud certificate-manager maps describe "${CERTIFICATE_MAP}" --project="${PROJECT_ID}" >/dev/null 2>&1; then
  gcloud certificate-manager maps create "${CERTIFICATE_MAP}" --project="${PROJECT_ID}"
fi

# Helper function to ensure a Certificate Manager certificate exists and covers the target domain
ensure_certificate() {
  local cert_name="$1"
  local target_domain="$2"
  local out_var="$3"

  # Check if the desired certificate name already exists
  if gcloud certificate-manager certificates describe "${cert_name}" --project="${PROJECT_ID}" >/dev/null 2>&1; then
    local existing_domains
    existing_domains=$(gcloud certificate-manager certificates describe "${cert_name}" \
      --project="${PROJECT_ID}" --format='value(domains)' 2>/dev/null || true)
    if [[ "${existing_domains}" == *"${target_domain}"* ]]; then
      echo "Certificate '${cert_name}' covers '${target_domain}'."
      eval "${out_var}=\"${cert_name}\""
      return 0
    fi
    echo "Existing certificate '${cert_name}' covers '${existing_domains}', but target domain is '${target_domain}'."
  fi

  # Check if another certificate in the project already covers target_domain
  local alt_cert
  alt_cert=$(gcloud certificate-manager certificates list --project="${PROJECT_ID}" \
    --format='json' 2>/dev/null | jq -r --arg domain "${target_domain}" \
    '.[] | select(any(.domains[]?; . == $domain)) | .name' | awk -F'/' '{print $NF}' | head -n1 || true)
  if [[ -n "${alt_cert}" ]]; then
    echo "Found existing certificate '${alt_cert}' covering '${target_domain}'."
    eval "${out_var}=\"${alt_cert}\""
    return 0
  fi

  # If cert_name exists with the wrong domain, recreate it
  if gcloud certificate-manager certificates describe "${cert_name}" --project="${PROJECT_ID}" >/dev/null 2>&1; then
    echo "Recreating certificate '${cert_name}' for domain '${target_domain}'..."
    local entries_to_delete
    entries_to_delete=$(gcloud certificate-manager maps entries list --map="${CERTIFICATE_MAP}" \
      --project="${PROJECT_ID}" --format='json' 2>/dev/null | jq -r --arg cert "${cert_name}" \
      '.[] | select(any(.certificates[]?; endswith("/certificates/" + $cert))) | .name' | awk -F'/' '{print $NF}' || true)
    for entry in ${entries_to_delete}; do
      echo "Removing map entry '${entry}' referencing '${cert_name}' before certificate recreation..."
      gcloud certificate-manager maps entries delete "${entry}" \
        --map="${CERTIFICATE_MAP}" --project="${PROJECT_ID}" --quiet || true
    done
    gcloud certificate-manager certificates delete "${cert_name}" --project="${PROJECT_ID}" --quiet
  fi

  echo "Creating Certificate Manager certificate '${cert_name}' for '${target_domain}'..."
  gcloud certificate-manager certificates create "${cert_name}" \
    --domains="${target_domain}" --project="${PROJECT_ID}"
  eval "${out_var}=\"${cert_name}\""
}

# Helper function to ensure a Certificate Map entry exists for the target hostname
ensure_map_entry() {
  local entry_name="$1"
  local target_host="$2"
  local cert_name="$3"

  # Check if an entry for this exact hostname already exists in the map under any entry name
  local existing_entry_for_host
  existing_entry_for_host=$(gcloud certificate-manager maps entries list --map="${CERTIFICATE_MAP}" \
    --project="${PROJECT_ID}" --format='json' 2>/dev/null | jq -r --arg host "${target_host}" \
    '.[] | select(.hostname == $host) | .name' | awk -F'/' '{print $NF}' | head -n1 || true)

  if [[ -n "${existing_entry_for_host}" ]]; then
    local current_certs
    current_certs=$(gcloud certificate-manager maps entries describe "${existing_entry_for_host}" \
      --map="${CERTIFICATE_MAP}" --project="${PROJECT_ID}" --format='value(certificates)' 2>/dev/null || true)
    if [[ "${current_certs}" != *"${cert_name}"* ]]; then
      echo "Updating certificate on existing map entry '${existing_entry_for_host}' to '${cert_name}'..."
      gcloud certificate-manager maps entries update "${existing_entry_for_host}" \
        --map="${CERTIFICATE_MAP}" --certificates="${cert_name}" --project="${PROJECT_ID}"
    fi
    return 0
  fi

  # If entry_name exists but points to a different hostname, remove it first
  if gcloud certificate-manager maps entries describe "${entry_name}" --map="${CERTIFICATE_MAP}" --project="${PROJECT_ID}" >/dev/null 2>&1; then
    echo "Certificate map entry '${entry_name}' exists with different hostname. Recreating for '${target_host}'..."
    gcloud certificate-manager maps entries delete "${entry_name}" \
      --map="${CERTIFICATE_MAP}" --project="${PROJECT_ID}" --quiet
  fi

  echo "Creating Certificate Map entry '${entry_name}' for '${target_host}'..."
  gcloud certificate-manager maps entries create "${entry_name}" \
    --map="${CERTIFICATE_MAP}" --certificates="${cert_name}" \
    --hostname="${target_host}" --project="${PROJECT_ID}"
}

ACTUAL_WORKSPACES_CERT=""
ensure_certificate "${CERTIFICATE_NAME}" "${WORKSPACES_HOST}" ACTUAL_WORKSPACES_CERT

DESKTOP_CERTIFICATE="${CERTIFICATE_NAME}-desktop"
ACTUAL_DESKTOP_CERT=""
ensure_certificate "${DESKTOP_CERTIFICATE}" "${DESKTOP_HOST}" ACTUAL_DESKTOP_CERT

ensure_map_entry notebooks "${WORKSPACES_HOST}" "${ACTUAL_WORKSPACES_CERT}"
ensure_map_entry notebooks-desktop "${DESKTOP_HOST}" "${ACTUAL_DESKTOP_CERT}"

# Ensure a GKE-node-tagged firewall rule exists for Google Cloud Load Balancer
# health checks and Google Front Ends (35.191.0.0/16, 130.211.0.0/22).
# In Google-internal GCP projects, GCE Enforcer (gceenforcer-enforcer@system.gserviceaccount.com)
# deletes the untagged gkegw1-*-l7-default-global rule every 5 minutes because it
# lacks targetTags, causing periodic 503 failed_to_pick_backend errors in JupyterLab.
# A rule named gke-*-gclb-hc with --target-tags=<gke-node-tag> is exempted by GCE Enforcer.
FIRST_NODE=$(kubectl --context="${CONTEXT}" get nodes -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
if [[ -n "${FIRST_NODE}" ]]; then
  FIRST_NODE_ZONE=$(kubectl --context="${CONTEXT}" get node "${FIRST_NODE}" \
    -o jsonpath='{.metadata.labels.topology\.kubernetes\.io/zone}' 2>/dev/null || true)
  GKE_NODE_TAG=$(gcloud compute instances describe "${FIRST_NODE}" \
    --zone="${FIRST_NODE_ZONE}" --project="${PROJECT_ID}" \
    --format='value(tags.items)' 2>/dev/null | tr ';' '\n' | grep -E '^gke-.*-node$' | head -n1 || true)
  CLUSTER_NETWORK=$(gcloud container clusters describe "${CLUSTER_NAME}" \
    --location="${LOCATION}" --project="${PROJECT_ID}" \
    --format='value(network)' 2>/dev/null || echo "default")
  if [[ -n "${GKE_NODE_TAG}" ]]; then
    GCLB_FW_NAME="${GKE_NODE_TAG%-node}-gclb-hc"
    if ! gcloud compute firewall-rules describe "${GCLB_FW_NAME}" --project="${PROJECT_ID}" >/dev/null 2>&1; then
      echo "Creating GKE-node-tagged firewall rule '${GCLB_FW_NAME}' (target tag: ${GKE_NODE_TAG}) for GCLB health checks..."
      gcloud compute firewall-rules create "${GCLB_FW_NAME}" \
        --project="${PROJECT_ID}" \
        --network="${CLUSTER_NETWORK}" \
        --target-tags="${GKE_NODE_TAG}" \
        --allow=tcp:8080,tcp:8081 \
        --source-ranges=35.191.0.0/16,130.211.0.0/22 \
        --description='{"kubernetes.io/cluster-id":"'"${CLUSTER_NAME}"'","purpose":"allow-gclb-health-checks-and-gfe"}'
    else
      echo "GKE-node-tagged firewall rule '${GCLB_FW_NAME}' already exists."
    fi
  fi
fi

# ==============================================================================
# Step 5: Render & Apply Standalone Kubeflow Workspaces (Fail-Closed Bootstrap)
# ==============================================================================
echo "=================================================================="
echo "Step 5: Rendering & Applying Standalone Kubeflow Workspaces..."
echo "=================================================================="
if [[ -n "${OAUTH_FILE}" && -f "${OAUTH_FILE}" ]]; then
  export IAP_CLIENT_ID=$(jq -er '.web.client_id' "${OAUTH_FILE}")
  export IAP_SECRET_NAME="${IAP_SECRET_NAME:-iap-oauth}"
fi

CONTROL_PLANE_IP=$(gcloud container clusters describe "${CLUSTER_NAME}" \
  --location="${LOCATION}" --project="${PROJECT_ID}" \
  --format='value(privateClusterConfig.privateEndpoint,controlPlaneEndpointsConfig.ipEndpointsConfig.privateEndpoint)' | awk '{print $1}')
if [[ -z "${CONTROL_PLANE_IP}" ]]; then
  CONTROL_PLANE_IP=$(gcloud container clusters describe "${CLUSTER_NAME}" \
    --location="${LOCATION}" --project="${PROJECT_ID}" \
    --format='value(endpoint)')
fi
export CONTROL_PLANE_CIDR="${CONTROL_PLANE_CIDR:-${CONTROL_PLANE_IP}/32}"

PROXY_IMAGE=$(gcloud artifacts docker images describe "${REGISTRY}/gke-access-proxy:${TAG}" \
  --project="${PROJECT_ID}" --format='value(image_summary.fully_qualified_digest)')
SNAPSHOT_IMAGE=$(gcloud artifacts docker images describe "${REGISTRY}/gke-snapshot-addon:${TAG}" \
  --project="${PROJECT_ID}" --format='value(image_summary.fully_qualified_digest)')
FRONTEND_IMAGE=$(gcloud artifacts docker images describe "${REGISTRY}/gke-frontend:${TAG}" \
  --project="${PROJECT_ID}" --format='value(image_summary.fully_qualified_digest)')
CONTROLLER_IMAGE=$(gcloud artifacts docker images describe "${REGISTRY}/gke-controller:${TAG}" \
  --project="${PROJECT_ID}" --format='value(image_summary.fully_qualified_digest)')
BACKEND_IMAGE=$(gcloud artifacts docker images describe "${REGISTRY}/gke-backend:${TAG}" \
  --project="${PROJECT_ID}" --format='value(image_summary.fully_qualified_digest)')

jq -n \
  --arg cidr "${CONTROL_PLANE_CIDR}" \
  --arg host "${WORKSPACES_HOST}" \
  --arg desktopHost "${DESKTOP_HOST}" \
  --arg certificateMap "${CERTIFICATE_MAP}" \
  --arg addressName "${ADDRESS_NAME}" \
  --arg client "${IAP_CLIENT_ID}" \
  --arg secret "${IAP_SECRET_NAME}" \
  --arg tenant "${TENANT_NAMESPACE}" \
  --arg snapshotBucket "${SNAPSHOT_GCS_BUCKET}" \
  --argjson qps "${KUBE_CLIENT_QPS}" \
  --argjson burst "${KUBE_CLIENT_BURST}" \
  --arg proxy "${PROXY_IMAGE}" \
  --arg snapshot "${SNAPSHOT_IMAGE}" \
  --arg frontend "${FRONTEND_IMAGE}" \
  --arg controller "${CONTROLLER_IMAGE}" \
  --arg backend "${BACKEND_IMAGE}" \
  '{controlPlaneCIDR:$cidr,hostname:$host,desktopHostname:$desktopHost,certificateMap:$certificateMap,
    addressName:$addressName,iapClientID:$client,iapSecretName:$secret,
    iapAudience:"",kubeClientQPS:$qps,kubeClientBurst:$burst,snapshotGCSBucket:$snapshotBucket,tenants:[$tenant],
    images:{proxy:$proxy,snapshot:$snapshot,frontend:$frontend,controller:$controller,backend:$backend}}' \
  > "${SCRIPT_DIR}/deployment.local.json"

rm -rf "${SCRIPT_DIR}/rendered/bootstrap"
make -C "${SCRIPT_DIR}" plan CONFIG=deployment.local.json OUTPUT=rendered/bootstrap

kubectl --context="${CONTEXT}" apply --server-side --force-conflicts --field-manager=notebooks-gke \
  -f "${SCRIPT_DIR}/rendered/bootstrap/namespaces.json"
kubectl --context="${CONTEXT}" apply --server-side --force-conflicts --field-manager=notebooks-gke \
  -f "${SCRIPT_DIR}/rendered/bootstrap/isolation.json"

if [[ -n "${OAUTH_FILE}" && -f "${OAUTH_FILE}" && -n "${IAP_SECRET_NAME}" ]]; then
  if ! kubectl --context="${CONTEXT}" -n kubeflow-workspaces get secret "${IAP_SECRET_NAME}" >/dev/null 2>&1; then
    jq -jr '.web.client_secret' "${OAUTH_FILE}" | \
      kubectl --context="${CONTEXT}" -n kubeflow-workspaces create secret generic "${IAP_SECRET_NAME}" \
        --from-file=client_secret=/dev/stdin
  fi
fi

jq '{apiVersion,kind,items:[.items[]|select(.kind=="CustomResourceDefinition")]}' \
  "${SCRIPT_DIR}/rendered/bootstrap/applications.json" | \
  kubectl --context="${CONTEXT}" apply --server-side --force-conflicts --field-manager=notebooks-gke -f -

kubectl --context="${CONTEXT}" wait --for=condition=Established --timeout=2m \
  crd/workspaces.kubeflow.org crd/workspacekinds.kubeflow.org

kubectl --context="${CONTEXT}" apply --server-side --force-conflicts --field-manager=notebooks-gke \
  -f "${SCRIPT_DIR}/rendered/bootstrap/applications.json"
kubectl --context="${CONTEXT}" apply --server-side --force-conflicts --field-manager=notebooks-gke \
  -f "${SCRIPT_DIR}/rendered/bootstrap/edge.json"

# ==============================================================================
# Step 6: Discover IAP Backend Audience & Finalize Access Proxy
# ==============================================================================
echo "=================================================================="
echo "Step 6: Discovering GKE Backend Service Audience for IAP..."
echo "=================================================================="
NEG_NAME=""
for i in {1..60}; do
  NEG_NAME=$(kubectl --context="${CONTEXT}" -n kubeflow-workspaces get service gke-access-proxy \
    -o json 2>/dev/null | jq -er '.metadata.annotations["cloud.google.com/neg-status"] | fromjson | .network_endpoint_groups["8080"]' 2>/dev/null || true)
  if [[ -n "${NEG_NAME}" ]]; then
    break
  fi
  echo "Waiting for gke-access-proxy NEG annotation (${i}/60)..."
  sleep 5
done

MATCHED_BACKEND=""
for i in {1..60}; do
  MATCHED_BACKEND=$(gcloud compute backend-services list --global --project="${PROJECT_ID}" \
    --format='json(name,id,backends,iap.enabled)' | jq -ce --arg neg "${NEG_NAME}" \
    '[.[] | select(any(.backends[]?; .group | endswith("/networkEndpointGroups/"+$neg)))]
     | if length==1 then .[0] else empty end' 2>/dev/null || true)
  if [[ -n "${MATCHED_BACKEND}" ]]; then
    break
  fi
  echo "Waiting for GKE Gateway to attach global backend service for NEG ${NEG_NAME} (${i}/60)..."
  sleep 5
done

export BACKEND_SERVICE=$(jq -er '.name' <<< "${MATCHED_BACKEND}")
BACKEND_ID=$(jq -er '.id' <<< "${MATCHED_BACKEND}")
PROJECT_NUMBER=$(gcloud projects describe "${PROJECT_ID}" --format='value(projectNumber)')
export IAP_AUDIENCE="/projects/${PROJECT_NUMBER}/global/backendServices/${BACKEND_ID}"
echo "Discovered IAP Backend Service: ${BACKEND_SERVICE} (Audience: ${IAP_AUDIENCE})"

rm -rf "${SCRIPT_DIR}/rendered/ready"
jq --arg audience "${IAP_AUDIENCE}" '.iapAudience=$audience' \
  "${SCRIPT_DIR}/deployment.local.json" > "${SCRIPT_DIR}/rendered/deployment.ready.json"
make -C "${SCRIPT_DIR}" plan CONFIG=rendered/deployment.ready.json OUTPUT=rendered/ready

kubectl --context="${CONTEXT}" apply --server-side --force-conflicts --field-manager=notebooks-gke \
  -f "${SCRIPT_DIR}/rendered/ready/applications.json"
kubectl --context="${CONTEXT}" -n kubeflow-workspaces rollout restart deployment/gke-access-proxy

for component in workspaces-controller workspaces-backend workspaces-frontend gke-access-proxy gke-workspace-snapshot-addon; do
  kubectl --context="${CONTEXT}" -n kubeflow-workspaces rollout status deployment/"${component}" --timeout=5m
done

# ==============================================================================
# Step 7: Deploy Kubeflow Trainer (v2) & Kubeflow Spark Operator (No Istio)
# ==============================================================================
if [[ "${INSTALL_TRAINER}" == "true" || "${INSTALL_SPARK_OPERATOR}" == "true" ]]; then
  echo "=================================================================="
  echo "Step 7: Deploying Kubeflow Trainer (v2) & Spark Operator..."
  echo "=================================================================="
  if [[ ! -d "${DIST_DIR}" ]]; then
    git clone https://github.com/kubeflow/community-distribution.git "${DIST_DIR}"
  fi

  # Create kubeflow-system and kubeflow namespaces directly instead of applying
  # common/kubeflow-namespace/base (which contains Istio-specific labels and NetworkPolicies)
  kubectl --context="${CONTEXT}" create namespace kubeflow-system --dry-run=client -o yaml | kubectl --context="${CONTEXT}" apply -f -
  kubectl --context="${CONTEXT}" create namespace kubeflow --dry-run=client -o yaml | kubectl --context="${CONTEXT}" apply -f -
  kubectl --context="${CONTEXT}" apply -k "${DIST_DIR}/common/kubeflow-roles/base"

  if [[ "${INSTALL_TRAINER}" == "true" ]]; then
    echo "Deploying Kubeflow Trainer (v2) in kubeflow-system..."
    kubectl --context="${CONTEXT}" apply -k "${DIST_DIR}/applications/trainer/overlays" --server-side --force-conflicts || true
    kubectl --context="${CONTEXT}" wait --for=condition=Established crd/clustertrainingruntimes.trainer.kubeflow.org --timeout=60s
    kubectl --context="${CONTEXT}" wait --for=condition=Established crd/trainingruntimes.trainer.kubeflow.org --timeout=60s
    kubectl --context="${CONTEXT}" wait --for=condition=Established crd/trainjobs.trainer.kubeflow.org --timeout=60s
    kubectl --context="${CONTEXT}" apply -k "${DIST_DIR}/applications/trainer/overlays" --server-side --force-conflicts
    kubectl --context="${CONTEXT}" rollout status deployment/kubeflow-trainer-controller-manager -n kubeflow-system --timeout=180s
    kubectl --context="${CONTEXT}" rollout status deployment/jobset-controller-manager -n kubeflow-system --timeout=180s
  fi

  if [[ "${INSTALL_SPARK_OPERATOR}" == "true" ]]; then
    echo "Deploying Kubeflow Spark Operator in kubeflow..."
    kubectl --context="${CONTEXT}" apply -k "${DIST_DIR}/applications/spark/spark-operator/overlays/kubeflow" --server-side --force-conflicts
    kubectl --context="${CONTEXT}" wait --for=condition=Established crd/sparkapplications.sparkoperator.k8s.io --timeout=60s
    kubectl --context="${CONTEXT}" wait --for=condition=Established crd/scheduledsparkapplications.sparkoperator.k8s.io --timeout=60s
    kubectl --context="${CONTEXT}" wait --for=condition=Established crd/sparkconnects.sparkoperator.k8s.io --timeout=60s
    kubectl --context="${CONTEXT}" rollout status deployment/spark-operator-controller -n kubeflow --timeout=180s
    kubectl --context="${CONTEXT}" rollout status deployment/spark-operator-webhook -n kubeflow --timeout=180s
  fi
fi

# ==============================================================================
# Step 8: Admit Users via IAP & Apply Tenant RBAC + Sample WorkspaceKind
# ==============================================================================
echo "=================================================================="
echo "Step 8: Admitting Users via IAP & Configuring Tenant Workspace..."
echo "=================================================================="
for user_email in $(echo "${PILOT_USERS}" | tr ',' ' '); do
  if [[ -n "${user_email}" ]]; then
    echo "Granting IAP access to ${user_email}..."
    gcloud iap web add-iam-policy-binding --project="${PROJECT_ID}" \
      --resource-type=backend-services --service="${BACKEND_SERVICE}" \
      --member="user:${user_email}" --role=roles/iap.httpsResourceAccessor --condition=None
  fi
done

kubectl kustomize --load-restrictor=LoadRestrictionsNone "${SCRIPT_DIR}/manifests/pilot" | \
  python3 -c 'import yaml, json, sys; print(json.dumps({"apiVersion": "v1", "kind": "List", "items": [d for d in yaml.safe_load_all(sys.stdin) if d]}))' | \
  jq --arg users "${PILOT_USERS}" --arg ns "${TENANT_NAMESPACE}" \
    '([ $users | split(",")[] | split(" ")[] | select(length > 0) | {kind: "User", name: ., apiGroup: "rbac.authorization.k8s.io"} ]) as $user_subjects |
     {apiVersion:"v1",kind:"List",items:(.items | map(
      (if .kind=="Namespace" then .metadata.name=$ns else . end) |
      (if .metadata.namespace != null then .metadata.namespace=$ns else . end) |
      (if .kind=="ValidatingAdmissionPolicy" then .spec.matchConstraints.namespaceSelector.matchLabels["kubernetes.io/metadata.name"]=$ns else . end) |
      (if .kind=="RoleBinding" or .kind=="ClusterRoleBinding" then
        .subjects = (
          (.subjects | map(
            select(.kind != "User") |
            (if .namespace != null then .namespace=$ns else . end) |
            (if .kind=="Group" and (.name | startswith("system:serviceaccounts:")) then .name=("system:serviceaccounts:"+$ns) else . end)
          )) + $user_subjects
        )
      else . end)
    ))}' > "${SCRIPT_DIR}/rendered/ready/customer-pilot.json"

kubectl --context="${CONTEXT}" apply --server-side --field-manager=notebooks-gke-pilot \
  -f "${SCRIPT_DIR}/rendered/ready/customer-pilot.json"

# Register the upstream sample JupyterLab WorkspaceKind so that the tenant has a
# ready-to-use Workspace option as soon as the deployment finishes. It only uses
# public ghcr.io/kubeflow images, so no custom image build is required.
# To register the custom (JupyterLab / VS Code) WorkspaceKinds that back the
# examples, build the images with ../../images/build.sh and apply the templates
# in ../../images/workspacekinds/.
if [[ "${APPLY_SAMPLE_WORKSPACEKIND}" == "true" ]]; then
  if [[ -f "${SAMPLE_WORKSPACEKIND}" ]]; then
    echo "Registering sample WorkspaceKind from ${SAMPLE_WORKSPACEKIND}..."
    kubectl --context="${CONTEXT}" apply --server-side --force-conflicts \
      --field-manager=notebooks-gke-pilot -f "${SAMPLE_WORKSPACEKIND}"
  else
    echo "WARNING: SAMPLE_WORKSPACEKIND '${SAMPLE_WORKSPACEKIND}' not found; skipping." >&2
  fi
fi

# ==============================================================================
# Step 9: Configure Snapshot GCS Bucket & Workload Identity IAM Bindings
# ==============================================================================
echo "=================================================================="
echo "Step 9: Configuring Snapshot GCS Bucket & Workload Identity..."
echo "=================================================================="
if ! gcloud storage buckets describe "gs://${SNAPSHOT_GCS_BUCKET}" --project="${PROJECT_ID}" >/dev/null 2>&1; then
  echo "Creating GCS bucket gs://${SNAPSHOT_GCS_BUCKET}..."
  gcloud storage buckets create "gs://${SNAPSHOT_GCS_BUCKET}" --location="${REGION}" --project="${PROJECT_ID}" || true
fi
gcloud storage buckets add-iam-policy-binding "gs://${SNAPSHOT_GCS_BUCKET}" \
  --member="principalSet://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${PROJECT_ID}.svc.id.goog/namespace/${TENANT_NAMESPACE}" \
  --role="roles/storage.objectUser" >/dev/null || true
gcloud storage buckets add-iam-policy-binding "gs://${SNAPSHOT_GCS_BUCKET}" \
  --member="principalSet://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${PROJECT_ID}.svc.id.goog/namespace/${TENANT_NAMESPACE}" \
  --role="roles/storage.bucketViewer" >/dev/null || true

# Grant GKE Service Agent roles/storage.objectUser on SNAPSHOT_GCS_BUCKET so
# podsnapshot.gke.io/podsnapshot-finalizer can delete consumed/expired snapshot files in GCS
gcloud storage buckets add-iam-policy-binding "gs://${SNAPSHOT_GCS_BUCKET}" \
  --member="serviceAccount:service-${PROJECT_NUMBER}@container-engine-robot.iam.gserviceaccount.com" \
  --role="roles/storage.objectUser" >/dev/null || true

# Configure a GCS Object Lifecycle Delete rule (default 14 days) on SNAPSHOT_GCS_BUCKET
# as a hard billing backstop against orphaned snapshots
export SNAPSHOT_RETENTION_DAYS="${SNAPSHOT_RETENTION_DAYS:-14}"
cat <<EOF > /tmp/snapshot-lifecycle.json
{
  "rule": [
    {
      "action": {"type": "Delete"},
      "condition": {"age": ${SNAPSHOT_RETENTION_DAYS}}
    }
  ]
}
EOF
gcloud storage buckets update "gs://${SNAPSHOT_GCS_BUCKET}" \
  --lifecycle-file=/tmp/snapshot-lifecycle.json --project="${PROJECT_ID}" >/dev/null || true
rm -f /tmp/snapshot-lifecycle.json

echo "=================================================================="
echo "✅ Standalone Kubeflow Workspaces Deployment Complete!"
echo "=================================================================="
echo "  Public HTTPS URL:  https://${WORKSPACES_HOST}/workspaces/"
echo "  VS Code Tokens:    https://${WORKSPACES_HOST}/workspaces/connections"
echo "  Desktop Endpoint:  https://${DESKTOP_HOST}/"
echo "  Admitted Users:    ${PILOT_USERS}"
echo "  Tenant Namespace:  ${TENANT_NAMESPACE}"
echo "  Snapshot Bucket:   gs://${SNAPSHOT_GCS_BUCKET}"
echo ""
echo "Next steps:"
echo "  1. Open https://${WORKSPACES_HOST}/workspaces/ and create a Workspace from the"
echo "     'jupyterlab' or 'codeserver' WorkspaceKind in namespace '${TENANT_NAMESPACE}'."
echo "  2. To build custom workspace images, see ../../images/README.md."
echo "  3. To run the end-to-end examples, see ../../examples/README.md."
echo ""
echo "------------------------------------------------------------------"
echo "ℹ️  Initial Access & Certificate Provisioning Status:"
echo "------------------------------------------------------------------"
echo "Certificate Manager certificates '${ACTUAL_WORKSPACES_CERT}' and '${ACTUAL_DESKTOP_CERT}' use Load Balancer"
echo "authorization (ACME TLS-ALPN-01 on port 443). They begin issuance only after the GKE Gateway"
echo "becomes healthy, and typically take 5-15 minutes to reach ACTIVE state."
echo ""
echo "Current Certificate Status:"
gcloud certificate-manager certificates list --project="${PROJECT_ID}" \
  --filter="name:(${ACTUAL_WORKSPACES_CERT} OR ${ACTUAL_DESKTOP_CERT})" \
  --format="table(name,managed.state:label=STATE,managed.authorizationAttemptInfo[0].state:label=AUTHORIZATION,updateTime:label=UPDATED)" 2>/dev/null || true
echo ""
echo "Troubleshooting connection errors when accessing https://${WORKSPACES_HOST}/workspaces/:"
echo "  • ERR_CONNECTION_CLOSED (or 'unexpectedly closed the connection'):"
echo "    The certificate is still in PROVISIONING / AUTHORIZING state. Google Front End terminates"
echo "    the TLS handshake until the certificate becomes ACTIVE. Wait 5-15 minutes and monitor:"
echo "      gcloud certificate-manager certificates describe ${ACTUAL_WORKSPACES_CERT} --project=${PROJECT_ID}"
echo ""
echo "  • ERR_CONNECTION_RESET (or 'site can’t be reached'):"
echo "    The GKE Gateway has not finished provisioning the load balancer forwarding rule."
echo "    Check Gateway status and sync events for possible org policy or quota issues:"
echo "      kubectl describe gateway notebooks -n kubeflow-workspaces"
echo "=================================================================="
