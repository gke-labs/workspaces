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
# Build and Push Custom Kubeflow Code-Server Python Image (CPU, GPU, TPU) & Spark Image
# to Google Artifact Registry for Standalone Kubeflow Workspaces on GKE
#
# Reference:
#   https://github.com/kubeflow/notebooks/tree/notebooks-v1/components/example-notebook-servers/codeserver-python
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONTEXT_DIR="${SCRIPT_DIR}/codeserver-python"
EXAMPLES_DIR="${SCRIPT_DIR}/examples"
MANIFESTS_DIR="${SCRIPT_DIR}/manifests/compute-classes"

# ==============================================================================
# 1. Default Configuration & Environment Variables
# ==============================================================================
export PROJECT_ID="${PROJECT_ID:-${PROJECT:-$(gcloud config get-value project 2>/dev/null || true)}}"
export REGION="${REGION:-us-central1}"
export REPO_NAME="${REPO_NAME:-${REPOSITORY:-notebooks}}"
export IMAGE_NAME="${IMAGE_NAME:-codeserver-python}"
# Default to a build timestamp so each build gets its own immutable tag and the
# WorkspaceKind can pin to it. Seconds are included on purpose: a bare date would be
# reused by a second build on the same day, which is the mutable-tag problem again.
# Override IMAGE_TAG to pin a build to a name of your choosing.
export IMAGE_TAG="${IMAGE_TAG:-v$(date -u +%Y%m%d-%H%M%S)}"
export TENANT_NAMESPACE="${TENANT_NAMESPACE:-${USER_NAMESPACE:-team-a}}"
export GCS_BUCKET="${GCS_BUCKET:-${TENANT_NAMESPACE}-bucket}"

VARIANT="all"
BUILD_MODE="fast"
USE_CLOUD_BUILD=false
PUSH_IMAGE=true
REGISTER_WSK=false

show_usage() {
  cat <<EOF
Usage: $(basename "$0") [options]

Build and push custom Kubeflow VS Code (codeserver-python) images (CPU, GPU, TPU) and Spark images
to Google Artifact Registry for standalone Kubeflow Workspaces on GKE.

All built images are tagged with both \${IMAGE_TAG} and 'latest'. IMAGE_TAG defaults to a
build timestamp (e.g. v20260917-161530) so every build is separately addressable, and the
WorkspaceKind pins the exact tag rather than the floating 'latest' one:
  \${REGION}-docker.pkg.dev/\${PROJECT_ID}/\${REPO_NAME}/\${IMAGE_NAME}:\${IMAGE_TAG}-cpu  (& :latest-cpu)
  \${REGION}-docker.pkg.dev/\${PROJECT_ID}/\${REPO_NAME}/\${IMAGE_NAME}:\${IMAGE_TAG}-gpu  (& :latest-gpu)
  \${REGION}-docker.pkg.dev/\${PROJECT_ID}/\${REPO_NAME}/\${IMAGE_NAME}:\${IMAGE_TAG}-tpu  (& :latest-tpu)
  \${REGION}-docker.pkg.dev/\${PROJECT_ID}/\${REPO_NAME}/spark-py312:\${IMAGE_TAG}          (& :latest)

Environment Variables:
  PROJECT_ID        Google Cloud Project ID (default: current gcloud project)
  REGION            Google Cloud Region (default: us-central1)
  REPO_NAME         Artifact Registry repository name (default: notebooks)
  IMAGE_NAME        Docker image name (default: codeserver-python)
  IMAGE_TAG         Docker image tag prefix (default: build timestamp)
  TENANT_NAMESPACE  Tenant namespace for notebooks (default: team-a)
  GCS_BUCKET        Shared GCS bucket for notebook pipeline (default: \${TENANT_NAMESPACE}-bucket)

Options:
  --variant <cpu|gpu|tpu|spark|all> Build specific variant or all 4 (default: all)
  --fast                            Fast layer build extending upstream codeserver-python:v1.11.0 (default)
  --full                            Full build from upstream codeserver:v1.11.0 (installs Conda/Python from scratch, CPU only)
  --cloud-build                     Use Google Cloud Build (gcloud builds submit) instead of local Docker
  --no-push                         Build locally only; do not push to Artifact Registry
  --register-workspacekind          Apply/update 'codeserver' WorkspaceKind and GKE ComputeClasses in the current cluster
  -h, --help                        Show this help message
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --variant)
      VARIANT="$2"
      shift 2
      ;;
    --fast)
      BUILD_MODE="fast"
      shift
      ;;
    --full)
      BUILD_MODE="full"
      shift
      ;;
    --cloud-build)
      USE_CLOUD_BUILD=true
      shift
      ;;
    --no-push)
      PUSH_IMAGE=false
      shift
      ;;
    --register-workspacekind)
      REGISTER_WSK=true
      shift
      ;;
    -h|--help)
      show_usage
      exit 0
      ;;
    *)
      echo "ERROR: Unknown option: $1" >&2
      show_usage
      exit 1
      ;;
  esac
