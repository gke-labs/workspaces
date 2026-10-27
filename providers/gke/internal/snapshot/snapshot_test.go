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
	"net/http"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const testNamespace = "kubeflow-user"

func newTestController(t *testing.T, objects ...runtime.Object) (*Controller, *k8sfake.Clientset, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	gvrToListKind := map[schema.GroupVersionResource]string{
		workspaceGVR:                "WorkspaceList",
		workspaceKindGVR:            "WorkspaceKindList",
		podSnapshotStorageConfigGVR: "PodSnapshotStorageConfigList",
		podSnapshotPolicyGVR:        "PodSnapshotPolicyList",
		podSnapshotManualTriggerGVR: "PodSnapshotManualTriggerList",
		podSnapshotGVR:              "PodSnapshotList",
		podMigrationGVR:             "PodMigrationList",
		podMigrationJobGVR:          "PodMigrationJobList",
	}
	var coreObjects []runtime.Object
	var dynamicObjects []runtime.Object
	for _, object := range objects {
		if _, ok := object.(*unstructured.Unstructured); ok {
			dynamicObjects = append(dynamicObjects, object)
		} else {
			coreObjects = append(coreObjects, object)
		}
	}
	coreClient := k8sfake.NewSimpleClientset(coreObjects...)
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind, dynamicObjects...)
	controller := &Controller{
		core:       coreClient.CoreV1(),
		dynamic:    dynamicClient,
		namespaces: map[string]bool{testNamespace: true},
		bucket:     "test-bucket",
		settle:     SocketSettleGracePeriod,
		resync:     defaultResyncPeriod,
		workers:    1,
		podIndexer: cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{podWorkspaceIndex: podWorkspaceIndexFunc}),
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[types.NamespacedName]()),
	}
	return controller, coreClient, dynamicClient
}

func workspace(name string, annotations map[string]any, paused bool) *unstructured.Unstructured {
	if annotations == nil {
		annotations = map[string]any{}
	}
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubeflow.org/v1beta1",
			"kind":       "Workspace",
			"metadata": map[string]any{
				"name":        name,
				"namespace":   testNamespace,
				"uid":         "uid-" + name,
				"annotations": annotations,
			},
			"spec": map[string]any{"paused": paused, "kind": "jupyterlab"},
		},
	}
}

func runningPod(name, workspaceName string, gate, ready corev1.ConditionStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Labels:    map[string]string{WorkspaceLabel: workspaceName},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: ReadinessGateConditionType, Status: gate},
				{Type: corev1.PodReady, Status: ready},
			},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "main", Ready: true}},
		},
	}
}

func TestMutateWorkspaceInterceptsPauseAndFlipsReadinessGate(t *testing.T) {
	pod := runningPod("ws-ws-test-x2wbv-0", "ws-test", corev1.ConditionTrue, corev1.ConditionTrue)
	controller, coreClient, _ := newTestController(t, pod)

	enabled := map[string]any{AnnotationEnabled: "true"}
	oldRaw, _ := json.Marshal(workspace("ws-test", enabled, false).Object)
	newRaw, _ := json.Marshal(workspace("ws-test", enabled, true).Object)

	response := controller.mutateWorkspace(context.Background(), &admissionv1.AdmissionRequest{
		Operation: admissionv1.Update,
		OldObject: runtime.RawExtension{Raw: oldRaw},
		Object:    runtime.RawExtension{Raw: newRaw},
	})
	if !response.Allowed || len(response.Patch) == 0 {
		t.Fatalf("expected a patch intercepting the pause, got allowed=%v patch=%s", response.Allowed, response.Patch)
	}

	var operations []jsonPatchOp
	if err := json.Unmarshal(response.Patch, &operations); err != nil {
		t.Fatalf("failed to decode patch: %v", err)
	}
	heldPaused, markedCheckpointing := false, false
	for _, operation := range operations {
		if operation.Path == "/spec/paused" && operation.Value == false {
			heldPaused = true
		}
		if operation.Path == "/metadata/annotations" {
			annotations, _ := operation.Value.(map[string]any)
			if annotations[AnnotationCheckpointState] == CheckpointStateCheckpointing && annotations[AnnotationCheckpointStartedAt] != "" {
				markedCheckpointing = true
			}
		}
	}
	if !heldPaused || !markedCheckpointing {
		t.Fatalf("expected the patch to hold spec.paused=false and mark Checkpointing, got %+v", operations)
	}

	updated, err := coreClient.CoreV1().Pods(testNamespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get pod: %v", err)
	}
	if isPodReady(updated) {
		t.Fatal("expected PodReady to be flipped to False while checkpointing")
	}
}

// A Workspace update that does not toggle spec.paused must not cost any API calls:
// this is what keeps the webhook cheap under the constant status updates the
// upstream controller performs.
func TestMutateWorkspaceIgnoresNonTransitionUpdates(t *testing.T) {
	controller, coreClient, dynamicClient := newTestController(t)
	enabled := map[string]any{AnnotationEnabled: "true"}
	oldRaw, _ := json.Marshal(workspace("ws-test", enabled, false).Object)
	updated := workspace("ws-test", enabled, false)
	_ = unstructured.SetNestedField(updated.Object, "Running", "status", "state")
	newRaw, _ := json.Marshal(updated.Object)

	response := controller.mutateWorkspace(context.Background(), &admissionv1.AdmissionRequest{
		Operation: admissionv1.Update,
		OldObject: runtime.RawExtension{Raw: oldRaw},
		Object:    runtime.RawExtension{Raw: newRaw},
	})
	if !response.Allowed || len(response.Patch) != 0 {
		t.Fatalf("expected an unmodified allow, got patch=%s", response.Patch)
	}
	if calls := len(coreClient.Actions()) + len(dynamicClient.Actions()); calls != 0 {
		t.Fatalf("expected no API calls for a non-transition update, got %d", calls)
	}
}

func TestMutatePodInjectsGVisorIPCAndRestoreAnnotation(t *testing.T) {
	controller, _, _ := newTestController(t, workspace("ws-test", map[string]any{
		AnnotationEnabled:            "true",
		AnnotationLastCheckpointName: "snap-12345",
	}, false))

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ws-test-0",
			Namespace: testNamespace,
			Labels:    map[string]string{WorkspaceLabel: "ws-test"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "jupyter:latest"}}},
	}
	raw, _ := json.Marshal(pod)
	response := controller.mutatePod(context.Background(), &admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Namespace: testNamespace,
		Object:    runtime.RawExtension{Raw: raw},
	})
	if !response.Allowed || len(response.Patch) == 0 {
		t.Fatalf("expected a pod mutation patch, got allowed=%v patch=%s", response.Allowed, response.Patch)
	}

	var operations []jsonPatchOp
	if err := json.Unmarshal(response.Patch, &operations); err != nil {
		t.Fatalf("failed to decode patch: %v", err)
	}
	paths := map[string]bool{}
	for _, operation := range operations {
		paths[operation.Path] = true
	}
	for _, expected := range []string{"/spec/runtimeClassName", "/spec/readinessGates", "/spec/volumes", "/spec/containers/0/volumeMounts", "/metadata/annotations"} {
		if !paths[expected] {
			t.Errorf("expected patch path %s, got %v", expected, paths)
		}
	}
}

func TestMutatePodSkipsTPUWorkspaces(t *testing.T) {
	controller, _, dynamicClient := newTestController(t, workspace("ws-tpu", map[string]any{AnnotationEnabled: "true"}, false))
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ws-tpu-0",
			Namespace: testNamespace,
			Labels:    map[string]string{WorkspaceLabel: "ws-tpu"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "main",
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{"google.com/tpu": resource.MustParse("4")},
			},
		}}},
	}
	raw, _ := json.Marshal(pod)
	response := controller.mutatePod(context.Background(), &admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Namespace: testNamespace,
		Object:    runtime.RawExtension{Raw: raw},
	})
	if len(response.Patch) != 0 {
		t.Fatalf("TPU pods must not be mutated, got %s", response.Patch)
	}
	if len(dynamicClient.Actions()) != 0 {
		t.Fatalf("TPU pods must be rejected before any API lookup, got %v", dynamicClient.Actions())
	}
}

// workspaceKindWithPodConfigs builds a WorkspaceKind whose podConfig values carry the
// given resource limits, so enablement can be exercised against declared hardware.
func workspaceKindWithPodConfigs(name string, annotations map[string]any, podConfigs map[string]map[string]any) *unstructured.Unstructured {
	values := []any{}
	for id, limits := range podConfigs {
		values = append(values, map[string]any{
			"id":   id,
			"spec": map[string]any{"resources": map[string]any{"limits": limits}},
		})
	}
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kubeflow.org/v1beta1",
			"kind":       "WorkspaceKind",
			"metadata":   map[string]any{"name": name, "annotations": annotations},
			"spec": map[string]any{
				"podTemplate": map[string]any{
					"options": map[string]any{
						"podConfig": map[string]any{"values": values},
					},
				},
			},
		},
	}
}

