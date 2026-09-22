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
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func fixture() Config {
	images := map[string]string{}
	for _, component := range []string{"controller", "backend", "frontend", "proxy", "snapshot"} {
		images[component] = "example.com/" + component + "@sha256:" + strings.Repeat("a", 64)
	}
	return Config{ControlPlaneCIDR: "10.0.0.1/32", Hostname: "notebooks.example.com", CertificateMap: "notebooks", AddressName: "notebooks", IAPClientID: "123-example.apps.googleusercontent.com", IAPSecretName: "iap-oauth", Tenants: []string{"team-a", "team-b"}, Images: images}
}

func TestConfigValidation(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(config *Config) { config.Images["proxy"] = "example.com/proxy:latest" },
		func(config *Config) { config.Tenants = []string{"kube-system"} },
		func(config *Config) { config.Tenants = []string{"team-a", "team-a"} },
		func(config *Config) { config.IAPAudience = "/projects/123/*" },
		func(config *Config) { config.Hostname = "example.com/path" },
		func(config *Config) { config.IAPClientID = "" },
		func(config *Config) { config.IAPSecretName = "" },
		func(config *Config) { config.ControlPlaneCIDR = "0.0.0.0/0" },
		func(config *Config) { config.DesktopHostname = config.Hostname },
		func(config *Config) { config.DesktopHostname = "invalid/path" },
		func(config *Config) { config.Tenants = []string{"notebooks-connections"} },
		func(config *Config) { config.ConnectionTokenDefaultSeconds = -1 },
		func(config *Config) { config.ConnectionTokenMaxSeconds = 2592001 },
		func(config *Config) { config.ConnectionTokenDefaultSeconds = 604801 },
	} {
		config := fixture()
		mutate(&config)
		if config.Validate() == nil {
			t.Fatal("invalid config accepted")
		}
	}
	encoded, _ := json.Marshal(fixture())
	if _, err := ReadConfig(strings.NewReader(string(encoded))); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadConfig(strings.NewReader(string(encoded) + " {}")); err == nil {
		t.Fatal("extra JSON accepted")
	}
}

