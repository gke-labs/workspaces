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
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
)

// reconcileWorkspace drives the checkpoint/restore state machine for one Workspace.
// Every read is served from an informer cache; the API server is only contacted to
// make a change. A non-zero duration asks for a delayed re-reconcile (used for the
// socket settle grace period); everything else is driven by watch events.
func (c *Controller) reconcileWorkspace(ctx context.Context, key types.NamespacedName) (time.Duration, error) {
	workspace, err := c.getWorkspace(ctx, key.Namespace, key.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.reconcileDeletedWorkspace(ctx, key.Namespace, key.Name)
		}
		return 0, err
	}
	enabled, storageConfigName := c.snapshotEnabled(ctx, workspace)
	if !enabled {
		return 0, nil
	}

	namespace, name := workspace.GetNamespace(), workspace.GetName()
	if err := c.ensureIPCConfigMap(ctx, namespace); err != nil {
		return 0, fmt.Errorf("ensure ConfigMap %s/%s: %w", namespace, IPCConfigMapName, err)
	}
	if err := c.ensureStorageConfigAndPolicy(ctx, workspace, storageConfigName); err != nil {
		return 0, fmt.Errorf("ensure PodSnapshot policy: %w", err)
	}

	annotations := workspace.GetAnnotations()
	checkpointState := annotations[AnnotationCheckpointState]
	lastCheckpoint := annotations[AnnotationLastCheckpointName]
	paused, _, _ := unstructured.NestedBool(workspace.Object, "spec", "paused")

	if checkpointState == CheckpointStateCheckpointing {
		return c.reconcileCheckpointing(ctx, workspace)
	}
	if paused {
		// Paused with a saved checkpoint: nothing to do until the user resumes.
		return 0, nil
	}
	return 0, c.reconcileActive(ctx, namespace, name, checkpointState, lastCheckpoint)
}

// reconcileCheckpointing runs while the user asked for a pause and spec.paused is
// held at false so the StatefulSet keeps the Pod alive for GKE to snapshot.
func (c *Controller) reconcileCheckpointing(ctx context.Context, workspace *unstructured.Unstructured) (time.Duration, error) {
	namespace, name := workspace.GetNamespace(), workspace.GetName()
	annotations := workspace.GetAnnotations()

	pod := c.findWorkspacePod(ctx, namespace, name)
	if pod == nil || pod.DeletionTimestamp != nil {
		// There is nothing left to checkpoint; let the pause complete.
		logf("reconciler", "Workspace %s/%s has no Pod to checkpoint; completing pause", namespace, name)
		return 0, c.patchWorkspacePauseAndAnnotations(ctx, namespace, name, true, map[string]any{
			AnnotationCheckpointState:     CheckpointStateReady,
			AnnotationCheckpointStartedAt: nil,
		})
	}

	// Keep the Pod out of the Service EndpointSlice for the whole checkpoint.
	if err := c.setPodActiveCondition(ctx, pod, corev1.ConditionFalse, "CheckpointingInProgress", "Pausing: taking GKE PodSnapshot"); err != nil {
		return 0, fmt.Errorf("flip readinessGate on %s/%s: %w", namespace, pod.Name, err)
	}

	startedAt, hasStart := parseCheckpointStart(annotations[AnnotationCheckpointStartedAt])
	if hasStart {
		// Socket settle grace period: give EndpointSlice removal time to drain open
		// WebSockets before the memory image is written.
		if remaining := c.settle - time.Since(startedAt); remaining > 0 {
			return remaining, nil
		}
	}

	triggerName := triggerNameFor(name)
	trigger, err := c.getResource(ctx, podSnapshotManualTriggerGVR, c.triggerLister, namespace, triggerName)
	switch {
	case apierrors.IsNotFound(err):
		return 0, c.createManualTrigger(ctx, workspace, pod.Name, annotations[AnnotationCheckpointStartedAt])
	case err != nil:
		return 0, err
	}
	// A trigger stamped with a different checkpoint belongs to an earlier pause and
	// cannot satisfy this one. Comparing the stamp rather than timestamps keeps this
	// independent of clock skew between the webhook and the API server.
	if stamp := trigger.GetAnnotations()[AnnotationCheckpointStartedAt]; stamp != annotations[AnnotationCheckpointStartedAt] {
		logf("reconciler", "Deleting PodSnapshotManualTrigger %s/%s left over from an earlier pause", namespace, triggerName)
		if err := c.dynamic.Resource(podSnapshotManualTriggerGVR).Namespace(namespace).Delete(ctx, triggerName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return 0, err
		}
		return 0, nil
	}

	snapshotName, _, _ := unstructured.NestedString(trigger.Object, "status", "snapshotCreated", "name")
	if snapshotName == "" {
		// Waiting for the GKE controller; the trigger watch wakes us up.
		return 0, nil
	}
	snapshot, err := c.getResource(ctx, podSnapshotGVR, c.snapshotLister, namespace, snapshotName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return 0, nil
		}
		return 0, err
	}

	// Note: we deliberately do not attach an ownerReference to the PodSnapshot here.
	// GKE's ValidatingAdmissionPolicy rejects every update to a PodSnapshot that does
	// not come from its own controller, so cleanup is explicit (see reconcileActive
	// and deleteWorkspaceSnapshots).
	if !conditionTrue(snapshot, "Ready") {
		return 0, nil
	}

	logf("reconciler", "PodSnapshot %s/%s is Ready; completing pause for Workspace %s", namespace, snapshotName, name)
	return 0, c.patchWorkspacePauseAndAnnotations(ctx, namespace, name, true, map[string]any{
		AnnotationLastCheckpointName:  snapshotName,
		AnnotationCheckpointState:     CheckpointStateReady,
		AnnotationCheckpointStartedAt: nil,
	})
}

