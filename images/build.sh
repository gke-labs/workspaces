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
# Cloud-Agnostic Build and Push Script for Custom Kubeflow Images:
# - VS Code (codeserver-python) [CPU, CUDA GPU, TPU]
# - JupyterLab [CPU, CUDA GPU, TPU]
# - Apache Spark (spark-py312)
#
# Copies sample notebooks from images/samples into the images.
# Users pass the container registry path via --registry-path (or REGISTRY env var).
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

SAMPLES_DIR="${SCRIPT_DIR}/samples"
CODESERVER_DIR="${SCRIPT_DIR}/codeserver-python"
JUPYTERLAB_DIR="${SCRIPT_DIR}/jupyterlab"
SPARK_DIR="${SCRIPT_DIR}/spark"
MCP_SERVER_DIR="${SCRIPT_DIR}/agent-sandbox-mcp-server"

# Default Configuration & Environment Variables
REGISTRY_PATH="${REGISTRY_PATH:-${REGISTRY:-}}"
IMAGE_TAG="${IMAGE_TAG:-}"
# Upstream kubernetes-sigs/agent-sandbox release used for the MCP server image.
# Forwarded to the Dockerfile's AGENT_SANDBOX_VERSION build arg. Keep this in
# step with AGENT_SANDBOX_VERSION in examples/agent-sandbox/deploy_agent_sandbox.sh,
# which selects the matching sandbox runtime image.
AGENT_SANDBOX_VERSION="${AGENT_SANDBOX_VERSION:-v1.0.3}"

TARGET_IMAGES=()
TARGET_HARDWARE=()
USE_CLOUD_BUILD=false
PUSH_IMAGE=true
DRY_RUN=false
INSTALL_TORCH_TPU="${INSTALL_TORCH_TPU:-false}"
TORCH_TPU_TOKEN="${TORCH_TPU_TOKEN:-}"

show_usage() {
  cat <<EOF
Usage: $(basename "$0") [options] [registry-path]

Cloud-agnostic build and push script for custom Kubeflow workspace images and Spark images.

Images and Hardware Variants:
  codeserver   VS Code (codeserver-python) [cpu, gpu, tpu]
  jupyterlab   JupyterLab (jupyterlab)     [cpu, gpu, tpu]
  spark        Apache Spark (spark-py312)  [generic/cpu]
  mcp-server   Agent Sandbox MCP server (agent-sandbox-mcp-server) [generic/cpu]

Target Selection Options:
  --image, -i <name>        Select target image: codeserver, jupyterlab, spark, mcp-server, or all
                            (default: all, which excludes mcp-server)
  --codeserver              Shorthand to build codeserver-python
                            (alias: --codeserver-python)
  --jupyterlab              Shorthand to build jupyterlab
  --spark                   Shorthand to build spark-py312
  --mcp-server              Shorthand to build the Agent Sandbox MCP server
                            (alias: --agent-sandbox-mcp-server)
                            (needed by examples/agent-sandbox; not built by --all)
  --all                     Build codeserver, jupyterlab and spark (default)

Hardware Accelerator Options:
  --hardware, --hw, -hw,    Select accelerator variant: cpu, gpu, tpu, or all (default: all)
  --variant, -v <variant>
  --cpu                     Shorthand for hardware variant 'cpu'
  --gpu                     Shorthand for hardware variant 'gpu'
  --tpu                     Shorthand for hardware variant 'tpu'
  --torch-tpu               Install optional torch_tpu in TPU images
                            (requires access to https://github.com/google-pytorch/torch_tpu
                            and gcloud auth; auto-fetches access token)
  --torch-tpu-token <token> Explicit access token for torch_tpu registry

Registry & Tag Options:
  --registry-path, -r <path> Container registry path (aliases: --registry; or pass as a
                             positional argument, or set REGISTRY_PATH or REGISTRY).
                             The flag wins over the environment.
                             Example: us-central1-docker.pkg.dev/my-proj/notebooks
                             Images produced:
                               <registry-path>/codeserver-python:<tag>
                               <registry-path>/jupyterlab:<tag>
                               <registry-path>/spark-py312:<tag>
                               <registry-path>/agent-sandbox-mcp-server:<tag>
  --tag, -t <tag>            Image tag prefix (env var: IMAGE_TAG; default: timestamp,
                             e.g. v20260922-120000). The hardware suffix -cpu/-gpu/-tpu
                             is appended automatically; do not include one here.
  --agent-sandbox-version <v> Upstream kubernetes-sigs/agent-sandbox release to build the
                             MCP server from (env var: AGENT_SANDBOX_VERSION;
                             default: v1.0.3). Only affects --mcp-server. Keep in step
                             with AGENT_SANDBOX_VERSION in
                             examples/agent-sandbox/deploy_agent_sandbox.sh.

Execution Options:
  --no-push                  Build locally only; do not push to container registry
  --push                     Push images to registry after build (default: true)
  --cloud-build              Use Google Cloud Build (gcloud builds submit) instead of local Docker
  --dry-run                  Print build plan and commands without executing
  -h, --help                 Show this help message

Examples:
  # 1. Build all images under a registry path:
  ./build.sh --all --registry-path us-central1-docker.pkg.dev/my-proj/notebooks

  # 2. Build only VS Code for NVIDIA GPU:
  ./build.sh --codeserver --gpu --registry-path us-central1-docker.pkg.dev/my-proj/notebooks

  # 3. Build JupyterLab for Cloud TPU passing registry as positional argument:
  ./build.sh --jupyterlab --tpu us-central1-docker.pkg.dev/my-proj/notebooks

  # 4. Build Apache Spark:
  ./build.sh --spark --registry-path us-central1-docker.pkg.dev/my-proj/notebooks

  # 4b. Build the Agent Sandbox MCP server (for examples/agent-sandbox):
  ./build.sh --mcp-server --registry-path us-central1-docker.pkg.dev/my-proj/notebooks

  # 5. Build locally without pushing (local testing):
  ./build.sh --codeserver --cpu --no-push my-local-repo

  # 6. Dry run preview of all commands:
  ./build.sh --all --registry-path us-central1-docker.pkg.dev/my-proj/notebooks --dry-run
EOF
}

