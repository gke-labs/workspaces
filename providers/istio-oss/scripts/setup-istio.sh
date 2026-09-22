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


# Setup script for Istio service mesh
# This script checks if Istio is installed and installs it if needed.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROVIDER_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
LOCALBIN="${PROVIDER_DIR}/bin"

ISTIO_VERSION="1.29.1"
ISTIO_URL="https://istio.io/downloadIstio"

if [[ ! -d "${LOCALBIN}" ]]; then
  echo "INFO: Creating local bin directory at ${LOCALBIN}"
  mkdir -p "${LOCALBIN}"
fi

ISTIOCTL_PATH="${LOCALBIN}/istio-${ISTIO_VERSION}"
if [[ ! -d "${ISTIOCTL_PATH}" ]]; then
  pushd "$LOCALBIN" > /dev/null
    echo "INFO: Fetching Istio ${ISTIO_VERSION} installer..."
    curl -sL "$ISTIO_URL" | ISTIO_VERSION=${ISTIO_VERSION} sh -
  popd
fi

# Add istioctl to PATH for this script
export PATH=${ISTIOCTL_PATH}/bin:$PATH

# Ensure istioctl is available
if ! command -v istioctl >/dev/null 2>&1; then
  echo "ERROR: istioctl not found in PATH. Try removing ${LOCALBIN} and re-running."
  exit 1
else
  echo "INFO: using istioctl from $(which istioctl)"
  echo "INFO: istioctl version output:"
  istioctl version --remote=false
fi

echo "INFO: Installing Istio ${ISTIO_VERSION} ..."
istioctl install -f "${PROVIDER_DIR}/istio-cni.yaml" -y

echo "INFO: applying Istio Gateway resources..."
kubectl apply -k "${PROVIDER_DIR}/manifests/istio-gateway"
kubectl wait --for=condition=ready certificate/gateway-tls -n istio-system --timeout=60s

echo "INFO: Istio setup complete"
