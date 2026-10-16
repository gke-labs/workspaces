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

// fleet-helper is a small, demo-only companion to AX for the
// examples/agent-substrate walkthrough. It runs next to ax-server (in a
// namespace labeled ate.dev/api-access=true) and does two things that a tenant
// Workspace cannot do itself, because tenants are deliberately kept away from
// the Substrate control API:
//
//  1. Egress defaulting. Substrate denies all actor egress until the actor has
//     an EgressPolicy, and AX does not create one. For every actor in an
//     allowed atespace that has no policy yet, fleet-helper creates one from
//     a configured template (for example: TLS passthrough to the Gemini API).
//     Existing policies are never modified.
//
//  2. A read-only actor view. GET /v1/actors?atespace=<name> returns each
//     actor's state and the worker pod it is packed onto, so the notebook can
//     show how many agents share each worker pod.
//
// Only the atespaces listed in --atespaces are touched or exposed.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"sigs.k8s.io/yaml"
)

type tokenAuth struct{ path string }

func (t tokenAuth) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	b, err := os.ReadFile(t.path)
	if err != nil {
		return nil, fmt.Errorf("reading token file %q: %w", t.path, err)
	}
	return map[string]string{"authorization": "Bearer " + strings.TrimSpace(string(b))}, nil
}

func (tokenAuth) RequireTransportSecurity() bool { return true }

// actorView is the JSON shape served to the notebook.
type actorView struct {
	Name         string `json:"name"`
	State        string `json:"state"`
	WorkerPod    string `json:"workerPod,omitempty"`
	WorkerPool   string `json:"workerPool,omitempty"`
	Node         string `json:"node,omitempty"`
	Snapshots    int    `json:"snapshots"`
	EgressPolicy bool   `json:"egressPolicy"`
}

type helper struct {
	control   ateapipb.ControlClient
	atespaces map[string]bool
	rules     []*ateapipb.EgressRule

	mu       sync.RWMutex
	snapshot map[string][]actorView // atespace -> actors, refreshed by the loop
	lastSync time.Time
	lastErr  string
}

func main() {
	var (
		endpoint   = flag.String("substrate-endpoint", "api.ate-system.svc.cluster.local:443", "Substrate Control API endpoint")
		authority  = flag.String("substrate-authority", "api.ate-system.svc", "TLS server name / authority of the Control API")
		tokenFile  = flag.String("substrate-token-file", "/var/run/secrets/ateapi/token", "Projected ServiceAccount token (audience api.ate-system.svc)")
		caFile     = flag.String("substrate-ca-file", "/run/servicedns-ca/trust-bundle.pem", "CA bundle that signs the Control API serving certificate")
		atespaces  = flag.String("atespaces", "", "Comma-separated atespaces this helper manages (required)")
		policyFile = flag.String("egress-policy-file", "/etc/fleet-helper/egress-policy.yaml", "YAML file with the default EgressPolicy rules; empty disables egress defaulting")
		interval   = flag.Duration("interval", 3*time.Second, "Reconcile interval")
		addr       = flag.String("addr", ":8080", "HTTP listen address")
	)
	flag.Parse()

	if *atespaces == "" {
		slog.Error("--atespaces is required")
		os.Exit(2)
	}
	allowed := map[string]bool{}
	for _, a := range strings.Split(*atespaces, ",") {
		if a = strings.TrimSpace(a); a != "" {
			allowed[a] = true
		}
	}

	rules, err := loadRules(*policyFile)
	if err != nil {
		slog.Error("loading egress policy", "file", *policyFile, "error", err)
		os.Exit(1)
	}

	conn, err := dial(*endpoint, *authority, *tokenFile, *caFile)
	if err != nil {
		slog.Error("dialing Substrate", "error", err)
		os.Exit(1)
	}
	defer conn.Close()

	h := &helper{
		control:   ateapipb.NewControlClient(conn),
		atespaces: allowed,
		rules:     rules,
		snapshot:  map[string][]actorView{},
	}

	ctx := context.Background()
	go func() {
		for {
			h.reconcileAll(ctx)
			time.Sleep(*interval)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/v1/actors", h.serveActors)
	slog.Info("fleet-helper listening", "addr", *addr, "atespaces", *atespaces, "egressRules", len(rules))
	if err := http.ListenAndServe(*addr, mux); err != nil {
		slog.Error("http server", "error", err)
		os.Exit(1)
	}
}

func dial(endpoint, authority, tokenFile, caFile string) (*grpc.ClientConn, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("no certificates found in CA file")
	}
	creds := credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: authority, MinVersion: tls.VersionTLS12})
	return grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(creds),
		grpc.WithAuthority(authority),
		grpc.WithPerRPCCredentials(tokenAuth{path: tokenFile}),
	)
}

