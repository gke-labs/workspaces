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

set -euo pipefail

# ------------------------------------------------------------------------------
# Configure Gemini CLI and Code Assist to use Agent Sandbox MCP Server
# ------------------------------------------------------------------------------
# Usage:
#   Inside a workspace terminal:
#     bash examples/agent-sandbox/setup_gemini.sh
#
#   From cluster admin workstation (via kubectl):
#     bash examples/agent-sandbox/setup_gemini.sh <WORKSPACE_NAME>
# ------------------------------------------------------------------------------

if [[ -f /var/run/secrets/kubernetes.io/serviceaccount/namespace ]]; then
  # Running inside the workspace pod
  TENANT_NAMESPACE="${TENANT_NAMESPACE:-$(cat /var/run/secrets/kubernetes.io/serviceaccount/namespace 2>/dev/null || echo "kubeflow-user")}"
  GEMINI_DIR="${HOME}/.gemini"

  echo "==> Configuring Gemini for Agent Sandbox in namespace: ${TENANT_NAMESPACE}..."
  mkdir -p "${GEMINI_DIR}"

  cat <<EOF > "${GEMINI_DIR}/settings.json"
{
  "mcpServers": {
    "agent-sandbox": {
      "url": "http://agent-sandbox-mcp-server.${TENANT_NAMESPACE}.svc.cluster.local:8000/mcp",
      "type": "http",
      "trust": true
    }
  }
}
EOF

  cat <<EOF > "${GEMINI_DIR}/trustedFolders.json"
{
  "/": "TRUST_PARENT",
  "/home/jovyan": "TRUST_FOLDER"
}
EOF

  cat <<EOF > "${GEMINI_DIR}/GEMINI.md"
# Kubernetes Agent Sandbox Execution Rules

You have access to the Kubernetes Agent Sandbox MCP server (\`agent-sandbox\`).
Whenever the user asks to run Python code, execute test suites, run benchmarks, or perform untrusted shell operations:
1. Provision an isolated execution environment using \`mcp_agent-sandbox_create_sandbox\`:
   - \`namespace\`: "${TENANT_NAMESPACE}"
   - \`warmpool\`: "python-warmpool"
2. Upload any necessary files or scripts using \`mcp_agent-sandbox_upload_file\`.
3. Execute the workload using \`mcp_agent-sandbox_execute_command\`.
4. Retrieve results or artifacts using \`mcp_agent-sandbox_download_file\`.
5. Always clean up and release cluster resources when done using \`mcp_agent-sandbox_delete_sandbox\`.
EOF

  echo "==> Successfully configured Gemini in ${GEMINI_DIR}!"
  echo "    MCP Server URL: http://agent-sandbox-mcp-server.${TENANT_NAMESPACE}.svc.cluster.local:8000/mcp"
  echo ""
  echo "Next steps:"
  echo "  1. Export your Gemini API key: export GEMINI_API_KEY=\"your-api-key\""
  echo "  2. Test tool discovery:       gemini -p 'List the tools available from the agent-sandbox MCP server.'"
else
  # Running from outside the workspace via kubectl
  WORKSPACE_NAME="${1:-${WORKSPACE_NAME:-}}"
  TENANT_NAMESPACE="${TENANT_NAMESPACE:-kubeflow-user}"

  if [[ -z "${WORKSPACE_NAME}" ]]; then
    echo "Usage:" >&2
    echo "  Inside workspace terminal:  bash examples/agent-sandbox/setup_gemini.sh" >&2
    echo "  From outside via kubectl:   bash examples/agent-sandbox/setup_gemini.sh <WORKSPACE_NAME>" >&2
    exit 1
  fi

  WORKSPACE_POD=$(kubectl -n "${TENANT_NAMESPACE}" get pods \
    -l "notebooks.kubeflow.org/workspace-name=${WORKSPACE_NAME}" \
    --field-selector=status.phase=Running \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)

  if [[ -z "${WORKSPACE_POD}" ]]; then
    echo "ERROR: No running pod found for workspace '${WORKSPACE_NAME}' in namespace '${TENANT_NAMESPACE}'." >&2
    exit 1
  fi

  echo "==> Configuring Gemini for workspace '${WORKSPACE_NAME}' (pod: ${WORKSPACE_POD})..."
  kubectl -n "${TENANT_NAMESPACE}" exec "${WORKSPACE_POD}" -c main -- bash -c "
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
  echo "==> Workspace '${WORKSPACE_NAME}' successfully configured for Gemini!"
fi
