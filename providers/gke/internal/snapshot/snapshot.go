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

// Package snapshot implements the zero-intrusion GKE PodSnapshot addon described in
// gke/pod_snapshot_webhook_design.md.
//
// The addon is a standalone deployment (gke-workspace-snapshot-addon) that contains:
//
//  1. A Workspace mutating admission webhook (UPDATE on kubeflow.org/v1beta1 workspaces)
//     that holds spec.paused=false while a checkpoint is taken.
//  2. A Pod mutating admission webhook (CREATE on v1/pods) that injects the gVisor
//     runtime class, the Jupyter IPC config and the GKE restore annotation.
//  3. A level-triggered reconciler that drives the checkpoint/restore state machine.
//
// The reconciler is event driven: every object it depends on is watched through a
// shared informer and reconciliation is funnelled through a rate limited work queue
// keyed by Workspace. Nothing is polled, and every read is served from an informer
// cache, so the steady-state cost is independent of the number of namespaces and
// Workspaces in the cluster.
package snapshot

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const (
	AnnotationEnabled             = "podsnapshot.gke.kubeflow.org/enabled"
	AnnotationStorageConfig       = "podsnapshot.gke.kubeflow.org/storage-config"
	AnnotationCheckpointState     = "podsnapshot.gke.kubeflow.org/checkpoint-state"
	AnnotationCheckpointStartedAt = "podsnapshot.gke.kubeflow.org/checkpoint-started-at"
	AnnotationLastCheckpointName  = "podsnapshot.gke.kubeflow.org/last-checkpoint-name"

	CheckpointStateCheckpointing = "Checkpointing"
	CheckpointStateReady         = "Ready"
	CheckpointStateRestoring     = "Restoring"

	GKERestoreAnnotation       = "podsnapshot.gke.io/ps-name"
	DefaultGKERuntimeClass     = "gvisor"
	ReadinessGateConditionType = corev1.PodConditionType("podsnapshot.gke.kubeflow.org/active")

	// ResourceTPU is the extended resource a Pod requests to get TPU chips. GKE
	// checkpoints through gVisor, which cannot host TPU devices, so asking for this
	// resource makes a workload un-snapshottable no matter what the annotations say.
	// Both the Pod admission path and the Workspace enablement check key off this,
	// so they cannot drift apart.
	ResourceTPU = "google.com/tpu"

	// LabelSnapshotTriggeredBy is set by GKE on every PodSnapshot, naming the
	// PodSnapshotManualTrigger that produced it. It is the only handle we have on a
	// Workspace's snapshots, because GKE forbids us from adding our own metadata.
	LabelSnapshotTriggeredBy = "gke-pod-snapshot-triggered-by"

	DefaultStorageConfigName = "kubeflow-pod-snapshot-storage-config"
	IPCConfigMapName         = "jupyter-ipc-config"
	IPCConfigMapKey          = "jupyter_server_config.py"
	IPCConfigMapMountPath    = "/etc/jupyter/jupyter_server_config.py"
	WorkspaceLabel           = "notebooks.kubeflow.org/workspace-name"

	// SocketSettleGracePeriod is the delay between flipping the readiness gate to
	// False (which drops the Pod out of the Service EndpointSlice) and asking GKE
	// for a checkpoint, so the kernel can finish closing drained client sockets.
	SocketSettleGracePeriod = 3 * time.Second

	// defaultResyncPeriod is a safety net only; correctness comes from watches.
	defaultResyncPeriod = 10 * time.Minute

	// snapshotDrainInterval is how often a deleted Workspace is re-checked while
	// GKE finalizes its PodSnapshots. This is the one place the reconciler polls,
	// because a terminating PodSnapshot produces no further watch events once GKE
	// has stamped its deletionTimestamp.
	snapshotDrainInterval = 10 * time.Second

	// snapshotDrainTimeout bounds that wait. GKE removes the GCS objects within
	// seconds of the delete and only then releases its finalizer, so a PodSnapshot
	// still held after this long means the finalizer is wedged rather than that data
	// is still at risk. Give up and retire the policy instead of pinning it forever.
	snapshotDrainTimeout  = 5 * time.Minute
	defaultWorkers        = 4
	defaultLeaseNamespace = "kubeflow-workspaces"
	defaultLeaseName      = "gke-workspace-snapshot-addon"

	podsnapshotGroupVersion = "podsnapshot.gke.io/v1"
	podWorkspaceIndex       = "workspace"
)