done

if [[ -z "${PROJECT_ID}" ]]; then
  echo "ERROR: PROJECT_ID is not set and could not be determined from gcloud config." >&2
  echo "Please run: export PROJECT_ID=your-gcp-project-id" >&2
  exit 1
fi

case "${VARIANT}" in
  all)
    VARIANTS=("cpu" "gpu" "tpu" "spark")
    ;;
  cpu|gpu|tpu|spark)
    VARIANTS=("${VARIANT}")
    ;;
  *)
    echo "ERROR: Invalid variant '${VARIANT}'. Allowed: cpu, gpu, tpu, spark, all" >&2
    exit 1
    ;;
esac

# The WorkspaceKind pins an exact, immutable tag (see section 4). If we never
# push that tag, registering it leaves the cluster pointing at an image that
# does not exist in the registry, and every new Workspace fails with
# ImagePullBackOff. Fail before the (long) build rather than after it.
if [[ "${REGISTER_WSK}" == "true" && "${PUSH_IMAGE}" != "true" ]]; then
  echo "ERROR: --register-workspacekind cannot be combined with --no-push." >&2
  echo "  The WorkspaceKind would pin image tag '${IMAGE_TAG}', which would" >&2
  echo "  never be pushed, breaking Workspace creation." >&2
  echo "  Drop --no-push, or drop --register-workspacekind." >&2
  exit 1
fi

echo "=================================================================="
echo "Build Configuration:"
echo "  PROJECT_ID:       ${PROJECT_ID}"
echo "  REGION:           ${REGION}"
echo "  REPO_NAME:        ${REPO_NAME}"
echo "  IMAGE_NAME:       ${IMAGE_NAME}"
echo "  IMAGE_TAG:        ${IMAGE_TAG}"
echo "  TENANT_NAMESPACE: ${TENANT_NAMESPACE}"
echo "  GCS_BUCKET:       ${GCS_BUCKET}"
echo "  Variants:         ${VARIANTS[*]}"
echo "  Build Mode:       ${BUILD_MODE}"
echo "  Use Cloud Build:  ${USE_CLOUD_BUILD}"
echo "  Push Image:       ${PUSH_IMAGE}"
echo "  Register WSK:     ${REGISTER_WSK}"
echo "=================================================================="

# ==============================================================================
# 2. Ensure Artifact Registry Repository Exists & Configure Docker Auth
# ==============================================================================
if [[ "${PUSH_IMAGE}" == "true" || "${USE_CLOUD_BUILD}" == "true" ]]; then
  echo "Checking Artifact Registry repository '${REPO_NAME}' in ${REGION}..."
  if ! gcloud artifacts repositories describe "${REPO_NAME}" \
      --location="${REGION}" \
      --project="${PROJECT_ID}" >/dev/null 2>&1; then
    echo "Creating Artifact Registry Docker repository '${REPO_NAME}' in ${REGION}..."
    gcloud artifacts repositories create "${REPO_NAME}" \
      --repository-format=docker \
      --location="${REGION}" \
      --description="Kubeflow Notebook custom images" \
      --project="${PROJECT_ID}"
  else
    echo "Artifact Registry repository '${REPO_NAME}' already exists."
  fi

  if [[ "${USE_CLOUD_BUILD}" == "false" ]]; then
    echo "Configuring Docker authentication for ${REGION}-docker.pkg.dev..."
    gcloud auth configure-docker "${REGION}-docker.pkg.dev" --quiet
  fi