// workspaceWithPodConfig is workspace() plus a selected podConfig id.
func workspaceWithPodConfig(name string, annotations map[string]any, podConfigID string) *unstructured.Unstructured {
	ws := workspace(name, annotations, false)
	_ = unstructured.SetNestedField(ws.Object, podConfigID, "spec", "podTemplate", "options", "podConfig")
	return ws
}

// Snapshot enablement must key off the hardware a podConfig declares, not off its
// name. The previous implementation matched strings.Contains(id, "tpu"), so renaming
// the podConfig silently re-enabled checkpointing on hardware that cannot do it.
func TestSnapshotEnabledUsesPodConfigResourcesNotNames(t *testing.T) {
	kind := workspaceKindWithPodConfigs("jupyterlab",
		map[string]any{AnnotationEnabled: "true"},
		map[string]map[string]any{
			// Deliberately named without the substring "tpu".
			"v5litepod":   {ResourceTPU: "4"},
			"small_cpu":   {"cpu": "2", "memory": "4Gi"},
			"tpu_in_name": {"cpu": "2", "memory": "4Gi"},
		})

	cases := []struct {
		name        string
		podConfig   string
		annotations map[string]any
		want        bool
	}{
		{"tpu hardware under a non-tpu name is excluded", "v5litepod", nil, false},
		{"cpu podConfig is enabled by the kind", "small_cpu", nil, true},
		{"a name containing tpu but no TPU resource is still enabled", "tpu_in_name", nil, true},
		{"hardware beats an explicit opt-in", "v5litepod", map[string]any{AnnotationEnabled: "true"}, false},
		{"explicit opt-out still wins", "small_cpu", map[string]any{AnnotationEnabled: "false"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			controller, _, _ := newTestController(t, kind, workspaceWithPodConfig("ws", tc.annotations, tc.podConfig))
			ws := workspaceWithPodConfig("ws", tc.annotations, tc.podConfig)
			got, _ := controller.snapshotEnabled(context.Background(), ws)
			if got != tc.want {
				t.Fatalf("snapshotEnabled(podConfig=%q, annotations=%v) = %v, want %v",
					tc.podConfig, tc.annotations, got, tc.want)
			}
		})
	}
}

// While the socket settle grace period is running the reconciler must ask for a
// delayed retry instead of creating the trigger, and it must not busy-poll.
func TestReconcileWaitsForSocketSettleGracePeriod(t *testing.T) {
	startedAt := time.Now().UTC().Format(time.RFC3339)
	workspaceObject := workspace("ws-test", map[string]any{
		AnnotationEnabled:             "true",
		AnnotationCheckpointState:     CheckpointStateCheckpointing,
		AnnotationCheckpointStartedAt: startedAt,
	}, false)
	pod := runningPod("ws-test-0", "ws-test", corev1.ConditionFalse, corev1.ConditionFalse)
	controller, _, dynamicClient := newTestController(t, workspaceObject, pod)

	requeueAfter, err := controller.reconcileWorkspace(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "ws-test"})
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	if requeueAfter <= 0 || requeueAfter > SocketSettleGracePeriod {
		t.Fatalf("expected a requeue within the settle grace period, got %s", requeueAfter)
	}
	for _, action := range dynamicClient.Actions() {
		if action.GetVerb() == "create" && action.GetResource() == podSnapshotManualTriggerGVR {
			t.Fatal("a checkpoint was requested before the sockets settled")
		}
	}
}

// Once the grace period has elapsed the trigger is created exactly once.
func TestReconcileCreatesManualTriggerAfterGracePeriod(t *testing.T) {
	startedAt := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	workspaceObject := workspace("ws-test", map[string]any{
		AnnotationEnabled:             "true",
		AnnotationCheckpointState:     CheckpointStateCheckpointing,
		AnnotationCheckpointStartedAt: startedAt,
	}, false)
	pod := runningPod("ws-test-0", "ws-test", corev1.ConditionFalse, corev1.ConditionFalse)
	controller, _, dynamicClient := newTestController(t, workspaceObject, pod)

	if _, err := controller.reconcileWorkspace(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "ws-test"}); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	trigger, err := dynamicClient.Resource(podSnapshotManualTriggerGVR).Namespace(testNamespace).
		Get(context.Background(), triggerNameFor("ws-test"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected a PodSnapshotManualTrigger: %v", err)
	}
	if target, _, _ := unstructured.NestedString(trigger.Object, "spec", "targetPod"); target != pod.Name {
		t.Fatalf("trigger targets %q, want %q", target, pod.Name)
	}
	if owner := workspaceOwner(trigger); owner != "ws-test" {
		t.Fatalf("trigger must be owned by its Workspace for garbage collection, got %q", owner)
	}

	// A second reconcile must be idempotent.
	before := len(dynamicClient.Actions())
	if _, err := controller.reconcileWorkspace(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "ws-test"}); err != nil {
		t.Fatalf("second reconcile failed: %v", err)
	}
	for _, action := range dynamicClient.Actions()[before:] {
		if action.GetVerb() == "create" || action.GetVerb() == "delete" {
			t.Fatalf("reconcile is not idempotent, it issued a %s on %s", action.GetVerb(), action.GetResource())
		}
	}
}

// A Ready PodSnapshot completes the pause by letting spec.paused=true through.
func TestReconcileCompletesPauseWhenSnapshotIsReady(t *testing.T) {
	startedAt := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	workspaceObject := workspace("ws-test", map[string]any{
		AnnotationEnabled:             "true",
		AnnotationCheckpointState:     CheckpointStateCheckpointing,
		AnnotationCheckpointStartedAt: startedAt,
	}, false)
	pod := runningPod("ws-test-0", "ws-test", corev1.ConditionFalse, corev1.ConditionFalse)
	trigger := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": podsnapshotGroupVersion,
		"kind":       "PodSnapshotManualTrigger",
		"metadata": map[string]any{
			"name":              triggerNameFor("ws-test"),
			"namespace":         testNamespace,
			"annotations":       map[string]any{AnnotationCheckpointStartedAt: startedAt},
			"creationTimestamp": time.Now().UTC().Format(time.RFC3339),
		},
		"status": map[string]any{"snapshotCreated": map[string]any{"name": "snap-1"}},
	}}
	snapshot := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": podsnapshotGroupVersion,
		"kind":       "PodSnapshot",
		"metadata":   map[string]any{"name": "snap-1", "namespace": testNamespace},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": "True"},
		}},
	}}
	controller, _, dynamicClient := newTestController(t, workspaceObject, pod, trigger, snapshot)

	if _, err := controller.reconcileWorkspace(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "ws-test"}); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	patched := findPatch(dynamicClient.Actions(), workspaceGVR)
	if patched == nil {
		t.Fatal("expected the Workspace to be patched once the snapshot was Ready")
	}
	if paused, _, _ := unstructured.NestedBool(patched, "spec", "paused"); !paused {
		t.Fatalf("expected spec.paused=true, got %v", patched)
	}
	annotations, _, _ := unstructured.NestedMap(patched, "metadata", "annotations")
	if annotations[AnnotationLastCheckpointName] != "snap-1" || annotations[AnnotationCheckpointState] != CheckpointStateReady {
		t.Fatalf("unexpected annotations %v", annotations)
	}
	assertNoPodSnapshotMutations(t, dynamicClient.Actions())
}

// GKE installs a ValidatingAdmissionPolicy that rejects every create/update/patch of
// a PodSnapshot from any principal other than its own snapshot controller. An earlier
// revision tried to attach a Workspace ownerReference to the snapshot and wedged the
// pause in "Checkpointing" forever, retrying against a permanent 403. Nothing in the
// reconcile path may write to a PodSnapshot; deleting one is the only allowed verb.
func assertNoPodSnapshotMutations(t *testing.T, actions []clienttesting.Action) {
	t.Helper()
	for _, action := range actions {
		if action.GetResource() != podSnapshotGVR {
			continue
		}
		switch action.GetVerb() {
		case "get", "list", "watch", "delete", "delete-collection":
		default:
			t.Fatalf("reconcile issued a %q on a PodSnapshot; GKE's ValidatingAdmissionPolicy always rejects this", action.GetVerb())
		}
	}
}

