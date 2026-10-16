#!/bin/sh
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

# Entry point (spec.command) of every AX Task in this example.
#
# ax-task-runner starts it in the first workspace directory (/workspace/textkit)
# once the workspace is ready. In "gemini" mode the Antigravity agent has
# already worked on the bug by then (the Task's workspace goal runs before the
# command); in "scripted" mode the deterministic repair bot does the work here.
# Either way the script finishes by writing /workspace/result.json, which lives
# on the actor's durable volume and so survives suspend/resume.
set -u
BUG="$1"
MODE="${2:-scripted}"
START="$(date +%s)"

if [ "${MODE}" = "scripted" ]; then
  python3 tools/repair_bot.py --bug "${BUG}" --budget 120 > /workspace/agent.log 2>&1 || true
fi

python3 tools/report.py --bug "${BUG}" --mode "${MODE}" --started "${START}" --out /workspace/result.json