const jupyterIPCServerConfig = `import asyncio
import selectors
class PollEventLoopPolicy(asyncio.DefaultEventLoopPolicy):
    def _loop_factory(self):
        return asyncio.SelectorEventLoop(selectors.PollSelector())
asyncio.set_event_loop_policy(PollEventLoopPolicy())

c.KernelManager.transport = 'ipc'
`

var (
	workspaceGVR = schema.GroupVersionResource{
		Group:    "kubeflow.org",
		Version:  "v1beta1",
		Resource: "workspaces",
	}
	workspaceKindGVR = schema.GroupVersionResource{
		Group:    "kubeflow.org",
		Version:  "v1beta1",
		Resource: "workspacekinds",
	}
	podSnapshotStorageConfigGVR = schema.GroupVersionResource{
		Group:    "podsnapshot.gke.io",
		Version:  "v1",
		Resource: "podsnapshotstorageconfigs",
	}
	podSnapshotPolicyGVR = schema.GroupVersionResource{
		Group:    "podsnapshot.gke.io",
		Version:  "v1",
		Resource: "podsnapshotpolicies",
	}
	podSnapshotManualTriggerGVR = schema.GroupVersionResource{
		Group:    "podsnapshot.gke.io",
		Version:  "v1",
		Resource: "podsnapshotmanualtriggers",
	}
	podSnapshotGVR = schema.GroupVersionResource{
		Group:    "podsnapshot.gke.io",
		Version:  "v1",
		Resource: "podsnapshots",
	}
)

type jsonPatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// Options configures the addon.
type Options struct {
	// Namespaces restricts the addon to the listed tenant namespaces. An empty
	// list watches every namespace, which keeps a single watch per resource type
	// no matter how many tenants exist.
	Namespaces []string
	// SnapshotBucket is the GCS bucket backing the PodSnapshotStorageConfig.
	SnapshotBucket string
	// ResyncPeriod is the informer relist interval used as a safety net against
	// missed watch events. It is not the reconcile interval.
	ResyncPeriod time.Duration
	// SettleGracePeriod overrides SocketSettleGracePeriod.
	SettleGracePeriod time.Duration
	// Workers is the number of concurrent Workspace reconcile workers.
	Workers int
	// LeaderElection restricts reconciliation to a single replica. Admission
	// webhooks are always served by every replica.
	LeaderElection bool
	// LeaseNamespace and LeaseName identify the coordination Lease.
	LeaseNamespace string
	LeaseName      string
	// Identity uniquely names this replica in the Lease; defaults to the hostname.
	Identity string
}

func (o Options) withDefaults() Options {
	if o.ResyncPeriod <= 0 {
		o.ResyncPeriod = defaultResyncPeriod
	}
	if o.SettleGracePeriod <= 0 {
		o.SettleGracePeriod = SocketSettleGracePeriod
	}
	if o.Workers <= 0 {
		o.Workers = defaultWorkers
	}
	if o.LeaseNamespace == "" {
		o.LeaseNamespace = defaultLeaseNamespace
	}
	if o.LeaseName == "" {
		o.LeaseName = defaultLeaseName
	}
	if o.Identity == "" {
		hostname, err := os.Hostname()
		if err != nil || hostname == "" {
			hostname = fmt.Sprintf("snapshot-addon-%d", time.Now().UnixNano())
		}
		o.Identity = hostname
	}
	if o.SnapshotBucket == "" {
		if len(o.Namespaces) > 0 && o.Namespaces[0] != "" {
			o.SnapshotBucket = fmt.Sprintf("%s-snapshots-bucket", o.Namespaces[0])
		} else {
			o.SnapshotBucket = "kubeflow-user-snapshots-bucket"
		}
	}
	return o
}

// Controller serves the mutating admission webhooks and reconciles Workspaces
// against the GKE PodSnapshot API.
type Controller struct {
	kube      kubernetes.Interface
	core      coreclient.CoreV1Interface
	dynamic   dynamic.Interface
	discovery discovery.DiscoveryInterface

	namespaces map[string]bool
	bucket     string
	settle     time.Duration
	resync     time.Duration
	workers    int

	leaderElection bool
	leaseNamespace string
	leaseName      string
	identity       string

	queue workqueue.TypedRateLimitingInterface[types.NamespacedName]

	podIndexer          cache.Indexer
	podLister           corelisters.PodLister
	configMapLister     corelisters.ConfigMapLister
	workspaceLister     cache.GenericLister
	kindLister          cache.GenericLister
	policyLister        cache.GenericLister
	triggerLister       cache.GenericLister
	snapshotLister      cache.GenericLister
	storageConfigLister cache.GenericLister

	cachesReady   atomic.Bool
	snapshotReady atomic.Bool
}

