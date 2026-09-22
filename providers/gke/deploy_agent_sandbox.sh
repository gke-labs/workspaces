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
# Deploy and Configure Kubernetes Agent Sandbox for Standalone Kubeflow on GKE
# ==============================================================================
# This script provisions:
# 1. Agent Sandbox Operator (CRDs + Controller in agent-sandbox-system from release artifacts)
# 2. Aggregated RBAC ClusterRole (agent-sandbox-kubeflow-edit)
# 3. Workload Identity & Vertex AI / Gemini IAM permissions
# 4. Tenant SandboxTemplate (official release image) and SandboxWarmPool
# 5. Tenant Agent Sandbox MCP Server (streamable HTTP on port 8000)
# 6. Workspace configuration for Gemini CLI and Gemini Code Assist
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ------------------------------------------------------------------------------
# 1. Configuration & Defaults
# ------------------------------------------------------------------------------
export PROJECT="${PROJECT:-${PROJECT_ID:-$(gcloud config get-value project 2>/dev/null || true)}}"
export PROJECT_ID="${PROJECT}"
export CLUSTER="${CLUSTER:-kubeflow-notebooks}"
export LOCATION="${LOCATION:-us-west1}"
export REGION="${REGION:-us-west1}"
export TENANT_NAMESPACE="${TENANT_NAMESPACE:-kubeflow-user}"
export REPOSITORY="${REPOSITORY:-kubeflow-repo}"
export CONTEXT="${CONTEXT:-gke_${PROJECT}_${LOCATION}_${CLUSTER}}"
export REGISTRY="${REGISTRY:-${REGION}-docker.pkg.dev/${PROJECT}/${REPOSITORY}}"

export AGENT_SANDBOX_VERSION="${AGENT_SANDBOX_VERSION:-v1.0.3}"
export WARMPOOL_REPLICAS="${WARMPOOL_REPLICAS:-1}"
export CONFIGURE_WORKSPACE="${CONFIGURE_WORKSPACE:-true}"

# Use official release container image from https://github.com/kubernetes-sigs/agent-sandbox/releases#release-v1.0.3
export SANDBOX_RUNTIME_IMAGE="${SANDBOX_RUNTIME_IMAGE:-registry.k8s.io/agent-sandbox/python-runtime-sandbox:${AGENT_SANDBOX_VERSION}}"
export AGENT_SANDBOX_MCP_IMAGE="${AGENT_SANDBOX_MCP_IMAGE:-${REGISTRY}/agent-sandbox-mcp-server:latest}"

if [[ -z "${PROJECT}" ]]; then
  echo "ERROR: PROJECT is not set. Please run: export PROJECT=your-gcp-project-id" >&2
  exit 1
fi

echo "=============================================================================="
echo "Deploying Kubernetes Agent Sandbox to GKE"
echo "  Project:          ${PROJECT}"
echo "  Cluster:          ${CLUSTER} (${LOCATION})"
echo "  Tenant Namespace: ${TENANT_NAMESPACE}"
echo "  Artifact Reg:     ${REGISTRY}"
echo "  Operator Version: ${AGENT_SANDBOX_VERSION}"
echo "=============================================================================="

# ------------------------------------------------------------------------------
# 2. Cluster Authentication
# ------------------------------------------------------------------------------
echo "==> Verifying GKE cluster credentials..."
gcloud container clusters get-credentials "${CLUSTER}" \
  --location="${LOCATION}" \
  --project="${PROJECT}"

kubectl config use-context "${CONTEXT}" || true

# ------------------------------------------------------------------------------
# 3. Deploy Agent Sandbox Operator
# ------------------------------------------------------------------------------
echo "==> Checking Agent Sandbox operator CRDs..."
if ! kubectl --context="${CONTEXT}" get crd sandboxes.agents.x-k8s.io &>/dev/null; then
  echo "==> Installing Agent Sandbox Operator (${AGENT_SANDBOX_VERSION}) from release artifacts..."
  kubectl --context="${CONTEXT}" apply --server-side \
    -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AGENT_SANDBOX_VERSION}/sandbox-with-extensions.yaml"
else
  echo "==> Agent Sandbox CRDs already present."
fi

echo "==> Waiting for Agent Sandbox Controller rollout in agent-sandbox-system..."
kubectl --context="${CONTEXT}" -n agent-sandbox-system rollout status \
  deployment/agent-sandbox-controller --timeout=3m