// reconcileActive runs while the Workspace is meant to be serving. It publishes the
// readiness gate and, once a restored Pod is Ready, retires the consumed snapshot.
func (c *Controller) reconcileActive(ctx context.Context, namespace, name, checkpointState, lastCheckpoint string) error {
	pod := c.findWorkspacePod(ctx, namespace, name)
	if pod == nil || pod.DeletionTimestamp != nil {
		return nil
	}
	if pod.Status.Phase != corev1.PodRunning || !allContainersReady(pod) {
		return nil
	}
	if err := c.setPodActiveCondition(ctx, pod, corev1.ConditionTrue, "WorkspaceActive", "Workspace pod is active"); err != nil {
		return fmt.Errorf("publish readinessGate on %s/%s: %w", namespace, pod.Name, err)
	}
	if lastCheckpoint == "" && checkpointState == "" {
		return nil
	}
	if !isPodReady(pod) {
		// The Pod becoming Ready produces another event; no polling required.
		return nil
	}

	logf("reconciler", "Workspace %s/%s restored and Ready; retiring PodSnapshot %q", namespace, name, lastCheckpoint)
	// Delete by label rather than by name so a snapshot left behind by an interrupted
	// earlier cycle is collected too.
	if err := c.deleteWorkspaceSnapshots(ctx, namespace, name); err != nil {
		return err
	}
	if err := c.dynamic.Resource(podSnapshotManualTriggerGVR).Namespace(namespace).Delete(ctx, triggerNameFor(name), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return c.patchWorkspacePauseAndAnnotations(ctx, namespace, name, false, map[string]any{
		AnnotationLastCheckpointName:  nil,
		AnnotationCheckpointState:     nil,
		AnnotationCheckpointStartedAt: nil,
	})
}

func triggerNameFor(workspace string) string { return fmt.Sprintf("ws-%s-trigger", workspace) }

func policyNameFor(workspace string) string { return fmt.Sprintf("ws-%s-policy", workspace) }

// workspaceFromTriggerName is the inverse of triggerNameFor. It lets us map a
// PodSnapshot back to its Workspace using the trigger name GKE records in the
// gke-pod-snapshot-triggered-by label.
func workspaceFromTriggerName(trigger string) (string, bool) {
	return workspaceFromDerivedName(trigger, "-trigger")
}

// workspaceFromPolicyName is the inverse of policyNameFor.
func workspaceFromPolicyName(policy string) (string, bool) {
	return workspaceFromDerivedName(policy, "-policy")
}

func workspaceFromDerivedName(derived, suffix string) (string, bool) {
	name, ok := strings.CutPrefix(derived, "ws-")
	if !ok {
		return "", false
	}
	name, ok = strings.CutSuffix(name, suffix)
	if !ok || name == "" {
		return "", false
	}
	return name, true
}

func parseCheckpointStart(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	startedAt, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, false
	}
	return startedAt, true
}

