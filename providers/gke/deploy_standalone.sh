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
export PROJECT="${PROJECT:-${PROJECT_ID:-$(gcloud config get-value project 2>/dev/null || true)}}"
export PROJECT_ID="${PROJECT}"
export CLUSTER="${CLUSTER:-kubeflow-notebooks}"
export LOCATION="${LOCATION:-us-central1-c}"
export REGION="${REGION:-us-central1}"
# Comma- or space-separated list of Google account emails to grant IAP & RBAC access
export PILOT_USERS="${PILOT_USERS:-${PILOT_USER:-}}"
export TENANT_NAMESPACE="${TENANT_NAMESPACE:-team-a}"
export REPOSITORY="${REPOSITORY:-notebooks}"
export ADDRESS_NAME="${ADDRESS_NAME:-notebooks-gke-global}"
export CERTIFICATE_NAME="${CERTIFICATE_NAME:-notebooks-gke}"
export CERTIFICATE_MAP="${CERTIFICATE_MAP:-notebooks-gke}"
export CONTEXT="${CONTEXT:-gke_${PROJECT}_${LOCATION}_${CLUSTER}}"
export REGISTRY="${REGISTRY:-${REGION}-docker.pkg.dev/${PROJECT}/${REPOSITORY}}"
if [[ -z "${TAG:-}" ]]; then
  if [[ "${BUILD_IMAGES:-true}" == "false" ]]; then
    TAG=$(gcloud artifacts docker tags list "${REGISTRY}/gke-access-proxy" \
      --project="${PROJECT}" --format='value(tag)' --limit=1 2>/dev/null | head -n1)
  fi
  export TAG="${TAG:-pilot-$(date -u +%Y%m%d%H%M%S)}"
fi

# Optional: Custom domain (e.g., "notebooks.example.com").
# If unset or empty, defaults automatically to "notebooks.<GLOBAL_EXTERNAL_IP>.sslip.io" (zero DNS setup required).
export NOTEBOOK_HOST="${NOTEBOOK_HOST:-}"
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

# Optional: Build standalone core images (access-proxy, frontend, controller, backend)
export BUILD_IMAGES="${BUILD_IMAGES:-true}"
# Optional: Build custom JupyterLab (CPU/GPU/TPU) and Spark images for distributed_tpu_example.ipynb
export BUILD_JUPYTERLAB_IMAGES="${BUILD_JUPYTERLAB_IMAGES:-true}"
# Optional: Build custom VS Code (codeserver-python) images (CPU/GPU/TPU)
export BUILD_CODESERVER_IMAGES="${BUILD_CODESERVER_IMAGES:-true}"

# Optional: Kubernetes client QPS & Burst for gke-access-proxy
export KUBE_CLIENT_QPS="${KUBE_CLIENT_QPS:-100}"
export KUBE_CLIENT_BURST="${KUBE_CLIENT_BURST:-200}"

# GCS Bucket for distributed_tpu_example.ipynb (Spark ETL & TPU Training data)
export GCS_BUCKET="${GCS_BUCKET:-${TENANT_NAMESPACE}-bucket}"
# Dedicated GCS Bucket for GKE Pod Snapshots (stateful Workspace Pause & Resume)
export SNAPSHOT_GCS_BUCKET="${SNAPSHOT_GCS_BUCKET:-${TENANT_NAMESPACE}-snapshots-bucket}"

if [[ -z "${PROJECT}" ]]; then
  echo "ERROR: PROJECT / PROJECT_ID is not set. Please run: export PROJECT=your-gcp-project-id" >&2
  exit 1
fi

if [[ -z "${PILOT_USERS}" ]]; then
  echo "ERROR: PILOT_USERS is not set. Please run: export PILOT_USERS=\"user1@example.com,user2@example.com\"" >&2
  exit 1
fi

