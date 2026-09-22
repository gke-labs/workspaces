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

package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/kubeflow/notebooks/gke/internal/access"
	"github.com/kubeflow/notebooks/gke/internal/connectionpolicy"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func lifetimeEnvironment(name string) int64 {
	value := os.Getenv(name)
	if value == "" {
		return 0
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		log.Fatalf("%s must be an integer number of seconds", name)
	}
	return seconds
}

func floatEnvironment(name string, defaultVal float64) float64 {
	value := os.Getenv(name)
	if value == "" {
		return defaultVal
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed <= 0 {
		log.Fatalf("%s must be a positive float", name)
	}
	return parsed
}

func intEnvironment(name string, defaultVal int) int {
	value := os.Getenv(name)
	if value == "" {
		return defaultVal
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		log.Fatalf("%s must be a positive integer", name)
	}
	return parsed
}

func main() {
	listen := flag.String("listen", ":8080", "HTTP listen address; TLS terminates at the GKE load balancer")
	audience := flag.String("iap-audience", os.Getenv("IAP_AUDIENCE"), "Exact IAP signed assertion audience")
	publicURL := flag.String("public-url", os.Getenv("PUBLIC_URL"), "Public HTTPS origin without a path")
	desktopURL := flag.String("desktop-url", os.Getenv("DESKTOP_URL"), "Optional separate HTTPS origin for token-authenticated desktop access")
	connectionDefault := flag.Int64("connection-token-default-seconds", lifetimeEnvironment("CONNECTION_TOKEN_DEFAULT_SECONDS"), "Default connection token lifetime in seconds; 0 uses 86400")
	connectionMaximum := flag.Int64("connection-token-max-seconds", lifetimeEnvironment("CONNECTION_TOKEN_MAX_SECONDS"), "Maximum requested token lifetime in seconds; 0 uses 604800; upper bound 2592000")
	frontendURL := flag.String("frontend-url", os.Getenv("FRONTEND_URL"), "Internal frontend HTTP origin")
	backendURL := flag.String("backend-url", os.Getenv("BACKEND_URL"), "Internal backend HTTP origin")
	tenants := flag.String("tenants", os.Getenv("TENANT_NAMESPACES"), "Comma-separated managed tenant namespaces")
	kubeQPS := flag.Float64("kube-qps", floatEnvironment("KUBE_CLIENT_QPS", 100), "Kubernetes API client rate-limiter QPS (default 100)")
	kubeBurst := flag.Int("kube-burst", intEnvironment("KUBE_CLIENT_BURST", 200), "Kubernetes API client rate-limiter burst (default 200)")
	kubeconfig := flag.String("kubeconfig", "", "Optional kubeconfig for local testing; defaults to in-cluster credentials")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	keyClient := &http.Client{Timeout: 10 * time.Second}
	ctx = oidc.ClientContext(ctx, keyClient)
	authenticator, err := access.NewIAPAuthenticator(ctx, *audience)
	if err != nil {
		log.Fatal(err)
	}
	var config *rest.Config
	if *kubeconfig == "" {
		config, err = rest.InClusterConfig()
	} else {
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	}
	if err != nil {
		log.Fatal(err)
	}
	config.Timeout = 10 * time.Second
	config.QPS = float32(*kubeQPS)
	config.Burst = *kubeBurst
	managed := strings.Split(*tenants, ",")
	for index := range managed {
		managed[index] = strings.TrimSpace(managed[index])
	}
	workspaces, err := access.NewKubernetesAccess(config, managed)
	if err != nil {
		log.Fatal(err)
	}
	// Workspace state changes (for example a pause taking a GKE PodSnapshot) must
	// invalidate the resolve cache of every replica, so each one watches Workspaces
	// instead of relying on an in-process callback.
	go workspaces.WatchWorkspaces(ctx)
	parse := func(value string) *url.URL {
		parsed, err := url.Parse(value)
		if err != nil {
			log.Fatal(err)
		}
		return parsed
	}
	proxy, err := access.NewProxy(authenticator, workspaces, parse(*frontendURL), parse(*backendURL), parse(*publicURL))
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Addr: *listen, Handler: proxy, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 650 * time.Second, MaxHeaderBytes: 64 << 10}
	var desktopServer *http.Server
	if *desktopURL != "" {
		if parse(*desktopURL).Host == parse(*publicURL).Host {
			log.Fatal("desktop origin must differ from browser origin")
		}
		connections, err := access.NewConnections(config, workspaces, parse(*desktopURL), connectionpolicy.Policy{DefaultSeconds: *connectionDefault, MaxSeconds: *connectionMaximum})
		if err != nil {
			log.Fatal(err)
		}
		desktopServer = &http.Server{Addr: ":8081", Handler: proxy.EnableConnections(connections), ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 650 * time.Second, MaxHeaderBytes: 16 << 10}
		go connections.Cleanup(ctx)
		go func() {
			if err := desktopServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatal("desktop listener failed")
			}
		}()
	}
	go func() {
		<-ctx.Done()
		shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_ = server.Shutdown(shutdown)
		if desktopServer != nil {
			_ = desktopServer.Shutdown(shutdown)
		}
	}()
	log.Printf("access proxy listening on %s", *listen)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
