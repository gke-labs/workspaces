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
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const maxAdmissionBody = 4 << 20

// Handler serves the mutating admission webhooks plus health endpoints.
func (c *Controller) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mutate-workspace", c.HandleWorkspaceMutate)
	mux.HandleFunc("/mutate-pod", c.HandlePodMutate)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !c.CachesSynced() {
			http.Error(w, "informer caches are still syncing", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// certificateReloader serves the webhook certificate from memory and reloads it
// when cert-manager rotates the mounted Secret, so TLS handshakes never touch disk.
type certificateReloader struct {
	certFile string
	keyFile  string

	mu        sync.RWMutex
	current   *tls.Certificate
	loadedAt  time.Time
	certMtime time.Time
	keyMtime  time.Time
}

func (r *certificateReloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.RLock()
	current, loadedAt := r.current, r.loadedAt
	r.mu.RUnlock()
	if current != nil && time.Since(loadedAt) < 30*time.Second {
		return current, nil
	}
	return r.reload(current)
}

func (r *certificateReloader) reload(current *tls.Certificate) (*tls.Certificate, error) {
	certInfo, certErr := os.Stat(r.certFile)
	keyInfo, keyErr := os.Stat(r.keyFile)
	r.mu.Lock()
	defer r.mu.Unlock()
	if current != nil && certErr == nil && keyErr == nil &&
		certInfo.ModTime().Equal(r.certMtime) && keyInfo.ModTime().Equal(r.keyMtime) {
		r.loadedAt = time.Now()
		return r.current, nil
	}
	certificate, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		if r.current != nil {
			// Keep serving the previous certificate through a transient rotation.
			return r.current, nil
		}
		return nil, err
	}
	r.current = &certificate
	r.loadedAt = time.Now()
	if certErr == nil {
		r.certMtime = certInfo.ModTime()
	}
	if keyErr == nil {
		r.keyMtime = keyInfo.ModTime()
	}
	return r.current, nil
}

// NewTLSServer builds the HTTPS server that the API server calls.
func NewTLSServer(addr, certFile, keyFile string, handler http.Handler) *http.Server {
	reloader := &certificateReloader{certFile: certFile, keyFile: keyFile}
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
		TLSConfig: &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: reloader.get,
		},
	}
}

// serveAdmission decodes an AdmissionReview, runs mutate and writes the response.
func serveAdmission(w http.ResponseWriter, r *http.Request, mutate func(context.Context, *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAdmissionBody))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	var review admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &review); err != nil || review.Request == nil {
		http.Error(w, "invalid admission review", http.StatusBadRequest)
		return
	}
	response := mutate(r.Context(), review.Request)
	response.UID = review.Request.UID
	review.Response = response
	review.Request = nil
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(review)
}

func allow() *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{Allowed: true}
}

func allowWithPatch(patches []jsonPatchOp) *admissionv1.AdmissionResponse {
	if len(patches) == 0 {
		return allow()
	}
	encoded, err := json.Marshal(patches)
	if err != nil {
		return allow()
	}
	patchType := admissionv1.PatchTypeJSONPatch
	return &admissionv1.AdmissionResponse{Allowed: true, PatchType: &patchType, Patch: encoded}
}

func (c *Controller) HandleWorkspaceMutate(w http.ResponseWriter, r *http.Request) {
	serveAdmission(w, r, c.mutateWorkspace)
}

func (c *Controller) HandlePodMutate(w http.ResponseWriter, r *http.Request) {
	serveAdmission(w, r, c.mutatePod)
}

// mutateWorkspace intercepts pause and resume requests on a Workspace.
//
// The webhook only performs the two actions that must be synchronous with the
// user's API call: rewriting spec.paused and locking the Pod out of its Service.
// Creating the GKE PodSnapshot resources is left to the reconciler, which observes
// the annotation through its Workspace watch.
func (c *Controller) mutateWorkspace(ctx context.Context, request *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	if request.Operation != admissionv1.Update || request.SubResource != "" {
		return allow()
	}
	var updated, previous unstructured.Unstructured
	if err := json.Unmarshal(request.Object.Raw, &updated.Object); err != nil {
		return allow()
	}
	if len(request.OldObject.Raw) > 0 {
		_ = json.Unmarshal(request.OldObject.Raw, &previous.Object)
	}

	wasPaused, _, _ := unstructured.NestedBool(previous.Object, "spec", "paused")
	nowPaused, _, _ := unstructured.NestedBool(updated.Object, "spec", "paused")
	if wasPaused == nowPaused {
		// Not a pause or resume transition; skip the enablement lookups entirely.
		return allow()
	}
	if enabled, _ := c.snapshotEnabled(ctx, &updated); !enabled {
		return allow()
	}

	annotations := updated.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	checkpointState := annotations[AnnotationCheckpointState]
	namespace, name := updated.GetNamespace(), updated.GetName()
	if namespace == "" {
		namespace = request.Namespace
	}

	// Case 1: pause requested (spec.paused false -> true).
	if !wasPaused && nowPaused {
		if checkpointState == CheckpointStateReady {
			logf("webhook", "Workspace %s/%s snapshot is Ready; allowing spec.paused=true", namespace, name)
			return allow()
		}
		pod := c.findWorkspacePod(ctx, namespace, name)
		if pod == nil || pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil {
			logf("webhook", "Workspace %s/%s has no running Pod; allowing spec.paused=true immediately", namespace, name)
			return allow()
		}

		logf("webhook", "Intercepting pause on Workspace %s/%s (pod %s): holding spec.paused=false while GKE takes a PodSnapshot", namespace, name, pod.Name)
		// Drop the Pod out of the Service EndpointSlice now so the browser session is
		// cut immediately rather than one watch round-trip later.
		if err := c.setPodActiveCondition(ctx, pod, corev1.ConditionFalse, "CheckpointingInProgress", "Pausing: taking GKE PodSnapshot"); err != nil {
			logf("webhook", "Warning: failed to update Pod readinessGate on %s/%s: %v", namespace, pod.Name, err)
		}

		annotations[AnnotationCheckpointState] = CheckpointStateCheckpointing
		annotations[AnnotationCheckpointStartedAt] = time.Now().UTC().Format(time.RFC3339)
		return allowWithPatch([]jsonPatchOp{
			{Op: "add", Path: "/spec/paused", Value: false},
			{Op: "add", Path: "/metadata/annotations", Value: annotations},
		})
	}

	// Case 2: resume requested (spec.paused true -> false) with a saved checkpoint.
	if wasPaused && !nowPaused && checkpointState == CheckpointStateReady {
		logf("webhook", "Workspace %s/%s resuming from checkpoint %q", namespace, name, annotations[AnnotationLastCheckpointName])
		annotations[AnnotationCheckpointState] = CheckpointStateRestoring
		return allowWithPatch([]jsonPatchOp{
			{Op: "add", Path: "/metadata/annotations", Value: annotations},
		})
	}
	return allow()
}