func TestGoogleManagedOAuth(t *testing.T) {
	config := fixture()
	config.IAPClientID = ""
	config.IAPSecretName = ""
	resources, err := Render(context.Background(), "", "edge", config, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range resources {
		if resource.GetKind() != "GCPBackendPolicy" {
			continue
		}
		iap, found, err := unstructured.NestedMap(resource.Object, "spec", "default", "iap")
		if err != nil || !found || len(iap) != 1 || iap["enabled"] != true {
			t.Fatalf("managed OAuth must enable IAP without credential fields: %v", iap)
		}
		return
	}
	t.Fatal("missing IAP policy")
}

func TestWebhookIngressAllowsOnlyControlPlaneSources(t *testing.T) {
	config := fixture()
	resources, err := Render(context.Background(), "", "isolation", config, func(context.Context, string) ([]unstructured.Unstructured, error) {
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Both webhook servers (the upstream controller and the standalone snapshot
	// addon) must be reachable only from the control plane.
	if len(resources) != 2 {
		t.Fatalf("unexpected webhook policies: %v", resources)
	}
	names := map[string]string{
		"gke-webhook-ingress":          "workspaces-controller",
		"gke-snapshot-webhook-ingress": "gke-workspace-snapshot-addon",
	}
	for _, resource := range resources {
		app, known := names[resource.GetName()]
		if !known || resource.GetNamespace() != systemNamespace {
			t.Fatalf("unexpected webhook policy: %v", resource.GetName())
		}
		assertWebhookIngress(t, resource, app, config.ControlPlaneCIDR)
	}
}

func assertWebhookIngress(t *testing.T, resource unstructured.Unstructured, app, cidr string) {
	t.Helper()
	spec, found, err := unstructured.NestedMap(resource.Object, "spec")
	if err != nil || !found {
		t.Fatalf("missing policy spec: %v", err)
	}
	want := map[string]any{
		"podSelector": map[string]any{"matchLabels": map[string]any{"app": app}},
		"policyTypes": []any{"Ingress"},
		"ingress": []any{map[string]any{
			"from": []any{
				map[string]any{"ipBlock": map[string]any{"cidr": cidr}},
				map[string]any{
					"namespaceSelector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": "kube-system"}},
					"podSelector":       map[string]any{"matchLabels": map[string]any{"k8s-app": "konnectivity-agent"}},
				},
			},
			"ports": []any{map[string]any{"protocol": "TCP", "port": int64(9443)}},
		}},
	}
	if !reflect.DeepEqual(spec, want) {
		t.Fatalf("webhook ingress must restrict both the namespace and pod selector to TCP 9443: %v", spec)
	}
}

func TestDesktopDeploymentBoundary(t *testing.T) {
	config := fixture()
	config.DesktopHostname = "connect.example.com"
	resources, err := Render(context.Background(), "", "edge", config, nil)
	if err != nil {
		t.Fatal(err)
	}
	policies, routes := 0, 0
	for _, resource := range resources {
		if resource.GetKind() == "GCPBackendPolicy" {
			policies++
			enabled, _, _ := unstructured.NestedBool(resource.Object, "spec", "default", "iap", "enabled")
			service, _, _ := unstructured.NestedString(resource.Object, "spec", "targetRef", "name")
			if enabled != (service == "gke-access-proxy") {
				t.Fatal("browser IAP must remain enabled independently of desktop backend")
			}
			if service == "gke-desktop-proxy" {
				logging, found, _ := unstructured.NestedBool(resource.Object, "spec", "default", "logging", "enabled")
				if !found || logging {
					t.Fatal("desktop token URLs must not be logged")
				}
			}
		}
		if resource.GetKind() == "HTTPRoute" && resource.GetName() == "notebooks-desktop" {
			routes++
			hosts, _, _ := unstructured.NestedStringSlice(resource.Object, "spec", "hostnames")
			if len(hosts) != 1 || hosts[0] != config.DesktopHostname {
				t.Fatal("desktop route must have a separate host")
			}
		}
	}
	if policies != 2 || routes != 1 {
		t.Fatal("missing separated edge resources")
	}
	for _, resource := range desktopApplications() {
		if resource.GetKind() == "Role" && resource.GetNamespace() != "notebooks-connections" {
			t.Fatal("grant minting permissions escaped the dedicated namespace")
		}
	}
}

func TestConnectionLifetimeRendering(t *testing.T) {
	for _, custom := range []bool{false, true} {
		config := fixture()
		config.DesktopHostname = "connect.example.com"
		wantDefault, wantMax := "86400", "604800"
		if custom {
			config.ConnectionTokenDefaultSeconds = 604800
			config.ConnectionTokenMaxSeconds = 2592000
			wantDefault, wantMax = "604800", "2592000"
		}
		resources, err := Render(context.Background(), "", "applications", config, func(context.Context, string) ([]unstructured.Unstructured, error) { return nil, nil })
		if err != nil {
			t.Fatal(err)
		}
		defaultSeconds, _, _ := unstructured.NestedString(resources[0].Object, "data", "CONNECTION_TOKEN_DEFAULT_SECONDS")
		maxSeconds, _, _ := unstructured.NestedString(resources[0].Object, "data", "CONNECTION_TOKEN_MAX_SECONDS")
		if defaultSeconds != wantDefault || maxSeconds != wantMax {
			t.Fatalf("lifetime configuration lost: %s, %s", defaultSeconds, maxSeconds)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestRenderStages(t *testing.T) {
	root := repoRoot(t)
	config := fixture()
	for _, stage := range []string{"namespaces", "isolation", "applications", "edge"} {
		t.Run(stage, func(t *testing.T) {
			resources, err := Render(context.Background(), root, stage, config, Kustomize)
			if err != nil {
				t.Fatal(err)
			}
			if len(resources) == 0 {
				t.Fatal("empty stage")
			}
			deployments := 0
			for _, resource := range resources {
				if resource.GetKind() == "ClusterRole" && resource.Object["aggregationRule"] != nil {
					if _, exists := resource.Object["rules"]; exists {
						t.Fatal("aggregated role must not own controller-populated rules")
					}
				}
				if strings.Contains(resource.GetAPIVersion(), "istio.io") {
					t.Fatal("Istio resource rendered")
				}
				if resource.GetKind() == "Secret" {
					t.Fatal("renderer must not contain credentials")
				}
				if resource.GetKind() == "Deployment" {
					deployments++
					containers, _, _ := unstructured.NestedSlice(resource.Object, "spec", "template", "spec", "containers")
					if len(containers) != 1 || !strings.Contains(containers[0].(map[string]any)["image"].(string), "@sha256:") {
						t.Fatal("unpinned deployment")
					}
				}
				if resource.GetKind() == "ConfigMap" {
					audience, _, _ := unstructured.NestedString(resource.Object, "data", "IAP_AUDIENCE")
					if audience != "" {
						t.Fatal("bootstrap audience must remain empty, not fabricated")
					}
				}
				if resource.GetKind() == "GCPBackendPolicy" {
					enabled, _, _ := unstructured.NestedBool(resource.Object, "spec", "default", "iap", "enabled")
					if !enabled {
						t.Fatal("IAP disabled")
					}
				}
				if stage == "isolation" && resource.GetNamespace() == "" {
					t.Fatal("unscoped isolation resource")
				}
			}
			if stage == "applications" && deployments != 5 {
				t.Fatalf("got %d deployments", deployments)
			}
		})
	}
}

func TestPlanCommand(t *testing.T) {
	root := repoRoot(t)
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.json")
	encoded, err := json.Marshal(fixture())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(directory, "plan")
	run := func() error {
		planScript := filepath.Join(root, "providers/gke/scripts/plan.sh")
		if _, err := os.Stat(planScript); err != nil {
			planScript = filepath.Join(root, "gke/scripts/plan.sh")
		}
		command := exec.Command("bash", planScript, configPath, output)
		result, err := command.CombinedOutput()
		if err != nil {
			t.Log(string(result))
		}
		return err
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"namespaces", "isolation", "applications", "edge"} {
		content, err := os.ReadFile(filepath.Join(output, stage+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var list unstructured.UnstructuredList
		if err := json.Unmarshal(content, &list); err != nil {
			t.Fatal(err)
		}
		if list.GetKind() != "List" || len(list.Items) == 0 {
			t.Fatalf("invalid stage %s", stage)
		}
	}
	if err := run(); err == nil {
		t.Fatal("existing plan was overwritten")
	}
}

func TestUpstreamContracts(t *testing.T) {
	root := repoRoot(t)
	resources, err := Render(context.Background(), root, "applications", fixture(), Kustomize)
	if err != nil {
		t.Fatal(err)
	}
	certificate, webhook, backend, controller := false, false, false, false
	for _, resource := range resources {
		if resource.GetKind() == "CustomResourceDefinition" && resource.GetAnnotations()["cert-manager.io/inject-ca-from"] != "kubeflow-workspaces/workspaces-serving-cert" {
			t.Fatal("CRD certificate injection reference is not configured")
		}
		switch resource.GetKind() + "/" + resource.GetName() {
		case "Certificate/workspaces-serving-cert":
			certificate = true
			dnsNames, _, _ := unstructured.NestedStringSlice(resource.Object, "spec", "dnsNames")
			secret, _, _ := unstructured.NestedString(resource.Object, "spec", "secretName")
			if len(dnsNames) != 2 || dnsNames[0] != "workspaces-webhook-service.kubeflow-workspaces.svc" || secret != "webhook-server-cert" {
				t.Fatal("webhook certificate contract changed")
			}
		case "ValidatingWebhookConfiguration/workspaces-validating-webhook-configuration":
			webhook = true
			if resource.GetAnnotations()["cert-manager.io/inject-ca-from"] != "kubeflow-workspaces/workspaces-serving-cert" {
				t.Fatal("missing webhook trust injection")
			}
		case "Deployment/workspaces-backend", "Deployment/workspaces-controller":
			containers, _, _ := unstructured.NestedSlice(resource.Object, "spec", "template", "spec", "containers")
			environment := map[string]string{}
			for _, entry := range containers[0].(map[string]any)["env"].([]any) {
				variable := entry.(map[string]any)
				if value, ok := variable["value"].(string); ok {
					environment[variable["name"].(string)] = value
				}
			}
			if resource.GetName() == "workspaces-backend" {
				backend = true
				if environment["DISABLE_AUTH"] != "false" || environment["USERID_HEADER"] != "kubeflow-userid" || environment["PROXY_URL_PREFIX"] != "/workspaces" {
					t.Fatal("backend auth contract changed")
				}
			} else {
				controller = true
				if environment["USE_ISTIO"] != "false" {
					t.Fatal("Istio enabled")
				}
			}
		}
	}
	if !certificate || !webhook || !backend || !controller {
		t.Fatal("required upstream resources missing")
	}
}