echo "=================================================================="
echo "Standalone Kubeflow Workspaces on GKE (No Istio) Deployment"
echo "=================================================================="
echo "  PROJECT:             ${PROJECT}"
echo "  CLUSTER:             ${CLUSTER} (${LOCATION})"
echo "  REGION:              ${REGION}"
echo "  PILOT_USERS:         ${PILOT_USERS}"
echo "  TENANT_NAMESPACE:    ${TENANT_NAMESPACE}"
echo "  REGISTRY:            ${REGISTRY}"
echo "  GCS_BUCKET:          ${GCS_BUCKET}"
echo "  SNAPSHOT_GCS_BUCKET: ${SNAPSHOT_GCS_BUCKET}"
echo "  INSTALL_TRAINER:     ${INSTALL_TRAINER}"
echo "  INSTALL_SPARK:       ${INSTALL_SPARK_OPERATOR}"
echo "  BUILD_JUPYTERLAB:    ${BUILD_JUPYTERLAB_IMAGES}"
echo "  BUILD_CODESERVER:    ${BUILD_CODESERVER_IMAGES}"
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
  --project="${PROJECT}"

gcloud container clusters get-credentials "${CLUSTER}" \
  --location="${LOCATION}" \
  --project="${PROJECT}"

if kubectl --context="${CONTEXT}" get gatewayclass/gke-l7-global-external-managed -o jsonpath='{.status.conditions[?(@.type=="Accepted")].status}' 2>/dev/null | grep -q "True"; then
  echo "GatewayClass 'gke-l7-global-external-managed' is already Accepted on cluster '${CLUSTER}'; skipping cluster update."
else
  echo "Enabling standard Gateway API on GKE cluster '${CLUSTER}'..."
  gcloud container clusters update "${CLUSTER}" \
    --location="${LOCATION}" \
    --project="${PROJECT}" \
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

# Ensure Artifact Registry repository exists
if ! gcloud artifacts repositories describe "${REPOSITORY}" \
    --location="${REGION}" \
    --project="${PROJECT}" >/dev/null 2>&1; then
  echo "Creating Artifact Registry repository '${REPOSITORY}' in ${REGION}..."
  gcloud artifacts repositories create "${REPOSITORY}" \
    --repository-format=docker \
    --location="${REGION}" \
    --project="${PROJECT}"
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

if [[ "${BUILD_JUPYTERLAB_IMAGES}" == "true" ]]; then
  echo "=================================================================="
  echo "Step 3b: Checking Custom JupyterLab (CPU/GPU/TPU) & Spark Images..."
  echo "=================================================================="
  if [[ "${FORCE_BUILD_JUPYTERLAB_IMAGES:-false}" != "true" ]] && \
     gcloud artifacts docker images describe "${REGISTRY}/jupyterlab:latest-cpu" --project="${PROJECT}" >/dev/null 2>&1 && \
     gcloud artifacts docker images describe "${REGISTRY}/spark-py312:latest" --project="${PROJECT}" >/dev/null 2>&1; then
    echo "Custom JupyterLab and Spark images already exist in ${REGISTRY}; skipping rebuild."
  else
    PROJECT_ID="${PROJECT}" REGION="${REGION}" REPO_NAME="${REPOSITORY}" \
      TENANT_NAMESPACE="${TENANT_NAMESPACE}" GCS_BUCKET="${GCS_BUCKET}" \
      bash "${SCRIPT_DIR}/build_jupyterlab.sh"
  fi
fi

if [[ "${BUILD_CODESERVER_IMAGES}" == "true" ]]; then
  echo "=================================================================="
  echo "Step 3c: Checking Custom VS Code (codeserver-python) Images..."
  echo "=================================================================="
  if [[ "${FORCE_BUILD_CODESERVER_IMAGES:-false}" != "true" ]] && \
     gcloud artifacts docker images describe "${REGISTRY}/codeserver-python:latest-cpu" --project="${PROJECT}" >/dev/null 2>&1; then
    echo "Custom codeserver-python images already exist in ${REGISTRY}; skipping rebuild."
  else
    PROJECT_ID="${PROJECT}" REGION="${REGION}" REPO_NAME="${REPOSITORY}" \
      TENANT_NAMESPACE="${TENANT_NAMESPACE}" GCS_BUCKET="${GCS_BUCKET}" \
      bash "${SCRIPT_DIR}/build_codeserver_python.sh" --variant "${CODESERVER_VARIANT:-cpu}" --fast
  fi
fi