func conditionTrue(object *unstructured.Unstructured, conditionType string) bool {
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, entry := range conditions {
		condition, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if condition["type"] == conditionType && condition["status"] == "True" {
			return true
		}
	}
	return false
}

// workspaceOwnerReference ties a resource we create to its Workspace so that the
// Kubernetes garbage collector retires it when the Workspace is deleted.
func workspaceOwnerReference(workspace *unstructured.Unstructured) map[string]any {
	return map[string]any{
		"apiVersion":         "kubeflow.org/v1beta1",
		"kind":               "Workspace",
		"name":               workspace.GetName(),
		"uid":                string(workspace.GetUID()),
		"controller":         false,
		"blockOwnerDeletion": true,
	}
}

func (c *Controller) createManualTrigger(ctx context.Context, workspace *unstructured.Unstructured, podName, checkpointStamp string) error {
	namespace, name := workspace.GetNamespace(), workspace.GetName()
	triggerName := triggerNameFor(name)
	logf("reconciler", "Creating PodSnapshotManualTrigger %s/%s for Pod %s", namespace, triggerName, podName)
	trigger := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "podsnapshot.gke.io/v1",
			"kind":       "PodSnapshotManualTrigger",
			"metadata": map[string]any{
				"name":            triggerName,
				"namespace":       namespace,
				"annotations":     map[string]any{AnnotationCheckpointStartedAt: checkpointStamp},
				"ownerReferences": []any{workspaceOwnerReference(workspace)},
			},
			"spec": map[string]any{
				"targetPod": podName,
			},
		},
	}
	_, err := c.dynamic.Resource(podSnapshotManualTriggerGVR).Namespace(namespace).Create(ctx, trigger, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// reconcileDeletedWorkspace tears down what a Workspace left behind, in an order
// that GKE requires.
//
// GKE's snapshot finalizer resolves a PodSnapshot's storage location by following
// spec.policyName to the PodSnapshotPolicy and on to the PodSnapshotStorageConfig.
// If the policy disappears first, the finalizer cannot find the bucket: it drops the
// PodSnapshot object anyway and silently leaves the checkpoint files behind in GCS.
// So we delete the snapshots, wait for GKE to finish finalizing them, and only then
// retire the policy.
//
// The policy doubles as the teardown marker. While it exists there is work to do;
// once it is gone this is a no-op, which is what stops the delete events we generate
// here from feeding back into another round. It also self-heals after downtime:
// because the policy outlives the Workspace, the policy informer's initial sync
// re-enqueues the absent Workspace on startup and we resume an interrupted teardown.
func (c *Controller) reconcileDeletedWorkspace(ctx context.Context, namespace, name string) (time.Duration, error) {
	_, err := c.getResource(ctx, podSnapshotPolicyGVR, c.policyLister, namespace, policyNameFor(name))
	if apierrors.IsNotFound(err) {
		hasPM, pmErr := c.hasPodMigration(ctx, namespace)
		if pmErr != nil || !hasPM {
			return 0, pmErr
		}
		draining, snapErr := c.drainingWorkspaceSnapshots(ctx, namespace, name)
		if snapErr != nil {
			return 0, snapErr
		}
		if len(draining) > 0 && !allTerminating(draining) {
			if err := c.deleteWorkspaceSnapshots(ctx, namespace, name); err != nil {
				return 0, err
			}
			return 0, c.retirePolicy(ctx, namespace, name)
		}
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	draining, err := c.drainingWorkspaceSnapshots(ctx, namespace, name)
	if err != nil {
		return 0, err
	}
	if !allTerminating(draining) {
		// Either snapshots we have not asked about yet, or none visible at all. One
		// delete call covers both, and covers a cold cache that would otherwise let
		// us retire the policy while snapshots still depend on it.
		if err := c.deleteWorkspaceSnapshots(ctx, namespace, name); err != nil {
			return 0, err
		}
		if len(draining) == 0 {
			return 0, c.retirePolicy(ctx, namespace, name)
		}
		return snapshotDrainInterval, nil
	}

	// Everything is already terminating, so do not re-issue the delete. GKE rewrites a
	// terminating PodSnapshot's status many times per second and each of those writes
	// is a watch event that lands us back here; deleting again on every pass turns
	// that churn into a write loop against the API server.
	waited, _ := drainProgress(draining)
	if waited < snapshotDrainTimeout {
		// Keep the policy alive so GKE's finalizer can still resolve the bucket.
		return snapshotDrainInterval, nil
	}
	// GKE has had long enough. It removes the GCS objects within seconds of the delete
	// and only then releases its finalizer, so a snapshot still held after
	// snapshotDrainTimeout means the finalizer is wedged, not that data is at risk.
	// Stop pinning the policy rather than leaking it forever. In the worst case the
	// files really were missed, and the Delete lifecycle rule that deploy_standalone.sh
	// puts on the bucket (SNAPSHOT_RETENTION_DAYS, 14 days by default) bounds the cost.
	logf("reconciler", "PodSnapshots for Workspace %s/%s have been terminating for %s; retiring the PodSnapshotPolicy anyway", namespace, name, waited.Round(time.Second))
	return 0, c.retirePolicy(ctx, namespace, name)
}

// retirePolicy removes the last traces of a Workspace once its snapshots no longer
// need the policy to resolve their storage location.
func (c *Controller) retirePolicy(ctx context.Context, namespace, name string) error {
	logf("reconciler", "Workspace %s/%s is gone and its snapshots are retired; removing PodSnapshotPolicy", namespace, name)
	for gvr, resourceName := range map[schema.GroupVersionResource]string{
		podSnapshotPolicyGVR:        policyNameFor(name),
		podSnapshotManualTriggerGVR: triggerNameFor(name),
	} {
		if err := c.dynamic.Resource(gvr).Namespace(namespace).Delete(ctx, resourceName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// disownPolicy removes a Workspace ownerReference from a PodSnapshotPolicy written
// by an earlier version of this addon. Those policies would be garbage collected the
// moment their Workspace is deleted, which races our snapshot teardown and strands
// checkpoint files in GCS (see reconcileDeletedWorkspace). Steady state is a no-op.
func (c *Controller) disownPolicy(ctx context.Context, policy *unstructured.Unstructured) error {
	if len(policy.GetOwnerReferences()) == 0 {
		return nil
	}
	logf("reconciler", "Removing legacy Workspace ownerReference from PodSnapshotPolicy %s/%s", policy.GetNamespace(), policy.GetName())
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"ownerReferences": []any{}},
	})
	if err != nil {
		return err
	}
	_, err = c.dynamic.Resource(podSnapshotPolicyGVR).Namespace(policy.GetNamespace()).
		Patch(ctx, policy.GetName(), types.MergePatchType, patch, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// drainingWorkspaceSnapshots returns a Workspace's PodSnapshots that are still
// present, including those that are terminating but still hold GKE's finalizer.
func (c *Controller) drainingWorkspaceSnapshots(ctx context.Context, namespace, workspace string) ([]*unstructured.Unstructured, error) {
	selector := labels.SelectorFromSet(labels.Set{LabelSnapshotTriggeredBy: triggerNameFor(workspace)})
	if c.snapshotLister == nil {
		// Caches are not warm yet; a stale "empty" here would retire the policy too
		// early, so pay for a live read instead.
		list, err := c.dynamic.Resource(podSnapshotGVR).Namespace(namespace).List(
			ctx, metav1.ListOptions{LabelSelector: selector.String()})
		if err != nil {
			return nil, err
		}
		snapshots := make([]*unstructured.Unstructured, 0, len(list.Items))
		for i := range list.Items {
			snapshots = append(snapshots, &list.Items[i])
		}
		return snapshots, nil
	}
	objects, err := c.snapshotLister.ByNamespace(namespace).List(selector)
	if err != nil {
		return nil, err
	}
	snapshots := make([]*unstructured.Unstructured, 0, len(objects))
	for _, object := range objects {
		if snapshot := asUnstructured(object); snapshot != nil {
			snapshots = append(snapshots, snapshot)
		}
	}
	return snapshots, nil
}

// allTerminating reports whether every snapshot has already been asked to go away.
// An empty set is deliberately *not* "all terminating": we still want one delete
// call before concluding there is nothing left to wait for.
func allTerminating(snapshots []*unstructured.Unstructured) bool {
	if len(snapshots) == 0 {
		return false
	}
	for _, snapshot := range snapshots {
		if snapshot.GetDeletionTimestamp() == nil {
			return false
		}
	}
	return true
}

// drainProgress reports whether any snapshots are left and, if so, how long the most
// recently deleted one has been terminating. The newest deletion is the one that
// needs the most remaining time, so reporting its age keeps the caller conservative.
// A snapshot we have only just asked to delete may not carry a deletionTimestamp
// yet, which counts as zero elapsed time.
func drainProgress(snapshots []*unstructured.Unstructured) (time.Duration, bool) {
	if len(snapshots) == 0 {
		return 0, false
	}
	newest := time.Duration(math.MaxInt64)
	for _, snapshot := range snapshots {
		deletedAt := snapshot.GetDeletionTimestamp()
		if deletedAt == nil {
			return 0, true
		}
		if waited := time.Since(deletedAt.Time); waited < newest {
			newest = waited
		}
	}
	return newest, true
}

// deleteWorkspaceSnapshots retires every PodSnapshot that was produced for a
// Workspace.
//
// PodSnapshots deliberately carry no Workspace ownerReference. GKE installs a
// ValidatingAdmissionPolicy ("gke-pod-snapshot-validating-admission-policy") that
// rejects any update to a PodSnapshot from a principal other than its own snapshot
// controller and agent, so attaching one is impossible and we cannot rely on
// Kubernetes garbage collection. GKE does however label each snapshot with the
// trigger that produced it, and our trigger name is derived from the Workspace name,
// which gives us a reliable way to find them.
func (c *Controller) deleteWorkspaceSnapshots(ctx context.Context, namespace, workspace string) error {
	selector := fmt.Sprintf("%s=%s", LabelSnapshotTriggeredBy, triggerNameFor(workspace))
	err := c.dynamic.Resource(podSnapshotGVR).Namespace(namespace).DeleteCollection(
		ctx, metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: selector})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete PodSnapshots for Workspace %s/%s: %w", namespace, workspace, err)
	}
	return nil
}

// ensureIPCConfigMap makes sure the Jupyter IPC configuration exists in a tenant
// namespace. The lookup is cache backed, so the common case costs nothing.
func (c *Controller) ensureIPCConfigMap(ctx context.Context, namespace string) error {
	existing, err := c.lookupIPCConfigMap(ctx, namespace)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if existing != nil {
		if existing.Data[IPCConfigMapKey] == jupyterIPCServerConfig {
			return nil
		}
		updated := existing.DeepCopy()
		updated.Data = map[string]string{IPCConfigMapKey: jupyterIPCServerConfig}
		_, err = c.core.ConfigMaps(namespace).Update(ctx, updated, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	_, err = c.core.ConfigMaps(namespace).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: IPCConfigMapName, Namespace: namespace},
		Data:       map[string]string{IPCConfigMapKey: jupyterIPCServerConfig},
	}, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (c *Controller) lookupIPCConfigMap(ctx context.Context, namespace string) (*corev1.ConfigMap, error) {
	if c.configMapLister != nil && c.cachesReady.Load() {
		configMap, err := c.configMapLister.ConfigMaps(namespace).Get(IPCConfigMapName)
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return configMap, err
	}
	configMap, err := c.core.ConfigMaps(namespace).Get(ctx, IPCConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return configMap, err
}

// ensureStorageConfigAndPolicy creates the cluster-scoped PodSnapshotStorageConfig
// and the per-Workspace PodSnapshotPolicy if they are missing. When live Pod Migration
// opt-in (pod-migration.gke.io/enabled: "true") is enabled for the Workspace, it
// delegates policy and storage provisioning to the namespace-scoped singleton
// PodMigration CR instead of creating a duplicate per-Workspace PodSnapshotPolicy.
func (c *Controller) ensureStorageConfigAndPolicy(ctx context.Context, workspace *unstructured.Unstructured, storageConfigName string) error {
	if c.migrationEnabled(ctx, workspace) {
		return c.ensurePodMigration(ctx, workspace, storageConfigName)
	}

	if _, err := c.getResource(ctx, podSnapshotStorageConfigGVR, c.storageConfigLister, "", storageConfigName); apierrors.IsNotFound(err) {
		storageConfig := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "podsnapshot.gke.io/v1",
				"kind":       "PodSnapshotStorageConfig",
				"metadata":   map[string]any{"name": storageConfigName},
				"spec": map[string]any{
					"snapshotStorageConfig": map[string]any{
						"gcs": map[string]any{"bucket": c.bucket, "path": "kubeflow-notebooks"},
					},
				},
			},
		}
		if _, err := c.dynamic.Resource(podSnapshotStorageConfigGVR).Create(ctx, storageConfig, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	} else if err != nil {
		return err
	}

	namespace, name := workspace.GetNamespace(), workspace.GetName()
	policyName := policyNameFor(name)
	existing, err := c.getResource(ctx, podSnapshotPolicyGVR, c.policyLister, namespace, policyName)
	if err == nil {
		return c.disownPolicy(ctx, existing)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	// The policy deliberately carries no Workspace ownerReference. GKE's snapshot
	// finalizer resolves a PodSnapshot's GCS location through spec.policyName, so if
	// the garbage collector removed the policy the instant the Workspace went away,
	// the finalizer would drop the snapshot object while orphaning its checkpoint
	// files in the bucket. reconcileDeletedWorkspace retires the policy itself, only
	// once every snapshot that depends on it is gone.
	policy := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "podsnapshot.gke.io/v1",
			"kind":       "PodSnapshotPolicy",
			"metadata": map[string]any{
				"name":      policyName,
				"namespace": namespace,
			},
			"spec": map[string]any{
				"storageConfigName": storageConfigName,
				"selector": map[string]any{
					"matchLabels": map[string]any{WorkspaceLabel: name},
				},
				"triggerConfig": map[string]any{
					"type":           "manual",
					"postCheckpoint": "stop",
				},
				"retentionConfig": map[string]any{
					"lastAccessTimeout": "7d",
				},
			},
		},
	}
	if _, err := c.dynamic.Resource(podSnapshotPolicyGVR).Namespace(namespace).Create(ctx, policy, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// hasPodMigration reports whether at least one PodMigration CR exists in namespace.
func (c *Controller) hasPodMigration(ctx context.Context, namespace string) (bool, error) {
	list, err := c.dynamic.Resource(podMigrationGVR).Namespace(namespace).List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return len(list.Items) > 0, nil
}

// resolveStorageLocation determines the gs://<bucket>/<path> URI for a PodMigration
// CR, honoring a custom cluster-scoped PodSnapshotStorageConfig when referenced by
// podsnapshot.gke.kubeflow.org/storage-config and falling back to the addon's
// default bucket otherwise.
func (c *Controller) resolveStorageLocation(ctx context.Context, storageConfigName string) (string, error) {
	defaultLocation := fmt.Sprintf("gs://%s/kubeflow-notebooks", c.bucket)
	if storageConfigName == "" {
		return defaultLocation, nil
	}
	storageConfig, err := c.getResource(ctx, podSnapshotStorageConfigGVR, c.storageConfigLister, "", storageConfigName)
	if apierrors.IsNotFound(err) {
		return defaultLocation, nil
	}
	if err != nil {
		return "", err
	}
	bucket, _, _ := unstructured.NestedString(storageConfig.Object, "spec", "snapshotStorageConfig", "gcs", "bucket")
	if bucket == "" {
		return defaultLocation, nil
	}
	path, _, _ := unstructured.NestedString(storageConfig.Object, "spec", "snapshotStorageConfig", "gcs", "path")
	path = strings.Trim(path, "/")
	if path == "" {
		return fmt.Sprintf("gs://%s", bucket), nil
	}
	return fmt.Sprintf("gs://%s/%s", bucket, path), nil
}

// ensurePodMigration ensures a singleton PodMigration CR exists in the tenant
// namespace and retires any legacy per-Workspace PodSnapshotPolicy once no
// unconsumed checkpoints depend on it.
func (c *Controller) ensurePodMigration(ctx context.Context, workspace *unstructured.Unstructured, storageConfigName string) error {
	namespace, name := workspace.GetNamespace(), workspace.GetName()
	storageLocation, err := c.resolveStorageLocation(ctx, storageConfigName)
	if err != nil {
		return err
	}
	list, err := c.dynamic.Resource(podMigrationGVR).Namespace(namespace).List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if list == nil || len(list.Items) == 0 {
		pm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": podmigrationGroupVersion,
				"kind":       "PodMigration",
				"metadata": map[string]any{
					"name":      DefaultPodMigrationName,
					"namespace": namespace,
				},
				"spec": map[string]any{
					"selector": map[string]any{
						"matchLabels": map[string]any{
							AnnotationPodMigrationEnabled: "true",
						},
					},
					"strategy": "CheckpointAndRestore",
					"storage": map[string]any{
						"type":     "GCS",
						"location": storageLocation,
					},
				},
			},
		}
		if _, err := c.dynamic.Resource(podMigrationGVR).Namespace(namespace).Create(ctx, pm, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	} else if existing := &list.Items[0]; existing.GetName() == DefaultPodMigrationName {
		if currentLocation, _, _ := unstructured.NestedString(existing.Object, "spec", "storage", "location"); currentLocation != storageLocation {
			patch, err := json.Marshal(map[string]any{
				"spec": map[string]any{
					"storage": map[string]any{
						"type":     "GCS",
						"location": storageLocation,
					},
				},
			})
			if err != nil {
				return err
			}
			if _, err := c.dynamic.Resource(podMigrationGVR).Namespace(namespace).
				Patch(ctx, DefaultPodMigrationName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}

	policyName := policyNameFor(name)
	_, err = c.getResource(ctx, podSnapshotPolicyGVR, c.policyLister, namespace, policyName)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	// Only retire a legacy per-Workspace PodSnapshotPolicy when no unconsumed or
	// draining PodSnapshots depend on it to resolve their GCS storage location.
	if workspace.GetAnnotations()[AnnotationLastCheckpointName] != "" {
		return nil
	}
	draining, err := c.drainingWorkspaceSnapshots(ctx, namespace, name)
	if err != nil || len(draining) > 0 {
		return err
	}
	if err := c.dynamic.Resource(podSnapshotPolicyGVR).Namespace(namespace).Delete(ctx, policyName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// setPodActiveCondition publishes the podsnapshot.gke.kubeflow.org/active readiness
// gate. The desired state is compared against the cached Pod first so the steady
// state issues no writes at all.
func (c *Controller) setPodActiveCondition(ctx context.Context, cached *corev1.Pod, status corev1.ConditionStatus, reason, message string) error {
	if current, found := podConditionStatus(cached, ReadinessGateConditionType); found && current == status {
		if status == corev1.ConditionFalse || isPodReady(cached) || !allContainersReady(cached) {
			return nil
		}
	}
	namespace, podName := cached.Namespace, cached.Name
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		pod, err := c.core.Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if !applyPodConditions(pod, status, reason, message) {
			return nil
		}
		_, err = c.core.Pods(namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
		return err
	})
}

// applyPodConditions mutates the readiness gate and the derived PodReady condition.
// PodReady is updated in the same write so the upstream WorkspaceReconciler observes
// the transition immediately instead of waiting for the next kubelet status sync.
func applyPodConditions(pod *corev1.Pod, status corev1.ConditionStatus, reason, message string) bool {
	now := metav1.Now()
	changed := false
	found := false
	for index := range pod.Status.Conditions {
		if pod.Status.Conditions[index].Type != ReadinessGateConditionType {
			continue
		}
		found = true
		if pod.Status.Conditions[index].Status != status {
			pod.Status.Conditions[index].Status = status
			pod.Status.Conditions[index].LastTransitionTime = now
			pod.Status.Conditions[index].Reason = reason
			pod.Status.Conditions[index].Message = message
			changed = true
		}
		break
	}
	if !found {
		pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
			Type:               ReadinessGateConditionType,
			Status:             status,
			LastTransitionTime: now,
			Reason:             reason,
			Message:            message,
		})
		changed = true
	}
	for index := range pod.Status.Conditions {
		if pod.Status.Conditions[index].Type != corev1.PodReady {
			continue
		}
		switch {
		case status == corev1.ConditionFalse && pod.Status.Conditions[index].Status != corev1.ConditionFalse:
			pod.Status.Conditions[index].Status = corev1.ConditionFalse
			pod.Status.Conditions[index].LastTransitionTime = now
			pod.Status.Conditions[index].Reason = reason
			pod.Status.Conditions[index].Message = message
			changed = true
		case status == corev1.ConditionTrue && pod.Status.Conditions[index].Status != corev1.ConditionTrue && allContainersReady(pod):
			pod.Status.Conditions[index].Status = corev1.ConditionTrue
			pod.Status.Conditions[index].LastTransitionTime = now
			pod.Status.Conditions[index].Reason = "ContainersReady"
			pod.Status.Conditions[index].Message = ""
			changed = true
		}
		break
	}
	return changed
}

func (c *Controller) patchWorkspacePauseAndAnnotations(ctx context.Context, namespace, name string, paused bool, annotations map[string]any) error {
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": annotations},
		"spec":     map[string]any{"paused": paused},
	})
	if err != nil {
		return err
	}
	_, err = c.dynamic.Resource(workspaceGVR).Namespace(namespace).
		Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
