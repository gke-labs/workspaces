#
# JupyterLab + AX: a Workspace image that can drive AX agent fleets out of the box
# (examples/agent-substrate).
#
# Adds, on top of the upstream Kubeflow JupyterLab (SciPy) image:
#   - the `ax` CLI, built from the same AX source as the cluster's ax-server
#     (AX_REPO / AX_REF build args, pinned in examples/agent-substrate/versions.env)
#   - kubectl, which `ax ssh` uses to port-forward to the Substrate router
#   - defaults that point `ax` at ax-server and the notebook at fleet-helper
#   - the walkthrough notebook, pre-seeded into ~/agent-substrate on first start
#
# Single CPU variant: the agents run on Agent Substrate worker pods, not in the
# Workspace, so the Workspace itself needs no accelerator.
#
# Build with:  images/build.sh --jupyterlab-ax --registry-path <registry>
# (examples/agent-substrate/deploy_agent_substrate.sh does this for you.)

ARG BASE_IMG=ghcr.io/kubeflow/kubeflow/notebook-servers/jupyter-scipy:v1.11.0@sha256:8f8a000b7eece82c7e874924dc45a3922e9197137eefe67f55c711cc3ce068fd
ARG GO_IMG=golang:1.27-bookworm

# ------------------------------------------------------------------------------
# Stage 1: build the ax CLI (static, linux/amd64)
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
ARG KUBECTL_VERSION=v1.35.5

USER root
COPY --from=ax-build /out/ax /usr/local/bin/ax
# The Kubeflow base images ship kubectl; install it only if this base does not.
RUN chmod 0755 /usr/local/bin/ax \
 && if ! command -v kubectl >/dev/null 2>&1; then \
      curl -fsSL -o /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" \
      && chmod 0755 /usr/local/bin/kubectl; \
    fi \
 && ax version && kubectl version --client
USER $NB_UID

# Where the deploy script installs AX. Override per Workspace if yours differ.
ENV AX_SYSTEM_NAMESPACE=ax-system \
    AX_SERVER=ax-server.ax-system.svc.cluster.local:8080 \
    FLEET_HELPER_URL=http://fleet-helper.ax-system.svc.cluster.local:8080

ENV PATH="${HOME}/.local/bin:${PATH}"

# Pre-populate home with the walkthrough and the shared samples, and sync to
# $HOME_TMP so the base image's s6 01-copy-tmp-home seeds a fresh home volume.
COPY --chown=${NB_USER}:${NB_GID} agent-substrate/. ${HOME}/agent-substrate/
COPY --chown=${NB_USER}:${NB_GID} samples/. ${HOME}/samples/
RUN cp -p -r -T "${HOME}" "${HOME_TMP}" \
 && chmod -R g=u "${HOME_TMP}"