# ==============================================================================
# Step 4: Reserve Global External IP, Configure Domain & Certificate Manager
# ==============================================================================
echo "=================================================================="
echo "Step 4: Configuring Global External IP, Domain & Certificate Manager..."
echo "=================================================================="
if ! gcloud compute addresses describe "${ADDRESS_NAME}" --global --project="${PROJECT}" >/dev/null 2>&1; then
  gcloud compute addresses create "${ADDRESS_NAME}" --global --ip-version=IPV4 \
    --network-tier=PREMIUM --project="${PROJECT}"
fi

export ADDRESS=$(gcloud compute addresses describe "${ADDRESS_NAME}" \
  --global --project="${PROJECT}" --format='value(address)')
echo "Global External IP (${ADDRESS_NAME}): ${ADDRESS}"

if [[ -z "${NOTEBOOK_HOST}" ]]; then
  export NOTEBOOK_HOST="notebooks.${ADDRESS}.sslip.io"
  echo "No NOTEBOOK_HOST specified. Automatically using sslip.io domain: ${NOTEBOOK_HOST}"
else
  echo "Using custom NOTEBOOK_HOST: ${NOTEBOOK_HOST}"
  echo "Ensure your DNS A record maps ${NOTEBOOK_HOST} -> ${ADDRESS}"
fi

if [[ -z "${DESKTOP_HOST}" ]]; then
  export DESKTOP_HOST="connect.${ADDRESS}.sslip.io"
  echo "No DESKTOP_HOST specified. Automatically using sslip.io domain: ${DESKTOP_HOST}"
else
  echo "Using custom DESKTOP_HOST: ${DESKTOP_HOST}"
  echo "Ensure your DNS A record maps ${DESKTOP_HOST} -> ${ADDRESS}"
fi

if ! gcloud certificate-manager certificates describe "${CERTIFICATE_NAME}" --project="${PROJECT}" >/dev/null 2>&1; then
  gcloud certificate-manager certificates create "${CERTIFICATE_NAME}" \
    --domains="${NOTEBOOK_HOST}" --project="${PROJECT}"
fi

DESKTOP_CERTIFICATE="${CERTIFICATE_NAME}-desktop"
if ! gcloud certificate-manager certificates describe "${DESKTOP_CERTIFICATE}" --project="${PROJECT}" >/dev/null 2>&1; then
  gcloud certificate-manager certificates create "${DESKTOP_CERTIFICATE}" \
    --domains="${DESKTOP_HOST}" --project="${PROJECT}"
fi

if ! gcloud certificate-manager maps describe "${CERTIFICATE_MAP}" --project="${PROJECT}" >/dev/null 2>&1; then
  gcloud certificate-manager maps create "${CERTIFICATE_MAP}" --project="${PROJECT}"
fi

if ! gcloud certificate-manager maps entries describe notebooks --map="${CERTIFICATE_MAP}" --project="${PROJECT}" >/dev/null 2>&1; then
  gcloud certificate-manager maps entries create notebooks \
    --map="${CERTIFICATE_MAP}" --certificates="${CERTIFICATE_NAME}" \
    --hostname="${NOTEBOOK_HOST}" --project="${PROJECT}"
fi

if ! gcloud certificate-manager maps entries describe notebooks-desktop --map="${CERTIFICATE_MAP}" --project="${PROJECT}" >/dev/null 2>&1; then
  gcloud certificate-manager maps entries create notebooks-desktop \
    --map="${CERTIFICATE_MAP}" --certificates="${DESKTOP_CERTIFICATE}" \
    --hostname="${DESKTOP_HOST}" --project="${PROJECT}"
fi

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
    --zone="${FIRST_NODE_ZONE}" --project="${PROJECT}" \
    --format='value(tags.items)' 2>/dev/null | tr ';' '\n' | grep -E '^gke-.*-node$' | head -n1 || true)
  CLUSTER_NETWORK=$(gcloud container clusters describe "${CLUSTER}" \
    --location="${LOCATION}" --project="${PROJECT}" \
    --format='value(network)' 2>/dev/null || echo "default")
  if [[ -n "${GKE_NODE_TAG}" ]]; then
    GCLB_FW_NAME="${GKE_NODE_TAG%-node}-gclb-hc"
    if ! gcloud compute firewall-rules describe "${GCLB_FW_NAME}" --project="${PROJECT}" >/dev/null 2>&1; then
      echo "Creating GKE-node-tagged firewall rule '${GCLB_FW_NAME}' (target tag: ${GKE_NODE_TAG}) for GCLB health checks..."
      gcloud compute firewall-rules create "${GCLB_FW_NAME}" \
        --project="${PROJECT}" \
        --network="${CLUSTER_NETWORK}" \
        --target-tags="${GKE_NODE_TAG}" \
        --allow=tcp:8080,tcp:8081 \
        --source-ranges=35.191.0.0/16,130.211.0.0/22 \
        --description='{"kubernetes.io/cluster-id":"'"${CLUSTER}"'","purpose":"allow-gclb-health-checks-and-gfe"}'
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

