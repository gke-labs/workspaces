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
#
# Code Server + AX agents: a VS Code Workspace image for examples/agent-substrate.
#
# On top of the upstream Kubeflow code-server image it adds:
#   - Gemini CLI, with the `agent-substrate` extension linked for every user:
#     a skill and slash commands that teach Gemini to delegate work to
#     background agents on Agent Substrate
#   - `ax-agents` (the CLI the skill drives) and the `ax` CLI it wraps, built
#     from the same AX source as the cluster's ax-server
#   - defaults pointing `ax` at ax-server, and Gemini CLI settings that use the
#     GEMINI_API_KEY the WorkspaceKind injects and allow `ax-agents` commands
#   - a sample project with failing tests, seeded into ~/textkit
#
# Build with:  images/build.sh --codeserver-ax --registry-path <registry>
# (examples/agent-substrate/deploy_agent_substrate.sh does this for you.)
# The build context must hold this directory plus `agent-substrate/`
# (gemini-extension/ and textkit/ from the example) and, optionally, a
# prebuilt `ax` binary; build.sh assembles it.

ARG BASE_IMG=ghcr.io/kubeflow/kubeflow/notebook-servers/codeserver-python:v1.11.0
ARG GO_IMG=golang:1.27-bookworm

# ------------------------------------------------------------------------------
# Stage 1: the ax CLI (static, linux/amd64)
# ------------------------------------------------------------------------------
FROM ${GO_IMG} AS ax-build
ARG AX_REPO=https://github.com/siyuanfoundation/ax.git
ARG AX_REF=bump
# AX's go.mod may ask for a newer patch release than the builder image has.
ENV GOTOOLCHAIN=auto CGO_ENABLED=0 GOOS=linux GOARCH=amd64
COPY ax* /tmp/
RUN mkdir -p /out \
 && if [ -f /tmp/ax ]; then \
      cp /tmp/ax /out/ax; \
    else \
      git clone --quiet --branch "${AX_REF}" "${AX_REPO}" /src/ax \
      && cd /src/ax \
      && go build -trimpath -ldflags="-s -w" -o /out/ax ./cmd/ax; \
    fi \
 && /out/ax version

# ------------------------------------------------------------------------------
# Stage 2: the Workspace image
# ------------------------------------------------------------------------------
FROM ${BASE_IMG}

ARG TARGETARCH=amd64
ARG NODE_VERSION=22.14.0
ARG GEMINI_CLI_VERSION=0.64.0
ARG KUBECTL_VERSION=v1.35.5

USER root
RUN case "${TARGETARCH}" in \
      amd64) NODE_ARCH="x64" ;; \
      arm64) NODE_ARCH="arm64" ;; \
      *) echo "Unsupported architecture: ${TARGETARCH}"; exit 1 ;; \
    esac \
 && curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${NODE_ARCH}.tar.xz" \
      | tar -xJ -C /usr/local --strip-components=1 \
 && npm install -g "@google/gemini-cli@${GEMINI_CLI_VERSION}" \
 && npm cache clean --force \
 && gemini --version

COPY --from=ax-build /out/ax /usr/local/bin/ax
# `ax ssh` port-forwards with kubectl; the Kubeflow base images ship it, but
# install it if this base does not.
RUN chmod 0755 /usr/local/bin/ax \
 && if ! command -v kubectl > /dev/null 2>&1; then \
      curl -fsSL -o /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" \
      && chmod 0755 /usr/local/bin/kubectl; \
    fi \
 && ax version && kubectl version --client

# The Gemini CLI extension lives outside $HOME so that image updates reach
# existing home volumes; each user's ~/.gemini only holds a link to it.
COPY agent-substrate/gemini-extension /opt/agent-substrate/gemini-extension
RUN chmod -R a+rX /opt/agent-substrate \
 && chmod 0755 /opt/agent-substrate/gemini-extension/skills/agent-substrate/scripts/ax-agents \
 && ln -s /opt/agent-substrate/gemini-extension/skills/agent-substrate/scripts/ax-agents /usr/local/bin/ax-agents \
 && ax-agents --help > /dev/null
USER $NB_UID

# Where deploy_agent_substrate.sh installs AX. Override per Workspace if needed.
ENV AX_SYSTEM_NAMESPACE=ax-system \
    AX_SERVER=ax-server.ax-system.svc.cluster.local:8080 \
    FLEET_HELPER_URL=http://fleet-helper.ax-system.svc.cluster.local:8080

ENV PATH="${HOME}/.local/bin:${PATH}"

# Seed the home directory (copied with `cp -n` into a fresh or existing home
# volume by the base image's s6 01-copy-tmp-home, so user files always win):
#   ~/.gemini/settings.json       API-key auth; lets Gemini read the extension
#   ~/.gemini/policies/agent-substrate.toml  run ax-agents without asking
#   ~/.gemini/trustedFolders.json trust the home directory
#   ~/.gemini/extensions/agent-substrate  link to /opt/agent-substrate/...
#   ~/textkit                     the sample project
# Whole directories are copied so that --chown also covers ~/.gemini itself.
COPY --chown=${NB_USER}:${NB_GID} home/ ${HOME}/
COPY --chown=${NB_USER}:${NB_GID} agent-substrate/textkit ${HOME}/textkit/
RUN cp -p -r -T "${HOME}" "${HOME_TMP}" \
 && chmod -R g=u "${HOME_TMP}"