// A deleted Workspace must take its snapshots with it. They carry no ownerReference
// (see above), so the Kubernetes garbage collector cannot do it for us.
//
// Ordering matters: GKE resolves a PodSnapshot's bucket through its policy, so the
// policy has to survive until every snapshot has finished finalizing. Retiring the
// policy first makes GKE drop the snapshot object and orphan its files in GCS.
func TestReconcileDeletesSnapshotsWhenWorkspaceIsGone(t *testing.T) {
	newSnapshot := func() *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": podsnapshotGroupVersion,
			"kind":       "PodSnapshot",
			"metadata": map[string]any{
				"name":      "snap-1",
				"namespace": testNamespace,
				"labels":    map[string]any{LabelSnapshotTriggeredBy: triggerNameFor("ws-test")},
			},
		}}
	}
	policy := func() *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": podsnapshotGroupVersion,
			"kind":       "PodSnapshotPolicy",
			"metadata":   map[string]any{"name": policyNameFor("ws-test"), "namespace": testNamespace},
		}}
	}
	key := types.NamespacedName{Namespace: testNamespace, Name: "ws-test"}

	t.Run("snapshot still finalizing keeps the policy", func(t *testing.T) {
		// The fake client honours the label selector but does not run finalizers, so
		// the snapshot stays listed — exactly like a terminating one on a real cluster.
		controller, _, dynamicClient := newTestController(t, newSnapshot(), policy())

		requeue, err := controller.reconcileWorkspace(context.Background(), key)
		if err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}
		if requeue != snapshotDrainInterval {
			t.Fatalf("requeue = %v, want %v so we come back once GKE has finalized", requeue, snapshotDrainInterval)
		}
		assertDeleted(t, dynamicClient.Actions(), podSnapshotGVR, true)
		assertDeleted(t, dynamicClient.Actions(), podSnapshotPolicyGVR, false)
	})

	t.Run("snapshots drained retires the policy", func(t *testing.T) {
		controller, _, dynamicClient := newTestController(t, policy())

		requeue, err := controller.reconcileWorkspace(context.Background(), key)
		if err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}
		if requeue != 0 {
			t.Fatalf("requeue = %v, want 0 once nothing is left to drain", requeue)
		}
		assertDeleted(t, dynamicClient.Actions(), podSnapshotPolicyGVR, true)
	})
	// Regression test. GKE rewrites a terminating PodSnapshot's status many times a
	// second, and every one of those writes is a watch event that re-enqueues the
	// Workspace. Deleting again on each pass turned that churn into a write loop: a
	// single teardown produced 200+ deletecollection calls in the audit log.
	t.Run("re-reconciling a terminating snapshot issues no further deletes", func(t *testing.T) {
		terminating := newSnapshot()
		deletedAt := metav1.NewTime(time.Now())
		terminating.SetDeletionTimestamp(&deletedAt)
		terminating.SetFinalizers([]string{"podsnapshot.gke.io/podsnapshot-finalizer"})
		controller, _, dynamicClient := newTestController(t, terminating, policy())

		for pass := range 5 {
			requeue, err := controller.reconcileWorkspace(context.Background(), key)
			if err != nil {
				t.Fatalf("reconcile pass %d failed: %v", pass, err)
			}
			if requeue != snapshotDrainInterval {
				t.Fatalf("pass %d requeue = %v, want %v", pass, requeue, snapshotDrainInterval)
			}
		}
		if got := countDeletes(dynamicClient.Actions(), podSnapshotGVR); got != 0 {
			t.Fatalf("issued %d deletes against already-terminating snapshots, want 0", got)
		}
		assertDeleted(t, dynamicClient.Actions(), podSnapshotPolicyGVR, false)
	})

	// Once the policy is retired and all snapshots are gone (or already terminating
	// after the drain timeout), the delete events we generated ourselves must not
	// start another round.
	t.Run("teardown is a no-op once the policy is retired", func(t *testing.T) {
		terminating := newSnapshot()
		deletedAt := metav1.NewTime(time.Now().Add(-2 * snapshotDrainTimeout))
		terminating.SetDeletionTimestamp(&deletedAt)
		terminating.SetFinalizers([]string{"podsnapshot.gke.io/podsnapshot-finalizer"})
		controller, _, dynamicClient := newTestController(t, terminating)

		requeue, err := controller.reconcileWorkspace(context.Background(), key)
		if err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}
		if requeue != 0 {
			t.Fatalf("requeue = %v, want 0 once the teardown has finished", requeue)
		}
		assertDeleted(t, dynamicClient.Actions(), podSnapshotGVR, false)
		assertDeleted(t, dynamicClient.Actions(), podSnapshotPolicyGVR, false)
	})

	t.Run("wedged finalizer eventually releases the policy", func(t *testing.T) {
		// GKE removes the GCS objects within seconds and only then drops its
		// finalizer, so a snapshot still terminating this long after its deletion is
		// wedged. Waiting forever would pin the policy forever.
		stuck := newSnapshot()
		deletedAt := metav1.NewTime(time.Now().Add(-2 * snapshotDrainTimeout))
		stuck.SetDeletionTimestamp(&deletedAt)
		stuck.SetFinalizers([]string{"podsnapshot.gke.io/podsnapshot-finalizer"})
		controller, _, dynamicClient := newTestController(t, stuck, policy())

		requeue, err := controller.reconcileWorkspace(context.Background(), key)
		if err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}
		if requeue != 0 {
			t.Fatalf("requeue = %v, want 0 once the drain deadline has passed", requeue)
		}
		assertDeleted(t, dynamicClient.Actions(), podSnapshotPolicyGVR, true)
	})
}

func assertDeleted(t *testing.T, actions []clienttesting.Action, gvr schema.GroupVersionResource, want bool) {
	t.Helper()
	if got := countDeletes(actions, gvr) > 0; got != want {
		t.Fatalf("deleted %s = %v, want %v", gvr.Resource, got, want)
	}
}

// countDeletes reports how many delete calls were made against a resource. Tests use
// the count, not just the presence, to pin down write amplification during teardown.
func countDeletes(actions []clienttesting.Action, gvr schema.GroupVersionResource) int {
	deletes := 0
	for _, action := range actions {
		if action.GetResource() != gvr {
			continue
		}
		if verb := action.GetVerb(); verb == "delete" || verb == "delete-collection" {
			deletes++
		}
	}
	return deletes
}

// The steady state (a healthy, running Workspace) must not write anything.
func TestReconcileActiveWorkspaceIsWriteFree(t *testing.T) {
	workspaceObject := workspace("ws-test", map[string]any{AnnotationEnabled: "true"}, false)
	pod := runningPod("ws-test-0", "ws-test", corev1.ConditionTrue, corev1.ConditionTrue)
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: IPCConfigMapName, Namespace: testNamespace},
		Data:       map[string]string{IPCConfigMapKey: jupyterIPCServerConfig},
	}
	storageConfig := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": podsnapshotGroupVersion,
		"kind":       "PodSnapshotStorageConfig",
		"metadata":   map[string]any{"name": DefaultStorageConfigName},
	}}
	policy := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": podsnapshotGroupVersion,
		"kind":       "PodSnapshotPolicy",
		"metadata":   map[string]any{"name": policyNameFor("ws-test"), "namespace": testNamespace},
	}}
	controller, coreClient, dynamicClient := newTestController(t, workspaceObject, pod, configMap, storageConfig, policy)

	if _, err := controller.reconcileWorkspace(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "ws-test"}); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	for _, action := range append(toActions(coreClient.Actions()), toActions(dynamicClient.Actions())...) {
		switch action.GetVerb() {
		case "get", "list", "watch":
		default:
			t.Fatalf("steady state reconcile issued a %s on %s", action.GetVerb(), action.GetResource())
		}
	}
}

// Workspaces outside the managed tenant namespaces never enter the queue.
func TestEnqueueIgnoresUnmanagedNamespaces(t *testing.T) {
	controller, _, _ := newTestController(t)
	controller.enqueueWorkspace("other-namespace", "ws-test")
	if controller.queue.Len() != 0 {
		t.Fatal("a Workspace outside the managed namespaces was enqueued")
	}
	controller.enqueueWorkspace(testNamespace, "ws-test")
	if controller.queue.Len() != 1 {
		t.Fatal("a managed Workspace was not enqueued")
	}
}

// Pod events must map back to their Workspace through the shared index.
func TestPodIndexResolvesWorkspacePod(t *testing.T) {
	controller, _, _ := newTestController(t)
	controller.cachesReady.Store(true)
	pod := runningPod("ws-test-0", "ws-test", corev1.ConditionTrue, corev1.ConditionTrue)
	if err := controller.podIndexer.Add(pod); err != nil {
		t.Fatal(err)
	}
	found := controller.findWorkspacePod(context.Background(), testNamespace, "ws-test")
	if found == nil || found.Name != pod.Name {
		t.Fatalf("expected the indexed Pod, got %v", found)
	}
	if controller.findWorkspacePod(context.Background(), testNamespace, "other") != nil {
		t.Fatal("the index returned a Pod for the wrong Workspace")
	}
}

func toActions(actions []clienttesting.Action) []clienttesting.Action { return actions }

