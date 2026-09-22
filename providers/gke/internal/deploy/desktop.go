// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package deploy

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

func desktopApplications() []unstructured.Unstructured {
	return []unstructured.Unstructured{
		object("v1", "Service", "gke-desktop-proxy", systemNamespace, map[string]any{"spec": map[string]any{"type": "ClusterIP", "selector": map[string]any{"app": "gke-access-proxy"}, "ports": []any{map[string]any{"name": "http", "protocol": "TCP", "port": int64(8081), "targetPort": int64(8081)}}}}),
		object("rbac.authorization.k8s.io/v1", "Role", "notebooks-connections", "notebooks-connections", map[string]any{"rules": []any{
			map[string]any{"apiGroups": []any{""}, "resources": []any{"serviceaccounts"}, "verbs": []any{"get", "list", "create", "update", "delete"}},
			map[string]any{"apiGroups": []any{""}, "resources": []any{"serviceaccounts/token"}, "verbs": []any{"create"}},
		}}),
		object("rbac.authorization.k8s.io/v1", "RoleBinding", "notebooks-connections", "notebooks-connections", map[string]any{
			"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "notebooks-connections"},
			"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": "gke-access-proxy", "namespace": systemNamespace}},
		}),
		object("rbac.authorization.k8s.io/v1", "ClusterRole", "notebooks-token-review", "", map[string]any{"rules": []any{map[string]any{"apiGroups": []any{"authentication.k8s.io"}, "resources": []any{"tokenreviews"}, "verbs": []any{"create"}}}}),
		object("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "notebooks-token-review", "", map[string]any{
			"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "notebooks-token-review"},
			"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": "gke-access-proxy", "namespace": systemNamespace}},
		}),
		object("v1", "ResourceQuota", "notebooks-connections", "notebooks-connections", map[string]any{"spec": map[string]any{"hard": map[string]any{"count/serviceaccounts": "1000", "pods": "0"}}}),
	}
}

func desktopEdge(config Config) []unstructured.Unstructured {
	target := map[string]any{"group": "", "kind": "Service", "name": "gke-desktop-proxy"}
	return []unstructured.Unstructured{
		object("networking.gke.io/v1", "GCPBackendPolicy", "notebooks-desktop", systemNamespace, map[string]any{"spec": map[string]any{
			"targetRef": target, "default": map[string]any{"iap": map[string]any{"enabled": false}, "timeoutSec": int64(3600), "logging": map[string]any{"enabled": false}},
		}}),
		object("networking.gke.io/v1", "HealthCheckPolicy", "notebooks-desktop", systemNamespace, map[string]any{"spec": map[string]any{
			"targetRef": target, "default": map[string]any{"config": map[string]any{"type": "HTTP", "httpHealthCheck": map[string]any{"port": int64(8081), "requestPath": "/healthz"}}},
		}}),
		object("gateway.networking.k8s.io/v1", "HTTPRoute", "notebooks-desktop", systemNamespace, map[string]any{"spec": map[string]any{
			"parentRefs": []any{map[string]any{"name": "notebooks", "sectionName": "desktop"}}, "hostnames": []any{config.DesktopHostname},
			"rules": []any{map[string]any{"matches": []any{map[string]any{"path": map[string]any{"type": "PathPrefix", "value": "/workspace/connect/"}}}, "backendRefs": []any{map[string]any{"name": "gke-desktop-proxy", "port": int64(8081)}}}},
		}}),
	}
}
