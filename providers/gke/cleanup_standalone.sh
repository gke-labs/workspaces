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
# Teardown & Cleanup Standalone Kubeflow Workspaces, Trainer, & Spark Operator on GKE
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
DIST_DIR="${DIST_DIR:-/tmp/kubeflow-community-distribution}"

export PROJECT_ID="${PROJECT_ID:-${PROJECT:-$(gcloud config get-value project 2>/dev/null || true)}}"
export CLUSTER_NAME="${CLUSTER_NAME:-${CLUSTER:-kubeflow-notebooks}}"
export LOCATION="${LOCATION:-us-central1-c}"
export TENANT_NAMESPACE="${TENANT_NAMESPACE:-kubeflow-user}"
export ADDRESS_NAME="${ADDRESS_NAME:-notebooks-gke-global}"
export CERTIFICATE_NAME="${CERTIFICATE_NAME:-notebooks-gke}"
export CERTIFICATE_MAP="${CERTIFICATE_MAP:-notebooks-gke}"
export CONTEXT="${CONTEXT:-gke_${PROJECT_ID}_${LOCATION}_${CLUSTER_NAME}}"
export SNAPSHOT_GCS_BUCKET="${SNAPSHOT_GCS_BUCKET:-${PROJECT_ID}-${TENANT_NAMESPACE}-snapshots-bucket}"
export DELETE_EDGE_RESOURCES="${DELETE_EDGE_RESOURCES:-false}"
export DELETE_SNAPSHOT_BUCKET="${DELETE_SNAPSHOT_BUCKET:-false}"

echo "=================================================================="
echo "Cleaning up Standalone Kubeflow Workspaces on GKE (${CLUSTER_NAME})..."
echo "=================================================================="

# 0. Remove Snapshot Mutating Webhook first so Workspace/Pod teardown is never intercepted
kubectl --context="${CONTEXT}" delete mutatingwebhookconfiguration gke-workspace-snapshot-mutating-webhook --ignore-not-found || true
kubectl --context="${CONTEXT}" -n kubeflow-workspaces delete certificate gke-snapshot-webhook-cert --ignore-not-found || true
kubectl --context="${CONTEXT}" -n kubeflow-workspaces delete secret gke-snapshot-webhook-cert --ignore-not-found || true

# 1. Remove tenant workloads and GKE Pod Snapshot resources
echo "Deleting tenant workspaces, podsnapshots, trainjobs, sparkapplications, and deployments in ${TENANT_NAMESPACE}..."
kubectl --context="${CONTEXT}" delete workspaces --all -n "${TENANT_NAMESPACE}" --ignore-not-found || true
kubectl --context="${CONTEXT}" delete podsnapshotmanualtriggers.podsnapshot.gke.io --all -n "${TENANT_NAMESPACE}" --ignore-not-found || true
kubectl --context="${CONTEXT}" delete podsnapshots.podsnapshot.gke.io --all -n "${TENANT_NAMESPACE}" --ignore-not-found || true
kubectl --context="${CONTEXT}" delete podsnapshotpolicies.podsnapshot.gke.io --all -n "${TENANT_NAMESPACE}" --ignore-not-found || true
kubectl --context="${CONTEXT}" delete podsnapshotstorageconfigs.podsnapshot.gke.io kubeflow-pod-snapshot-storage-config --ignore-not-found || true
kubectl --context="${CONTEXT}" delete configmap jupyter-ipc-config -n "${TENANT_NAMESPACE}" --ignore-not-found || true
kubectl --context="${CONTEXT}" delete trainjobs --all -n "${TENANT_NAMESPACE}" --ignore-not-found || true
kubectl --context="${CONTEXT}" delete sparkconnects --all -n "${TENANT_NAMESPACE}" --ignore-not-found || true
kubectl --context="${CONTEXT}" delete sparkapplications --all -n "${TENANT_NAMESPACE}" --ignore-not-found || true
kubectl --context="${CONTEXT}" delete deployment fashion-mnist-inference -n "${TENANT_NAMESPACE}" --ignore-not-found || true
kubectl --context="${CONTEXT}" delete service fashion-mnist-inference -n "${TENANT_NAMESPACE}" --ignore-not-found || true

