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

# Remove everything deploy_agent_substrate.sh created. The Kubeflow Workspaces
# platform itself is left untouched. The node pool and the snapshot bucket are
# kept unless DELETE_NODE_POOL=true / DELETE_SNAPSHOT_BUCKET=true.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=versions.env
source "${SCRIPT_DIR}/versions.env"

export PROJECT_ID="${PROJECT_ID:-${PROJECT:-$(gcloud config get-value project 2>/dev/null || true)}}"
export CLUSTER_NAME="${CLUSTER_NAME:-${CLUSTER:-kubeflow-notebooks}}"
export LOCATION="${LOCATION:-us-central1-c}"
export TENANT_NAMESPACE="${TENANT_NAMESPACE:-kubeflow-user}"
export CONTEXT="${CONTEXT:-gke_${PROJECT_ID}_${LOCATION}_${CLUSTER_NAME}}"
export ATE_SNAPSHOT_BUCKET="${ATE_SNAPSHOT_BUCKET:-${PROJECT_ID}-ate-snapshots}"
export SUBSTRATE_NODE_POOL="${SUBSTRATE_NODE_POOL:-substrate-pool}"
export WORKER_NAMESPACE="${WORKER_NAMESPACE:-ate-workers}"
export WORK_DIR="${WORK_DIR:-${HOME}/.cache/gke-workspaces/agent-substrate}"
export SUBSTRATE_SRC="${WORK_DIR}/substrate-${SUBSTRATE_VERSION}"
DELETE_NODE_POOL="${DELETE_NODE_POOL:-false}"
DELETE_SNAPSHOT_BUCKET="${DELETE_SNAPSHOT_BUCKET:-false}"
K=(kubectl --context "${CONTEXT}")

echo "==> Deleting AX tasks in atespace ${TENANT_NAMESPACE} (best effort)"
if [[ -x "${WORK_DIR}/bin/ax" ]]; then
  AX=("${WORK_DIR}/bin/ax" --context "${CONTEXT}" -a "${TENANT_NAMESPACE}")
  for t in $("${AX[@]}" get tasks 2>/dev/null | awk 'NR>1 {print $1}'); do
    "${AX[@]}" delete task "${t}" || true
  done
fi

echo "==> Removing the \"Code Server + AX agents\" image option from WorkspaceKinds"
# Workspaces still using the image keep running until they are recreated; the
# Workspaces API may refuse to remove an option that is in use. The optional
# GEMINI_API_KEY environment variable stays, as on the jupyterlab kind.
for kind in ${WORKSPACE_KINDS:-codeserver}; do
  "${K[@]}" get workspacekind "${kind}" >/dev/null 2>&1 || continue
  patch="$("${K[@]}" get workspacekind "${kind}" -o json \
    | python3 "${SCRIPT_DIR}/register_workspace_image.py" --remove)"
  if [[ -n "${patch}" ]]; then
    "${K[@]}" patch workspacekind "${kind}" --type=json -p "${patch}" \
      || echo "WARNING: could not remove the option from ${kind} (still in use?)"
  fi
done

echo "==> Deleting AX, fleet-helper and tenant access"
"${K[@]}" -n ate-system delete rolebinding "ax-ssh-router-portforward-${TENANT_NAMESPACE}" --ignore-not-found
"${K[@]}" -n ate-system delete role ax-ssh-router-portforward --ignore-not-found
"${K[@]}" delete clusterrole,clusterrolebinding ax-server --ignore-not-found
"${K[@]}" delete namespace ax-system --ignore-not-found --wait=false

echo "==> Deleting the WorkerPool"
"${K[@]}" delete namespace "${WORKER_NAMESPACE}" --ignore-not-found

echo "==> Deleting Agent Substrate (ate-system)"
if [[ -d "${SUBSTRATE_SRC}" ]]; then
  (cd "${SUBSTRATE_SRC}" && ATE_CONFIG="${WORK_DIR}/substrate-install.yaml" ./hack/install-ate.sh --delete-ate-system) || true
else
  "${K[@]}" delete namespace ate-system --ignore-not-found --wait=false
fi

if [[ "${DELETE_NODE_POOL}" == "true" ]]; then
  echo "==> Deleting node pool ${SUBSTRATE_NODE_POOL}"
  gcloud container node-pools delete "${SUBSTRATE_NODE_POOL}" --cluster "${CLUSTER_NAME}" \
    --location "${LOCATION}" --project "${PROJECT_ID}" --quiet || true
fi
if [[ "${DELETE_SNAPSHOT_BUCKET}" == "true" ]]; then
  echo "==> Deleting gs://${ATE_SNAPSHOT_BUCKET}"
  gcloud storage rm --recursive "gs://${ATE_SNAPSHOT_BUCKET}" --quiet || true
fi
echo "Done."
