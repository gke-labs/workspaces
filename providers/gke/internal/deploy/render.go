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

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/kubeflow/notebooks/gke/internal/connectionpolicy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const systemNamespace = "kubeflow-workspaces"

// snapshotAddonName is the standalone GKE PodSnapshot controller and webhook
// deployment. It is deliberately separate from gke-access-proxy so a failure in
// the snapshot control plane cannot take user traffic down.
const snapshotAddonName = "gke-workspace-snapshot-addon"

type Config struct {
	ControlPlaneCIDR              string            `json:"controlPlaneCIDR"`
	Hostname                      string            `json:"hostname"`
	DesktopHostname               string            `json:"desktopHostname,omitempty"`
	ConnectionTokenDefaultSeconds int64             `json:"connectionTokenDefaultSeconds,omitempty"`
	ConnectionTokenMaxSeconds     int64             `json:"connectionTokenMaxSeconds,omitempty"`
	CertificateMap                string            `json:"certificateMap"`
	AddressName                   string            `json:"addressName"`
	IAPClientID                   string            `json:"iapClientID"`
	IAPSecretName                 string            `json:"iapSecretName"`
	IAPAudience                   string            `json:"iapAudience"`
	KubeClientQPS                 float64           `json:"kubeClientQPS,omitempty"`
	KubeClientBurst               int               `json:"kubeClientBurst,omitempty"`
	SnapshotGCSBucket             string            `json:"snapshotGCSBucket,omitempty"`
	Tenants                       []string          `json:"tenants"`
	Images                        map[string]string `json:"images"`
}

type Builder func(context.Context, string) ([]unstructured.Unstructured, error)

func ReadConfig(reader io.Reader) (Config, error) {
	var config Config
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return config, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return config, fmt.Errorf("expected one JSON configuration object")
	}
	return config, config.Validate()
}

