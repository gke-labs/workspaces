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

FROM node:22-bookworm-slim AS builder
WORKDIR /src
COPY workspaces/frontend/package*.json ./
RUN npm ci
COPY workspaces/frontend/ ./
ENV DEPLOYMENT_MODE=standalone
ENV PUBLIC_PATH=/workspaces/
RUN npm run build:prod

FROM nginxinc/nginx-unprivileged:1.28-alpine
COPY --from=builder /src/dist /usr/share/nginx/html/workspaces
COPY --chmod=644 gke/frontend-nginx.conf /etc/nginx/conf.d/default.conf
USER 101:101
EXPOSE 8080
