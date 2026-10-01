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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

// Run starts the informers and, once the caches are warm, the reconcile workers.
// It blocks until ctx is cancelled.
//
// The webhook handlers only need the Workspace, WorkspaceKind, Pod and ConfigMap
// caches, so those are started first and the addon starts serving admission
// requests even if the GKE PodSnapshot CRDs are not installed yet.
func (c *Controller) Run(ctx context.Context) error {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	dynamicFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(c.dynamic, c.resync, metav1.NamespaceAll, nil)

	// Pods are watched cluster-wide but filtered to Workspace Pods by label, so the
	// cache holds one entry per Workspace Pod instead of every Pod in the cluster.
	podFactory := informers.NewSharedInformerFactoryWithOptions(c.kube, c.resync,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = WorkspaceLabel
		}))
	// Only the single jupyter-ipc-config ConfigMap per namespace is cached.
	configMapFactory := informers.NewSharedInformerFactoryWithOptions(c.kube, c.resync,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.FieldSelector = fields.OneTermEqualSelector("metadata.name", IPCConfigMapName).String()
		}))

	podInformer := podFactory.Core().V1().Pods().Informer()
	if err := podInformer.AddIndexers(cache.Indexers{podWorkspaceIndex: podWorkspaceIndexFunc}); err != nil {
		return err
	}
	c.podIndexer = podInformer.GetIndexer()
	c.podLister = podFactory.Core().V1().Pods().Lister()
	c.configMapLister = configMapFactory.Core().V1().ConfigMaps().Lister()

	workspaceInformer := dynamicFactory.ForResource(workspaceGVR)
	kindInformer := dynamicFactory.ForResource(workspaceKindGVR)
	c.workspaceLister = workspaceInformer.Lister()
	c.kindLister = kindInformer.Lister()

	if _, err := workspaceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(object any) { c.enqueueWorkspaceObject(object) },
		UpdateFunc: func(_, object any) { c.enqueueWorkspaceObject(object) },
		DeleteFunc: func(object any) { c.enqueueWorkspaceObject(object) },
	}); err != nil {
		return err
	}
	if _, err := kindInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(old, current any) { c.enqueueWorkspacesForKind(old, current) },
	}); err != nil {
		return err
	}
	if _, err := podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(object any) { c.enqueuePod(object) },
		UpdateFunc: func(_, object any) { c.enqueuePod(object) },
		DeleteFunc: func(object any) { c.enqueuePod(object) },
	}); err != nil {
		return err
	}
	if _, err := configMapFactory.Core().V1().ConfigMaps().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: func(object any) { c.enqueueNamespace(object) },
	}); err != nil {
		return err
	}

	podFactory.Start(ctx.Done())
	configMapFactory.Start(ctx.Done())
	dynamicFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(),
		podInformer.HasSynced,
		configMapFactory.Core().V1().ConfigMaps().Informer().HasSynced,
		workspaceInformer.Informer().HasSynced,
		kindInformer.Informer().HasSynced,
	) {
		return fmt.Errorf("failed to sync Workspace caches")
	}
	c.cachesReady.Store(true)
	logf("reconciler", "Workspace, WorkspaceKind, Pod and ConfigMap caches are warm; admission webhooks are serving from cache")

	if err := c.waitForPodSnapshotAPI(ctx); err != nil {
		return err
	}

	// The GKE PodSnapshot resources get their own factory so it can be created
	// after the CRDs are confirmed to exist.
	snapshotFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(c.dynamic, c.resync, metav1.NamespaceAll, nil)
	policyInformer := snapshotFactory.ForResource(podSnapshotPolicyGVR)
	triggerInformer := snapshotFactory.ForResource(podSnapshotManualTriggerGVR)
	snapshotInformer := snapshotFactory.ForResource(podSnapshotGVR)
	storageConfigInformer := snapshotFactory.ForResource(podSnapshotStorageConfigGVR)
	c.policyLister = policyInformer.Lister()
	c.triggerLister = triggerInformer.Lister()
	c.snapshotLister = snapshotInformer.Lister()
	c.storageConfigLister = storageConfigInformer.Lister()

	for _, informer := range []cache.SharedIndexInformer{
		policyInformer.Informer(),
		triggerInformer.Informer(),
	} {
		if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    func(object any) { c.enqueueOwnedByWorkspace(object) },
			UpdateFunc: func(_, object any) { c.enqueueOwnedByWorkspace(object) },
			DeleteFunc: func(object any) { c.enqueueOwnedByWorkspace(object) },
		}); err != nil {
			return err
		}
	}
	// PodSnapshots are created by GKE and initially have no Workspace owner, so
	// their events wake up the Workspaces that are currently checkpointing in the
	// same namespace.
	if _, err := snapshotInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(object any) { c.enqueueSnapshotObservers(object) },
		UpdateFunc: func(_, object any) { c.enqueueSnapshotObservers(object) },
		DeleteFunc: func(object any) { c.enqueueSnapshotObservers(object) },
	}); err != nil {
		return err
	}

	snapshotFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(),
		policyInformer.Informer().HasSynced,
		triggerInformer.Informer().HasSynced,
		snapshotInformer.Informer().HasSynced,
		storageConfigInformer.Informer().HasSynced,
	) {
		return fmt.Errorf("failed to sync GKE PodSnapshot caches")
	}
	c.snapshotReady.Store(true)
	logf("reconciler", "GKE PodSnapshot caches are warm")

	// Every replica serves admission requests, but only the leader writes, so the
	// deployment can be scaled for webhook availability without duplicating work.
	return c.runElected(ctx, c.startWorkers)
}