# ------------------------------------------------------------------------------
# 4. Configure RBAC (ClusterRole & WorkspaceKind Attachment)
# ------------------------------------------------------------------------------
echo "==> Applying aggregated ClusterRole for Agent Sandbox..."
kubectl --context="${CONTEXT}" apply -f "${SCRIPT_DIR}/manifests/agent-sandbox/clusterrole.yaml"

echo "==> Attaching Agent Sandbox permissions to WorkspaceKinds..."
# In Kubeflow, WorkspaceKind.spec.podTemplate.serviceAccount.clusterRoles automatically creates
# namespaced RoleBindings for every workspace of that kind in any tenant namespace.
for wk in $(kubectl --context="${CONTEXT}" get workspacekinds -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true); do
  echo "  Attaching agent-sandbox-kubeflow-edit to WorkspaceKind/${wk}..."
  kubectl --context="${CONTEXT}" patch workspacekind "${wk}" --type=merge \
    -p '{"spec":{"podTemplate":{"serviceAccount":{"clusterRoles":[{"name":"kubeflow-edit"},{"name":"agent-sandbox-kubeflow-edit"}]}}}}' || true
done

# # Clean up any obsolete manual single-workspace RoleBinding if it exists
# kubectl --context="${CONTEXT}" -n "${TENANT_NAMESPACE}" delete rolebinding agent-sandbox-edit --ignore-not-found

# ------------------------------------------------------------------------------
# 5. Configure GCP IAM & Workload Identity for Gemini / Vertex AI
# ------------------------------------------------------------------------------
GSA="${TENANT_NAMESPACE}-sa@${PROJECT}.iam.gserviceaccount.com"
echo "==> Checking Google Service Account ${GSA} for Vertex AI / Gemini access..."
if gcloud iam service-accounts describe "${GSA}" --project="${PROJECT}" &>/dev/null; then
  echo "==> Granting roles/aiplatform.user on project ${PROJECT} to ${GSA}..."
  gcloud projects add-iam-policy-binding "${PROJECT}" \
    --member="serviceAccount:${GSA}" \
    --role="roles/aiplatform.user" \
    --condition=None --quiet >/dev/null

  PROJECT_NUMBER=$(gcloud projects describe "${PROJECT}" --format="value(projectNumber)")
  echo "==> Binding Workload Identity to entire tenant namespace ${TENANT_NAMESPACE}..."
  gcloud iam service-accounts add-iam-policy-binding "${GSA}" \
    --project="${PROJECT}" \
    --role="roles/iam.workloadIdentityUser" \
    --member="principalSet://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${PROJECT}.svc.id.goog/namespace/${TENANT_NAMESPACE}" \
    --condition=None --quiet >/dev/null

  echo "==> Annotating tenant namespace ServiceAccounts with ${GSA}..."
  kubectl --context="${CONTEXT}" -n "${TENANT_NAMESPACE}" annotate sa \
    -l "notebooks.kubeflow.org/workspace-name" \
    iam.gke.io/gcp-service-account="${GSA}" --overwrite || true
  kubectl --context="${CONTEXT}" -n "${TENANT_NAMESPACE}" annotate sa default \
    iam.gke.io/gcp-service-account="${GSA}" --overwrite || true
else
  echo "INFO: GSA ${GSA} does not exist. Skipping Workload Identity binding for Vertex AI."
fi

# ------------------------------------------------------------------------------
# 6. Deploy SandboxTemplate and SandboxWarmPool
# ------------------------------------------------------------------------------
echo "==> Deploying SandboxTemplate and SandboxWarmPool in ${TENANT_NAMESPACE}..."
envsubst < "${SCRIPT_DIR}/manifests/agent-sandbox/sandbox-template.yaml" | kubectl --context="${CONTEXT}" apply -f -

# ------------------------------------------------------------------------------
# 7. Deploy Agent Sandbox MCP Server Deployment & Service
# ------------------------------------------------------------------------------
echo "==> Deploying Agent Sandbox MCP Server in ${TENANT_NAMESPACE}..."
envsubst < "${SCRIPT_DIR}/manifests/agent-sandbox/mcp-server.yaml" | kubectl --context="${CONTEXT}" apply -f -

echo "==> Waiting for MCP Server rollout in ${TENANT_NAMESPACE}..."
kubectl --context="${CONTEXT}" -n "${TENANT_NAMESPACE}" rollout status \
  deployment/agent-sandbox-mcp-server --timeout=3m