// loadRules parses a YAML document of the form accepted by
// `kubectl ate create egress-policy -f`, i.e. {rules: [...]}.
func loadRules(path string) ([]*ateapipb.EgressRule, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	js, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return nil, err
	}
	p := &ateapipb.EgressPolicy{}
	if err := protojson.Unmarshal(js, p); err != nil {
		return nil, fmt.Errorf("parsing EgressPolicy: %w", err)
	}
	return p.GetRules(), nil
}

func (h *helper) reconcileAll(ctx context.Context) {
	var errs []string
	next := map[string][]actorView{}
	for as := range h.atespaces {
		views, err := h.reconcile(ctx, as)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", as, err))
			slog.Warn("reconcile failed", "atespace", as, "error", err)
			continue
		}
		next[as] = views
	}
	h.mu.Lock()
	for as, v := range next {
		h.snapshot[as] = v
	}
	h.lastSync = time.Now()
	h.lastErr = strings.Join(errs, "; ")
	h.mu.Unlock()
}

func (h *helper) reconcile(ctx context.Context, atespace string) ([]actorView, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	var actors []*ateapipb.Actor
	token := ""
	for {
		resp, err := h.control.ListActors(ctx, &ateapipb.ListActorsRequest{
			Atespace: atespace,
			Options:  &ateapipb.ListOptions{PageSize: 200, PageToken: token},
		})
		if err != nil {
			if status.Code(err) == codes.NotFound {
				return nil, nil // atespace not created yet (AX creates it on first apply)
			}
			return nil, err
		}
		actors = append(actors, resp.GetActors()...)
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}

	views := make([]actorView, 0, len(actors))
	for _, a := range actors {
		name := a.GetMetadata().GetName()
		hasPolicy, err := h.ensureEgress(ctx, atespace, name)
		if err != nil {
			slog.Warn("egress policy", "actor", atespace+"/"+name, "error", err)
		}
		st := a.GetStatus()
		views = append(views, actorView{
			Name:         name,
			State:        strings.TrimPrefix(st.GetState().String(), "ACTOR_STATE_"),
			WorkerPod:    st.GetWorkerAssignment().GetWorkerPod(),
			WorkerPool:   st.GetWorkerAssignment().GetWorkerPool(),
			Node:         st.GetWorkerAssignment().GetNodeName(),
			Snapshots:    len(st.GetSnapshots()),
			EgressPolicy: hasPolicy,
		})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	return views, nil
}

// ensureEgress creates the default EgressPolicy for an actor that has none.
// It reports whether the actor has a policy afterwards.
func (h *helper) ensureEgress(ctx context.Context, atespace, actor string) (bool, error) {
	ref := &ateapipb.ObjectRef{Atespace: atespace, Name: actor}
	_, err := h.control.GetActorEgressPolicy(ctx, &ateapipb.GetActorEgressPolicyRequest{Actor: ref, Name: "default"})
	if err == nil {
		return true, nil
	}
	if status.Code(err) != codes.NotFound {
		return false, err
	}
	if len(h.rules) == 0 {
		return false, nil
	}
	_, err = h.control.CreateActorEgressPolicy(ctx, &ateapipb.CreateActorEgressPolicyRequest{
		Actor: ref,
		EgressPolicy: &ateapipb.EgressPolicy{
			Metadata: &ateapipb.ResourceMetadata{Atespace: atespace, Name: "default"},
			Rules:    h.rules,
		},
	})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return false, err
	}
	slog.Info("created default egress policy", "actor", atespace+"/"+actor)
	return true, nil
}

func (h *helper) serveActors(w http.ResponseWriter, r *http.Request) {
	as := r.URL.Query().Get("atespace")
	if !h.atespaces[as] {
		http.Error(w, fmt.Sprintf("atespace %q is not managed by this helper", as), http.StatusForbidden)
		return
	}
	h.mu.RLock()
	body := map[string]any{
		"atespace": as,
		"actors":   h.snapshot[as],
		"lastSync": h.lastSync.UTC().Format(time.RFC3339),
		"error":    h.lastErr,
	}
	h.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