func (c *Controller) startWorkers(ctx context.Context) {
	logf("reconciler", "starting %d Workspace reconcile workers", c.workers)
	for worker := 0; worker < c.workers; worker++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}
	<-ctx.Done()
}

func podWorkspaceIndexFunc(object any) ([]string, error) {
	pod, ok := object.(*corev1.Pod)
	if !ok {
		return nil, nil
	}
	workspace := pod.Labels[WorkspaceLabel]
	if workspace == "" {
		return nil, nil
	}
	return []string{pod.Namespace + "/" + workspace}, nil
}

// workspacePod returns the live Pod backing a Workspace from the informer cache.
func (c *Controller) workspacePod(namespace, name string) *corev1.Pod {
	if c.podIndexer == nil {
		return nil
	}
	objects, err := c.podIndexer.ByIndex(podWorkspaceIndex, namespace+"/"+name)
	if err != nil {
		return nil
	}
	var fallback *corev1.Pod
	for _, object := range objects {
		pod, ok := object.(*corev1.Pod)
		if !ok {
			continue
		}
		if pod.DeletionTimestamp == nil {
			return pod
		}
		fallback = pod
	}
	return fallback
}

func (c *Controller) enqueueWorkspaceObject(object any) {
	workspace := asUnstructured(object)
	if workspace == nil {
		return
	}
	c.enqueueWorkspace(workspace.GetNamespace(), workspace.GetName())
}

func (c *Controller) enqueuePod(object any) {
	pod := asPod(object)
	if pod == nil {
		return
	}
	c.enqueueWorkspace(pod.Namespace, pod.Labels[WorkspaceLabel])
}

// enqueueNamespace re-reconciles every Workspace in the namespace of an object,
// which is how a deleted jupyter-ipc-config ConfigMap gets recreated.
func (c *Controller) enqueueNamespace(object any) {
	if tombstone, ok := object.(cache.DeletedFinalStateUnknown); ok {
		object = tombstone.Obj
	}
	accessor, err := meta.Accessor(object)
	if err != nil {
		return
	}
	c.enqueueWorkspacesIn(accessor.GetNamespace(), nil)
}

