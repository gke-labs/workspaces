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
# Add Additional Users or Google Groups to Standalone Kubeflow Workspaces on GKE
#
# Enrolls new users into Identity-Aware Proxy (IAP) and grants Kubernetes RBAC
# in the tenant namespace, based on Section 9 of providers/gke/USER_GUIDE.md.
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
  cat <<EOF
Usage:
  $(basename "$0") [OPTIONS] <user1@example.com> [user2@example.com ...]

Description:
  Enrolls additional Google accounts or Google Groups into Standalone Kubeflow
  Workspaces on GKE by:
    1. Granting roles/iap.httpsResourceAccessor on the IAP backend service.
    2. Granting Kubernetes RBAC in the tenant namespace (RoleBinding notebooks-gke-pilot)
       and cluster discovery RBAC (ClusterRoleBinding notebooks-gke-pilot-discovery).

Arguments:
  <user-email ...>          One or more Google account emails or groups to enroll.
                            Format: "user@example.com", "user:user@example.com", or "group:devs@example.com".

Options:
  -n, --namespace <name>    Tenant namespace (default: \$TENANT_NAMESPACE or "kubeflow-user")
  -p, --project <id>        Google Cloud project ID (default: \$PROJECT_ID or current gcloud project)
  -c, --cluster <name>      GKE cluster name (default: \$CLUSTER_NAME or "kubeflow-notebooks")
  -l, --location <loc>      GKE cluster location/zone (default: \$LOCATION or "us-central1-c")
  -s, --service <name>      IAP backend service name (default: auto-discovered from GKE proxy NEG)
      --context <ctx>       kubectl context (default: gke_\${PROJECT_ID}_\${LOCATION}_\${CLUSTER_NAME})
      --skip-iap            Skip IAP policy binding (only apply Kubernetes RBAC)
      --skip-rbac           Skip Kubernetes RBAC patching (only apply IAP policy binding)
  -h, --help                Show this help message

Environment Variables:
  Users can also be passed via NEW_USERS, ADDITIONAL_USERS, or PILOT_USERS
  (comma- or space-separated):
    export NEW_USERS="alice@example.com,bob@example.com"
    $(basename "$0")

Examples:
  $(basename "$0") colleague@example.com
  $(basename "$0") user1@example.com user2@example.com group:ml-team@example.com
  $(basename "$0") -n team-b -p my-gcp-project user@example.com
EOF
  exit 0
}

# Defaults from environment or standard values
PROJECT_ID="${PROJECT_ID:-${PROJECT:-$(gcloud config get-value project 2>/dev/null || true)}}"
CLUSTER_NAME="${CLUSTER_NAME:-${CLUSTER:-kubeflow-notebooks}}"
LOCATION="${LOCATION:-us-central1-c}"
TENANT_NAMESPACE="${TENANT_NAMESPACE:-kubeflow-user}"
BACKEND_SERVICE="${BACKEND_SERVICE:-}"
CONTEXT="${CONTEXT:-}"
SKIP_IAP="false"
SKIP_RBAC="false"

CLI_USERS=()

while [[ $# -gt 0 ]]; do
  case "$1" in
    -n|--namespace)
      TENANT_NAMESPACE="$2"
      shift 2
      ;;
    -p|--project)
      PROJECT_ID="$2"
      shift 2
      ;;
    -c|--cluster)
      CLUSTER_NAME="$2"
      shift 2
      ;;
    -l|--location)
      LOCATION="$2"
      shift 2
      ;;
    -s|--service)
      BACKEND_SERVICE="$2"
      shift 2
      ;;
    --context)
      CONTEXT="$2"
      shift 2
      ;;
    --skip-iap)
      SKIP_IAP="true"
      shift
      ;;
    --skip-rbac)
      SKIP_RBAC="true"
      shift
      ;;
    -h|--help)
      usage
      ;;
    -*)
      echo "ERROR: Unknown option '$1'" >&2
      echo "Run '$(basename "$0") --help' for usage." >&2
      exit 1
      ;;
    *)
      CLI_USERS+=("$1")
      shift
      ;;
  esac
done