fi

# ==============================================================================
# 3. Build & Push Images
# ==============================================================================
BUILT_IMAGES=()

for v in "${VARIANTS[@]}"; do
  case "$v" in
    cpu)
      if [[ "${BUILD_MODE}" == "full" ]]; then
        V_DOCKERFILE="Dockerfile"
      else
        V_DOCKERFILE="Dockerfile.fast"
      fi
      V_TAG="${IMAGE_TAG}-cpu"
      V_LATEST_TAG="latest-cpu"
      V_DESC="VS Code (code-server) CPU"
      ;;
    gpu)
      V_DOCKERFILE="Dockerfile.fast.gpu"
      V_TAG="${IMAGE_TAG}-gpu"
      V_LATEST_TAG="latest-gpu"
      V_DESC="VS Code (code-server) CUDA GPU"
      ;;
    tpu)
      V_DOCKERFILE="Dockerfile.fast.tpu"
      V_TAG="${IMAGE_TAG}-tpu"
      V_LATEST_TAG="latest-tpu"
      V_DESC="VS Code (code-server) TPU"
      ;;
    spark)
      V_DOCKERFILE="Dockerfile.spark"
      V_TAG="${IMAGE_TAG}"
      V_LATEST_TAG="latest"
      V_DESC="Spark 4.0.1 (Python 3.12)"
      ;;
  esac

  if [[ "$v" == "spark" ]]; then
    V_IMAGE_URI="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}/spark-py312:${V_TAG}"
    V_LATEST_URI="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}/spark-py312:${V_LATEST_TAG}"
  else
    V_IMAGE_URI="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}/${IMAGE_NAME}:${V_TAG}"
    V_LATEST_URI="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}/${IMAGE_NAME}:${V_LATEST_TAG}"
  fi

  echo "=================================================================="
  echo "Building [${V_DESC}] variant -> ${V_IMAGE_URI} (and ${V_LATEST_URI})"
  echo "Dockerfile: ${CONTEXT_DIR}/${V_DOCKERFILE}"
  echo "=================================================================="

  TMP_BUILD_DIR="$(mktemp -d)"
  cp -r "${CONTEXT_DIR}/." "${TMP_BUILD_DIR}/"
  cp -r "${EXAMPLES_DIR}" "${TMP_BUILD_DIR}/examples"
  if [[ -d "${SCRIPT_DIR}/../../images/samples" ]]; then
    cp -r "${SCRIPT_DIR}/../../images/samples/." "${TMP_BUILD_DIR}/examples/" 2>/dev/null || true
  fi
  cp "${CONTEXT_DIR}/${V_DOCKERFILE}" "${TMP_BUILD_DIR}/Dockerfile"

  if [[ "${USE_CLOUD_BUILD}" == "true" ]]; then
    gcloud builds submit "${TMP_BUILD_DIR}" \
      --tag="${V_IMAGE_URI}" \
      --project="${PROJECT_ID}"
    rm -rf "${TMP_BUILD_DIR}"
    if [[ "${V_IMAGE_URI}" != "${V_LATEST_URI}" ]]; then
      gcloud container images add-tag "${V_IMAGE_URI}" "${V_LATEST_URI}" --quiet
    fi
    if [[ "$v" == "cpu" ]]; then
      DEFAULT_IMAGE_URI="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}/${IMAGE_NAME}:${IMAGE_TAG}"
      DEFAULT_LATEST_URI="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}/${IMAGE_NAME}:latest"
      gcloud container images add-tag "${V_IMAGE_URI}" "${DEFAULT_IMAGE_URI}" --quiet
      gcloud container images add-tag "${V_IMAGE_URI}" "${DEFAULT_LATEST_URI}" --quiet
    fi
  else
    docker build \
      --platform linux/amd64 \
      -f "${TMP_BUILD_DIR}/Dockerfile" \
      -t "${V_IMAGE_URI}" \
      -t "${V_LATEST_URI}" \
      "${TMP_BUILD_DIR}"
    rm -rf "${TMP_BUILD_DIR}"

    if [[ "${PUSH_IMAGE}" == "true" ]]; then
      echo "Pushing ${V_IMAGE_URI}..."
      docker push "${V_IMAGE_URI}"
      if [[ "${V_IMAGE_URI}" != "${V_LATEST_URI}" ]]; then
        echo "Pushing ${V_LATEST_URI}..."
        docker push "${V_LATEST_URI}"
      fi
    fi

    if [[ "$v" == "cpu" ]]; then
      DEFAULT_IMAGE_URI="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}/${IMAGE_NAME}:${IMAGE_TAG}"
      DEFAULT_LATEST_URI="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}/${IMAGE_NAME}:latest"
      docker tag "${V_IMAGE_URI}" "${DEFAULT_IMAGE_URI}"
      docker tag "${V_IMAGE_URI}" "${DEFAULT_LATEST_URI}"
      if [[ "${PUSH_IMAGE}" == "true" ]]; then
        docker push "${DEFAULT_IMAGE_URI}"
        docker push "${DEFAULT_LATEST_URI}"
      fi
    fi
  fi

  BUILT_IMAGES+=("${V_IMAGE_URI}" "${V_LATEST_URI}")
