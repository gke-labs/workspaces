#!/bin/bash
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

# This script helps to rebase the custom changes in this repository against upstream.
#
# Usage:
#   ./hack/rebase-upstream.sh [upstream_remote] [upstream_branch]
#
# Defaults:
#   upstream_remote: upstream
#   upstream_branch: notebooks-v2
#
# Pre-requisites:
#   - A remote named 'upstream' pointing to https://github.com/kubeflow/notebooks.git
#   - Clean working directory

set -e

UPSTREAM_REMOTE=${1:-upstream}
UPSTREAM_BRANCH=${2:-notebooks-v2}

# Verify upstream remote exists
if ! git remote get-url "${UPSTREAM_REMOTE}" > /dev/null 2>&1; then
    echo "Error: Remote '${UPSTREAM_REMOTE}' not found."
    echo "Please add it using: git remote add ${UPSTREAM_REMOTE} https://github.com/kubeflow/notebooks.git"
    exit 1
fi

echo "Fetching from ${UPSTREAM_REMOTE}..."
git fetch "${UPSTREAM_REMOTE}"

echo "Rebasing against ${UPSTREAM_REMOTE}/${UPSTREAM_BRANCH}..."
# We try to rebase, but if it fails due to conflicts, we can try to resolve them
if ! git rebase "${UPSTREAM_REMOTE}/${UPSTREAM_BRANCH}"; then
    echo "Conflicts detected. Attempting automatic resolution for compliance files..."
    
    # List of files to keep from OUR custom branch (which is 'theirs' in rebase context)
    COMPLIANCE_FILES=("README.md" "LICENSE" "CONTRIBUTING.md" "docs/contributing.md")
    
    for file in "${COMPLIANCE_FILES[@]}"; do
        if git status --porcelain | grep -q "^UU $file"; then
            echo "Resolving conflict in $file keeping custom version..."
            git checkout --theirs "$file"
            git add "$file"
        fi
    done
    
    # If there are still conflicts, let the user resolve them
    if git status --porcelain | grep -q "^UU"; then
        echo "Error: Unresolved conflicts remaining. Please resolve them manually and run 'git rebase --continue'."
        exit 1
    else
        echo "Automatic resolution successful. Continuing rebase..."
        git rebase --continue
    fi
fi

echo "Rebase complete."
echo "Please verify that custom code in providers/ still works and run all tests."
