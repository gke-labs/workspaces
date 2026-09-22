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

context=${1:?Usage: test-connection-tokens.sh CONTEXT NAMESPACE}
namespace=${2:?Usage: test-connection-tokens.sh CONTEXT NAMESPACE}
audience=https://notebooks-token-probe.invalid
temporary=$(mktemp -d)
name=
cleanup() {
  if [[ -n "$name" ]]; then
    kubectl --context="$context" -n "$namespace" delete serviceaccount "$name" --ignore-not-found >/dev/null
  fi
  rm -rf "$temporary"
}
trap cleanup EXIT
name=$(jq -n --arg namespace "$namespace" '{apiVersion:"v1",kind:"ServiceAccount",metadata:{generateName:"notebooks-token-probe-",namespace:$namespace},automountServiceAccountToken:false}' |
  kubectl --context="$context" create -f - -o jsonpath='{.metadata.name}')
original_uid=$(kubectl --context="$context" -n "$namespace" get serviceaccount "$name" -o jsonpath='{.metadata.uid}')
kubectl --context="$context" -n "$namespace" create token "$name" --audience="$audience" --duration=10m > "$temporary/token"
review() {
  jq -n --rawfile token "$temporary/token" --arg audience "$1" \
    '{apiVersion:"authentication.k8s.io/v1",kind:"TokenReview",spec:{token:($token|rtrimstr("\n")),audiences:[$audience]}}' |
    kubectl --context="$context" create --raw=/apis/authentication.k8s.io/v1/tokenreviews -f - |
    jq '.status'
}
review "$audience" | jq -e --arg audience "$audience" '.authenticated == true and (.audiences | index($audience) != null)' >/dev/null
printf 'PASS: dedicated audience accepted\n'
review https://wrong-audience.invalid | jq -e '.authenticated != true' >/dev/null
printf 'PASS: wrong audience rejected\n'
kubectl --context="$context" config view --minify --flatten --raw -o json |
  jq --rawfile token "$temporary/token" '.users=[{name:"probe",user:{token:($token|rtrimstr("\n"))}}] | .contexts[0].context.user="probe"' > "$temporary/kubeconfig"
if kubectl --kubeconfig="$temporary/kubeconfig" get namespaces --request-timeout=10s > "$temporary/api-result" 2>&1; then
  printf 'FAIL: token authenticated to the Kubernetes API\n' >&2
  exit 1
fi
grep -Eq 'Unauthorized|must be logged in|provide credentials' "$temporary/api-result"
printf 'PASS: ordinary Kubernetes API authentication rejected\n'
for seconds in 86400 604800 2592000; do
  requested_at=$(date +%s)
  jq -n --arg audience "$audience" --argjson seconds "$seconds" \
    '{apiVersion:"authentication.k8s.io/v1",kind:"TokenRequest",spec:{audiences:[$audience],expirationSeconds:$seconds}}' |
    kubectl --context="$context" create --raw="/api/v1/namespaces/$namespace/serviceaccounts/$name/token" -f - |
    jq -e --argjson requested "$seconds" --argjson started "$requested_at" \
      '.status | select((.token | length) > 0) | {requestedSeconds:$requested,grantedSeconds:((.expirationTimestamp|fromdateiso8601)-$started),expiresAt:.expirationTimestamp} | select(.grantedSeconds > 0)'
done
kubectl --context="$context" -n "$namespace" delete serviceaccount "$name" --wait=true >/dev/null
deleted_review=$(review "$audience")
printf 'TokenReview authenticated immediately after deletion: %s\n' "$(jq -r '.authenticated // false' <<< "$deleted_review")"
[[ -z $(kubectl --context="$context" -n "$namespace" get serviceaccount "$name" --ignore-not-found -o name) ]]
printf 'PASS: authoritative grant lookup rejects deletion regardless of TokenReview cache\n'
kubectl --context="$context" -n "$namespace" create serviceaccount "$name" >/dev/null
new_uid=$(kubectl --context="$context" -n "$namespace" get serviceaccount "$name" -o jsonpath='{.metadata.uid}')
[[ "$new_uid" != "$original_uid" ]]
printf 'PASS: authoritative UID comparison rejects recreated grant\n'