done

echo "=================================================================="
echo "Successfully built $( [[ "${PUSH_IMAGE}" == "true" ]] && echo "and pushed " )images:"
for img in "${BUILT_IMAGES[@]}"; do
  echo "  - ${img}"
done
echo "=================================================================="

# ==============================================================================
# 4. Resolve the image tag each WorkspaceKind variant pins to
# ==============================================================================
# The WorkspaceKind pins an exact tag per variant instead of a floating :latest-*,
# so a Workspace restarts onto the same image it was created with. That makes a
# partial build a hazard: rendering the manifest with this run's tag after
# `--variant gpu` would repoint cpu and tpu at an image that was never built. So a
# variant we did not build keeps whatever the live cluster already references, and
# only falls back to :latest-<variant> when there is nothing deployed to read.
#
# "Built" here means built AND pushed. A --no-push build produces tags that exist
# nowhere but the local Docker daemon, so claiming one below would hand
# deploy_standalone.sh a tag the cluster cannot pull. An unpushed variant is therefore
# treated exactly like one we never built.
variant_tag() {
  local variant="$1"
  if [[ "${PUSH_IMAGE}" == "true" && " ${VARIANTS[*]} " == *" ${variant} "* ]]; then
    echo "${IMAGE_TAG}-${variant}"
    return
  fi

  local deployed
  deployed="$(kubectl get workspacekind codeserver \
    -o "jsonpath={.spec.podTemplate.options.imageConfig.values[?(@.id=='codeserver-python-${variant}')].spec.image}" \
    2>/dev/null || true)"
  if [[ -n "${deployed}" ]]; then
    echo "${deployed##*:}"
  else
    echo "latest-${variant}"
  fi
}

CPU_IMAGE_TAG="$(variant_tag cpu)"
GPU_IMAGE_TAG="$(variant_tag gpu)"
TPU_IMAGE_TAG="$(variant_tag tpu)"
export CPU_IMAGE_TAG GPU_IMAGE_TAG TPU_IMAGE_TAG

# Publish them so deploy_standalone.sh pins the same images when it renders the
# WorkspaceKind, rather than re-deriving them and risking a different answer.
TAGS_FILE="${SCRIPT_DIR}/rendered/codeserver-image-tags.env"
mkdir -p "$(dirname "${TAGS_FILE}")"
cat > "${TAGS_FILE}" <<EOF
# Written by build_codeserver_python.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ). Consumed by
# deploy_standalone.sh so the WorkspaceKind pins the images this build produced.
CPU_IMAGE_TAG="${CPU_IMAGE_TAG}"
GPU_IMAGE_TAG="${GPU_IMAGE_TAG}"
TPU_IMAGE_TAG="${TPU_IMAGE_TAG}"
EOF