func (config Config) Validate() error {
	if _, err := (connectionpolicy.Policy{DefaultSeconds: config.ConnectionTokenDefaultSeconds, MaxSeconds: config.ConnectionTokenMaxSeconds}).Resolve(); err != nil {
		return err
	}
	if config.KubeClientQPS < 0 || config.KubeClientBurst < 0 {
		return fmt.Errorf("kubeClientQPS and kubeClientBurst must be non-negative")
	}
	if prefix, err := netip.ParsePrefix(config.ControlPlaneCIDR); err != nil || prefix.Bits() == 0 {
		return fmt.Errorf("controlPlaneCIDR must be the cluster control-plane source prefix, not a default route")
	}
	if len(validation.IsDNS1123Subdomain(config.Hostname)) != 0 || !strings.Contains(config.Hostname, ".") {
		return fmt.Errorf("hostname must be a DNS name")
	}
	if config.DesktopHostname != "" && (config.DesktopHostname == config.Hostname || len(validation.IsDNS1123Subdomain(config.DesktopHostname)) != 0 || !strings.Contains(config.DesktopHostname, ".")) {
		return fmt.Errorf("desktopHostname must be a different DNS name")
	}
	for _, name := range []string{config.CertificateMap, config.AddressName} {
		if len(validation.IsDNS1123Label(name)) != 0 {
			return fmt.Errorf("certificateMap, addressName and iapSecretName must be valid resource names")
		}
	}
	if config.IAPClientID != "" || config.IAPSecretName != "" {
		if !regexp.MustCompile(`^[0-9]+-[a-zA-Z0-9_-]+\.apps\.googleusercontent\.com$`).MatchString(config.IAPClientID) || len(validation.IsDNS1123Label(config.IAPSecretName)) != 0 {
			return fmt.Errorf("custom OAuth requires both a Google iapClientID and a valid iapSecretName; omit both for Google-managed OAuth")
		}
	}
	if config.IAPAudience != "" && !regexp.MustCompile(`^/projects/[0-9]+/global/backendServices/[0-9]+$`).MatchString(config.IAPAudience) {
		return fmt.Errorf("iapAudience must identify an exact global backend service")
	}
	if len(config.Tenants) == 0 {
		return fmt.Errorf("at least one tenant is required")
	}
	seen := map[string]bool{}
	for _, tenant := range config.Tenants {
		if len(validation.IsDNS1123Label(tenant)) != 0 || seen[tenant] || tenant == systemNamespace || tenant == "notebooks-connections" || tenant == "default" || strings.HasPrefix(tenant, "kube-") || strings.HasPrefix(tenant, "gke-") {
			return fmt.Errorf("invalid, reserved or duplicate tenant %q", tenant)
		}
		seen[tenant] = true
	}
	if len(config.Images) != 5 {
		return fmt.Errorf("exactly five component images are required")
	}
	for _, component := range []string{"controller", "backend", "frontend", "proxy", "snapshot"} {
		if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$`).MatchString(config.Images[component]) {
			return fmt.Errorf("%s image must be pinned by sha256 digest", component)
		}
	}
	return nil
}

// snapshotBucket resolves the GCS bucket backing PodSnapshotStorageConfig.
func (config Config) snapshotBucket() string {
	if config.SnapshotGCSBucket != "" {
		return config.SnapshotGCSBucket
	}
	if len(config.Tenants) > 0 && config.Tenants[0] != "" {
		return config.Tenants[0] + "-snapshots-bucket"
	}
	return ""
}

func Kustomize(ctx context.Context, directory string) ([]unstructured.Unstructured, error) {
	command := exec.CommandContext(ctx, "kubectl", "kustomize", directory)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("kustomize %s: %w: %s", directory, err, stderr.String())
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	var result []unstructured.Unstructured
	for {
		var object unstructured.Unstructured
		if err := decoder.Decode(&object); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if len(object.Object) != 0 {
			result = append(result, object)
		}
	}
	return result, nil
}

func object(apiVersion, kind, name, namespace string, fields map[string]any) unstructured.Unstructured {
	metadata := map[string]any{"name": name}
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	result := unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": metadata}}
	for key, value := range fields {
		result.Object[key] = value
	}
	return result
}

func Render(ctx context.Context, root, stage string, config Config, build Builder) ([]unstructured.Unstructured, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	var result []unstructured.Unstructured
	switch stage {
	case "namespaces":
		for _, namespace := range append([]string{systemNamespace}, config.Tenants...) {
			result = append(result, object("v1", "Namespace", namespace, "", nil))
		}
		if config.DesktopHostname != "" {
			result = append(result, object("v1", "Namespace", "notebooks-connections", "", nil))
		}
	case "isolation":
		// The API server (or its konnectivity agent) is the only client allowed to
		// reach a webhook port.
		webhookIngress := func(name, app string) unstructured.Unstructured {
			return object("networking.k8s.io/v1", "NetworkPolicy", name, systemNamespace, map[string]any{"spec": map[string]any{
				"podSelector": map[string]any{"matchLabels": map[string]any{"app": app}},
				"policyTypes": []any{"Ingress"},
				"ingress": []any{map[string]any{
					"from": []any{
						map[string]any{"ipBlock": map[string]any{"cidr": config.ControlPlaneCIDR}},
						map[string]any{
							"namespaceSelector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": "kube-system"}},
							"podSelector":       map[string]any{"matchLabels": map[string]any{"k8s-app": "konnectivity-agent"}},
						},
					},
					"ports": []any{map[string]any{"protocol": "TCP", "port": int64(9443)}},
				}},
			}})
		}
		result = append(result, webhookIngress("gke-webhook-ingress", "workspaces-controller"))
		// The snapshot addon runs in its own deployment, so it carries its own
		// isolation instead of sharing the access proxy's.
		result = append(result, webhookIngress("gke-snapshot-webhook-ingress", snapshotAddonName))
		for _, overlay := range []string{"upstream", "proxy", "snapshot"} {
			resources, err := build(ctx, filepath.Join(root, "gke/manifests", overlay))
			if err != nil {
				return nil, err
			}
			for _, resource := range resources {
				if resource.GetKind() == "NetworkPolicy" {
					if resource.GetName() == "gke-proxy-ingress" && config.DesktopHostname != "" {
						ingress, _, _ := unstructured.NestedSlice(resource.Object, "spec", "ingress")
						rule := ingress[0].(map[string]any)
						rule["ports"] = append(rule["ports"].([]any), map[string]any{"protocol": "TCP", "port": int64(8081)})
						_ = unstructured.SetNestedSlice(resource.Object, ingress, "spec", "ingress")
					}
					result = append(result, resource)
				}
			}
		}
		for _, tenant := range config.Tenants {
			resources, err := build(ctx, filepath.Join(root, "gke/manifests/tenant"))
			if err != nil {
				return nil, err
			}
			for _, resource := range resources {
				resource.SetNamespace(tenant)
				result = append(result, resource)
			}
		}
	case "applications":
		for _, overlay := range []string{"upstream", "proxy", "snapshot"} {
			resources, err := build(ctx, filepath.Join(root, "gke/manifests", overlay))
			if err != nil {
				return nil, err
			}
			for _, resource := range resources {
				if resource.GetKind() == "Namespace" || resource.GetKind() == "NetworkPolicy" {
					continue
				}
				if resource.GetKind() == "ClusterRole" && resource.Object["aggregationRule"] != nil {
					unstructured.RemoveNestedField(resource.Object, "rules")
				}
				if resource.GetKind() == "Deployment" {
					component := strings.TrimPrefix(resource.GetName(), "workspaces-")
					switch resource.GetName() {
					case "gke-access-proxy":
						component = "proxy"
					case snapshotAddonName:
						component = "snapshot"
					}
					image, ok := config.Images[component]
					if !ok {
						return nil, fmt.Errorf("unknown deployment %s", resource.GetName())
					}
					containers, found, err := unstructured.NestedSlice(resource.Object, "spec", "template", "spec", "containers")
					if err != nil || !found || len(containers) != 1 {
						return nil, fmt.Errorf("upstream container contract changed for %s", resource.GetName())
					}
					containers[0].(map[string]any)["image"] = image
					if err := unstructured.SetNestedSlice(resource.Object, containers, "spec", "template", "spec", "containers"); err != nil {
						return nil, err
					}
				}
				result = append(result, resource)
			}
		}
		qps := config.KubeClientQPS
		if qps == 0 {
			qps = 100
		}
		burst := config.KubeClientBurst
		if burst == 0 {
			burst = 200
		}
		result = append([]unstructured.Unstructured{
			object("v1", "ConfigMap", "gke-access-proxy", systemNamespace, map[string]any{"data": map[string]any{
				"PUBLIC_URL":        "https://" + config.Hostname,
				"IAP_AUDIENCE":      config.IAPAudience,
				"FRONTEND_URL":      "http://workspaces-frontend.kubeflow-workspaces.svc:8080",
				"BACKEND_URL":       "http://workspaces-backend.kubeflow-workspaces.svc:4000",
				"TENANT_NAMESPACES": strings.Join(config.Tenants, ","),
				"KUBE_CLIENT_QPS":   strconv.FormatFloat(qps, 'f', -1, 64),
				"KUBE_CLIENT_BURST": strconv.Itoa(burst),
			}}),
			// The addon is rate limited independently of user traffic: it only talks
			// to the API server, and a burst of Workspace churn must never consume
			// the access proxy's client budget.
			object("v1", "ConfigMap", snapshotAddonName, systemNamespace, map[string]any{"data": map[string]any{
				"TENANT_NAMESPACES":   strings.Join(config.Tenants, ","),
				"SNAPSHOT_GCS_BUCKET": config.snapshotBucket(),
				"SNAPSHOT_WORKERS":    "4",
				"SNAPSHOT_RESYNC":     "10m",
				"LEADER_ELECTION":     "true",
				"KUBE_CLIENT_QPS":     "50",
				"KUBE_CLIENT_BURST":   "100",
			}}),
		}, result...)
		if config.DesktopHostname != "" {
			_ = unstructured.SetNestedField(result[0].Object, "https://"+config.DesktopHostname, "data", "DESKTOP_URL")
			policy, _ := (connectionpolicy.Policy{DefaultSeconds: config.ConnectionTokenDefaultSeconds, MaxSeconds: config.ConnectionTokenMaxSeconds}).Resolve()
			_ = unstructured.SetNestedField(result[0].Object, strconv.FormatInt(policy.DefaultSeconds, 10), "data", "CONNECTION_TOKEN_DEFAULT_SECONDS")
			_ = unstructured.SetNestedField(result[0].Object, strconv.FormatInt(policy.MaxSeconds, 10), "data", "CONNECTION_TOKEN_MAX_SECONDS")
			result = append(result, desktopApplications()...)
		}
	case "edge":
		gateway := object("gateway.networking.k8s.io/v1", "Gateway", "notebooks", systemNamespace, map[string]any{"spec": map[string]any{
			"gatewayClassName": "gke-l7-global-external-managed",
			"addresses":        []any{map[string]any{"type": "NamedAddress", "value": config.AddressName}},
			"listeners":        []any{map[string]any{"name": "https", "hostname": config.Hostname, "port": int64(443), "protocol": "HTTPS", "allowedRoutes": map[string]any{"namespaces": map[string]any{"from": "Same"}}}},
		}})
		gateway.SetAnnotations(map[string]string{"networking.gke.io/certmap": config.CertificateMap})
		if config.DesktopHostname != "" {
			listeners, _, _ := unstructured.NestedSlice(gateway.Object, "spec", "listeners")
			listeners = append(listeners, map[string]any{"name": "desktop", "hostname": config.DesktopHostname, "port": int64(443), "protocol": "HTTPS", "allowedRoutes": map[string]any{"namespaces": map[string]any{"from": "Same"}}})
			_ = unstructured.SetNestedSlice(gateway.Object, listeners, "spec", "listeners")
		}
		result = append(result, gateway)
		target := map[string]any{"group": "", "kind": "Service", "name": "gke-access-proxy"}
		iap := map[string]any{"enabled": true}
		if config.IAPClientID != "" {
			iap["clientID"] = config.IAPClientID
			iap["oauth2ClientSecret"] = map[string]any{"name": config.IAPSecretName}
		}
		result = append(result, object("networking.gke.io/v1", "GCPBackendPolicy", "notebooks-iap", systemNamespace, map[string]any{"spec": map[string]any{
			"targetRef": target, "default": map[string]any{
				"iap":        iap,
				"timeoutSec": int64(3600),
				"logging": map[string]any{
					"enabled":    true,
					"sampleRate": int64(1000000),
				},
				"sessionAffinity": map[string]any{
					"type":         "GENERATED_COOKIE",
					"cookieTtlSec": int64(86400),
				},
			},
		}}))
		result = append(result, object("networking.gke.io/v1", "HealthCheckPolicy", "notebooks", systemNamespace, map[string]any{"spec": map[string]any{
			"targetRef": target, "default": map[string]any{"config": map[string]any{"type": "HTTP", "httpHealthCheck": map[string]any{"port": int64(8080), "requestPath": "/healthz"}}},
		}}))
		result = append(result, object("gateway.networking.k8s.io/v1", "HTTPRoute", "notebooks", systemNamespace, map[string]any{"spec": map[string]any{
			"parentRefs": []any{map[string]any{"name": "notebooks", "sectionName": "https"}}, "hostnames": []any{config.Hostname},
			"rules": []any{map[string]any{"matches": []any{map[string]any{"path": map[string]any{"type": "PathPrefix", "value": "/"}}}, "backendRefs": []any{map[string]any{"name": "gke-access-proxy", "port": int64(8080)}}}},
		}}))
		if config.DesktopHostname != "" {
			result = append(result, desktopEdge(config)...)
		}
	default:
		return nil, fmt.Errorf("stage must be namespaces, isolation, applications, or edge")
	}
	return result, nil
}
