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

// Command snapshot-addon runs gke-workspace-snapshot-addon: the standalone
// controller and mutating admission webhook pair that implements stateful
// Workspace pause and resume on top of the GKE PodSnapshot API.
//
// It is deployed separately from gke-access-proxy so that a crash, rollout or
// resource exhaustion in the snapshot control plane cannot take user traffic down,
// and so the two can be scaled and rolled independently.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kubeflow/notebooks/gke/internal/snapshot"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func floatEnvironment(name string, fallback float64) float64 {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed <= 0 {
		log.Fatalf("%s must be a positive float", name)
	}
	return parsed
}

func intEnvironment(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		log.Fatalf("%s must be a positive integer", name)
	}
	return parsed
}

func durationEnvironment(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		log.Fatalf("%s must be a positive Go duration", name)
	}
	return parsed
}

func boolEnvironment(name string, fallback bool) bool {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		log.Fatalf("%s must be a boolean", name)
	}
	return parsed
}

func main() {
	webhookListen := flag.String("webhook-listen", ":9443", "HTTPS listen address for the mutating admission webhooks")
	healthListen := flag.String("health-listen", ":8080", "HTTP listen address for liveness and readiness probes")
	webhookCert := flag.String("webhook-cert", "/tmp/k8s-webhook-server/serving-certs/tls.crt", "TLS certificate file for the admission webhooks")
	webhookKey := flag.String("webhook-key", "/tmp/k8s-webhook-server/serving-certs/tls.key", "TLS key file for the admission webhooks")
	snapshotBucket := flag.String("snapshot-bucket", os.Getenv("SNAPSHOT_GCS_BUCKET"), "GCS bucket backing the PodSnapshotStorageConfig")
	tenants := flag.String("tenants", os.Getenv("TENANT_NAMESPACES"), "Comma-separated tenant namespaces; empty watches every namespace")
	workers := flag.Int("workers", intEnvironment("SNAPSHOT_WORKERS", 4), "Concurrent Workspace reconcile workers")
	resync := flag.Duration("resync", durationEnvironment("SNAPSHOT_RESYNC", 10*time.Minute), "Informer relist period; a safety net, not a reconcile interval")
	settle := flag.Duration("settle-grace-period", durationEnvironment("SNAPSHOT_SETTLE_GRACE_PERIOD", snapshot.SocketSettleGracePeriod), "Delay between draining Pod endpoints and requesting a checkpoint")
	leaderElection := flag.Bool("leader-election", boolEnvironment("LEADER_ELECTION", true), "Reconcile from a single replica while all replicas serve webhooks")
	leaseNamespace := flag.String("lease-namespace", os.Getenv("POD_NAMESPACE"), "Namespace holding the leader election Lease")
	kubeQPS := flag.Float64("kube-qps", floatEnvironment("KUBE_CLIENT_QPS", 50), "Kubernetes API client rate-limiter QPS")
	kubeBurst := flag.Int("kube-burst", intEnvironment("KUBE_CLIENT_BURST", 100), "Kubernetes API client rate-limiter burst")
	kubeconfig := flag.String("kubeconfig", "", "Optional kubeconfig for local testing; defaults to in-cluster credentials")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var config *rest.Config
	var err error
	if *kubeconfig == "" {
		config, err = rest.InClusterConfig()
	} else {
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	}
	if err != nil {
		log.Fatal(err)
	}
	// Watches must not be cut off by a short client timeout.
	config.Timeout = 0
	config.QPS = float32(*kubeQPS)
	config.Burst = *kubeBurst

	var namespaces []string
	for _, namespace := range strings.Split(*tenants, ",") {
		if trimmed := strings.TrimSpace(namespace); trimmed != "" {
			namespaces = append(namespaces, trimmed)
		}
	}

	controller, err := snapshot.NewController(config, snapshot.Options{
		Namespaces:        namespaces,
		SnapshotBucket:    *snapshotBucket,
		ResyncPeriod:      *resync,
		SettleGracePeriod: *settle,
		Workers:           *workers,
		LeaderElection:    *leaderElection,
		LeaseNamespace:    *leaseNamespace,
		Identity:          os.Getenv("POD_NAME"),
	})
	if err != nil {
		log.Fatal(err)
	}

	handler := controller.Handler()
	webhookServer := snapshot.NewTLSServer(*webhookListen, *webhookCert, *webhookKey, handler)
	healthServer := &http.Server{
		Addr:              *healthListen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go func() {
		log.Printf("snapshot addon webhook listening on %s", *webhookListen)
		if err := webhookServer.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("snapshot webhook listener failed: %v", err)
		}
	}()
	go func() {
		log.Printf("snapshot addon health endpoints listening on %s", *healthListen)
		if err := healthServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("snapshot health listener failed: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_ = webhookServer.Shutdown(shutdown)
		_ = healthServer.Shutdown(shutdown)
	}()

	if err := controller.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
	log.Print("snapshot addon stopped")
}