echo ""
echo "WorkspaceKind will pin these tags:"
echo "  cpu -> ${CPU_IMAGE_TAG}"
echo "  gpu -> ${GPU_IMAGE_TAG}"
echo "  tpu -> ${TPU_IMAGE_TAG}"
echo "  (recorded in ${TAGS_FILE})"

# ==============================================================================
# 5. Register / Update WorkspaceKind & ComputeClasses in Kubernetes Cluster (Optional)
# ==============================================================================
if [[ "${REGISTER_WSK}" == "true" ]]; then
  # The WorkspaceKind bakes GCS_BUCKET into the env of every Workspace Pod, and
  # GCS_BUCKET defaults to ${TENANT_NAMESPACE}-bucket with TENANT_NAMESPACE itself
  # defaulting to "team-a". Registering from a shell that did not set TENANT_NAMESPACE
  # therefore silently points every Workspace at another tenant's bucket, or at one
  # that does not exist. Nothing fails at registration time; it surfaces much later as
  # a 404 from inside a user's job, a long way from the cause. Check it here.
  if ! gcloud storage ls "gs://${GCS_BUCKET}" --project="${PROJECT_ID}" >/dev/null 2>&1; then
    echo "ERROR: GCS bucket 'gs://${GCS_BUCKET}' does not exist or is not readable." >&2
    echo "  The WorkspaceKind injects GCS_BUCKET into every Workspace Pod, so" >&2
    echo "  registering this value would hand users a bucket that 404s on write." >&2
    echo "  Set GCS_BUCKET explicitly, or set TENANT_NAMESPACE (currently" >&2
    echo "  '${TENANT_NAMESPACE}'), which GCS_BUCKET is derived from." >&2
    exit 1
  fi

  echo "=================================================================="
  echo "Registering WorkspaceKind 'codeserver' in Kubernetes cluster..."
  echo "  GCS_BUCKET: ${GCS_BUCKET} (verified to exist)"
  echo "=================================================================="
  envsubst < "${CONTEXT_DIR}/workspacekind.yaml" | kubectl apply -f -
  echo "WorkspaceKind 'codeserver' applied successfully."

  if [[ -d "${MANIFESTS_DIR}" ]]; then
    echo "=================================================================="
    echo "Applying GKE ComputeClass manifests (GPU and TPU)..."
    echo "=================================================================="
    kubectl apply -f "${MANIFESTS_DIR}/"
    echo "ComputeClass manifests applied successfully."
  fi
else
  echo ""
  echo "To register or update this image and ComputeClasses in Kubeflow Workspaces on your GKE cluster, run:"
  echo "  PROJECT_ID=${PROJECT_ID} REGION=${REGION} REPO_NAME=${REPO_NAME} IMAGE_NAME=${IMAGE_NAME} GCS_BUCKET=${GCS_BUCKET} \\"
  echo "  CPU_IMAGE_TAG=${CPU_IMAGE_TAG} GPU_IMAGE_TAG=${GPU_IMAGE_TAG} TPU_IMAGE_TAG=${TPU_IMAGE_TAG} \\"
  echo "    envsubst < ${CONTEXT_DIR}/workspacekind.yaml | kubectl apply -f -"
  if [[ -f "${CONTEXT_DIR}/workspacekind-resumable.yaml" ]]; then
    echo "  PROJECT_ID=${PROJECT_ID} REGION=${REGION} REPO_NAME=${REPO_NAME} IMAGE_NAME=${IMAGE_NAME} GCS_BUCKET=${GCS_BUCKET} \\"
    echo "  CPU_IMAGE_TAG=${CPU_IMAGE_TAG} GPU_IMAGE_TAG=${GPU_IMAGE_TAG} TPU_IMAGE_TAG=${TPU_IMAGE_TAG} \\"
    echo "    envsubst < ${CONTEXT_DIR}/workspacekind-resumable.yaml | kubectl apply -f -"
  fi
  if [[ -d "${MANIFESTS_DIR}" ]]; then
    echo "  kubectl apply -f ${MANIFESTS_DIR}/"
  fi
  echo "Or re-run this script with --register-workspacekind"
fi