func findPatch(actions []clienttesting.Action, gvr schema.GroupVersionResource) map[string]any {
	for _, action := range actions {
		patch, ok := action.(clienttesting.PatchAction)
		if !ok || patch.GetResource() != gvr {
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal(patch.GetPatch(), &decoded); err != nil {
			continue
		}
		return decoded
	}
	return nil
}

func TestMutatePodInjectsPodMigrationLabelWhenEnabled(t *testing.T) {
	t.Run("migration enabled on Workspace injects pod-migration.gke.io/enabled label", func(t *testing.T) {
		controller, _, _ := newTestController(t, workspace("ws-mig", map[string]any{
			AnnotationEnabled:             "true",
			AnnotationPodMigrationEnabled: "true",
		}, false))

		pod := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ws-mig-0",
				Namespace: testNamespace,
				Labels:    map[string]string{WorkspaceLabel: "ws-mig"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "jupyter:latest"}}},
		}
		raw, _ := json.Marshal(pod)
		response := controller.mutatePod(context.Background(), &admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Namespace: testNamespace,
			Object:    runtime.RawExtension{Raw: raw},
		})
		if !response.Allowed || len(response.Patch) == 0 {
			t.Fatalf("expected pod mutation patch, got allowed=%v patch=%s", response.Allowed, response.Patch)
		}

		var operations []jsonPatchOp
		if err := json.Unmarshal(response.Patch, &operations); err != nil {
			t.Fatalf("failed to decode patch: %v", err)
		}
		foundLabel := false
		for _, op := range operations {
			if op.Path == "/metadata/labels" {
				labels, _ := op.Value.(map[string]any)
				if labels[AnnotationPodMigrationEnabled] == "true" && labels[WorkspaceLabel] == "ws-mig" {
					foundLabel = true
				}
			}
		}
		if !foundLabel {
			t.Fatalf("expected /metadata/labels patch with %s=true, got %+v", AnnotationPodMigrationEnabled, operations)
		}
	})

	t.Run("default snapshot-only Workspace does not inject migration label", func(t *testing.T) {
		controller, _, _ := newTestController(t, workspace("ws-snap", map[string]any{
			AnnotationEnabled: "true",
		}, false))

		pod := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ws-snap-0",
				Namespace: testNamespace,
				Labels:    map[string]string{WorkspaceLabel: "ws-snap"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "jupyter:latest"}}},
		}
		raw, _ := json.Marshal(pod)
		response := controller.mutatePod(context.Background(), &admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Namespace: testNamespace,
			Object:    runtime.RawExtension{Raw: raw},
		})
		var operations []jsonPatchOp
		_ = json.Unmarshal(response.Patch, &operations)
		for _, op := range operations {
			if op.Path == "/metadata/labels" {
				t.Fatalf("unexpected /metadata/labels patch on default snapshot-only Workspace: %+v", op)
			}
		}
	})
	t.Run("defers migration label injection on Pod CREATE while legacy per-Workspace policy still exists", func(t *testing.T) {
		legacyPolicy := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": podsnapshotGroupVersion,
			"kind":       "PodSnapshotPolicy",
			"metadata":   map[string]any{"name": policyNameFor("ws-mig"), "namespace": testNamespace},
		}}
		controller, _, _ := newTestController(t, workspace("ws-mig", map[string]any{
			AnnotationEnabled:             "true",
			AnnotationPodMigrationEnabled: "true",
			AnnotationLastCheckpointName:  "snap-legacy",
		}, false), legacyPolicy)

		pod := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ws-mig-0",
				Namespace: testNamespace,
				Labels:    map[string]string{WorkspaceLabel: "ws-mig"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "jupyter:latest"}}},
		}
		raw, _ := json.Marshal(pod)
		response := controller.mutatePod(context.Background(), &admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Namespace: testNamespace,
			Object:    runtime.RawExtension{Raw: raw},
		})
		var operations []jsonPatchOp
		_ = json.Unmarshal(response.Patch, &operations)
		for _, op := range operations {
			if op.Path == "/metadata/labels" {
				t.Fatalf("must not inject %s on Pod CREATE while legacy per-Workspace policy still exists: %+v", AnnotationPodMigrationEnabled, op)
			}
		}
	})
}