# 2. Remove Kubeflow Spark Operator and Trainer
if [[ -d "${DIST_DIR}" ]]; then
  echo "Removing Kubeflow Spark Operator and Kubeflow Trainer..."
  kubectl --context="${CONTEXT}" delete -k "${DIST_DIR}/applications/spark/spark-operator/overlays/kubeflow" --ignore-not-found || true
  kubectl --context="${CONTEXT}" delete -k "${DIST_DIR}/applications/trainer/overlays" --ignore-not-found || true
fi

# 3. Remove WorkspaceKinds and example ComputeClasses
kubectl --context="${CONTEXT}" delete workspacekind jupyterlab gke-jupyterlab jupyterlab-resumable codeserver --ignore-not-found || true
if [[ -d "${REPO_ROOT}/examples/compute-classes" ]]; then
  kubectl --context="${CONTEXT}" delete -f "${REPO_ROOT}/examples/compute-classes/" --ignore-not-found || true
fi

# 4. Remove standalone edge and application resources
if [[ -d "${SCRIPT_DIR}/rendered/ready" ]]; then
  kubectl --context="${CONTEXT}" delete -f "${SCRIPT_DIR}/rendered/ready/edge.json" --ignore-not-found || true
  kubectl --context="${CONTEXT}" delete -f "${SCRIPT_DIR}/rendered/ready/applications.json" --ignore-not-found || true
  kubectl --context="${CONTEXT}" delete -f "${SCRIPT_DIR}/rendered/ready/isolation.json" --ignore-not-found || true
fi

kubectl --context="${CONTEXT}" delete validatingadmissionpolicybinding notebooks-gke-pilot-workspaces --ignore-not-found || true
kubectl --context="${CONTEXT}" delete validatingadmissionpolicy notebooks-gke-pilot-workspaces --ignore-not-found || true
kubectl --context="${CONTEXT}" delete namespace notebooks-connections --ignore-not-found || true

if [[ "${DELETE_SNAPSHOT_BUCKET}" == "true" ]]; then
  echo "Deleting GKE Pod Snapshot GCS bucket gs://${SNAPSHOT_GCS_BUCKET}..."
  gcloud storage rm --recursive "gs://${SNAPSHOT_GCS_BUCKET}" --project="${PROJECT_ID}" --quiet || true
fi

if [[ "${DELETE_EDGE_RESOURCES}" == "true" ]]; then
  echo "Deleting Certificate Manager certificate map and Global IP (${ADDRESS_NAME})..."
  for entry in $(gcloud certificate-manager maps entries list --map="${CERTIFICATE_MAP}" --project="${PROJECT_ID}" --format='value(name)' 2>/dev/null || true); do
    entry_id=$(basename "${entry}")
    gcloud certificate-manager maps entries delete "${entry_id}" --map="${CERTIFICATE_MAP}" --project="${PROJECT_ID}" --quiet || true
  done
  gcloud certificate-manager maps delete "${CERTIFICATE_MAP}" --project="${PROJECT_ID}" --quiet || true
  gcloud certificate-manager certificates delete "${CERTIFICATE_NAME}" --project="${PROJECT_ID}" --quiet || true
  gcloud certificate-manager certificates delete "${CERTIFICATE_NAME}-desktop" --project="${PROJECT_ID}" --quiet || true
  gcloud compute addresses delete "${ADDRESS_NAME}" --global --project="${PROJECT_ID}" --quiet || true
fi

echo "=================================================================="
echo "✅ Standalone Kubeflow Workspaces cleanup complete."
echo "=================================================================="