// enqueueOwnedByWorkspace maps a resource we created back to its Workspace. Most
// carry an ownerReference, but the PodSnapshotPolicy deliberately does not (see
// reconcileDeletedWorkspace), so fall back to decoding the Workspace name out of
// the resource name. That fallback is what re-enqueues a deleted Workspace on
// startup so an interrupted teardown gets finished.
func (c *Controller) enqueueOwnedByWorkspace(object any) {
	owned := asUnstructured(object)
	if owned == nil {
		return
	}
	if owner := workspaceOwner(owned); owner != "" {
		c.enqueueWorkspace(owned.GetNamespace(), owner)
		return
	}
	if name, ok := workspaceFromPolicyName(owned.GetName()); ok {
		c.enqueueWorkspace(owned.GetNamespace(), name)
		return
	}
	if name, ok := workspaceFromTriggerName(owned.GetName()); ok {
		c.enqueueWorkspace(owned.GetNamespace(), name)
	}
}

// enqueueSnapshotObservers maps a PodSnapshot event back to the Workspace that
// caused it. PodSnapshots never carry an ownerReference (GKE's admission policy
// forbids us from adding one), but GKE does label them with the trigger that
// produced them, and our trigger names are derived from the Workspace name.
func (c *Controller) enqueueSnapshotObservers(object any) {
	snapshot := asUnstructured(object)
	if snapshot == nil {
		return
	}
	if trigger := snapshot.GetLabels()[LabelSnapshotTriggeredBy]; trigger != "" {
		if workspace, ok := workspaceFromTriggerName(trigger); ok {
			c.enqueueWorkspace(snapshot.GetNamespace(), workspace)
			return
		}
	}
	// Unlabelled or foreign snapshot: fall back to waking whoever is mid-checkpoint.
	c.enqueueWorkspacesIn(snapshot.GetNamespace(), func(workspace *unstructured.Unstructured) bool {
		return workspace.GetAnnotations()[AnnotationCheckpointState] == CheckpointStateCheckpointing
	})
}

// enqueueWorkspacesIn enqueues the cached Workspaces of a namespace that match a
// predicate. It reads from the informer cache only; no API call is made.
func (c *Controller) enqueueWorkspacesIn(namespace string, match func(*unstructured.Unstructured) bool) {
	if !c.managedNamespace(namespace) || c.workspaceLister == nil {
		return
	}
	objects, err := c.workspaceLister.ByNamespace(namespace).List(labels.Everything())
	if err != nil {
		return
	}
	for _, object := range objects {
		workspace := asUnstructured(object)
		if workspace == nil || (match != nil && !match(workspace)) {
			continue
		}
		c.enqueueWorkspace(workspace.GetNamespace(), workspace.GetName())
	}
}

// enqueueWorkspacesForKind wakes up the Workspaces of a WorkspaceKind whose
// PodSnapshot annotations changed.
func (c *Controller) enqueueWorkspacesForKind(old, current any) {
	previous, updated := asUnstructured(old), asUnstructured(current)
	if previous == nil || updated == nil {
		return
	}
	if previous.GetAnnotations()[AnnotationEnabled] == updated.GetAnnotations()[AnnotationEnabled] &&
		previous.GetAnnotations()[AnnotationStorageConfig] == updated.GetAnnotations()[AnnotationStorageConfig] &&
		previous.GetAnnotations()[AnnotationPodMigrationEnabled] == updated.GetAnnotations()[AnnotationPodMigrationEnabled] {
		return
	}
	objects, err := c.workspaceLister.List(labels.Everything())
	if err != nil {
		return
	}
	for _, object := range objects {
		workspace := asUnstructured(object)
		if workspace == nil {
			continue
		}
		if kind, _, _ := unstructured.NestedString(workspace.Object, "spec", "kind"); kind != updated.GetName() {
			continue
		}
		c.enqueueWorkspace(workspace.GetNamespace(), workspace.GetName())
	}
}

func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextItem(ctx) {
	}
}

func (c *Controller) processNextItem(ctx context.Context) bool {
	key, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(key)

	requeueAfter, err := c.reconcileWorkspace(ctx, key)
	switch {
	case err != nil:
		logf("reconciler", "Error reconciling Workspace %s: %v (retry %d)", key, err, c.queue.NumRequeues(key)+1)
		c.queue.AddRateLimited(key)
	case requeueAfter > 0:
		c.queue.Forget(key)
		c.queue.AddAfter(key, requeueAfter)
	default:
		c.queue.Forget(key)
	}
	return true
}