CONTROL_PLANE_IP=$(gcloud container clusters describe "${CLUSTER}" \
  --location="${LOCATION}" --project="${PROJECT}" \
  --format='value(privateClusterConfig.privateEndpoint,controlPlaneEndpointsConfig.ipEndpointsConfig.privateEndpoint)' | awk '{print $1}')
if [[ -z "${CONTROL_PLANE_IP}" ]]; then
  CONTROL_PLANE_IP=$(gcloud container clusters describe "${CLUSTER}" \
    --location="${LOCATION}" --project="${PROJECT}" \
    --format='value(endpoint)')
fi
export CONTROL_PLANE_CIDR="${CONTROL_PLANE_CIDR:-${CONTROL_PLANE_IP}/32}"

PROXY_IMAGE=$(gcloud artifacts docker images describe "${REGISTRY}/gke-access-proxy:${TAG}" \
  --project="${PROJECT}" --format='value(image_summary.fully_qualified_digest)')
SNAPSHOT_IMAGE=$(gcloud artifacts docker images describe "${REGISTRY}/gke-snapshot-addon:${TAG}" \
  --project="${PROJECT}" --format='value(image_summary.fully_qualified_digest)')
FRONTEND_IMAGE=$(gcloud artifacts docker images describe "${REGISTRY}/gke-frontend:${TAG}" \
  --project="${PROJECT}" --format='value(image_summary.fully_qualified_digest)')
CONTROLLER_IMAGE=$(gcloud artifacts docker images describe "${REGISTRY}/gke-controller:${TAG}" \
  --project="${PROJECT}" --format='value(image_summary.fully_qualified_digest)')
BACKEND_IMAGE=$(gcloud artifacts docker images describe "${REGISTRY}/gke-backend:${TAG}" \
  --project="${PROJECT}" --format='value(image_summary.fully_qualified_digest)')