func TestReconcileMigrationEnabledWorkspaceUsesSingletonPodMigration(t *testing.T) {
	key := types.NamespacedName{Namespace: testNamespace, Name: "ws-mig"}

	t.Run("creates kubeflow-pod-migration with strict CRD schema and patches running Pod label once Ready", func(t *testing.T) {
		ws := workspace("ws-mig", map[string]any{
			AnnotationEnabled:             "true",
			AnnotationPodMigrationEnabled: "true",
		}, false)
		pod := runningPod("ws-mig-0", "ws-mig", corev1.ConditionTrue, corev1.ConditionTrue)
		controller, coreClient, dynamicClient := newTestController(t, ws, pod)

		if _, err := controller.reconcileWorkspace(context.Background(), key); err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}

		pm, err := dynamicClient.Resource(podMigrationGVR).Namespace(testNamespace).
			Get(context.Background(), DefaultPodMigrationName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("expected singleton PodMigration %q to be created: %v", DefaultPodMigrationName, err)
		}
		spec, ok := pm.Object["spec"].(map[string]any)
		if !ok || len(spec) != 1 {
			t.Fatalf("expected PodMigration.spec to contain only 'storage', got %+v", pm.Object["spec"])
		}
		storage, ok := spec["storage"].(map[string]any)
		if !ok || len(storage) != 1 || storage["location"] != "gs://test-bucket/kubeflow-notebooks" {
			t.Fatalf("expected PodMigration.spec.storage to contain only location=\"gs://test-bucket/kubeflow-notebooks\", got %+v", spec["storage"])
		}

		if _, err := dynamicClient.Resource(podSnapshotPolicyGVR).Namespace(testNamespace).
			Get(context.Background(), policyNameFor("ws-mig"), metav1.GetOptions{}); err == nil {
			t.Fatal("per-Workspace PodSnapshotPolicy must not be created when PodMigration is enabled")
		}

		// Before PodMigration reaches Ready=True, the running Pod must not yet be labeled.
		initialPod, err := coreClient.CoreV1().Pods(testNamespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get Pod: %v", err)
		}
		if initialPod.Labels[AnnotationPodMigrationEnabled] != "" {
			t.Fatalf("expected running Pod label to wait until PodMigration is Ready, got labels=%v", initialPod.Labels)
		}

		// Simulate pod-migration-controller marking PodMigration Ready=True.
		pm.Object["status"] = map[string]any{
			"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
		}
		if _, err := dynamicClient.Resource(podMigrationGVR).Namespace(testNamespace).
			UpdateStatus(context.Background(), pm, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("failed to update PodMigration status: %v", err)
		}
		if _, err := controller.reconcileWorkspace(context.Background(), key); err != nil {
			t.Fatalf("second reconcile failed: %v", err)
		}

		updatedPod, err := coreClient.CoreV1().Pods(testNamespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get Pod: %v", err)
		}
		if updatedPod.Labels[AnnotationPodMigrationEnabled] != "true" {
			t.Fatalf("expected running Pod to be patched with %s=true once PodMigration is Ready, got labels=%v", AnnotationPodMigrationEnabled, updatedPod.Labels)
		}
	})

	t.Run("reuses existing singleton PodMigration in namespace, retires legacy policy, and labels running Pod", func(t *testing.T) {
		ws := workspace("ws-mig", map[string]any{
			AnnotationEnabled:             "true",
			AnnotationPodMigrationEnabled: "true",
		}, false)
		pod := runningPod("ws-mig-0", "ws-mig", corev1.ConditionTrue, corev1.ConditionTrue)
		existingPM := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": podmigrationGroupVersion,
			"kind":       "PodMigration",
			"metadata":   map[string]any{"name": "pm-team-shared", "namespace": testNamespace},
			"status": map[string]any{
				"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
			},
		}}
		legacyPolicy := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": podsnapshotGroupVersion,
			"kind":       "PodSnapshotPolicy",
			"metadata":   map[string]any{"name": policyNameFor("ws-mig"), "namespace": testNamespace},
		}}
		controller, coreClient, dynamicClient := newTestController(t, ws, pod, existingPM, legacyPolicy)

		if _, err := controller.reconcileWorkspace(context.Background(), key); err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}

		if _, err := dynamicClient.Resource(podMigrationGVR).Namespace(testNamespace).
			Get(context.Background(), DefaultPodMigrationName, metav1.GetOptions{}); err == nil {
			t.Fatal("must not create a second PodMigration when one already exists in the namespace")
		}
		assertDeleted(t, dynamicClient.Actions(), podSnapshotPolicyGVR, true)

		updatedPod, err := coreClient.CoreV1().Pods(testNamespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get Pod: %v", err)
		}
		if updatedPod.Labels[AnnotationPodMigrationEnabled] != "true" {
			t.Fatalf("expected running Pod to be patched with %s=true after retiring legacy policy, got %v", AnnotationPodMigrationEnabled, updatedPod.Labels)
		}
	})

	t.Run("holds legacy policy and Pod label while PodMigration is terminating or not Ready", func(t *testing.T) {
		ws := workspace("ws-mig", map[string]any{
			AnnotationEnabled:             "true",
			AnnotationPodMigrationEnabled: "true",
		}, false)
		pod := runningPod("ws-mig-0", "ws-mig", corev1.ConditionTrue, corev1.ConditionTrue)
		terminatingPM := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": podmigrationGroupVersion,
			"kind":       "PodMigration",
			"metadata": map[string]any{
				"name":              "pm-old",
				"namespace":         testNamespace,
				"deletionTimestamp": time.Now().UTC().Format(time.RFC3339),
				"finalizers":        []any{"podmigration.gke.io/storage-cleanup"},
			},
			"status": map[string]any{
				"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
			},
		}}
		legacyPolicy := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": podsnapshotGroupVersion,
			"kind":       "PodSnapshotPolicy",
			"metadata":   map[string]any{"name": policyNameFor("ws-mig"), "namespace": testNamespace},
		}}
		controller, coreClient, dynamicClient := newTestController(t, ws, pod, terminatingPM, legacyPolicy)

		if _, err := controller.reconcileWorkspace(context.Background(), key); err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}
		assertDeleted(t, dynamicClient.Actions(), podSnapshotPolicyGVR, false)
		updatedPod, err := coreClient.CoreV1().Pods(testNamespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get Pod: %v", err)
		}
		if updatedPod.Labels[AnnotationPodMigrationEnabled] != "" {
			t.Fatalf("must not label running Pod while PodMigration is terminating, got %v", updatedPod.Labels)
		}
	})

	t.Run("returns NotFound error when custom storage-config does not exist", func(t *testing.T) {
		ws := workspace("ws-mig", map[string]any{
			AnnotationPodMigrationEnabled: "true",
			AnnotationStorageConfig:       "missing-custom-config",
		}, false)
		pod := runningPod("ws-mig-0", "ws-mig", corev1.ConditionTrue, corev1.ConditionTrue)
		controller, _, dynamicClient := newTestController(t, ws, pod)

		if _, err := controller.reconcileWorkspace(context.Background(), key); !apierrors.IsNotFound(err) {
			t.Fatalf("expected NotFound error when custom storage-config is missing, got %v", err)
		}
		if _, err := dynamicClient.Resource(podMigrationGVR).Namespace(testNamespace).
			Get(context.Background(), DefaultPodMigrationName, metav1.GetOptions{}); err == nil {
			t.Fatal("must not create PodMigration with fallback bucket when custom storage-config is missing")
		}
	})

	t.Run("defers running Pod label patch while unconsumed checkpoint keeps legacy policy alive", func(t *testing.T) {
		ws := workspace("ws-mig", map[string]any{
			AnnotationEnabled:             "true",
			AnnotationPodMigrationEnabled: "true",
			AnnotationLastCheckpointName:  "snap-unconsumed",
		}, false)
		pod := runningPod("ws-mig-0", "ws-mig", corev1.ConditionTrue, corev1.ConditionTrue)
		legacyPolicy := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": podsnapshotGroupVersion,
			"kind":       "PodSnapshotPolicy",
			"metadata":   map[string]any{"name": policyNameFor("ws-mig"), "namespace": testNamespace},
		}}
		controller, coreClient, dynamicClient := newTestController(t, ws, pod, legacyPolicy)

		if _, err := controller.reconcileWorkspace(context.Background(), key); err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}

		assertDeleted(t, dynamicClient.Actions(), podSnapshotPolicyGVR, false)
		updatedPod, err := coreClient.CoreV1().Pods(testNamespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get Pod: %v", err)
		}
		if updatedPod.Labels[AnnotationPodMigrationEnabled] != "" {
			t.Fatalf("must not label running Pod with %s=true while per-Workspace policy is still retained, got %v", AnnotationPodMigrationEnabled, updatedPod.Labels)
		}
	})

	t.Run("removes migration label from running Pod when migration is disabled", func(t *testing.T) {
		ws := workspace("ws-mig", map[string]any{
			AnnotationEnabled:             "true",
			AnnotationPodMigrationEnabled: "false",
		}, false)
		pod := runningPod("ws-mig-0", "ws-mig", corev1.ConditionTrue, corev1.ConditionTrue)
		pod.Labels[AnnotationPodMigrationEnabled] = "true"
		controller, coreClient, dynamicClient := newTestController(t, ws, pod)

		if _, err := controller.reconcileWorkspace(context.Background(), key); err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}

		updatedPod, err := coreClient.CoreV1().Pods(testNamespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get Pod: %v", err)
		}
		if _, exists := updatedPod.Labels[AnnotationPodMigrationEnabled]; exists {
			t.Fatalf("expected %s label to be removed when migration is disabled, got %v", AnnotationPodMigrationEnabled, updatedPod.Labels)
		}
		if _, err := dynamicClient.Resource(podSnapshotPolicyGVR).Namespace(testNamespace).
			Get(context.Background(), policyNameFor("ws-mig"), metav1.GetOptions{}); err != nil {
			t.Fatalf("expected per-Workspace PodSnapshotPolicy to be created when migration is disabled: %v", err)
		}
	})

	t.Run("removes migration label from running Pod when full opt-out disables both snapshots and migration", func(t *testing.T) {
		ws := workspace("ws-mig", map[string]any{
			AnnotationPodMigrationEnabled: "false",
		}, false)
		pod := runningPod("ws-mig-0", "ws-mig", corev1.ConditionTrue, corev1.ConditionTrue)
		pod.Labels[AnnotationPodMigrationEnabled] = "true"
		controller, coreClient, _ := newTestController(t, ws, pod)

		if _, err := controller.reconcileWorkspace(context.Background(), key); err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}

		updatedPod, err := coreClient.CoreV1().Pods(testNamespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get Pod: %v", err)
		}
		if _, exists := updatedPod.Labels[AnnotationPodMigrationEnabled]; exists {
			t.Fatalf("expected %s label to be removed on full opt-out, got %v", AnnotationPodMigrationEnabled, updatedPod.Labels)
		}
	})

	t.Run("deletes snapshots and trigger when paused or mid-checkpoint migration-enabled Workspace is deleted even if PodMigration was already removed", func(t *testing.T) {
		snap := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": podsnapshotGroupVersion,
			"kind":       "PodSnapshot",
			"metadata": map[string]any{
				"name":      "snap-mig-1",
				"namespace": testNamespace,
				"labels":    map[string]any{LabelSnapshotTriggeredBy: triggerNameFor("ws-mig")},
			},
		}}
		trig := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": podsnapshotGroupVersion,
			"kind":       "PodSnapshotManualTrigger",
			"metadata": map[string]any{
				"name":      triggerNameFor("ws-mig"),
				"namespace": testNamespace,
			},
		}}
		controller, _, dynamicClient := newTestController(t, snap, trig)

		requeue, err := controller.reconcileWorkspace(context.Background(), key)
		if err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}
		if requeue != 0 {
			t.Fatalf("expected requeue=0, got %v", requeue)
		}
		assertDeleted(t, dynamicClient.Actions(), podSnapshotGVR, true)
		assertDeleted(t, dynamicClient.Actions(), podSnapshotManualTriggerGVR, true)
		assertDeleted(t, dynamicClient.Actions(), podMigrationGVR, false)
	})

	t.Run("honors custom PodSnapshotStorageConfig and updates kubeflow-pod-migration when unambiguous", func(t *testing.T) {
		kind := workspaceKindWithPodConfigs("jupyterlab", map[string]any{
			AnnotationPodMigrationEnabled: "true",
			AnnotationStorageConfig:       "custom-gcs-config",
		}, map[string]map[string]any{
			"small_cpu": {"cpu": "2", "memory": "4Gi"},
		})
		customStorage := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": podsnapshotGroupVersion,
			"kind":       "PodSnapshotStorageConfig",
			"metadata":   map[string]any{"name": "custom-gcs-config"},
			"spec": map[string]any{
				"snapshotStorageConfig": map[string]any{
					"gcs": map[string]any{
						"bucket": "custom-team-bucket",
						"path":   "team-a/checkpoints",
					},
				},
			},
		}}
		existingPM := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": podmigrationGroupVersion,
			"kind":       "PodMigration",
			"metadata":   map[string]any{"name": DefaultPodMigrationName, "namespace": testNamespace},
			"spec": map[string]any{
				"storage": map[string]any{"location": "gs://old-bucket/kubeflow-notebooks"},
			},
		}}
		ws := workspaceWithPodConfig("ws-mig", nil, "small_cpu")
		pod := runningPod("ws-mig-0", "ws-mig", corev1.ConditionTrue, corev1.ConditionTrue)
		controller, _, dynamicClient := newTestController(t, kind, customStorage, existingPM, ws, pod)

		if _, err := controller.reconcileWorkspace(context.Background(), key); err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}

		pm, err := dynamicClient.Resource(podMigrationGVR).Namespace(testNamespace).
			Get(context.Background(), DefaultPodMigrationName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("expected singleton PodMigration %q: %v", DefaultPodMigrationName, err)
		}
		location, _, _ := unstructured.NestedString(pm.Object, "spec", "storage", "location")
		if location != "gs://custom-team-bucket/team-a/checkpoints" {
			t.Fatalf("storage location = %q, want %q", location, "gs://custom-team-bucket/team-a/checkpoints")
		}
	})

	t.Run("does not flip-flop kubeflow-pod-migration storage when two Workspaces in namespace disagree", func(t *testing.T) {
		storageA := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": podsnapshotGroupVersion,
			"kind":       "PodSnapshotStorageConfig",
			"metadata":   map[string]any{"name": "storage-a"},
			"spec": map[string]any{
				"snapshotStorageConfig": map[string]any{
					"gcs": map[string]any{"bucket": "bucket-a", "path": "path-a"},
				},
			},
		}}
		storageB := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": podsnapshotGroupVersion,
			"kind":       "PodSnapshotStorageConfig",
			"metadata":   map[string]any{"name": "storage-b"},
			"spec": map[string]any{
				"snapshotStorageConfig": map[string]any{
					"gcs": map[string]any{"bucket": "bucket-b", "path": "path-b"},
				},
			},
		}}
		wsA := workspace("ws-a", map[string]any{
			AnnotationPodMigrationEnabled: "true",
			AnnotationStorageConfig:       "storage-a",
		}, false)
		wsB := workspace("ws-b", map[string]any{
			AnnotationPodMigrationEnabled: "true",
			AnnotationStorageConfig:       "storage-b",
		}, false)
		existingPM := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": podmigrationGroupVersion,
			"kind":       "PodMigration",
			"metadata":   map[string]any{"name": DefaultPodMigrationName, "namespace": testNamespace},
			"spec": map[string]any{
				"storage": map[string]any{"location": "gs://bucket-a/path-a"},
			},
		}}
		controller, _, dynamicClient := newTestController(t, storageA, storageB, wsA, wsB, existingPM)

		if _, err := controller.reconcileWorkspace(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "ws-b"}); err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}

		pm, err := dynamicClient.Resource(podMigrationGVR).Namespace(testNamespace).
			Get(context.Background(), DefaultPodMigrationName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get PodMigration: %v", err)
		}
		location, _, _ := unstructured.NestedString(pm.Object, "spec", "storage", "location")
		if location != "gs://bucket-a/path-a" {
			t.Fatalf("expected conflicting Workspace not to overwrite location, got %q", location)
		}
	})

	t.Run("falls back to per-Workspace PodSnapshotPolicy when PodMigration CRD is not installed", func(t *testing.T) {
		ws := workspace("ws-mig", map[string]any{
			AnnotationPodMigrationEnabled: "true",
		}, false)
		pod := runningPod("ws-mig-0", "ws-mig", corev1.ConditionTrue, corev1.ConditionTrue)
		controller, _, dynamicClient := newTestController(t, ws, pod)
		dynamicClient.PrependReactor("list", "podmigrations", func(_ clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(podMigrationGVR.GroupResource(), "")
		})

		if _, err := controller.reconcileWorkspace(context.Background(), key); err != nil {
			t.Fatalf("expected graceful fallback when PodMigration CRD is missing, got error: %v", err)
		}

		if _, err := dynamicClient.Resource(podSnapshotPolicyGVR).Namespace(testNamespace).
			Get(context.Background(), policyNameFor("ws-mig"), metav1.GetOptions{}); err != nil {
			t.Fatalf("expected fallback per-Workspace PodSnapshotPolicy to be created: %v", err)
		}
	})

	t.Run("steady-state migration-enabled Workspace reconcile is completely write-free", func(t *testing.T) {
		ws := workspace("ws-mig", map[string]any{
			AnnotationEnabled:             "true",
			AnnotationPodMigrationEnabled: "true",
		}, false)
		pod := runningPod("ws-mig-0", "ws-mig", corev1.ConditionTrue, corev1.ConditionTrue)
		pod.Labels[AnnotationPodMigrationEnabled] = "true"
		configMap := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: IPCConfigMapName, Namespace: testNamespace},
			Data:       map[string]string{IPCConfigMapKey: jupyterIPCServerConfig},
		}
		existingPM := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": podmigrationGroupVersion,
			"kind":       "PodMigration",
			"metadata":   map[string]any{"name": DefaultPodMigrationName, "namespace": testNamespace},
			"spec": map[string]any{
				"storage": map[string]any{"location": "gs://test-bucket/kubeflow-notebooks"},
			},
			"status": map[string]any{
				"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
			},
		}}
		controller, coreClient, dynamicClient := newTestController(t, ws, pod, configMap, existingPM)
		if err := controller.podIndexer.Add(pod); err != nil {
			t.Fatal(err)
		}
		pmIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
		if err := pmIndexer.Add(existingPM); err != nil {
			t.Fatal(err)
		}
		controller.podMigrationLister = cache.NewGenericLister(pmIndexer, podMigrationGVR.GroupResource())
		controller.podMigrationReady.Store(true)

		if _, err := controller.reconcileWorkspace(context.Background(), key); err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}
		for _, action := range append(toActions(coreClient.Actions()), toActions(dynamicClient.Actions())...) {
			switch action.GetVerb() {
			case "get", "list", "watch":
				if action.GetResource() == podMigrationGVR {
					t.Fatalf("expected PodMigration read to be served from cache, got live %s", action.GetVerb())
				}
			default:
				t.Fatalf("steady state migration reconcile issued a %s on %s", action.GetVerb(), action.GetResource())
			}
		}
	})
}

