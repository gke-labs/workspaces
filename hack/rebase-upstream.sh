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

# This script helps to sync custom changes in this repository against upstream.
# By default, it uses a 3-way merge which cleanly merges upstream changes without
# rewriting history or replaying ancient initial commits. A rebase mode is also supported.
#
# Usage:
#   ./hack/rebase-upstream.sh [--merge | --rebase] [upstream_remote] [upstream_branch]
#
# Defaults:
#   strategy:        --merge (recommended)
#   upstream_remote: upstream
#   upstream_branch: notebooks-v2
#
# Pre-requisites:
#   - A remote named 'upstream' pointing to https://github.com/kubeflow/notebooks.git
#   - Clean working directory

set -e

STRATEGY="merge"
UPSTREAM_REMOTE="upstream"
UPSTREAM_BRANCH="notebooks-v2"
POSITIONAL_ARGS=()

while [[ $# -gt 0 ]]; do
    case "$1" in
        --merge)
            STRATEGY="merge"
            shift
            ;;
        --rebase)
            STRATEGY="rebase"
            shift
            ;;
        -h|--help)
            echo "Usage: $0 [--merge | --rebase] [upstream_remote] [upstream_branch]"
            echo ""
            echo "Options:"
            echo "  --merge   Use git merge to sync upstream (default, recommended)"
            echo "  --rebase  Use git rebase to sync upstream"
            exit 0
            ;;
        *)
            POSITIONAL_ARGS+=("$1")
            shift
            ;;
    esac
done

if [ ${#POSITIONAL_ARGS[@]} -ge 1 ]; then
    UPSTREAM_REMOTE="${POSITIONAL_ARGS[0]}"
fi
if [ ${#POSITIONAL_ARGS[@]} -ge 2 ]; then
    UPSTREAM_BRANCH="${POSITIONAL_ARGS[1]}"
fi

# Check for clean working directory (ignore untracked files)
if [ -n "$(git status --porcelain -uno)" ]; then
    echo "Error: Working directory has uncommitted tracked changes. Please commit or stash them first."
    exit 1
fi

# Verify upstream remote exists
if ! git remote get-url "${UPSTREAM_REMOTE}" > /dev/null 2>&1; then
    echo "Error: Remote '${UPSTREAM_REMOTE}' not found."
    echo "Please add it using: git remote add ${UPSTREAM_REMOTE} https://github.com/kubeflow/notebooks.git"
    exit 1
fi

echo "Fetching from ${UPSTREAM_REMOTE} (${UPSTREAM_BRANCH})..."
git fetch "${UPSTREAM_REMOTE}" "${UPSTREAM_BRANCH}"

# Capture the current branch commit before sync begins to preserve custom compliance files
LOCAL_HEAD=$(git rev-parse HEAD)
COMPLIANCE_FILES=("README.md" "LICENSE" "CONTRIBUTING.md" "docs/contributing.md" "OWNERS")

resolve_compliance_conflicts() {
    local conflicted_files
    conflicted_files=$(git diff --name-only --diff-filter=U)

    for file in "${COMPLIANCE_FILES[@]}"; do
        if echo "${conflicted_files}" | grep -Fxq "$file"; then
            echo "Resolving conflict in $file: keeping custom version from ${LOCAL_HEAD:0:7}..."
            if git cat-file -e "${LOCAL_HEAD}:${file}" 2>/dev/null; then
                # Unmark conflict and restore exact version from pre-sync HEAD
                git checkout --ours -- "$file" 2>/dev/null || git checkout --theirs -- "$file" 2>/dev/null || true
                git checkout "${LOCAL_HEAD}" -- "$file" 2>/dev/null || git show "${LOCAL_HEAD}:${file}" > "$file"
                git add "$file"
            fi
        fi
    done
}

if [ "$STRATEGY" = "merge" ]; then
    echo "Syncing via merge with ${UPSTREAM_REMOTE}/${UPSTREAM_BRANCH}..."

    if ! git merge "${UPSTREAM_REMOTE}/${UPSTREAM_BRANCH}" --no-commit; then
        echo "Conflicts detected during merge. Attempting automatic resolution for compliance files..."
        resolve_compliance_conflicts

        remaining_conflicts=$(git diff --name-only --diff-filter=U)
        if [ -n "$remaining_conflicts" ]; then
            echo "Error: Unresolved conflicts remaining in:"
            echo "$remaining_conflicts"
            echo "Please resolve them manually and run 'git commit'."
            exit 1
        fi
    fi

    # Check if merge needs committing
    if [ -f .git/MERGE_HEAD ]; then
        git commit --no-edit -m "chore: sync with upstream ${UPSTREAM_REMOTE}/${UPSTREAM_BRANCH}"
        echo "Merge complete."
    else
        echo "Already up to date."
    fi

elif [ "$STRATEGY" = "rebase" ]; then
    echo "Rebasing against ${UPSTREAM_REMOTE}/${UPSTREAM_BRANCH}..."

    if ! git rebase "${UPSTREAM_REMOTE}/${UPSTREAM_BRANCH}"; then
        while [ -d .git/rebase-merge ] || [ -d .git/rebase-apply ]; do
            echo "Conflicts detected during rebase. Attempting automatic resolution for compliance files..."
            resolve_compliance_conflicts

            remaining_conflicts=$(git diff --name-only --diff-filter=U)
            if [ -n "$remaining_conflicts" ]; then
                echo "Error: Unresolved conflicts remaining in:"
                echo "$remaining_conflicts"
                echo "Please resolve them manually and run 'git rebase --continue'."
                exit 1
            else
                echo "Automatic resolution successful. Continuing rebase..."
                GIT_EDITOR=true git rebase --continue || break
            fi
        done
    fi
    echo "Rebase complete."
fi

echo "Please verify that custom code in providers/ still works and run all tests."