# Parse Command-Line Options
while [[ $# -gt 0 ]]; do
  case "$1" in
    --image|-i)
      TARGET_IMAGES+=("$2")
      shift 2
      ;;
    --codeserver|--codeserver-python)
      TARGET_IMAGES+=("codeserver")
      shift
      ;;
    --jupyterlab)
      TARGET_IMAGES+=("jupyterlab")
      shift
      ;;
    --spark)
      TARGET_IMAGES+=("spark")
      shift
      ;;
    --mcp-server|--agent-sandbox-mcp-server)
      TARGET_IMAGES+=("mcp-server")
      shift
      ;;
    --all)
      TARGET_IMAGES=("all")
      TARGET_HARDWARE=("all")
      shift
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
    --torch-tpu|--install-torch-tpu)
      INSTALL_TORCH_TPU=true
      shift
      ;;
    --torch-tpu-token)
      TORCH_TPU_TOKEN="$2"
      INSTALL_TORCH_TPU=true
      shift 2
      ;;
    --registry-path|--registry|-r)
      REGISTRY_PATH="$2"
      shift 2
      ;;
    --tag|-t)
      IMAGE_TAG="$2"
      shift 2
      ;;
    --agent-sandbox-version)
      AGENT_SANDBOX_VERSION="$2"
      shift 2
      ;;
    --cloud-build)
      USE_CLOUD_BUILD=true
      shift
      ;;
    --dry-run)
      DRY_RUN=true
      shift
      ;;
    --no-push)
      PUSH_IMAGE=false
      shift
      ;;
    --push)
      PUSH_IMAGE=true
      shift
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
      # Positional argument treated as REGISTRY_PATH
      if [[ -z "${REGISTRY_PATH}" ]]; then
        REGISTRY_PATH="$1"
      else
        echo "ERROR: Unexpected argument: $1" >&2
        show_usage
        exit 1
      fi
      shift
      ;;
  esac
done