# Collect users from arguments and environment variables
RAW_INPUT_USERS=()
if [[ ${#CLI_USERS[@]} -gt 0 ]]; then
  RAW_INPUT_USERS+=("${CLI_USERS[@]}")
fi

ENV_USERS="${NEW_USERS:-${ADDITIONAL_USERS:-${PILOT_USERS:-${NEW_USER:-}}}}"
if [[ -n "${ENV_USERS}" ]]; then
  # Split on commas and spaces
  for u in $(echo "${ENV_USERS}" | tr ',' ' '); do
    if [[ -n "${u}" ]]; then
      RAW_INPUT_USERS+=("${u}")
    fi
  done
fi

if [[ ${#RAW_INPUT_USERS[@]} -eq 0 ]]; then
  echo "ERROR: No user email(s) specified." >&2
  echo "Pass user email(s) as command-line arguments or export NEW_USERS=\"user1@example.com,user2@example.com\"." >&2
  echo "Run '$(basename "$0") --help' for details." >&2
  exit 1
fi

# Preflight checks
for cmd in gcloud kubectl jq; do
  if ! command -v "${cmd}" >/dev/null 2>&1; then
    echo "ERROR: Missing required command '${cmd}'. Please install it before running this script." >&2
    exit 1
  fi
done

if [[ -z "${PROJECT_ID}" ]]; then
  echo "ERROR: GCP project ID is not set. Set with --project <id> or export PROJECT_ID=<id>." >&2
  exit 1
fi

if [[ -z "${CONTEXT}" ]]; then
  CONTEXT="gke_${PROJECT_ID}_${LOCATION}_${CLUSTER_NAME}"
fi

# Verify cluster connectivity
if ! kubectl --context="${CONTEXT}" cluster-info >/dev/null 2>&1; then
  echo "ERROR: Cannot connect to cluster with kubectl context '${CONTEXT}'." >&2
  echo "Ensure credentials are configured using: gcloud container clusters get-credentials ${CLUSTER_NAME} --location=${LOCATION} --project=${PROJECT_ID}" >&2
  exit 1
fi

# Verify tenant namespace exists
if ! kubectl --context="${CONTEXT}" get namespace "${TENANT_NAMESPACE}" >/dev/null 2>&1; then
  echo "ERROR: Tenant namespace '${TENANT_NAMESPACE}' does not exist in cluster." >&2
  exit 1
fi

# Auto-discover IAP backend service if needed
if [[ "${SKIP_IAP}" != "true" && -z "${BACKEND_SERVICE}" ]]; then
  echo "Discovering GKE backend service for Identity-Aware Proxy (IAP)..."
  NEG_NAME=$(kubectl --context="${CONTEXT}" -n kubeflow-workspaces get service gke-access-proxy \
    -o json 2>/dev/null | jq -er '.metadata.annotations["cloud.google.com/neg-status"] | fromjson | .network_endpoint_groups["8080"]' 2>/dev/null || true)

  if [[ -n "${NEG_NAME}" ]]; then
    BACKEND_SERVICE=$(gcloud compute backend-services list --global --project="${PROJECT_ID}" \
      --format='json(name,backends)' 2>/dev/null | jq -er --arg neg "${NEG_NAME}" \
      '[.[] | select(any(.backends[]?; .group | endswith("/networkEndpointGroups/"+$neg)))][0].name' 2>/dev/null || true)
  fi

  if [[ -z "${BACKEND_SERVICE}" ]]; then
    echo "ERROR: Could not auto-discover backend service for gke-access-proxy." >&2
    echo "Please specify the backend service name with --service=<name> or export BACKEND_SERVICE=<name>." >&2
    exit 1
  fi
fi

# Deduplicate input users
UNIQUE_USERS=($(printf "%s\n" "${RAW_INPUT_USERS[@]}" | sort -u))

echo "=================================================================="
echo "Enrolling Additional Users into Kubeflow Workspaces on GKE"
echo "=================================================================="
echo "  Project ID:       ${PROJECT_ID}"
echo "  Cluster:          ${CLUSTER_NAME} (${LOCATION})"
echo "  Context:          ${CONTEXT}"
echo "  Tenant Namespace: ${TENANT_NAMESPACE}"
if [[ "${SKIP_IAP}" != "true" ]]; then
  echo "  IAP Backend:      ${BACKEND_SERVICE}"
fi
echo "  Users/Groups:     ${UNIQUE_USERS[*]}"
echo "=================================================================="

# Process each user
for entry in "${UNIQUE_USERS[@]}"; do
  # Determine member format and Kubernetes RBAC kind/name
  if [[ "${entry}" =~ ^group:(.*)$ ]]; then
    MEMBER="${entry}"
    K8S_KIND="Group"
    K8S_NAME="${BASH_REMATCH[1]}"
  elif [[ "${entry}" =~ ^user:(.*)$ ]]; then
    MEMBER="${entry}"
    K8S_KIND="User"
    K8S_NAME="${BASH_REMATCH[1]}"
  else
    MEMBER="user:${entry}"
    K8S_KIND="User"
    K8S_NAME="${entry}"
  fi

  echo "------------------------------------------------------------------"
  echo "Processing: ${MEMBER} (K8s ${K8S_KIND}: ${K8S_NAME})"

  # 1. Grant IAP Access
  if [[ "${SKIP_IAP}" != "true" ]]; then
    echo "  [1/2] Granting IAP admission on backend service '${BACKEND_SERVICE}'..."
    gcloud iap web add-iam-policy-binding --project="${PROJECT_ID}" \
      --resource-type=backend-services --service="${BACKEND_SERVICE}" \
      --member="${MEMBER}" --role=roles/iap.httpsResourceAccessor --condition=None --quiet
  else
    echo "  [1/2] Skipping IAP admission (--skip-iap specified)"
  fi

  # 2. Grant Kubernetes RBAC in Tenant Namespace & Discovery ClusterRoleBinding
  if [[ "${SKIP_RBAC}" != "true" ]]; then
    echo "  [2/2] Granting Kubernetes RBAC..."

    # Check RoleBinding in tenant namespace
    ALREADY_IN_RB=$(kubectl --context="${CONTEXT}" -n "${TENANT_NAMESPACE}" get rolebinding notebooks-gke-pilot -o json 2>/dev/null | \
      jq -er --arg kind "${K8S_KIND}" --arg name "${K8S_NAME}" \
      'any(.subjects[]?; .kind == $kind and .name == $name)' 2>/dev/null || echo "false")

    if [[ "${ALREADY_IN_RB}" == "true" ]]; then
      echo "    - RoleBinding notebooks-gke-pilot in ${TENANT_NAMESPACE}: already granted."
    else
      echo "    - Patching RoleBinding notebooks-gke-pilot in ${TENANT_NAMESPACE}..."
      kubectl --context="${CONTEXT}" -n "${TENANT_NAMESPACE}" patch rolebinding notebooks-gke-pilot --type=json \
        -p '[{"op":"add","path":"/subjects/-","value":{"kind":"'"${K8S_KIND}"'","apiGroup":"rbac.authorization.k8s.io","name":"'"${K8S_NAME}"'"}}]'
    fi

    # Check ClusterRoleBinding for discovery
    ALREADY_IN_CRB=$(kubectl --context="${CONTEXT}" get clusterrolebinding notebooks-gke-pilot-discovery -o json 2>/dev/null | \
      jq -er --arg kind "${K8S_KIND}" --arg name "${K8S_NAME}" \
      'any(.subjects[]?; .kind == $kind and .name == $name)' 2>/dev/null || echo "false")

    if [[ "${ALREADY_IN_CRB}" == "true" ]]; then
      echo "    - ClusterRoleBinding notebooks-gke-pilot-discovery: already granted."
    else
      echo "    - Patching ClusterRoleBinding notebooks-gke-pilot-discovery..."
      kubectl --context="${CONTEXT}" patch clusterrolebinding notebooks-gke-pilot-discovery --type=json \
        -p '[{"op":"add","path":"/subjects/-","value":{"kind":"'"${K8S_KIND}"'","apiGroup":"rbac.authorization.k8s.io","name":"'"${K8S_NAME}"'"}}]'
    fi

    # Optional: Update rendered/ready/customer-pilot.json if it exists locally to keep local plan in sync
    CUSTOMER_PILOT_JSON="${SCRIPT_DIR}/rendered/ready/customer-pilot.json"
    if [[ -f "${CUSTOMER_PILOT_JSON}" ]]; then
      TMP_JSON=$(mktemp)
      jq --arg kind "${K8S_KIND}" --arg name "${K8S_NAME}" \
        '.items |= map(
           if (.kind == "RoleBinding" and .metadata.name == "notebooks-gke-pilot") or
              (.kind == "ClusterRoleBinding" and .metadata.name == "notebooks-gke-pilot-discovery") then
             if any(.subjects[]?; .kind == $kind and .name == $name) then .
             else .subjects += [{kind: $kind, name: $name, apiGroup: "rbac.authorization.k8s.io"}] end
           else . end
         )' "${CUSTOMER_PILOT_JSON}" > "${TMP_JSON}" && mv "${TMP_JSON}" "${CUSTOMER_PILOT_JSON}"
    fi
  else
    echo "  [2/2] Skipping Kubernetes RBAC (--skip-rbac specified)"
  fi
done

echo "=================================================================="
echo "Successfully enrolled ${#UNIQUE_USERS[@]} user(s)/group(s):"
for entry in "${UNIQUE_USERS[@]}"; do
  echo "  - ${entry}"
done
echo "=================================================================="
echo "Note on Google Login (OAuth):"
echo "  - If using Google-managed OAuth (Option B1): Users must belong to the"
echo "    same Google Cloud Organization as project '${PROJECT_ID}'."
echo "  - If using Custom OAuth (Option B2) for external accounts (e.g. @gmail.com):"
echo "    Ensure new users are added to the 'Test users' list under Google Auth Platform"
echo "    > Audience (https://console.cloud.google.com/auth/audience) if the app status is Testing."
echo "=================================================================="
