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
# Build and Publish Docker Images to GitHub Container Registry (ghcr.io)
#
# Builds and publishes:
# 1. Custom Workspace & Runtime Images (from images/):
#    - VS Code (codeserver-python) [cpu, gpu, tpu]
#    - JupyterLab (jupyterlab)     [cpu, gpu, tpu]
#    - Apache Spark (spark-py312)
#    - Agent Sandbox MCP Server (agent-sandbox-mcp-server)
#
# 2. Standalone GKE Workspaces Core Images (from providers/gke/deploy_standalone.sh):
#    - GKE Access Proxy   (gke-access-proxy)   from providers/gke/Dockerfile
#    - GKE Snapshot Addon (gke-snapshot-addon) from providers/gke/snapshot.Dockerfile
#    - GKE Frontend       (gke-frontend)       from providers/gke/frontend.Dockerfile
#    - GKE Controller     (gke-controller)     from workspaces/controller/Dockerfile
#    - GKE Backend        (gke-backend)        from workspaces/backend/Dockerfile
# ==============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
IMAGES_DIR="${REPO_ROOT}/images"
PROVIDERS_GKE_DIR="${REPO_ROOT}/providers/gke"

# ------------------------------------------------------------------------------
# Default Configuration
# ------------------------------------------------------------------------------
REGISTRY_PATH="${REGISTRY_PATH:-${REGISTRY:-}}"
IMAGE_TAG="${IMAGE_TAG:-${TAG:-}}"
AGENT_SANDBOX_VERSION="${AGENT_SANDBOX_VERSION:-v1.0.3}"
PLATFORM="${PLATFORM:-linux/amd64}"
PUSH_IMAGE=true
TAG_LATEST=true
DRY_RUN=false
DO_LOGIN="auto"
USE_GIT_TAG=false
ALIAS_WORKSPACES=false
OUTPUT_ENV_FILE=""
OUTPUT_JSON_FILE=""
AUTH_USERNAME="${AUTH_USERNAME:-${GITHUB_ACTOR:-${GITHUB_USER:-}}}"

# Target selectors
BUILD_ALL=false
BUILD_WORKSPACE_IMAGES=false
BUILD_PROVIDER_IMAGES=false
INCLUDE_MCP_SERVER=true

SELECTED_WORKSPACE_IMAGES=()
SELECTED_PROVIDER_IMAGES=()
TARGET_HARDWARE=()

# ------------------------------------------------------------------------------
# Usage / Help
# ------------------------------------------------------------------------------
show_usage() {
  cat <<EOF
Usage: $(basename "$0") [options] [registry-path]

Build and publish prebuilt Docker images from images/ and providers/gke/ to GitHub (GHCR).

Target Selection Options:
  --all                          Build and publish ALL workspace and provider images (default)
  --workspace-images, --images   Build only custom workspace images (codeserver, jupyterlab, spark, mcp-server)
  --provider-images, --standalone Build only standalone core images (access-proxy, snapshot-addon, frontend, controller, backend)
  --include-mcp-server           Include agent-sandbox-mcp-server in workspace builds (default: true)
  --no-mcp-server                Exclude agent-sandbox-mcp-server from workspace builds

Individual Target Options:
  --codeserver                   VS Code (codeserver-python) [cpu, gpu, tpu]
  --jupyterlab                   JupyterLab [cpu, gpu, tpu]
  --spark                        Apache Spark (spark-py312)
  --mcp-server                   Agent Sandbox MCP server (agent-sandbox-mcp-server)
  --access-proxy                 GKE Access Proxy (gke-access-proxy)
  --snapshot-addon               GKE Snapshot Addon (gke-snapshot-addon)
  --frontend                     GKE Frontend (gke-frontend)
  --controller                   GKE Controller (gke-controller)
  --backend                      GKE Backend (gke-backend)
  --image, -i <name>             Specify an individual image target by name

Hardware Accelerator Variants (workspace images):
  --hardware, -hw, -v <variant>  cpu, gpu, tpu, or all (default: all)
  --cpu                          Build only CPU variant
  --gpu                          Build only CUDA GPU variant
  --tpu                          Build only Cloud TPU variant

Registry & Tagging Options:
  --registry, -r <path>          Container registry path (e.g. ghcr.io/my-org/my-repo or ghcr.io/my-org).
                                 Default: auto-detected as ghcr.io/<owner>/<repo> from git remote origin.
  --owner, --org, -o <owner>     Override GitHub organization/user (e.g. ghcr.io/<owner>/<repo>)
  --repo <name>                  Override GitHub repository name
  --tag, -t <tag>                Image tag prefix (default: vYYYYMMDD-HHMMSS).
  --git-tag                      Use git commit short SHA as tag prefix (e.g. sha-XXXXXXX)
  --latest / --no-latest         Also tag and push ':latest' and ':latest-<hw>' (default: true)
  --alias-workspaces             Also tag provider images with upstream names:
                                 workspaces-controller, workspaces-backend, workspaces-frontend
  --agent-sandbox-version <v>    Upstream kubernetes-sigs/agent-sandbox tag for MCP server (default: v1.0.3)

Execution & Auth Options:
  --push / --no-push             Push images to registry after build (default: true)
  --dry-run                      Print build and push commands without executing them
  --platform <platform>          Build target platform (default: linux/amd64)
  --login / --no-login           Attempt docker login to ghcr.io before pushing (default: auto)
  --username, -u <user>          Username for registry login (default: detected via gh / git)
  --output-env <file>            Export published image URLs to a shell env file
  --output-json <file>           Export published image URLs to a JSON file (deployment.local.json format)
  -h, --help                     Show this help message

Examples:
  # 1. Build and publish all images to GitHub Container Registry (auto-detects org/repo):
  ./hack/build-and-publish-images.sh

  # 2. Dry run preview of all commands:
  ./hack/build-and-publish-images.sh --dry-run

  # 3. Publish to a specific GitHub user/organization:
  ./hack/build-and-publish-images.sh --owner my-org

  # 4. Build and publish only standalone provider images needed by deploy_standalone.sh:
  ./hack/build-and-publish-images.sh --provider-images

  # 5. Build only JupyterLab for GPU and VS Code for CPU locally without pushing:
  ./hack/build-and-publish-images.sh --jupyterlab --gpu --codeserver --cpu --no-push

  # 6. Use git commit SHA for tagging:
  ./hack/build-and-publish-images.sh --git-tag
EOF
}