// mutatePod configures a Workspace Pod for GKE PodSnapshot on CREATE.
func (c *Controller) mutatePod(ctx context.Context, request *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	if request.Operation != admissionv1.Create {
		return allow()
	}
	var pod corev1.Pod
	if err := json.Unmarshal(request.Object.Raw, &pod); err != nil {
		return allow()
	}
	workspaceName := pod.Labels[WorkspaceLabel]
	if workspaceName == "" {
		return allow()
	}
	namespace := request.Namespace
	if namespace == "" {
		namespace = pod.Namespace
	}

	// gVisor cannot host TPU devices, so TPU Workspaces are never snapshotted. This
	// mirrors podConfigRequestsTPU on the Workspace path; both key off ResourceTPU.
	for _, container := range pod.Spec.Containers {
		if _, hasTPU := container.Resources.Limits[ResourceTPU]; hasTPU {
			return allow()
		}
		if _, hasTPU := container.Resources.Requests[ResourceTPU]; hasTPU {
			return allow()
		}
	}

	// The restore annotation must reflect the very latest Workspace state, so this
	// one read deliberately bypasses the informer cache. Pod creations are rare
	// (once per Workspace start), unlike the events the reconciler handles.
	workspace, err := c.dynamic.Resource(workspaceGVR).Namespace(namespace).Get(ctx, workspaceName, metav1.GetOptions{})
	if err != nil {
		logf("webhook", "Could not fetch Workspace %s/%s for Pod mutation: %v", namespace, workspaceName, err)
		return allow()
	}
	if enabled, _ := c.snapshotEnabled(ctx, workspace); !enabled {
		return allow()
	}

	// The reconciler owns this ConfigMap; this is only a safety net for the very
	// first Pod of a namespace, and it is a no-op once the cache holds the object.
	if err := c.ensureIPCConfigMap(ctx, namespace); err != nil {
		logf("webhook", "Warning: failed to ensure ConfigMap %s/%s: %v", namespace, IPCConfigMapName, err)
	}

	patches := []jsonPatchOp{{
		Op:    "add",
		Path:  "/spec/runtimeClassName",
		Value: DefaultGKERuntimeClass,
	}}

	hasGate := false
	for _, gate := range pod.Spec.ReadinessGates {
		if gate.ConditionType == ReadinessGateConditionType {
			hasGate = true
			break
		}
	}
	if !hasGate {
		patches = append(patches, jsonPatchOp{
			Op:    "add",
			Path:  "/spec/readinessGates",
			Value: append(pod.Spec.ReadinessGates, corev1.PodReadinessGate{ConditionType: ReadinessGateConditionType}),
		})
	}

	hasVolume := false
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == IPCConfigMapName {
			hasVolume = true
			break
		}
	}
	if !hasVolume {
		patches = append(patches, jsonPatchOp{
			Op:   "add",
			Path: "/spec/volumes",
			Value: append(pod.Spec.Volumes, corev1.Volume{
				Name: IPCConfigMapName,
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: IPCConfigMapName},
					},
				},
			}),
		})
	}

	if len(pod.Spec.Containers) > 0 {
		mounts := pod.Spec.Containers[0].VolumeMounts
		hasMount := false
		for _, mount := range mounts {
			if mount.Name == IPCConfigMapName || mount.MountPath == IPCConfigMapMountPath {
				hasMount = true
				break
			}
		}
		if !hasMount {
			patches = append(patches, jsonPatchOp{
				Op:   "add",
				Path: "/spec/containers/0/volumeMounts",
				Value: append(mounts, corev1.VolumeMount{
					Name:      IPCConfigMapName,
					MountPath: IPCConfigMapMountPath,
					SubPath:   IPCConfigMapKey,
				}),
			})
		}
	}

	if lastCheckpoint := workspace.GetAnnotations()[AnnotationLastCheckpointName]; lastCheckpoint != "" {
		annotations := pod.Annotations
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[GKERestoreAnnotation] = lastCheckpoint
		patches = append(patches, jsonPatchOp{Op: "add", Path: "/metadata/annotations", Value: annotations})
		logf("webhook", "Injected %s=%s onto Pod %s/%s", GKERestoreAnnotation, lastCheckpoint, namespace, pod.Name)
	} else {
		logf("webhook", "Configured Pod %s/%s for GKE PodSnapshot (runtimeClassName=%s, IPC config, readinessGate)", namespace, pod.Name, DefaultGKERuntimeClass)
	}
	return allowWithPatch(patches)
}
