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

package snapshot

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
)

// getResource reads an object from its informer cache, falling back to a live read
// while the caches are still warming up or when the resource is not watched.
func (c *Controller) getResource(ctx context.Context, gvr schema.GroupVersionResource, lister cache.GenericLister, namespace, name string) (*unstructured.Unstructured, error) {
	if lister != nil {
		var object any
		var err error
		if namespace == "" {
			object, err = lister.Get(name)
		} else {
			object, err = lister.ByNamespace(namespace).Get(name)
		}
		if err != nil {
			return nil, err
		}
		if converted := asUnstructured(object); converted != nil {
			return converted, nil
		}
		return nil, apierrors.NewNotFound(gvr.GroupResource(), name)
	}
	client := c.dynamic.Resource(gvr)
	if namespace != "" {
		return client.Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	}
	return client.Get(ctx, name, metav1.GetOptions{})
}

// getWorkspace returns a Workspace, preferring the informer cache.
func (c *Controller) getWorkspace(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	lister := c.workspaceLister
	if !c.cachesReady.Load() {
		lister = nil
	}
	return c.getResource(ctx, workspaceGVR, lister, namespace, name)
}

// getWorkspaceKind returns a cluster-scoped WorkspaceKind, preferring the cache.
func (c *Controller) getWorkspaceKind(ctx context.Context, name string) (*unstructured.Unstructured, error) {
	lister := c.kindLister
	if !c.cachesReady.Load() {
		lister = nil
	}
	return c.getResource(ctx, workspaceKindGVR, lister, "", name)
}

// findWorkspacePod returns the Pod backing a Workspace. It reads the indexed
// informer cache and only lists against the API server while the cache is cold,
// which can happen for admission requests during the first seconds after start-up.
func (c *Controller) findWorkspacePod(ctx context.Context, namespace, name string) *corev1.Pod {
	if c.cachesReady.Load() {
		return c.workspacePod(namespace, name)
	}
	pods, err := c.core.Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: WorkspaceLabel + "=" + name})
	if err != nil {
		return nil
	}
	var fallback *corev1.Pod
	for index := range pods.Items {
		pod := &pods.Items[index]
		if pod.DeletionTimestamp == nil {
			return pod
		}
		fallback = pod
	}
	return fallback
}

// podConfigRequestsTPU reports whether the named podConfig positively declares a
// request for TPU chips.
//
// This reads the resources the podConfig declares rather than matching on its id. An
// earlier version tested strings.Contains(id, "tpu"), which was wrong in both
// directions: renaming a podConfig from "tpu" to, say, "v5litepod" silently re-enabled
// snapshotting on hardware that cannot support it, and an unrelated id that happened to
// contain the substring was excluded for no reason. The Pod admission path already
// keyed off the resource, so the two paths could disagree about the same Workspace.
//
// Detection is deliberately positive-only: anything we cannot resolve (no podConfig
// named, or an id the WorkspaceKind does not define) returns false rather than
// guessing. Guessing "TPU" would silently disable pause/resume for ordinary
// Workspaces, and it is not needed for safety, because mutatePod performs the
// authoritative check against the real Pod's resources before anything is injected.
func podConfigRequestsTPU(kind *unstructured.Unstructured, podConfigID string) bool {
	if kind == nil || podConfigID == "" {
		return false
	}
	values, _, _ := unstructured.NestedSlice(kind.Object, "spec", "podTemplate", "options", "podConfig", "values")
	for _, value := range values {
		entry, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if id, _, _ := unstructured.NestedString(entry, "id"); id != podConfigID {
			continue
		}
		for _, field := range []string{"limits", "requests"} {
			resources, _, _ := unstructured.NestedMap(entry, "spec", "resources", field)
			for name := range resources {
				if name == ResourceTPU {
					return true
				}
			}
		}
		return false
	}
	return false
}

// snapshotEnabled reports whether stateful pause/resume is enabled for a Workspace
// and which PodSnapshotStorageConfig it should use.
//
// Precedence, highest first:
//  1. An explicit opt-out on the Workspace.
//  2. Hardware capability. A podConfig that requests TPUs can never be checkpointed,
//     because GKE checkpoints through gVisor and gVisor cannot host TPU devices. That
//     is a property of the hardware rather than a preference, so it beats an explicit
//     opt-in instead of losing to it.
//  3. The Workspace annotation.
//  4. The WorkspaceKind annotation.
func (c *Controller) snapshotEnabled(ctx context.Context, workspace *unstructured.Unstructured) (bool, string) {
	annotations := workspace.GetAnnotations()
	storageConfig := DefaultStorageConfigName
	if value, ok := annotations[AnnotationStorageConfig]; ok && value != "" {
		storageConfig = value
	}
	optedIn := strings.EqualFold(annotations[AnnotationEnabled], "true") ||
		strings.EqualFold(annotations[AnnotationPodMigrationEnabled], "true")

	// An explicit opt-out needs no further lookups.
	if value, ok := annotations[AnnotationEnabled]; ok && strings.EqualFold(value, "false") {
		return false, storageConfig
	}

	kindName, _, _ := unstructured.NestedString(workspace.Object, "spec", "kind")
	if kindName == "" {
		return false, storageConfig
	}
	kind, err := c.getWorkspaceKind(ctx, kindName)
	if err != nil {
		// The hardware cannot be checked without the WorkspaceKind. Fall back to the
		// Workspace's own annotation rather than failing closed on a transient read
		// error; mutatePod still refuses to touch a Pod that requests TPUs.
		return optedIn, storageConfig
	}

	podConfig, _, _ := unstructured.NestedString(workspace.Object, "spec", "podTemplate", "options", "podConfig")
	if podConfigRequestsTPU(kind, podConfig) {
		return false, storageConfig
	}

	kindAnnotations := kind.GetAnnotations()
	if value, ok := kindAnnotations[AnnotationStorageConfig]; ok && value != "" && annotations[AnnotationStorageConfig] == "" {
		storageConfig = value
	}
	if optedIn {
		return true, storageConfig
	}

	return strings.EqualFold(kindAnnotations[AnnotationEnabled], "true") ||
		strings.EqualFold(kindAnnotations[AnnotationPodMigrationEnabled], "true"), storageConfig
}

// migrationEnabled reports whether live Pod Migration opt-in (pod-migration.gke.io/enabled)
// is enabled for a Workspace, checking explicit Workspace opt-out, TPU hardware exclusion,
// Workspace annotation, and fallback to the parent WorkspaceKind annotation.
func (c *Controller) migrationEnabled(ctx context.Context, workspace *unstructured.Unstructured) bool {
	annotations := workspace.GetAnnotations()
	if value, ok := annotations[AnnotationPodMigrationEnabled]; ok && strings.EqualFold(value, "false") {
		return false
	}
	optedIn := strings.EqualFold(annotations[AnnotationPodMigrationEnabled], "true")

	kindName, _, _ := unstructured.NestedString(workspace.Object, "spec", "kind")
	if kindName == "" {
		return false
	}
	kind, err := c.getWorkspaceKind(ctx, kindName)
	if err != nil {
		return optedIn
	}
	podConfig, _, _ := unstructured.NestedString(workspace.Object, "spec", "podTemplate", "options", "podConfig")
	if podConfigRequestsTPU(kind, podConfig) {
		return false
	}
	if optedIn {
		return true
	}
	return strings.EqualFold(kind.GetAnnotations()[AnnotationPodMigrationEnabled], "true")
}
