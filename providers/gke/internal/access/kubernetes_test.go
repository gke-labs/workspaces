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

package access

import (
	"context"
	"errors"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func kubeFixture(t *testing.T) (*KubernetesAccess, *fake.Clientset) {
	t.Helper()
	workspace := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubeflow.org/v1beta1", "kind": "Workspace",
		"metadata": map[string]any{"name": "lab", "namespace": "team-a", "uid": "current"},
		"spec":     map[string]any{"kind": "jupyterlab", "podTemplate": map[string]any{"options": map[string]any{"imageConfig": "scipy"}}},
	}}
	kind := &unstructured.Unstructured{}
	if err := kind.UnmarshalJSON([]byte(`{"apiVersion":"kubeflow.org/v1beta1","kind":"WorkspaceKind","metadata":{"name":"jupyterlab"},"spec":{"podTemplate":{"ports":[{"id":"jupyterlab","protocol":"HTTP","httpProxy":{"removePathPrefix":false}}],"options":{"imageConfig":{"values":[{"id":"scipy","spec":{"ports":[{"id":"jupyterlab","port":8888}]}}]}}}}}`)); err != nil {
		t.Fatal(err)
	}
	controller := true
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "lab-random", Namespace: "team-a", Labels: map[string]string{workspaceLabel: "lab"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "kubeflow.org/v1beta1", Kind: "Workspace", Name: "lab", UID: types.UID("current"), Controller: &controller}}},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.0.0.10", Selector: map[string]string{workspaceLabel: "lab", "statefulset": "lab"}, Ports: []corev1.ServicePort{{Port: 8888, Protocol: corev1.ProtocolTCP}}},
	}
	client := fake.NewSimpleClientset(service)
	client.PrependReactor("create", "subjectaccessreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
		review := action.(clienttesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		attributes := review.Spec.ResourceAttributes
		if review.Spec.User != "alice@example.com" || len(review.Spec.Groups) != 0 || attributes.Group != "kubeflow.org" || attributes.Resource != "workspaces" {
			t.Fatalf("unexpected SAR: %+v", review.Spec)
		}
		review.Status.Allowed = attributes.Namespace == "team-a"
		return true, review, nil
	})
	return &KubernetesAccess{core: client.CoreV1(), authorization: client.AuthorizationV1(), resources: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), workspace, kind), tenants: map[string]bool{"team-a": true, "team-b": true}}, client
}

func TestKubernetesTenantDiscovery(t *testing.T) {
	access, client := kubeFixture(t)
	identity := Identity{Email: "alice@example.com"}
	namespaces, err := access.Namespaces(context.Background(), identity)
	if err != nil || len(namespaces) != 1 || namespaces[0].Name != "team-a" {
		t.Fatalf("%v, %v", namespaces, err)
	}
	client.PrependReactor("create", "subjectaccessreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, &authorizationv1.SubjectAccessReview{}, nil
	})
	namespaces, err = access.Namespaces(context.Background(), identity)
	if err != nil || len(namespaces) != 0 {
		t.Fatalf("revoked user: %v, %v", namespaces, err)
	}
	client.PrependReactor("create", "subjectaccessreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("API unavailable")
	})
	if _, err := access.Namespaces(context.Background(), identity); err == nil {
		t.Fatal("authorization failure accepted")
	}
}

func TestKubernetesWorkspaceResolution(t *testing.T) {
	access, client := kubeFixture(t)
	identity := Identity{Email: "alice@example.com"}
	target, err := access.Resolve(context.Background(), identity, "team-a", "lab", "jupyterlab")
	if err != nil || target.URL.String() != "http://10.0.0.10:8888" || target.RemovePrefix {
		t.Fatalf("%+v, %v", target, err)
	}
	for _, namespace := range []string{"team-b", "unmanaged"} {
		if _, err := access.Resolve(context.Background(), identity, namespace, "lab", "jupyterlab"); !errors.Is(err, ErrDenied) {
			t.Fatalf("expected denied: %v", err)
		}
	}
	if _, err := access.Resolve(context.Background(), identity, "team-a", "lab", "9999"); !errors.Is(err, ErrDenied) {
		t.Fatalf("undeclared port: %v", err)
	}
	service, _ := client.CoreV1().Services("team-a").Get(context.Background(), "lab-random", metav1.GetOptions{})
	service.OwnerReferences[0].UID = "old-workspace"
	if _, err := client.CoreV1().Services("team-a").Update(context.Background(), service, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := access.Resolve(context.Background(), identity, "team-a", "lab", "jupyterlab"); err == nil {
		t.Fatal("stale Service accepted")
	}
}

func TestKubernetesResolutionFailures(t *testing.T) {
	for _, scenario := range []string{"external service", "wrong selector", "ambiguous services", "paused workspace", "custom headers", "evaluation error"} {
		t.Run(scenario, func(t *testing.T) {
			access, client := kubeFixture(t)
			ctx := context.Background()
			service, err := client.CoreV1().Services("team-a").Get(ctx, "lab-random", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "external service":
				service.Spec.Type = corev1.ServiceTypeExternalName
				service.Spec.ExternalName = "attacker.example"
			case "wrong selector":
				service.Spec.Selector[workspaceLabel] = "other"
			case "ambiguous services":
				other := service.DeepCopy()
				other.Name = "other-service"
				if _, err := client.CoreV1().Services("team-a").Create(ctx, other, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			case "paused workspace":
				workspace, err := access.resources.Resource(workspaceResource).Namespace("team-a").Get(ctx, "lab", metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if err := unstructured.SetNestedField(workspace.Object, true, "spec", "paused"); err != nil {
					t.Fatal(err)
				}
				if _, err := access.resources.Resource(workspaceResource).Namespace("team-a").Update(ctx, workspace, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			case "custom headers":
				kind, err := access.resources.Resource(kindResource).Get(ctx, "jupyterlab", metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				ports, _, err := unstructured.NestedSlice(kind.Object, "spec", "podTemplate", "ports")
				if err != nil {
					t.Fatal(err)
				}
				ports[0].(map[string]any)["httpProxy"] = map[string]any{"requestHeaders": map[string]any{"set": map[string]any{"X-Custom": "value"}}}
				if err := unstructured.SetNestedSlice(kind.Object, ports, "spec", "podTemplate", "ports"); err != nil {
					t.Fatal(err)
				}
				if _, err := access.resources.Resource(kindResource).Update(ctx, kind, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			case "evaluation error":
				client.PrependReactor("create", "subjectaccessreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
					return true, &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true, EvaluationError: "failed"}}, nil
				})
			}
			if _, err := client.CoreV1().Services("team-a").Update(ctx, service, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := access.Resolve(ctx, Identity{Email: "alice@example.com"}, "team-a", "lab", "jupyterlab"); err == nil {
				t.Fatal("unsafe or unsupported target accepted")
			}
		})
	}
}

func TestKubernetesMultipleTenants(t *testing.T) {
	access, client := kubeFixture(t)
	client.PrependReactor("create", "subjectaccessreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
		review := action.(clienttesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		if review.Spec.ResourceAttributes.Verb != "list" || review.Spec.ResourceAttributes.Name != "" {
			t.Fatalf("unexpected discovery SAR: %+v", review.Spec)
		}
		return true, &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}, nil
	})
	namespaces, err := access.Namespaces(context.Background(), Identity{Email: "alice@example.com"})
	if err != nil || len(namespaces) != 2 || namespaces[0].Name != "team-a" || namespaces[1].Name != "team-b" {
		t.Fatalf("%v, %v", namespaces, err)
	}
}