jq -n \
  --arg cidr "${CONTROL_PLANE_CIDR}" \
  --arg host "${NOTEBOOK_HOST}" \
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
  MATCHED_BACKEND=$(gcloud compute backend-services list --global --project="${PROJECT}" \
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
PROJECT_NUMBER=$(gcloud projects describe "${PROJECT}" --format='value(projectNumber)')
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
# Step 8: Admit Users via IAP & Apply Tenant RBAC, WorkspaceKind, ComputeClasses
# ==============================================================================
echo "=================================================================="
echo "Step 8: Admitting Users via IAP & Configuring Tenant Workspace..."
echo "=================================================================="
for user_email in $(echo "${PILOT_USERS}" | tr ',' ' '); do
  if [[ -n "${user_email}" ]]; then
    echo "Granting IAP access to ${user_email}..."
    gcloud iap web add-iam-policy-binding --project="${PROJECT}" \
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

# Helper: Resolve the latest image tag for a variant from Artifact Registry,
# falling back to any existing rendered tags file, and finally to latest-${variant}.
resolve_latest_tag() {
  local image="$1"
  local variant="$2"
  local env_file="${3:-}"
  local default_tag="latest-${variant}"

  local tag=""
  tag=$(gcloud artifacts docker tags list "${REGISTRY}/${image}" \
    --project="${PROJECT}" --format="value(tag)" 2>/dev/null | grep -E "^v[0-9]{8}-[0-9]{6}-${variant}$" | sort -V | tail -n 1 || true)

  if [[ -n "${tag}" ]]; then
    echo "${tag}"
    return
  fi

  if [[ -n "${env_file}" && -f "${env_file}" ]]; then
    local var_key="$(echo "${variant}" | tr '[:lower:]' '[:upper:]')_IMAGE_TAG"
    local saved_tag
    saved_tag=$(grep -E "^${var_key}=" "${env_file}" 2>/dev/null | head -n1 | cut -d'"' -f2 || true)
    if [[ -n "${saved_tag}" ]]; then
      echo "${saved_tag}"
      return
    fi
  fi

  echo "${default_tag}"
}

if [[ -f "${SCRIPT_DIR}/jupyterlab/workspacekind.yaml" ]]; then
  echo "Registering WorkspaceKind 'jupyterlab' for distributed_tpu_example.ipynb..."
  JL_TAGS_FILE="${SCRIPT_DIR}/rendered/jupyterlab-image-tags.env"
  CPU_IMAGE_TAG="${CPU_IMAGE_TAG:-$(resolve_latest_tag "jupyterlab" "cpu" "${JL_TAGS_FILE}")}"
  GPU_IMAGE_TAG="${GPU_IMAGE_TAG:-$(resolve_latest_tag "jupyterlab" "gpu" "${JL_TAGS_FILE}")}"
  TPU_IMAGE_TAG="${TPU_IMAGE_TAG:-$(resolve_latest_tag "jupyterlab" "tpu" "${JL_TAGS_FILE}")}"

  mkdir -p "$(dirname "${JL_TAGS_FILE}")"
  cat > "${JL_TAGS_FILE}" <<EOF
# Updated by deploy_standalone.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ).
CPU_IMAGE_TAG="${CPU_IMAGE_TAG}"
GPU_IMAGE_TAG="${GPU_IMAGE_TAG}"
TPU_IMAGE_TAG="${TPU_IMAGE_TAG}"
EOF

  echo "  pinning cpu=${CPU_IMAGE_TAG} gpu=${GPU_IMAGE_TAG} tpu=${TPU_IMAGE_TAG}"
  PROJECT_ID="${PROJECT}" REGION="${REGION}" REPO_NAME="${REPOSITORY}" \
    IMAGE_NAME="jupyterlab" GCS_BUCKET="${GCS_BUCKET}" \
    CPU_IMAGE_TAG="${CPU_IMAGE_TAG}" GPU_IMAGE_TAG="${GPU_IMAGE_TAG}" TPU_IMAGE_TAG="${TPU_IMAGE_TAG}" \
    envsubst < "${SCRIPT_DIR}/jupyterlab/workspacekind.yaml" | kubectl --context="${CONTEXT}" apply -f -

  if [[ -f "${SCRIPT_DIR}/jupyterlab/workspacekind-resumable.yaml" ]]; then
    echo "Registering WorkspaceKind 'jupyterlab-resumable' (resumable CPU & GPU)..."
    PROJECT_ID="${PROJECT}" REGION="${REGION}" REPO_NAME="${REPOSITORY}" \
      IMAGE_NAME="jupyterlab" GCS_BUCKET="${GCS_BUCKET}" \
      CPU_IMAGE_TAG="${CPU_IMAGE_TAG}" GPU_IMAGE_TAG="${GPU_IMAGE_TAG}" TPU_IMAGE_TAG="${TPU_IMAGE_TAG}" \
      envsubst < "${SCRIPT_DIR}/jupyterlab/workspacekind-resumable.yaml" | kubectl --context="${CONTEXT}" apply -f -
  fi
fi

if [[ -f "${SCRIPT_DIR}/codeserver-python/workspacekind.yaml" ]]; then
  echo "Registering WorkspaceKind 'codeserver'..."
  CS_TAGS_FILE="${SCRIPT_DIR}/rendered/codeserver-image-tags.env"
  CS_CPU_IMAGE_TAG="${CS_CPU_IMAGE_TAG:-$(resolve_latest_tag "codeserver-python" "cpu" "${CS_TAGS_FILE}")}"
  CS_GPU_IMAGE_TAG="${CS_GPU_IMAGE_TAG:-$(resolve_latest_tag "codeserver-python" "gpu" "${CS_TAGS_FILE}")}"
  CS_TPU_IMAGE_TAG="${CS_TPU_IMAGE_TAG:-$(resolve_latest_tag "codeserver-python" "tpu" "${CS_TAGS_FILE}")}"

  mkdir -p "$(dirname "${CS_TAGS_FILE}")"
  cat > "${CS_TAGS_FILE}" <<EOF
# Updated by deploy_standalone.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ).
CPU_IMAGE_TAG="${CS_CPU_IMAGE_TAG}"
GPU_IMAGE_TAG="${CS_GPU_IMAGE_TAG}"
TPU_IMAGE_TAG="${CS_TPU_IMAGE_TAG}"
EOF

  echo "  pinning cpu=${CS_CPU_IMAGE_TAG} gpu=${CS_GPU_IMAGE_TAG} tpu=${CS_TPU_IMAGE_TAG}"
  PROJECT_ID="${PROJECT}" REGION="${REGION}" REPO_NAME="${REPOSITORY}" \
    IMAGE_NAME="codeserver-python" GCS_BUCKET="${GCS_BUCKET}" \
    CPU_IMAGE_TAG="${CS_CPU_IMAGE_TAG}" GPU_IMAGE_TAG="${CS_GPU_IMAGE_TAG}" TPU_IMAGE_TAG="${CS_TPU_IMAGE_TAG}" \
    envsubst < "${SCRIPT_DIR}/codeserver-python/workspacekind.yaml" | kubectl --context="${CONTEXT}" apply -f -
fi

if [[ -d "${SCRIPT_DIR}/manifests/compute-classes" ]]; then
  echo "Applying GKE ComputeClass manifests (GPU and TPU)..."
  kubectl --context="${CONTEXT}" apply -f "${SCRIPT_DIR}/manifests/compute-classes/"
fi

# ==============================================================================
# Step 9: Configure Data & Snapshot GCS Buckets & Workload Identity IAM Bindings
# ==============================================================================
echo "=================================================================="
echo "Step 9: Configuring Data & Snapshot GCS Buckets & Workload Identity..."
echo "=================================================================="
for bucket in "${GCS_BUCKET}" "${SNAPSHOT_GCS_BUCKET}"; do
  if ! gcloud storage buckets describe "gs://${bucket}" --project="${PROJECT}" >/dev/null 2>&1; then
    echo "Creating GCS bucket gs://${bucket}..."
    gcloud storage buckets create "gs://${bucket}" --location="${REGION}" --project="${PROJECT}" || true
  fi
  gcloud storage buckets add-iam-policy-binding "gs://${bucket}" \
    --member="principalSet://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${PROJECT}.svc.id.goog/namespace/${TENANT_NAMESPACE}" \
    --role="roles/storage.objectUser" >/dev/null || true
  gcloud storage buckets add-iam-policy-binding "gs://${bucket}" \
    --member="principalSet://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${PROJECT}.svc.id.goog/namespace/${TENANT_NAMESPACE}" \
    --role="roles/storage.bucketViewer" >/dev/null || true
done

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
  --lifecycle-file=/tmp/snapshot-lifecycle.json --project="${PROJECT}" >/dev/null || true
rm -f /tmp/snapshot-lifecycle.json

echo "=================================================================="
echo "✅ Standalone Kubeflow Workspaces Deployment Complete!"
echo "=================================================================="
echo "  Public HTTPS URL:  https://${NOTEBOOK_HOST}/workspaces/"
echo "  VS Code Tokens:    https://${NOTEBOOK_HOST}/workspaces/connections"
echo "  Desktop Endpoint:  https://${DESKTOP_HOST}/"
echo "  Admitted Users:    ${PILOT_USERS}"
echo "  Tenant Namespace:  ${TENANT_NAMESPACE}"
echo "  Data GCS Bucket:   gs://${GCS_BUCKET}"
echo "  Snapshot Bucket:   gs://${SNAPSHOT_GCS_BUCKET}"
echo ""
echo "Note: Certificate Manager certificates '${CERTIFICATE_NAME}' and '${CERTIFICATE_NAME}-desktop' use Load Balancer"
echo "authorization and may take 5-15 minutes after Gateway attachment to reach ACTIVE state."
echo "Check certificate status with:"
echo "  gcloud certificate-manager certificates list --project=${PROJECT}"
echo "=================================================================="