# Validate Registry Path
if [[ -z "${REGISTRY_PATH}" ]]; then
  echo "ERROR: Registry path is required." >&2
  echo "Specify --registry-path <path>, export REGISTRY=<path>, or pass registry path as an argument." >&2
  echo "" >&2
  echo "Example:" >&2
  echo "  export REGISTRY=\"\${REGION}-docker.pkg.dev/\${PROJECT_ID}/\${REPO_NAME}\"" >&2
  echo "  $(basename "$0") --all --registry-path \"\${REGISTRY}\"" >&2
  exit 1
fi

# Strip trailing slashes
REGISTRY_PATH="${REGISTRY_PATH%/}"

# Default Tag if unset
if [[ -z "${IMAGE_TAG}" ]]; then
  IMAGE_TAG="v$(date -u +%Y%m%d-%H%M%S)"
fi

# Normalize Target Images
RESOLVED_IMAGES=()
if [[ ${#TARGET_IMAGES[@]} -eq 0 ]] || [[ " ${TARGET_IMAGES[*]} " == *" all "* ]]; then
  RESOLVED_IMAGES=("codeserver" "jupyterlab" "spark")
else
  for img in "${TARGET_IMAGES[@]}"; do
    case "${img}" in
      codeserver|codeserver-python)
        if [[ ! " ${RESOLVED_IMAGES[*]} " =~ " codeserver " ]]; then
          RESOLVED_IMAGES+=("codeserver")
        fi
        ;;
      jupyterlab)
        if [[ ! " ${RESOLVED_IMAGES[*]} " =~ " jupyterlab " ]]; then
          RESOLVED_IMAGES+=("jupyterlab")
        fi
        ;;
      spark)
        if [[ ! " ${RESOLVED_IMAGES[*]} " =~ " spark " ]]; then
          RESOLVED_IMAGES+=("spark")
        fi
        ;;
      mcp-server|agent-sandbox-mcp-server)
        if [[ ! " ${RESOLVED_IMAGES[*]} " =~ " mcp-server " ]]; then
          RESOLVED_IMAGES+=("mcp-server")
        fi
        ;;
      *)
        echo "ERROR: Unknown target image '${img}'. Allowed: codeserver, jupyterlab, spark, mcp-server, all" >&2
        exit 1
        ;;
    esac
  done
fi

# Normalize Target Hardware
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
      spark)
        if [[ ! " ${RESOLVED_IMAGES[*]} " =~ " spark " ]]; then
          RESOLVED_IMAGES+=("spark")
        fi
        ;;
      *)
        echo "ERROR: Unknown hardware variant '${hw}'. Allowed: cpu, gpu, tpu, all" >&2
        exit 1
        ;;
    esac
  done
  if [[ ${#RESOLVED_HARDWARE[@]} -eq 0 ]]; then
    RESOLVED_HARDWARE=("cpu")
  fi
fi

# Resolve Target Image Base Path
get_target_image_base() {
  local target="$1"
  local base_name
  case "${target}" in
    codeserver)
      base_name="codeserver-python"
      ;;
    jupyterlab)
      base_name="jupyterlab"
      ;;
    spark)
      base_name="spark-py312"
      ;;
    mcp-server)
      base_name="agent-sandbox-mcp-server"
      ;;
  esac

  # If user already included the image name at the end of REGISTRY_PATH when building a single target, avoid duplicating
  if [[ "${REGISTRY_PATH}" == *"${base_name}" ]]; then
    echo "${REGISTRY_PATH}"
  else
    echo "${REGISTRY_PATH}/${base_name}"
  fi
}

echo "=================================================================="
echo "Custom Images Build Configuration:"
echo "  Registry Path:    ${REGISTRY_PATH}"
echo "  Target Images:    ${RESOLVED_IMAGES[*]}"
echo "  Target Hardware:  ${RESOLVED_HARDWARE[*]}"
echo "  Tag:              ${IMAGE_TAG}"
echo "  Push Image:       ${PUSH_IMAGE}"
echo "  Use Cloud Build:  ${USE_CLOUD_BUILD}"
echo "  Install torch_tpu: ${INSTALL_TORCH_TPU}"
echo "  Samples Dir:      ${SAMPLES_DIR}"
for t in "${RESOLVED_IMAGES[@]}"; do
  echo "  Image [${t}]:     $(get_target_image_base "${t}")"