# ------------------------------------------------------------------------------
# Parse Command-Line Options
# ------------------------------------------------------------------------------
POSITIONAL_ARGS=()
CUSTOM_OWNER=""
CUSTOM_REPO=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --all)
      BUILD_ALL=true
      shift
      ;;
    --workspace-images|--images|--workspaces)
      BUILD_WORKSPACE_IMAGES=true
      shift
      ;;
    --provider-images|--standalone|--standalone-images|--gke|--gke-images)
      BUILD_PROVIDER_IMAGES=true
      shift
      ;;
    --codeserver|--codeserver-python)
      SELECTED_WORKSPACE_IMAGES+=("codeserver")
      shift
      ;;
    --jupyterlab)
      SELECTED_WORKSPACE_IMAGES+=("jupyterlab")
      shift
      ;;
    --spark|--spark-py312)
      SELECTED_WORKSPACE_IMAGES+=("spark")
      shift
      ;;
    --mcp-server|--agent-sandbox-mcp-server)
      SELECTED_WORKSPACE_IMAGES+=("mcp-server")
      shift
      ;;
    --include-mcp-server)
      INCLUDE_MCP_SERVER=true
      shift
      ;;
    --no-mcp-server)
      INCLUDE_MCP_SERVER=false
      shift
      ;;
    --access-proxy|--gke-access-proxy)
      SELECTED_PROVIDER_IMAGES+=("access-proxy")
      shift
      ;;
    --snapshot-addon|--gke-snapshot-addon)
      SELECTED_PROVIDER_IMAGES+=("snapshot-addon")
      shift
      ;;
    --frontend|--gke-frontend)
      SELECTED_PROVIDER_IMAGES+=("frontend")
      shift
      ;;
    --controller|--gke-controller)
      SELECTED_PROVIDER_IMAGES+=("controller")
      shift
      ;;
    --backend|--gke-backend)
      SELECTED_PROVIDER_IMAGES+=("backend")
      shift
      ;;
    --image|-i)
      case "$2" in
        codeserver|codeserver-python)
          SELECTED_WORKSPACE_IMAGES+=("codeserver")
          ;;
        jupyterlab)
          SELECTED_WORKSPACE_IMAGES+=("jupyterlab")
          ;;
        spark|spark-py312)
          SELECTED_WORKSPACE_IMAGES+=("spark")
          ;;
        mcp-server|agent-sandbox-mcp-server)
          SELECTED_WORKSPACE_IMAGES+=("mcp-server")
          ;;
        access-proxy|gke-access-proxy)
          SELECTED_PROVIDER_IMAGES+=("access-proxy")
          ;;
        snapshot-addon|gke-snapshot-addon)
          SELECTED_PROVIDER_IMAGES+=("snapshot-addon")
          ;;
        frontend|gke-frontend)
          SELECTED_PROVIDER_IMAGES+=("frontend")
          ;;
        controller|gke-controller)
          SELECTED_PROVIDER_IMAGES+=("controller")
          ;;
        backend|gke-backend)
          SELECTED_PROVIDER_IMAGES+=("backend")
          ;;
        *)
          echo "ERROR: Unknown image name '$2'. Allowed: codeserver, jupyterlab, spark, mcp-server, access-proxy, snapshot-addon, frontend, controller, backend" >&2
          exit 1
          ;;
      esac
      shift 2
      ;;
    --hardware|--hw|-hw|--variant|-v)
      TARGET_HARDWARE+=("$2")
      shift 2
      ;;
    --cpu)
      TARGET_HARDWARE+=("cpu")
      shift
      ;;
    --gpu)
      TARGET_HARDWARE+=("gpu")
      shift
      ;;
    --tpu)
      TARGET_HARDWARE+=("tpu")
      shift
      ;;
    --registry-path|--registry|-r)
      REGISTRY_PATH="$2"
      shift 2
      ;;
    --owner|--org|-o)
      CUSTOM_OWNER="$2"
      shift 2
      ;;
    --repo)
      CUSTOM_REPO="$2"
      shift 2
      ;;
    --tag|-t)
      IMAGE_TAG="$2"
      shift 2
      ;;
    --git-tag)
      USE_GIT_TAG=true
      shift
      ;;
    --latest)
      TAG_LATEST=true
      shift
      ;;
    --no-latest)
      TAG_LATEST=false
      shift
      ;;
    --alias-workspaces)
      ALIAS_WORKSPACES=true
      shift
      ;;
    --agent-sandbox-version)
      AGENT_SANDBOX_VERSION="$2"
      shift 2
      ;;
    --platform)
      PLATFORM="$2"
      shift 2
      ;;
    --push)
      PUSH_IMAGE=true
      shift
      ;;
    --no-push)
      PUSH_IMAGE=false
      shift
      ;;
    --dry-run)
      DRY_RUN=true
      shift
      ;;
    --login)
      DO_LOGIN="true"
      shift
      ;;
    --no-login)
      DO_LOGIN="false"
      shift
      ;;
    --username|-u)
      AUTH_USERNAME="$2"
      shift 2
      ;;
    --output-env)
      OUTPUT_ENV_FILE="$2"
      shift 2
      ;;
    --output-json)
      OUTPUT_JSON_FILE="$2"
      shift 2
      ;;
    -h|--help)
      show_usage
      exit 0
      ;;
    -*)
      echo "ERROR: Unknown option: $1" >&2
      show_usage
      exit 1
      ;;
    *)
      POSITIONAL_ARGS+=("$1")
      shift
      ;;
  esac