func TestAnnotationOptOutPrecedenceBetweenSnapshotAndMigration(t *testing.T) {
	ctx := context.Background()

	t.Run("Workspace migration opt-out does not leave snapshot implicitly enabled when Kind only enabled migration", func(t *testing.T) {
		kind := workspaceKindWithPodConfigs("jupyterlab", map[string]any{
			AnnotationPodMigrationEnabled: "true",
		}, nil)
		ws := workspace("ws-optout", map[string]any{
			AnnotationPodMigrationEnabled: "false",
		}, false)
		controller, _, _ := newTestController(t, kind, ws)

		if controller.migrationEnabled(ctx, ws) {
			t.Fatal("expected migrationEnabled=false when Workspace sets pod-migration.gke.io/enabled=false")
		}
		if enabled, _ := controller.snapshotEnabled(ctx, ws); enabled {
			t.Fatal("expected snapshotEnabled=false when WorkspaceKind only enabled migration and Workspace opted out of migration")
		}
	})

	t.Run("Workspace migration opt-out falls back to snapshot-only when Kind explicitly enabled snapshots", func(t *testing.T) {
		kind := workspaceKindWithPodConfigs("jupyterlab", map[string]any{
			AnnotationEnabled:             "true",
			AnnotationPodMigrationEnabled: "true",
		}, nil)
		ws := workspace("ws-fallback", map[string]any{
			AnnotationPodMigrationEnabled: "false",
		}, false)
		controller, _, _ := newTestController(t, kind, ws)

		if controller.migrationEnabled(ctx, ws) {
			t.Fatal("expected migrationEnabled=false when Workspace sets pod-migration.gke.io/enabled=false")
		}
		if enabled, _ := controller.snapshotEnabled(ctx, ws); !enabled {
			t.Fatal("expected snapshotEnabled=true when WorkspaceKind explicitly enabled snapshots and Workspace only opted out of migration")
		}
	})

	t.Run("Workspace snapshot opt-out disables both snapshots and migration even when migration annotation is true", func(t *testing.T) {
		ws := workspace("ws-snap-off", map[string]any{
			AnnotationEnabled:             "false",
			AnnotationPodMigrationEnabled: "true",
		}, false)
		controller, _, _ := newTestController(t, ws)

		if enabled, _ := controller.snapshotEnabled(ctx, ws); enabled {
			t.Fatal("expected snapshotEnabled=false when snapshot.workspaces.gke.io/enabled=false")
		}
		if controller.migrationEnabled(ctx, ws) {
			t.Fatal("expected migrationEnabled=false when snapshot.workspaces.gke.io/enabled=false")
		}
	})
}