done
echo "=================================================================="

# ==============================================================================
# Build & Push Function
# ==============================================================================
BUILT_IMAGES=()

build_and_tag_image() {
  local context_dir="$1"
  local dockerfile_rel="$2"
  local base_image_uri="$3"
  local tag_suffix="$4"
  local desc="$5"
  local is_cpu_primary="${6:-false}"
  local is_spark="${7:-false}"
  # 8th arg: space-separated KEY=VALUE pairs forwarded as docker --build-arg.
  local build_args_raw="${8:-}"
  local build_args=()
  local docker_build_args=()
  if [[ -n "${build_args_raw}" ]]; then
    read -r -a build_args <<< "${build_args_raw}"
    local kv
    for kv in "${build_args[@]}"; do
      docker_build_args+=(--build-arg "${kv}")
    done
  fi

  local v_tag="${IMAGE_TAG}${tag_suffix}"
  local v_latest_tag="latest${tag_suffix}"

  local v_image_uri="${base_image_uri}:${v_tag}"
  local v_latest_uri="${base_image_uri}:${v_latest_tag}"

  echo ""
  echo "=================================================================="
  echo "Building [${desc}] -> ${v_image_uri} (${v_latest_uri})"
  echo "Dockerfile: ${context_dir}/${dockerfile_rel}"
  echo "=================================================================="

  if [[ "${DRY_RUN}" == "true" ]]; then
    echo "[DRY-RUN] Context: ${context_dir}"
    echo "[DRY-RUN] Dockerfile: ${context_dir}/${dockerfile_rel}"
    if [[ "${is_spark}" != "true" && -d "${SAMPLES_DIR}" ]]; then
      echo "[DRY-RUN] Copying samples from ${SAMPLES_DIR} to build context samples/"
    fi
    local dry_build_args=""
    if [[ ${#docker_build_args[@]} -gt 0 ]]; then
      dry_build_args="${docker_build_args[*]} "
    fi
    if [[ "${USE_CLOUD_BUILD}" == "true" ]]; then
      if [[ -z "${dry_build_args}" ]]; then
        echo "[DRY-RUN] gcloud builds submit <context> --tag=${v_image_uri}"
      else
        echo "[DRY-RUN] gcloud builds submit <context> --config=<generated cloudbuild.yaml> (${dry_build_args% })"
      fi
      if [[ "${v_image_uri}" != "${v_latest_uri}" ]]; then
        echo "[DRY-RUN] gcloud container images add-tag ${v_image_uri} ${v_latest_uri} --quiet"
      fi
      if [[ "${is_cpu_primary}" == "true" ]]; then
        echo "[DRY-RUN] gcloud container images add-tag ${v_image_uri} ${base_image_uri}:${IMAGE_TAG} --quiet"
        echo "[DRY-RUN] gcloud container images add-tag ${v_image_uri} ${base_image_uri}:latest --quiet"
      fi
    else
      echo "[DRY-RUN] docker build --platform linux/amd64 ${dry_build_args}-f <context>/Dockerfile -t ${v_image_uri} -t ${v_latest_uri} <context>"
      if [[ "${PUSH_IMAGE}" == "true" ]]; then
        echo "[DRY-RUN] docker push ${v_image_uri}"
        if [[ "${v_image_uri}" != "${v_latest_uri}" ]]; then
          echo "[DRY-RUN] docker push ${v_latest_uri}"
        fi
      fi
      if [[ "${is_cpu_primary}" == "true" ]]; then
        echo "[DRY-RUN] docker tag ${v_image_uri} ${base_image_uri}:${IMAGE_TAG}"
        echo "[DRY-RUN] docker tag ${v_image_uri} ${base_image_uri}:latest"
        if [[ "${PUSH_IMAGE}" == "true" ]]; then
          echo "[DRY-RUN] docker push ${base_image_uri}:${IMAGE_TAG}"
          echo "[DRY-RUN] docker push ${base_image_uri}:latest"
        fi
      fi
    fi
    BUILT_IMAGES+=("${v_image_uri}" "${v_latest_uri}")
    if [[ "${is_cpu_primary}" == "true" ]]; then
      BUILT_IMAGES+=("${base_image_uri}:${IMAGE_TAG}" "${base_image_uri}:latest")
    fi
    return
  fi

  local tmp_build_dir
  tmp_build_dir="$(mktemp -d)"

  # Copy context directory contents
  cp -r "${context_dir}/." "${tmp_build_dir}/"

  # Copy samples directory if image is not spark
  if [[ "${is_spark}" != "true" && -d "${SAMPLES_DIR}" ]]; then
    cp -r "${SAMPLES_DIR}" "${tmp_build_dir}/samples"
  fi

  # Place selected Dockerfile
  cp "${context_dir}/${dockerfile_rel}" "${tmp_build_dir}/Dockerfile"

  if [[ "${USE_CLOUD_BUILD}" == "true" ]]; then
    echo "Submitting build to Google Cloud Build..."
    if [[ ${#docker_build_args[@]} -eq 0 ]]; then
      gcloud builds submit "${tmp_build_dir}" --tag="${v_image_uri}"
    else
      # `gcloud builds submit --tag` accepts no build args, so emit a minimal
      # build config that forwards them rather than silently dropping them.
      {
        echo "steps:"
        echo "- name: gcr.io/cloud-builders/docker"
        echo "  args:"
        echo "  - build"
        local kv
        for kv in "${build_args[@]}"; do
          echo "  - --build-arg"
          echo "  - ${kv}"
        done
        echo "  - -t"
        echo "  - ${v_image_uri}"
        echo "  - ."
        echo "images:"
        echo "- ${v_image_uri}"
      } > "${tmp_build_dir}/cloudbuild.generated.yaml"
      gcloud builds submit "${tmp_build_dir}" \
        --config="${tmp_build_dir}/cloudbuild.generated.yaml"
    fi
    rm -rf "${tmp_build_dir}"

    if [[ "${v_image_uri}" != "${v_latest_uri}" ]]; then
      gcloud container images add-tag "${v_image_uri}" "${v_latest_uri}" --quiet
    fi

    if [[ "${is_cpu_primary}" == "true" ]]; then
      local default_image_uri="${base_image_uri}:${IMAGE_TAG}"
      local default_latest_uri="${base_image_uri}:latest"
      gcloud container images add-tag "${v_image_uri}" "${default_image_uri}" --quiet
      gcloud container images add-tag "${v_image_uri}" "${default_latest_uri}" --quiet
    fi
  else
    echo "Building locally with Docker..."
    docker build \
      --platform linux/amd64 \
      ${docker_build_args[@]+"${docker_build_args[@]}"} \
      -f "${tmp_build_dir}/Dockerfile" \
      -t "${v_image_uri}" \
      -t "${v_latest_uri}" \
      "${tmp_build_dir}"
    rm -rf "${tmp_build_dir}"

    if [[ "${PUSH_IMAGE}" == "true" ]]; then
      echo "Pushing ${v_image_uri}..."
      docker push "${v_image_uri}"
      if [[ "${v_image_uri}" != "${v_latest_uri}" ]]; then
        echo "Pushing ${v_latest_uri}..."
        docker push "${v_latest_uri}"
      fi
    fi

    if [[ "${is_cpu_primary}" == "true" ]]; then
      local default_image_uri="${base_image_uri}:${IMAGE_TAG}"
      local default_latest_uri="${base_image_uri}:latest"
      docker tag "${v_image_uri}" "${default_image_uri}"
      docker tag "${v_image_uri}" "${default_latest_uri}"
      if [[ "${PUSH_IMAGE}" == "true" ]]; then
        docker push "${default_image_uri}"
        docker push "${default_latest_uri}"
      fi
    fi
  fi

  BUILT_IMAGES+=("${v_image_uri}" "${v_latest_uri}")
  if [[ "${is_cpu_primary}" == "true" ]]; then
    BUILT_IMAGES+=("${base_image_uri}:${IMAGE_TAG}" "${base_image_uri}:latest")
  fi
}

# ==============================================================================
# Execute Builds
# ==============================================================================

# Helper to resolve TPU build args (e.g. optional torch_tpu installation)
get_tpu_build_args() {
  local tpu_args=""
  if [[ "${INSTALL_TORCH_TPU}" == "true" ]]; then
    local tpu_token="${TORCH_TPU_TOKEN}"
    if [[ -z "${tpu_token}" ]]; then
      tpu_token="$(gcloud auth application-default print-access-token 2>/dev/null || gcloud auth print-access-token 2>/dev/null || true)"
      tpu_token="$(echo "${tpu_token}" | tr -d '[:space:]')"
    fi
    tpu_args="INSTALL_TORCH_TPU=true TORCH_TPU_TOKEN=${tpu_token}"
  fi
  echo "${tpu_args}"
}

# A. Build Code-Server Python variants
if [[ " ${RESOLVED_IMAGES[*]} " =~ " codeserver " ]]; then
  cs_base="$(get_target_image_base "codeserver")"
  for hw in "${RESOLVED_HARDWARE[@]}"; do
    case "${hw}" in
      cpu)
        build_and_tag_image "${CODESERVER_DIR}" "Dockerfile" "${cs_base}" "-cpu" "VS Code (codeserver-python) CPU" "true" "false"
        ;;
      gpu)
        build_and_tag_image "${CODESERVER_DIR}" "Dockerfile.gpu" "${cs_base}" "-gpu" "VS Code (codeserver-python) CUDA GPU" "false" "false"
        ;;
      tpu)
        build_and_tag_image "${CODESERVER_DIR}" "Dockerfile.tpu" "${cs_base}" "-tpu" "VS Code (codeserver-python) TPU" "false" "false" "$(get_tpu_build_args)"
        ;;
    esac
  done
fi

# B. Build JupyterLab variants
if [[ " ${RESOLVED_IMAGES[*]} " =~ " jupyterlab " ]]; then
  jl_base="$(get_target_image_base "jupyterlab")"
  for hw in "${RESOLVED_HARDWARE[@]}"; do
    case "${hw}" in
      cpu)
        build_and_tag_image "${JUPYTERLAB_DIR}" "Dockerfile" "${jl_base}" "-cpu" "JupyterLab CPU" "true" "false"
        ;;
      gpu)
        build_and_tag_image "${JUPYTERLAB_DIR}" "Dockerfile.gpu" "${jl_base}" "-gpu" "JupyterLab CUDA GPU" "false" "false"
        ;;
      tpu)
        build_and_tag_image "${JUPYTERLAB_DIR}" "Dockerfile.tpu" "${jl_base}" "-tpu" "JupyterLab TPU" "false" "false" "$(get_tpu_build_args)"
        ;;
    esac
  done
fi

# C. Build Spark
if [[ " ${RESOLVED_IMAGES[*]} " =~ " spark " ]]; then
  spark_base="$(get_target_image_base "spark")"
  build_and_tag_image "${SPARK_DIR}" "Dockerfile" "${spark_base}" "" "Spark 4.0.1 (Python 3.12)" "false" "true"
fi

# D. Build the Agent Sandbox MCP server (not part of --all; opt in with --mcp-server)
if [[ " ${RESOLVED_IMAGES[*]} " =~ " mcp-server " ]]; then
  mcp_base="$(get_target_image_base "mcp-server")"
  build_and_tag_image "${MCP_SERVER_DIR}" "Dockerfile" "${mcp_base}" "" "Agent Sandbox MCP server ${AGENT_SANDBOX_VERSION}" "false" "true" \
    "AGENT_SANDBOX_VERSION=${AGENT_SANDBOX_VERSION}"
fi

echo ""
echo "=================================================================="
echo "Successfully built $( [[ "${PUSH_IMAGE}" == "true" ]] && echo "and pushed " )images:"
for img in "${BUILT_IMAGES[@]}"; do
  echo "  - ${img}"
done
echo "=================================================================="
echo "Done!"
