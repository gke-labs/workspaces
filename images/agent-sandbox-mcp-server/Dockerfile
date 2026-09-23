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
# Kubernetes Agent Sandbox MCP Server
#
# The upstream project (kubernetes-sigs/agent-sandbox) ships the source for this
# server under `clients/integrations/mcp-server/`, but as of v1.0.3 it does NOT
# publish a prebuilt image to registry.k8s.io. This Dockerfile builds it from a
# pinned upstream tag so that `examples/agent-sandbox/` is runnable without
# vendoring third-party source into this repository.
#
# Used by: examples/agent-sandbox/manifests/mcp-server.yaml
#          (${AGENT_SANDBOX_MCP_IMAGE}, default ${REGISTRY}/agent-sandbox-mcp-server:latest)
# ==============================================================================

ARG AGENT_SANDBOX_VERSION=v1.0.3

# ------------------------------------------------------------------------------
# Stage 1: fetch the pinned upstream source
# ------------------------------------------------------------------------------
FROM alpine/git:latest AS source
ARG AGENT_SANDBOX_VERSION
WORKDIR /src
RUN git clone --depth 1 --branch "${AGENT_SANDBOX_VERSION}" \
      https://github.com/kubernetes-sigs/agent-sandbox.git agent-sandbox \
 && test -f agent-sandbox/clients/integrations/mcp-server/pyproject.toml

# ------------------------------------------------------------------------------
# Stage 2: build the server image
# (mirrors upstream clients/integrations/mcp-server/Dockerfile)
# ------------------------------------------------------------------------------
FROM python:3.12-slim

ARG AGENT_SANDBOX_VERSION
LABEL org.opencontainers.image.title="agent-sandbox-mcp-server" \
      org.opencontainers.image.source="https://github.com/kubernetes-sigs/agent-sandbox" \
      org.opencontainers.image.version="${AGENT_SANDBOX_VERSION}" \
      org.opencontainers.image.description="MCP server for the Kubernetes Agent Sandbox project"

ENV PYTHONUNBUFFERED=1
WORKDIR /app

RUN groupadd -r appuser && useradd -r -g appuser appuser

COPY --from=source --chown=appuser:appuser \
     /src/agent-sandbox/clients/integrations/mcp-server/ /app/

RUN pip install --no-cache-dir . \
 && chown -R appuser:appuser /app

USER appuser
EXPOSE 8000

# Matches the probes in examples/agent-sandbox/manifests/mcp-server.yaml.
CMD ["uvicorn", "k8s_agent_sandbox_mcp_server.app:app", "--host", "0.0.0.0", "--port", "8000"]