// NewController builds the addon controller from a REST config.
func NewController(config *rest.Config, options Options) (*Controller, error) {
	options = options.withDefaults()
	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	managed := map[string]bool{}
	for _, namespace := range options.Namespaces {
		namespace = strings.TrimSpace(namespace)
		if namespace != "" {
			managed[namespace] = true
		}
	}
	return &Controller{
		kube:       kube,
		core:       kube.CoreV1(),
		dynamic:    dyn,
		discovery:  kube.Discovery(),
		namespaces: managed,
		bucket:     options.SnapshotBucket,
		settle:     options.SettleGracePeriod,
		resync:     options.ResyncPeriod,
		workers:    options.Workers,

		leaderElection: options.LeaderElection,
		leaseNamespace: options.LeaseNamespace,
		leaseName:      options.LeaseName,
		identity:       options.Identity,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[types.NamespacedName](),
			workqueue.TypedRateLimitingQueueConfig[types.NamespacedName]{Name: "workspace-snapshot"},
		),
	}, nil
}

// managedNamespace reports whether the addon is responsible for a namespace.
func (c *Controller) managedNamespace(namespace string) bool {
	if len(c.namespaces) == 0 {
		return namespace != ""
	}
	return c.namespaces[namespace]
}

// CachesSynced reports whether the informer caches backing the webhooks are warm.
func (c *Controller) CachesSynced() bool { return c.cachesReady.Load() }

// SnapshotAPIReady reports whether the podsnapshot.gke.io CRDs were discovered and
// their informers are running.
func (c *Controller) SnapshotAPIReady() bool { return c.snapshotReady.Load() }

func (c *Controller) enqueueWorkspace(namespace, name string) {
	if name == "" || !c.managedNamespace(namespace) {
		return
	}
	c.queue.Add(types.NamespacedName{Namespace: namespace, Name: name})
}

// asUnstructured converts an informer cache object into *unstructured.Unstructured.
func asUnstructured(object any) *unstructured.Unstructured {
	switch typed := object.(type) {
	case *unstructured.Unstructured:
		return typed
	case cache.DeletedFinalStateUnknown:
		return asUnstructured(typed.Obj)
	case runtime.Unstructured:
		return &unstructured.Unstructured{Object: typed.UnstructuredContent()}
	}
	return nil
}

func asPod(object any) *corev1.Pod {
	switch typed := object.(type) {
	case *corev1.Pod:
		return typed
	case cache.DeletedFinalStateUnknown:
		return asPod(typed.Obj)
	}
	return nil
}

// workspaceOwner returns the name of the Workspace that owns an object.
func workspaceOwner(object metav1.Object) string {
	for _, owner := range object.GetOwnerReferences() {
		if owner.Kind == "Workspace" && strings.HasPrefix(owner.APIVersion, "kubeflow.org/") {
			return owner.Name
		}
	}
	return ""
}

func allContainersReady(pod *corev1.Pod) bool {
	if len(pod.Status.ContainerStatuses) == 0 {
		return false
	}
	for _, status := range pod.Status.ContainerStatuses {
		if !status.Ready {
			return false
		}
	}
	return true
}

func isPodReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func podConditionStatus(pod *corev1.Pod, conditionType corev1.PodConditionType) (corev1.ConditionStatus, bool) {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == conditionType {
			return condition.Status, true
		}
	}
	return corev1.ConditionUnknown, false
}

func logf(component, format string, arguments ...any) {
	log.Printf("[snapshot-"+component+"] "+format, arguments...)
}

// waitForPodSnapshotAPI blocks until the podsnapshot.gke.io/v1 API group is served
// by the cluster, so the addon can start before the GKE PodSnapshot CRDs exist.
func (c *Controller) waitForPodSnapshotAPI(ctx context.Context) error {
	logged := false
	for {
		resources, err := c.discovery.ServerResourcesForGroupVersion(podsnapshotGroupVersion)
		if err == nil && resources != nil {
			found := map[string]bool{}
			for _, resource := range resources.APIResources {
				found[resource.Name] = true
			}
			if found[podSnapshotGVR.Resource] && found[podSnapshotManualTriggerGVR.Resource] &&
				found[podSnapshotPolicyGVR.Resource] && found[podSnapshotStorageConfigGVR.Resource] {
				return nil
			}
		}
		if !logged {
			logf("reconciler", "waiting for the %s API group (GKE PodSnapshot CRDs) to become available", podsnapshotGroupVersion)
			logged = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}