func TestMutualExclusionBetweenWorkspacePauseAndPodMigration(t *testing.T) {
	ctx := context.Background()
	key := types.NamespacedName{Namespace: testNamespace, Name: "ws-mig"}

	readyPM := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": podmigrationGroupVersion,
		"kind":       "PodMigration",
		"metadata":   map[string]any{"name": DefaultPodMigrationName, "namespace": testNamespace},
		"spec": map[string]any{
			"storage": map[string]any{"location": "gs://test-bucket/kubeflow-notebooks"},
		},
		"status": map[string]any{
			"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
		},
	}}

	t.Run("mutateWorkspace rejects Pause with 422 Invalid when an active PodMigrationJob exists", func(t *testing.T) {
		oldWS := workspace("ws-mig", map[string]any{
			AnnotationEnabled:             "true",
			AnnotationPodMigrationEnabled: "true",
		}, false)
		newWS := workspace("ws-mig", map[string]any{
			AnnotationEnabled:             "true",
			AnnotationPodMigrationEnabled: "true",
		}, true)
		pod := runningPod("ws-mig-0", "ws-mig", corev1.ConditionTrue, corev1.ConditionTrue)
		pod.Labels[AnnotationPodMigrationEnabled] = "true"
		activeJob := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": podmigrationGroupVersion,
			"kind":       "PodMigrationJob",
			"metadata":   map[string]any{"name": "pmj-ws-mig-snapshotting", "namespace": testNamespace},
			"spec": map[string]any{
				"podRef":       map[string]any{"name": "ws-mig-0"},
				"targetPodUID": "12345678-1111-2222-3333-444444444444",
			},
			"status": map[string]any{
				"phase": "Snapshotting",
			},
		}}
		controller, _, _ := newTestController(t, oldWS, pod, readyPM, activeJob)
		oldRaw, _ := json.Marshal(oldWS)
		newRaw, _ := json.Marshal(newWS)
		response := controller.mutateWorkspace(ctx, &admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			Namespace: testNamespace,
			OldObject: runtime.RawExtension{Raw: oldRaw},
			Object:    runtime.RawExtension{Raw: newRaw},
		})
		if response.Allowed {
			t.Fatal("expected mutateWorkspace to reject Pause while an active PodMigrationJob exists")
		}
		if response.Result == nil || response.Result.Reason != metav1.StatusReasonInvalid || response.Result.Code != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422 StatusReasonInvalid, got %+v", response.Result)
		}
	})

	t.Run("mutateWorkspace rejects Pause with 422 Invalid when Pod is fenced for migration", func(t *testing.T) {
		oldWS := workspace("ws-mig", map[string]any{
			AnnotationEnabled:             "true",
			AnnotationPodMigrationEnabled: "true",
		}, false)
		newWS := workspace("ws-mig", map[string]any{
			AnnotationEnabled:             "true",
			AnnotationPodMigrationEnabled: "true",
		}, true)
		pod := runningPod("ws-mig-0", "ws-mig", corev1.ConditionFalse, corev1.ConditionTrue)
		pod.Labels[AnnotationPodMigrationEnabled] = "true"
		pod.Labels[LabelPodMigrationFenced] = "true"

		controller, _, _ := newTestController(t, oldWS, pod, readyPM)
		oldRaw, _ := json.Marshal(oldWS)
		newRaw, _ := json.Marshal(newWS)
		response := controller.mutateWorkspace(ctx, &admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			Namespace: testNamespace,
			OldObject: runtime.RawExtension{Raw: oldRaw},
			Object:    runtime.RawExtension{Raw: newRaw},
		})
		if response.Allowed {
			t.Fatal("expected mutateWorkspace to reject Pause while Pod has pod-migration.gke.io/fenced=true")
		}
		if response.Result == nil || response.Result.Reason != metav1.StatusReasonInvalid || response.Result.Code != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422 StatusReasonInvalid, got %+v", response.Result)
		}
	})

	t.Run("mutateWorkspace allows Pause and holds for checkpointing when PodMigrationJob is already terminal", func(t *testing.T) {
		oldWS := workspace("ws-mig", map[string]any{
			AnnotationEnabled:             "true",
			AnnotationPodMigrationEnabled: "true",
		}, false)
		newWS := workspace("ws-mig", map[string]any{
			AnnotationEnabled:             "true",
			AnnotationPodMigrationEnabled: "true",
		}, true)
		pod := runningPod("ws-mig-0", "ws-mig", corev1.ConditionTrue, corev1.ConditionTrue)
		pod.Labels[AnnotationPodMigrationEnabled] = "true"
		succeededJob := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": podmigrationGroupVersion,
			"kind":       "PodMigrationJob",
			"metadata":   map[string]any{"name": "pmj-ws-mig-done", "namespace": testNamespace},
			"spec": map[string]any{
				"podRef":       map[string]any{"name": "ws-mig-0"},
				"targetPodUID": "12345678-1111-2222-3333-444444444444",
			},
			"status": map[string]any{
				"phase": "Succeeded",
			},
		}}
		controller, _, _ := newTestController(t, oldWS, pod, readyPM, succeededJob)
		oldRaw, _ := json.Marshal(oldWS)
		newRaw, _ := json.Marshal(newWS)
		response := controller.mutateWorkspace(ctx, &admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			Namespace: testNamespace,
			OldObject: runtime.RawExtension{Raw: oldRaw},
			Object:    runtime.RawExtension{Raw: newRaw},
		})
		if !response.Allowed || len(response.Patch) == 0 {
			t.Fatalf("expected mutateWorkspace to allow and hold spec.paused=false for checkpointing once PodMigrationJob is Succeeded, got allowed=%v patch=%s", response.Allowed, response.Patch)
		}
	})

	t.Run("reconcileCheckpointing waits for existing trigger snapshot Stage 2 GCS upload even after Pod stops running", func(t *testing.T) {
		ws := workspace("ws-mig", map[string]any{
			AnnotationEnabled:             "true",
			AnnotationPodMigrationEnabled: "true",
			AnnotationCheckpointState:     CheckpointStateCheckpointing,
			AnnotationCheckpointStartedAt: "2026-01-01T00:00:00Z",
		}, false)
		// Simulate postCheckpoint: stop having terminated the container so podRunning(pod) is false.
		stoppedPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ws-mig-0",
				Namespace: testNamespace,
				Labels:    map[string]string{WorkspaceLabel: "ws-mig", AnnotationPodMigrationEnabled: "true"},
			},
			Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
		}
		existingTrigger := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": podsnapshotGroupVersion,
			"kind":       "PodSnapshotManualTrigger",
			"metadata": map[string]any{
				"name":        triggerNameFor("ws-mig"),
				"namespace":   testNamespace,
				"annotations": map[string]any{AnnotationCheckpointStartedAt: "2026-01-01T00:00:00Z"},
			},
			"status": map[string]any{
				"snapshotCreated": map[string]any{"name": "ps-uploading"},
			},
		}}
		uploadingSnap := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": podsnapshotGroupVersion,
			"kind":       "PodSnapshot",
			"metadata": map[string]any{
				"name":      "ps-uploading",
				"namespace": testNamespace,
				"labels":    map[string]any{LabelSnapshotTriggeredBy: triggerNameFor("ws-mig")},
			},
			"status": map[string]any{
				"conditions": []any{map[string]any{"type": "Triggered", "status": "True"}},
			},
		}}
		controller, _, dynamicClient := newTestController(t, ws, stoppedPod, readyPM, existingTrigger, uploadingSnap)
		controller.settle = 0

		// Step 1: while Stage 2 GCS upload is still in progress (Ready!=True), Workspace must NOT cold-pause.
		if _, err := controller.reconcileWorkspace(ctx, key); err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}
		updatedWS, err := dynamicClient.Resource(workspaceGVR).Namespace(testNamespace).Get(ctx, "ws-mig", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get Workspace: %v", err)
		}
		if paused, _, _ := unstructured.NestedBool(updatedWS.Object, "spec", "paused"); paused {
			t.Fatal("must not cold-pause Workspace while trigger's PodSnapshot is still uploading to GCS")
		}

		// Step 2: once PodSnapshot reaches Ready=True, Workspace completes checkpointed pause with AnnotationLastCheckpointName.
		_ = unstructured.SetNestedSlice(uploadingSnap.Object, []any{
			map[string]any{"type": "Ready", "status": "True"},
		}, "status", "conditions")
		if _, err := dynamicClient.Resource(podSnapshotGVR).Namespace(testNamespace).Update(ctx, uploadingSnap, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("failed to update PodSnapshot: %v", err)
		}
		if _, err := controller.reconcileWorkspace(ctx, key); err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}
		updatedWS, err = dynamicClient.Resource(workspaceGVR).Namespace(testNamespace).Get(ctx, "ws-mig", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get Workspace: %v", err)
		}
		if paused, _, _ := unstructured.NestedBool(updatedWS.Object, "spec", "paused"); !paused {
			t.Fatal("expected Workspace to complete pause once PodSnapshot Ready=True")
		}
		if got := updatedWS.GetAnnotations()[AnnotationLastCheckpointName]; got != "ps-uploading" {
			t.Fatalf("expected AnnotationLastCheckpointName=%q, got %q", "ps-uploading", got)
		}
	})

	t.Run("reconcileActive retires consumed psmt-* PodSnapshot after migration completes and keeps it while PodMigrationJob is active", func(t *testing.T) {
		ws := workspace("ws-mig", map[string]any{
			AnnotationEnabled:             "true",
			AnnotationPodMigrationEnabled: "true",
		}, false)
		pod := runningPod("ws-ws-mig-abcde-0", "ws-mig", corev1.ConditionTrue, corev1.ConditionTrue)
		pod.Labels[AnnotationPodMigrationEnabled] = "true"
		migrationSnap := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": podsnapshotGroupVersion,
			"kind":       "PodSnapshot",
			"metadata": map[string]any{
				"name":      "ps-from-migration",
				"namespace": testNamespace,
				"labels":    map[string]any{LabelSnapshotTriggeredBy: "psmt-ws-ws-mig-abcde-0-12345678"},
			},
			"status": map[string]any{
				"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
			},
		}}
		activeJob := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": podmigrationGroupVersion,
			"kind":       "PodMigrationJob",
			"metadata":   map[string]any{"name": "pmj-ws-mig-1", "namespace": testNamespace},
			"spec": map[string]any{
				"podRef":       map[string]any{"name": "ws-ws-mig-abcde-0"},
				"targetPodUID": "12345678-1111-2222-3333-444444444444",
			},
			"status": map[string]any{
				"phase":       "Restoring",
				"snapshotRef": "ps-from-migration",
			},
		}}

		// While PodMigrationJob is still active, reconcileActive must not delete the psmt-* snapshot.
		controller, _, dynamicClient := newTestController(t, ws, pod, readyPM, migrationSnap, activeJob)
		if _, err := controller.reconcileWorkspace(ctx, key); err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}
		if _, err := dynamicClient.Resource(podSnapshotGVR).Namespace(testNamespace).Get(ctx, "ps-from-migration", metav1.GetOptions{}); err != nil {
			t.Fatalf("expected psmt-* PodSnapshot to be retained while PodMigrationJob is active, got err=%v", err)
		}

		// Once PodMigrationJob reaches Succeeded, reconcileActive deletes the consumed psmt-* snapshot.
		_ = unstructured.SetNestedField(activeJob.Object, "Succeeded", "status", "phase")
		if _, err := dynamicClient.Resource(podMigrationJobGVR).Namespace(testNamespace).Update(ctx, activeJob, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("failed to update PodMigrationJob: %v", err)
		}
		if _, err := controller.reconcileWorkspace(ctx, key); err != nil {
			t.Fatalf("reconcile failed: %v", err)
		}
		if _, err := dynamicClient.Resource(podSnapshotGVR).Namespace(testNamespace).Get(ctx, "ps-from-migration", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatalf("expected consumed psmt-* PodSnapshot to be deleted after migration Succeeded, got err=%v", err)
		}
	})

	t.Run("mutatePod injects pod-migration.gke.io/enabled on replacement Pod when PodMigration is terminating but Workspace has active PodMigrationJob", func(t *testing.T) {
		ws := workspace("ws-mig", map[string]any{
			AnnotationEnabled:             "true",
			AnnotationPodMigrationEnabled: "true",
		}, false)
		terminatingPM := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": podmigrationGroupVersion,
			"kind":       "PodMigration",
			"metadata": map[string]any{
				"name":              DefaultPodMigrationName,
				"namespace":         testNamespace,
				"deletionTimestamp": "2026-01-01T00:00:00Z",
			},
			"spec": map[string]any{
				"storage": map[string]any{"location": "gs://test-bucket/kubeflow-notebooks"},
			},
		}}
		activeJob := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": podmigrationGroupVersion,
			"kind":       "PodMigrationJob",
			"metadata":   map[string]any{"name": "pmj-ws-mig-restoring", "namespace": testNamespace},
			"spec": map[string]any{
				"podRef":       map[string]any{"name": "ws-mig-0"},
				"targetPodUID": "12345678-1111-2222-3333-444444444444",
			},
			"status": map[string]any{
				"phase": "Restoring",
			},
		}}
		newPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ws-mig-0",
				Namespace: testNamespace,
				Labels:    map[string]string{WorkspaceLabel: "ws-mig"},
			},
		}

		// With an active PodMigrationJob for ws-mig, the replacement Pod must receive pod-migration.gke.io/enabled=true
		// so pod-migration's replacement webhook can inject the PodSnapshotRestoreClaim.
		controller, _, _ := newTestController(t, ws, terminatingPM, activeJob)
		rawPod, _ := json.Marshal(newPod)
		resp := controller.mutatePod(ctx, &admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Namespace: testNamespace,
			Object:    runtime.RawExtension{Raw: rawPod},
		})
		if !resp.Allowed || !strings.Contains(string(resp.Patch), AnnotationPodMigrationEnabled) {
			t.Fatalf("expected mutatePod to inject pod-migration.gke.io/enabled when active PodMigrationJob exists during PodMigration deletion deferral, got patch=%s", resp.Patch)
		}

		// Without an active PodMigrationJob, a terminating PodMigration skips injecting pod-migration.gke.io/enabled.
		controllerNoJob, _, _ := newTestController(t, ws, terminatingPM)
		respNoJob := controllerNoJob.mutatePod(ctx, &admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Namespace: testNamespace,
			Object:    runtime.RawExtension{Raw: rawPod},
		})
		if !respNoJob.Allowed || strings.Contains(string(respNoJob.Patch), AnnotationPodMigrationEnabled) {
			t.Fatalf("expected mutatePod to skip pod-migration.gke.io/enabled when PodMigration is terminating and no active PodMigrationJob exists, got patch=%s", respNoJob.Patch)
		}
	})

	t.Run("F4: workspaceMatchesPodName short-circuits on live Pod and guards against 45-char StatefulSet prefix collisions", func(t *testing.T) {
		longPrefix := strings.Repeat("a", 45)
		ws1 := workspace(longPrefix+"-one", map[string]any{AnnotationEnabled: "true"}, false)
		ws2 := workspace(longPrefix+"-two", map[string]any{AnnotationEnabled: "true"}, false)
		pod1 := runningPod("ws-"+longPrefix[:42]+"-abcde-0", ws1.GetName(), corev1.ConditionTrue, corev1.ConditionTrue)
		pod2Name := "ws-" + longPrefix[:42] + "-zzzzz-0"

		controller, _, _ := newTestController(t, ws1, ws2, pod1)
		wsIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
		_ = wsIndexer.Add(ws1)
		_ = wsIndexer.Add(ws2)
		controller.workspaceLister = cache.NewGenericLister(wsIndexer, workspaceGVR.GroupResource())

		// Live Pod short-circuit: ws1 has pod1, so pod2Name must NOT match ws1 even though it shares the 45-char prefix.
		if controller.workspaceMatchesPodName(ctx, ws1, pod1, pod2Name) {
			t.Fatal("expected workspaceMatchesPodName to return false when live pod.Name differs from candidatePodName")
		}
		// Ambiguous prefix fallback: when pod is nil and status.podTemplatePod.name is unset,
		// neither ws1 nor ws2 exclusively matches pod2Name because both share the 45-char prefix.
		if controller.workspaceMatchesPodName(ctx, ws1, nil, pod2Name) {
			t.Fatal("expected workspaceMatchesPodName to return false when another Workspace in namespace shares the truncated prefix")
		}
	})
}

func TestEnqueueWorkspacesForKindOnAddUpdateDelete(t *testing.T) {
	ws := workspace("ws-kind-watch", nil, false)
	kind := workspaceKindWithPodConfigs("jupyterlab", map[string]any{
		AnnotationPodMigrationEnabled: "true",
	}, nil)
	controller, _, _ := newTestController(t, ws, kind)
	wsIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	if err := wsIndexer.Add(ws); err != nil {
		t.Fatal(err)
	}
	controller.workspaceLister = cache.NewGenericLister(wsIndexer, workspaceGVR.GroupResource())

	// AddFunc (old == nil) must enqueue matching Workspaces.
	controller.enqueueWorkspacesForKind(nil, kind)
	if controller.queue.Len() == 0 {
		t.Fatal("expected WorkspaceKind Add event to enqueue matching Workspace")
	}
	item, _ := controller.queue.Get()
	controller.queue.Done(item)
	controller.queue.Forget(item)

	// DeleteFunc (current == nil) must also enqueue matching Workspaces.
	controller.enqueueWorkspacesForKind(kind, nil)
	if controller.queue.Len() == 0 {
		t.Fatal("expected WorkspaceKind Delete event to enqueue matching Workspace")
	}
}