echo "==> Verifying MCP Server health..."
MCP_POD=$(kubectl --context="${CONTEXT}" -n "${TENANT_NAMESPACE}" get pods -l app=agent-sandbox-mcp-server --no-headers | grep "Running" | awk '{print $1}' | head -n 1)
kubectl --context="${CONTEXT}" -n "${TENANT_NAMESPACE}" exec "${MCP_POD}" -- python3 -c "import urllib.request; urllib.request.urlopen('http://localhost:8000/healthz')"
echo "  MCP Server /healthz check passed!"

# ------------------------------------------------------------------------------
# 8. Configure Running Workspace (VS Code / code-server)
# ------------------------------------------------------------------------------
if [[ "${CONFIGURE_WORKSPACE}" == "true" ]]; then
  echo "==> Checking for running VS Code / code-server pods in ${TENANT_NAMESPACE}..."
  WORKSPACE_POD=$(kubectl --context="${CONTEXT}" -n "${TENANT_NAMESPACE}" get pods \
    -l "notebooks.kubeflow.org/workspace-name" \
    --field-selector=status.phase=Running \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)

  if [[ -n "${WORKSPACE_POD}" ]]; then
    echo "==> Configuring workspace pod: ${WORKSPACE_POD}..."
    kubectl --context="${CONTEXT}" -n "${TENANT_NAMESPACE}" exec "${WORKSPACE_POD}" -c main -- bash -c "
      mkdir -p /home/jovyan/.gemini
      cat <<'EOF' > /home/jovyan/.gemini/settings.json
{
  \"mcpServers\": {
    \"agent-sandbox\": {
      \"url\": \"http://agent-sandbox-mcp-server.${TENANT_NAMESPACE}.svc.cluster.local:8000/mcp\",
      \"type\": \"http\",
      \"trust\": true
    }
  }
}
EOF
      cat <<'EOF' > /home/jovyan/.gemini/trustedFolders.json
{
  \"/\": \"TRUST_PARENT\",
  \"/home/jovyan\": \"TRUST_FOLDER\"
}
EOF
      cat <<'EOF' > /home/jovyan/.gemini/GEMINI.md
# Kubernetes Agent Sandbox Execution Rules

You have access to the Kubernetes Agent Sandbox MCP server (\`agent-sandbox\`).
Whenever the user asks to run Python code, execute test suites, run benchmarks, or perform untrusted shell operations:
1. Provision an isolated execution environment using \`mcp_agent-sandbox_create_sandbox\`:
   - \`namespace\`: \"${TENANT_NAMESPACE}\"
   - \`warmpool\`: \"python-warmpool\"
2. Upload any necessary files or scripts using \`mcp_agent-sandbox_upload_file\`.
3. Execute the workload using \`mcp_agent-sandbox_execute_command\`.
4. Retrieve results or artifacts using \`mcp_agent-sandbox_download_file\`.
5. Always clean up and release cluster resources when done using \`mcp_agent-sandbox_delete_sandbox\`.
EOF
      chown -R 1000:100 /home/jovyan/.gemini
    "
    echo "  Workspace .gemini/settings.json, trustedFolders.json, and GEMINI.md successfully updated!"
  else
    echo "INFO: No running workspace pod found. Configurations will apply on next workspace launch."
  fi
fi

# ------------------------------------------------------------------------------
# Summary & Next Steps
# ------------------------------------------------------------------------------
echo ""
echo "=============================================================================="
echo "Kubernetes Agent Sandbox Successfully Deployed & Configured!"
echo "=============================================================================="
echo "MCP Server Endpoint: http://agent-sandbox-mcp-server.${TENANT_NAMESPACE}.svc.cluster.local:8000/mcp"
echo ""
echo "Quick Verification from your Workspace Terminal:"
echo "  1. Open VS Code terminal in ${TENANT_NAMESPACE}."
echo "  2. Test Gemini CLI tool discovery:"
echo "     gemini -p 'List the tools available from the agent-sandbox MCP server.'"
echo "  3. Run an isolated task in a sandbox:"
echo "     gemini -p 'Create a sandbox from warmpool python-warmpool, run python3 -c \"print(2**64)\", and delete the sandbox.'"
echo ""
echo "To run the multi-sandbox orchestration walkthrough:"
echo "  python3 providers/gke/examples/multi_agent_sandbox_walkthrough.py"
echo "=============================================================================="