done

if [[ ${#POSITIONAL_ARGS[@]} -gt 0 ]]; then
  if [[ -z "${REGISTRY_PATH}" ]]; then
    REGISTRY_PATH="${POSITIONAL_ARGS[0]}"
  else
    echo "ERROR: Unexpected argument: ${POSITIONAL_ARGS[0]}" >&2
    show_usage
    exit 1
  fi
fi

# ------------------------------------------------------------------------------
# Helper: Detect GitHub Owner / Repository
# ------------------------------------------------------------------------------
detect_github_owner_repo() {
  local remote_url
  remote_url="$(git -C "${REPO_ROOT}" config --get remote.origin.url 2>/dev/null || true)"
  if [[ -z "${remote_url}" ]]; then
    remote_url="$(git -C "${REPO_ROOT}" config --get remote.upstream.url 2>/dev/null || true)"
  fi
  if [[ -n "${remote_url}" && "${remote_url}" =~ github\.com[:/]([^/]+)/([^/]+?)((\.git)?)$ ]]; then
    local owner="${BASH_REMATCH[1],,}"
    local repo="${BASH_REMATCH[2],,}"
    repo="${repo%.git}"
    echo "${owner}/${repo}"
    return 0
  fi
  return 1
}

# ------------------------------------------------------------------------------
# Resolve Registry Path
# ------------------------------------------------------------------------------
DETECTED_SLUG=""
if DETECTED_SLUG="$(detect_github_owner_repo 2>/dev/null)"; then
  DETECTED_OWNER="${DETECTED_SLUG%%/*}"
  DETECTED_REPO="${DETECTED_SLUG##*/}"
else
  DETECTED_OWNER=""
  DETECTED_REPO=""
fi

if [[ -n "${CUSTOM_OWNER}" ]]; then
  FINAL_OWNER="${CUSTOM_OWNER,,}"
  FINAL_REPO="${CUSTOM_REPO:-${DETECTED_REPO}}"
  if [[ -n "${FINAL_REPO}" ]]; then
    REGISTRY_PATH="ghcr.io/${FINAL_OWNER}/${FINAL_REPO}"
  else
    REGISTRY_PATH="ghcr.io/${FINAL_OWNER}"
  fi
elif [[ -z "${REGISTRY_PATH}" ]]; then
  if [[ -n "${GITHUB_REPOSITORY:-}" ]]; then
    REGISTRY_PATH="ghcr.io/${GITHUB_REPOSITORY,,}"
  elif [[ -n "${DETECTED_SLUG}" ]]; then
    REGISTRY_PATH="ghcr.io/${DETECTED_SLUG}"
  else
    echo "ERROR: Unable to auto-detect GitHub repository from git remotes." >&2
    echo "Please specify --registry <path> or --owner <owner>." >&2
    exit 1
  fi
fi

# Normalize: strip trailing slash and lowercase
REGISTRY_PATH="${REGISTRY_PATH%/}"
REGISTRY_PATH="${REGISTRY_PATH,,}"

# ------------------------------------------------------------------------------
# Resolve Image Tag
# ------------------------------------------------------------------------------
GIT_COMMIT="$(git -C "${REPO_ROOT}" rev-parse --short HEAD 2>/dev/null || true)"
GITHUB_REPO_URL="https://github.com/${DETECTED_SLUG:-gke-labs/workspaces}"

if [[ -z "${IMAGE_TAG}" ]]; then
  if [[ "${USE_GIT_TAG}" == "true" ]]; then
    if [[ -z "${GIT_COMMIT}" ]]; then
      echo "ERROR: Git repository commit not found; cannot use --git-tag." >&2
      exit 1
    fi
    IMAGE_TAG="sha-${GIT_COMMIT}"
  else
    IMAGE_TAG="v$(date -u +%Y%m%d-%H%M%S)"
  fi
fi

# ------------------------------------------------------------------------------
# Resolve Targets
# ------------------------------------------------------------------------------
FINAL_WORKSPACE_TARGETS=()
FINAL_PROVIDER_TARGETS=()

if [[ "${BUILD_ALL}" == "true" ]] || \
   ( [[ "${BUILD_WORKSPACE_IMAGES}" == "false" ]] && \
     [[ "${BUILD_PROVIDER_IMAGES}" == "false" ]] && \
     [[ ${#SELECTED_WORKSPACE_IMAGES[@]} -eq 0 ]] && \
     [[ ${#SELECTED_PROVIDER_IMAGES[@]} -eq 0 ]] ); then
  FINAL_WORKSPACE_TARGETS=("codeserver" "jupyterlab" "spark")
  if [[ "${INCLUDE_MCP_SERVER}" == "true" ]]; then
    FINAL_WORKSPACE_TARGETS+=("mcp-server")
  fi
  FINAL_PROVIDER_TARGETS=("access-proxy" "snapshot-addon" "frontend" "controller" "backend")
else
  if [[ "${BUILD_WORKSPACE_IMAGES}" == "true" ]]; then
    FINAL_WORKSPACE_TARGETS=("codeserver" "jupyterlab" "spark")
    if [[ "${INCLUDE_MCP_SERVER}" == "true" ]]; then
      FINAL_WORKSPACE_TARGETS+=("mcp-server")
    fi
  else
    for w in "${SELECTED_WORKSPACE_IMAGES[@]}"; do
      if [[ ! " ${FINAL_WORKSPACE_TARGETS[*]} " =~ " ${w} " ]]; then
        FINAL_WORKSPACE_TARGETS+=("${w}")
      fi
    done
  fi

  if [[ "${BUILD_PROVIDER_IMAGES}" == "true" ]]; then
    FINAL_PROVIDER_TARGETS=("access-proxy" "snapshot-addon" "frontend" "controller" "backend")
  else
    for p in "${SELECTED_PROVIDER_IMAGES[@]}"; do
      if [[ ! " ${FINAL_PROVIDER_TARGETS[*]} " =~ " ${p} " ]]; then
        FINAL_PROVIDER_TARGETS+=("${p}")
      fi
    done
  fi
fi

# Normalize Target Hardware for workspace images
RESOLVED_HARDWARE=()
if [[ ${#TARGET_HARDWARE[@]} -eq 0 ]] || [[ " ${TARGET_HARDWARE[*]} " == *" all "* ]]; then
  RESOLVED_HARDWARE=("cpu" "gpu" "tpu")
else
  for hw in "${TARGET_HARDWARE[@]}"; do
    case "${hw}" in
      cpu|gpu|tpu)
        if [[ ! " ${RESOLVED_HARDWARE[*]} " =~ " ${hw} " ]]; then
          RESOLVED_HARDWARE+=("${hw}")
        fi
        ;;
      *)
        echo "ERROR: Unknown hardware variant '${hw}'. Allowed: cpu, gpu, tpu, all" >&2
        exit 1
        ;;
    esac
  done
fi

# ------------------------------------------------------------------------------
# Display Configuration Summary
# ------------------------------------------------------------------------------
echo "=================================================================="
echo "GitHub Image Build & Publish Configuration:"
echo "  Registry Path:     ${REGISTRY_PATH}"
echo "  Tag:               ${IMAGE_TAG}"
echo "  Tag Latest:        ${TAG_LATEST}"
echo "  Push Images:       ${PUSH_IMAGE}"
echo "  Platform:          ${PLATFORM}"
echo "  Dry Run:           ${DRY_RUN}"
if [[ ${#FINAL_WORKSPACE_TARGETS[@]} -gt 0 ]]; then
  echo "  Workspace Targets: ${FINAL_WORKSPACE_TARGETS[*]}"
  echo "  Hardware Variants: ${RESOLVED_HARDWARE[*]}"
fi
if [[ ${#FINAL_PROVIDER_TARGETS[@]} -gt 0 ]]; then
  echo "  Provider Targets:  ${FINAL_PROVIDER_TARGETS[*]}"
fi
echo "=================================================================="

# ------------------------------------------------------------------------------
# GitHub Container Registry (ghcr.io) Authentication
# ------------------------------------------------------------------------------
if [[ "${PUSH_IMAGE}" == "true" && "${DRY_RUN}" == "false" && "${DO_LOGIN}" != "false" ]]; then
  if [[ "${REGISTRY_PATH}" == ghcr.io* ]]; then
    TOKEN="${CR_PAT:-${GITHUB_TOKEN:-${GH_TOKEN:-}}}"
    if [[ -z "${TOKEN}" ]] && command -v gh >/dev/null 2>&1; then
      TOKEN="$(gh auth token 2>/dev/null || true)"
    fi

    if [[ -z "${AUTH_USERNAME}" ]]; then
      if command -v gh >/dev/null 2>&1; then
        AUTH_USERNAME="$(gh api user -q .login 2>/dev/null || true)"
      fi
      if [[ -z "${AUTH_USERNAME}" ]]; then
        AUTH_USERNAME="$(git -C "${REPO_ROOT}" config --get user.name 2>/dev/null || echo "")"
      fi
      if [[ -z "${AUTH_USERNAME}" && -n "${DETECTED_OWNER}" ]]; then
        AUTH_USERNAME="${DETECTED_OWNER}"
      fi
    fi

    if [[ -n "${TOKEN}" && -n "${AUTH_USERNAME}" ]]; then
      echo "Logging in to GitHub Container Registry (ghcr.io) as ${AUTH_USERNAME}..."
      if echo "${TOKEN}" | docker login ghcr.io -u "${AUTH_USERNAME}" --password-stdin >/dev/null 2>&1; then
        echo "Successfully authenticated to ghcr.io."
      else
        echo "WARNING: Automatic docker login to ghcr.io failed. Proceeding with existing docker credentials." >&2
      fi
    else
      echo "Notice: No GITHUB_TOKEN / CR_PAT found. Assuming docker is already authenticated to ghcr.io."
      echo "  (If push fails, authenticate with: gh auth token | docker login ghcr.io -u <user> --password-stdin)"
    fi
  fi
fi

ALL_BUILT_IMAGES=()

# ------------------------------------------------------------------------------
# Step 1: Build & Publish Custom Workspace Images (images/)
# ------------------------------------------------------------------------------
if [[ ${#FINAL_WORKSPACE_TARGETS[@]} -gt 0 ]]; then
  echo ""
  echo "=================================================================="
  echo "Step 1: Building Custom Workspace Images (images/build.sh)..."
  echo "=================================================================="

  BUILD_SH_ARGS=(
    "--registry-path" "${REGISTRY_PATH}"
    "--tag" "${IMAGE_TAG}"
    "--agent-sandbox-version" "${AGENT_SANDBOX_VERSION}"
  )

  for img in "${FINAL_WORKSPACE_TARGETS[@]}"; do
    BUILD_SH_ARGS+=("--image" "${img}")
  done

  for hw in "${RESOLVED_HARDWARE[@]}"; do
    BUILD_SH_ARGS+=("--hardware" "${hw}")
  done

  if [[ "${PUSH_IMAGE}" == "false" ]]; then
    BUILD_SH_ARGS+=("--no-push")
  else
    BUILD_SH_ARGS+=("--push")
  fi

  if [[ "${DRY_RUN}" == "true" ]]; then
    BUILD_SH_ARGS+=("--dry-run")
  fi

  "${IMAGES_DIR}/build.sh" "${BUILD_SH_ARGS[@]}"

  # Collect expected workspace image URIs
  for img in "${FINAL_WORKSPACE_TARGETS[@]}"; do
    case "${img}" in
      codeserver)
        base="${REGISTRY_PATH}/codeserver-python"
        for hw in "${RESOLVED_HARDWARE[@]}"; do
          ALL_BUILT_IMAGES+=("${base}:${IMAGE_TAG}-${hw}")
          if [[ "${TAG_LATEST}" == "true" ]]; then
            ALL_BUILT_IMAGES+=("${base}:latest-${hw}")
          fi
          if [[ "${hw}" == "cpu" ]]; then
            ALL_BUILT_IMAGES+=("${base}:${IMAGE_TAG}")
            if [[ "${TAG_LATEST}" == "true" ]]; then
              ALL_BUILT_IMAGES+=("${base}:latest")
            fi
          fi
        done
        ;;
      jupyterlab)
        base="${REGISTRY_PATH}/jupyterlab"
        for hw in "${RESOLVED_HARDWARE[@]}"; do
          ALL_BUILT_IMAGES+=("${base}:${IMAGE_TAG}-${hw}")
          if [[ "${TAG_LATEST}" == "true" ]]; then
            ALL_BUILT_IMAGES+=("${base}:latest-${hw}")
          fi
          if [[ "${hw}" == "cpu" ]]; then
            ALL_BUILT_IMAGES+=("${base}:${IMAGE_TAG}")
            if [[ "${TAG_LATEST}" == "true" ]]; then
              ALL_BUILT_IMAGES+=("${base}:latest")
            fi
          fi
        done
        ;;
      spark)
        ALL_BUILT_IMAGES+=("${REGISTRY_PATH}/spark-py312:${IMAGE_TAG}")
        if [[ "${TAG_LATEST}" == "true" ]]; then
          ALL_BUILT_IMAGES+=("${REGISTRY_PATH}/spark-py312:latest")
        fi
        ;;
      mcp-server)
        ALL_BUILT_IMAGES+=("${REGISTRY_PATH}/agent-sandbox-mcp-server:${IMAGE_TAG}")
        if [[ "${TAG_LATEST}" == "true" ]]; then
          ALL_BUILT_IMAGES+=("${REGISTRY_PATH}/agent-sandbox-mcp-server:latest")
        fi
        ;;
    esac
  done
fi

# ------------------------------------------------------------------------------
# Step 2: Build & Publish Standalone Provider Images (providers/gke/)
# ------------------------------------------------------------------------------
build_provider_image() {
  local comp_name="$1"
  local image_name="$2"
  local context_dir="$3"
  local dockerfile_path="$4"
  local upstream_alias="${5:-}"

  local primary_tag="${REGISTRY_PATH}/${image_name}:${IMAGE_TAG}"
  local latest_tag="${REGISTRY_PATH}/${image_name}:latest"

  echo ""
  echo "=================================================================="
  echo "Building [${comp_name}] -> ${primary_tag}"
  echo "  Context:    ${context_dir}"
  echo "  Dockerfile: ${dockerfile_path}"
  echo "=================================================================="

  local tags=(-t "${primary_tag}")
  if [[ "${TAG_LATEST}" == "true" ]]; then
    tags+=(-t "${latest_tag}")
  fi

  if [[ -n "${upstream_alias}" && "${ALIAS_WORKSPACES}" == "true" ]]; then
    tags+=(-t "${REGISTRY_PATH}/${upstream_alias}:${IMAGE_TAG}")
    if [[ "${TAG_LATEST}" == "true" ]]; then
      tags+=(-t "${REGISTRY_PATH}/${upstream_alias}:latest")
    fi
  fi

  local oci_labels=()
  if [[ -n "${GITHUB_REPO_URL}" ]]; then
    oci_labels+=(--label "org.opencontainers.image.source=${GITHUB_REPO_URL}")
  fi
  oci_labels+=(--label "org.opencontainers.image.created=$(date -u +'%Y-%m-%dT%H:%M:%SZ')")
  if [[ -n "${GIT_COMMIT}" ]]; then
    oci_labels+=(--label "org.opencontainers.image.revision=${GIT_COMMIT}")
  fi

  if [[ "${DRY_RUN}" == "true" ]]; then
    echo "[DRY-RUN] docker build --platform ${PLATFORM} ${oci_labels[*]} -f ${dockerfile_path} ${tags[*]} ${context_dir}"
    if [[ "${PUSH_IMAGE}" == "true" ]]; then
      echo "[DRY-RUN] docker push ${primary_tag}"
      if [[ "${TAG_LATEST}" == "true" ]]; then
        echo "[DRY-RUN] docker push ${latest_tag}"
      fi
      if [[ -n "${upstream_alias}" && "${ALIAS_WORKSPACES}" == "true" ]]; then
        echo "[DRY-RUN] docker push ${REGISTRY_PATH}/${upstream_alias}:${IMAGE_TAG}"
        if [[ "${TAG_LATEST}" == "true" ]]; then
          echo "[DRY-RUN] docker push ${REGISTRY_PATH}/${upstream_alias}:latest"
        fi
      fi
    fi
  else
    docker build \
      --platform "${PLATFORM}" \
      "${oci_labels[@]}" \
      -f "${dockerfile_path}" \
      "${tags[@]}" \
      "${context_dir}"

    if [[ "${PUSH_IMAGE}" == "true" ]]; then
      echo "Pushing ${primary_tag}..."
      docker push "${primary_tag}"
      if [[ "${TAG_LATEST}" == "true" ]]; then
        echo "Pushing ${latest_tag}..."
        docker push "${latest_tag}"
      fi
      if [[ -n "${upstream_alias}" && "${ALIAS_WORKSPACES}" == "true" ]]; then
        echo "Pushing ${REGISTRY_PATH}/${upstream_alias}:${IMAGE_TAG}..."
        docker push "${REGISTRY_PATH}/${upstream_alias}:${IMAGE_TAG}"
        if [[ "${TAG_LATEST}" == "true" ]]; then
          echo "Pushing ${REGISTRY_PATH}/${upstream_alias}:latest..."
          docker push "${REGISTRY_PATH}/${upstream_alias}:latest"
        fi
      fi
    fi
  fi

  ALL_BUILT_IMAGES+=("${primary_tag}")
  if [[ "${TAG_LATEST}" == "true" ]]; then
    ALL_BUILT_IMAGES+=("${latest_tag}")
  fi
  if [[ -n "${upstream_alias}" && "${ALIAS_WORKSPACES}" == "true" ]]; then
    ALL_BUILT_IMAGES+=("${REGISTRY_PATH}/${upstream_alias}:${IMAGE_TAG}")
    if [[ "${TAG_LATEST}" == "true" ]]; then
      ALL_BUILT_IMAGES+=("${REGISTRY_PATH}/${upstream_alias}:latest")
    fi
  fi
}

if [[ ${#FINAL_PROVIDER_TARGETS[@]} -gt 0 ]]; then
  echo ""
  echo "=================================================================="
  echo "Step 2: Building Standalone Provider Images (providers/gke/)..."
  echo "=================================================================="

  for target in "${FINAL_PROVIDER_TARGETS[@]}"; do
    case "${target}" in
      access-proxy)
        build_provider_image \
          "GKE Access Proxy" \
          "gke-access-proxy" \
          "${PROVIDERS_GKE_DIR}" \
          "${PROVIDERS_GKE_DIR}/Dockerfile"
        ;;
      snapshot-addon)
        build_provider_image \
          "GKE Snapshot Addon" \
          "gke-snapshot-addon" \
          "${PROVIDERS_GKE_DIR}" \
          "${PROVIDERS_GKE_DIR}/snapshot.Dockerfile"
        ;;
      frontend)
        build_provider_image \
          "GKE Frontend" \
          "gke-frontend" \
          "${REPO_ROOT}" \
          "${PROVIDERS_GKE_DIR}/frontend.Dockerfile" \
          "workspaces-frontend"
        ;;
      controller)
        build_provider_image \
          "GKE Controller" \
          "gke-controller" \
          "${REPO_ROOT}/workspaces/controller" \
          "${REPO_ROOT}/workspaces/controller/Dockerfile" \
          "workspaces-controller"
        ;;
      backend)
        build_provider_image \
          "GKE Backend" \
          "gke-backend" \
          "${REPO_ROOT}/workspaces" \
          "${REPO_ROOT}/workspaces/backend/Dockerfile" \
          "workspaces-backend"
        ;;
    esac
  done
fi

# ------------------------------------------------------------------------------
# Export Outputs (Optional --output-env / --output-json)
# ------------------------------------------------------------------------------
if [[ -n "${OUTPUT_ENV_FILE}" ]]; then
  echo "Writing environment variables to ${OUTPUT_ENV_FILE}..."
  mkdir -p "$(dirname "${OUTPUT_ENV_FILE}")"
  cat <<EOF > "${OUTPUT_ENV_FILE}"
export REGISTRY="${REGISTRY_PATH}"
export TAG="${IMAGE_TAG}"
export PROXY_IMAGE="${REGISTRY_PATH}/gke-access-proxy:${IMAGE_TAG}"
export SNAPSHOT_IMAGE="${REGISTRY_PATH}/gke-snapshot-addon:${IMAGE_TAG}"
export FRONTEND_IMAGE="${REGISTRY_PATH}/gke-frontend:${IMAGE_TAG}"
export CONTROLLER_IMAGE="${REGISTRY_PATH}/gke-controller:${IMAGE_TAG}"
export BACKEND_IMAGE="${REGISTRY_PATH}/gke-backend:${IMAGE_TAG}"
export JUPYTERLAB_IMAGE="${REGISTRY_PATH}/jupyterlab:${IMAGE_TAG}"
export CODESERVER_IMAGE="${REGISTRY_PATH}/codeserver-python:${IMAGE_TAG}"
export SPARK_IMAGE="${REGISTRY_PATH}/spark-py312:${IMAGE_TAG}"
export MCP_SERVER_IMAGE="${REGISTRY_PATH}/agent-sandbox-mcp-server:${IMAGE_TAG}"
EOF
fi

if [[ -n "${OUTPUT_JSON_FILE}" ]]; then
  echo "Writing JSON deployment coordinates to ${OUTPUT_JSON_FILE}..."
  mkdir -p "$(dirname "${OUTPUT_JSON_FILE}")"
  cat <<EOF > "${OUTPUT_JSON_FILE}"
{
  "images": {
    "proxy": "${REGISTRY_PATH}/gke-access-proxy:${IMAGE_TAG}",
    "snapshot": "${REGISTRY_PATH}/gke-snapshot-addon:${IMAGE_TAG}",
    "frontend": "${REGISTRY_PATH}/gke-frontend:${IMAGE_TAG}",
    "controller": "${REGISTRY_PATH}/gke-controller:${IMAGE_TAG}",
    "backend": "${REGISTRY_PATH}/gke-backend:${IMAGE_TAG}"
  }
}
EOF
fi

# ------------------------------------------------------------------------------
# Final Execution Summary
# ------------------------------------------------------------------------------
echo ""
echo "=================================================================="
if [[ "${DRY_RUN}" == "true" ]]; then
  echo "Dry Run Completed. Images planned:"
elif [[ "${PUSH_IMAGE}" == "true" ]]; then
  echo "Successfully built and published images to GitHub:"
else
  echo "Successfully built images locally (push skipped):"
fi
for img in "${ALL_BUILT_IMAGES[@]}"; do
  echo "  - ${img}"
done
echo "=================================================================="
echo ""
echo "Deployment Usage Notes:"
echo "1. To reference standalone images in providers/gke/deploy_standalone.sh:"
echo "   export REGISTRY=\"${REGISTRY_PATH}\""
echo "   export TAG=\"${IMAGE_TAG}\""
echo "   export BUILD_IMAGES=false"
echo ""
echo "2. To render WorkspaceKinds using these custom images:"
echo "   See images/README.md for instructions on deploying jupyterlab and codeserver-python WorkspaceKinds."
echo ""
echo "Done!"
