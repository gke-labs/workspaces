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

# Agent runtime for examples/agent-substrate: the image every AX task runs.
#
# AX's default task image (ax-task-runner: Python 3.12, git, the Antigravity
# SDK, and ax-task-runner as PID 1) plus `ax-coding-agent`, which runs a
# Gemini-powered Antigravity agent on the task as the task's command and keeps
# its status, activity log and patch on the durable /workspace volume.
#
# deploy_agent_substrate.sh builds AX's ax-task-runner image first, passes it
# as BASE_IMAGE, and makes the result ax-server's default task image.
ARG BASE_IMAGE
FROM ${BASE_IMAGE}

COPY ax-coding-agent /usr/local/bin/ax-coding-agent
RUN chmod 0755 /usr/local/bin/ax-coding-agent \
 && git config --system init.defaultBranch main \
 && git config --system --add safe.directory '*' \
 && python3 -c "import google.antigravity" \
 && ax-coding-agent --help > /dev/null
