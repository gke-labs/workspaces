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

if [[ $# != 2 ]]; then
  printf 'Usage: bash providers/gke/scripts/plan.sh CONFIG_JSON NEW_OUTPUT_DIRECTORY\n' >&2
  exit 2
fi

root=$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)
config=$(realpath "$1")
output=$(realpath -m "$2")
if [[ -e "$output" ]]; then
  printf 'Output already exists; choose a new directory to preserve the prior plan.\n' >&2
  exit 2
fi
command -v go >/dev/null
command -v kubectl >/dev/null
mkdir -p "$(dirname "$output")"
temporary=$(mktemp -d "$(dirname "$output")/.notebooks-plan.XXXXXX")
trap 'rm -rf "$temporary"' EXIT
gke_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$gke_dir"
go build -o "$temporary/render" ./cmd/render
for stage in namespaces isolation applications edge; do
  "$temporary/render" --repo-root "$root" --config "$config" --stage "$stage" > "$temporary/$stage.json"
done
rm "$temporary/render"
git -C "$root" rev-parse HEAD > "$temporary/upstream-revision.txt"
sha256sum "$config" > "$temporary/config.sha256"
mv "$temporary" "$output"
trap - EXIT
printf 'Rendered plan to %s. No cluster or cloud resources were changed.\n' "$output"
