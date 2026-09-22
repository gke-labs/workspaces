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
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

var testPublicURL = &url.URL{Scheme: "https", Host: "example.com"}

type fakeAuth struct{}

func (fakeAuth) Authenticate(ctx context.Context, token string) (Identity, error) {
	if token != "valid" {
		return Identity{}, errors.New("invalid")
	}
	return Identity{Email: "alice@example.com"}, nil
}

type fakeWorkspaces struct {
	target *url.URL
	denied bool
	failed bool
	remove bool
	uid    string
}

func (fake *fakeWorkspaces) Namespaces(ctx context.Context, identity Identity) ([]Namespace, error) {
	if fake.failed {
		return nil, errors.New("unavailable")
	}
	return []Namespace{{Name: "team-a"}}, nil
}
func (fake *fakeWorkspaces) Resolve(ctx context.Context, identity Identity, namespace, workspace, port string) (Target, error) {
	if fake.denied || namespace != "team-a" || workspace != "lab" || port != "jupyterlab" {
		return Target{}, ErrDenied
	}
	if fake.failed {
		return Target{}, errors.New("unavailable")
	}
	return Target{URL: fake.target, RemovePrefix: fake.remove, WorkspaceUID: fake.uid}, nil
}

func TestProxyBoundary(t *testing.T) {
	var received *http.Request
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		received = request.Clone(request.Context())
		_, _ = io.WriteString(response, "upstream")
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	workspaces := &fakeWorkspaces{target: target}
	proxy, err := NewProxy(fakeAuth{}, workspaces, target, target, testPublicURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/workspaces/api/v1/workspaces/team-a", "/workspace/connect/team-a/lab/jupyterlab/api/status?x=1", "/workspaces/"} {
		request := httptest.NewRequest("GET", path, nil)
		request.Header.Set(IAPHeader, "valid")
		for _, name := range []string{"Kubeflow-Userid", "Kubeflow-Groups", "Authorization", "X-Goog-Authenticated-User-Email", "X-Auth-Request-Email", "Impersonate-User", "X-Forwarded-User"} {
			request.Header.Set(name, "forged")
		}
		request.Header.Set("Cookie", "GCP_IAP_AUTH_TOKEN_test=secret; _xsrf=keep")
		request.Header.Set("Connection", "Kubeflow-Userid")
		response := httptest.NewRecorder()
		proxy.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("%s: %d", path, response.Code)
		}
		wantEmail := ""
		if strings.HasPrefix(path, "/workspaces/api/") {
			wantEmail = "alice@example.com"
		}
		if received.Header.Get("Kubeflow-Userid") != wantEmail {
			t.Fatalf("unexpected user: %v", received.Header)
		}
		for _, name := range []string{IAPHeader, "Kubeflow-Groups", "Authorization", "X-Goog-Authenticated-User-Email", "X-Auth-Request-Email", "Impersonate-User", "X-Forwarded-User"} {
			if received.Header.Get(name) != "" {
				t.Fatalf("leaked %s", name)
			}
		}
		if received.Header.Get("Cookie") != "_xsrf=keep" {
			t.Fatalf("unexpected cookies: %s", received.Header.Get("Cookie"))
		}
		if strings.HasPrefix(path, "/workspaces/api/") && received.URL.Path != "/api/v1/workspaces/team-a" {
			t.Fatal(received.URL.Path)
		}
	}
	for _, path := range []string{"/workspace/connect/team-b/lab/jupyterlab/", "/workspace/connect/team-a/lab/8888/"} {
		request := httptest.NewRequest("GET", path, nil)
		request.Header.Set(IAPHeader, "valid")
		response := httptest.NewRecorder()
		proxy.ServeHTTP(response, request)
		if response.Code != 403 {
			t.Fatalf("%s: %d", path, response.Code)
		}
	}
}

func TestProxyRejectionsAndDiscovery(t *testing.T) {
	target, _ := url.Parse("http://unused.invalid")
	workspaces := &fakeWorkspaces{target: target}
	proxy, _ := NewProxy(fakeAuth{}, workspaces, target, target, testPublicURL)
	for _, test := range []struct {
		path, token string
		status      int
	}{
		{"/healthz", "", 200},
		{"/workspaces/", "", 401},
		{"/workspaces/", "forged", 401},
		{"/workspace/connect/team-a/lab/jupyterlab/../other", "valid", 400},
		{"/workspace/connect/team-a%2flab/lab/jupyterlab/", "valid", 400},
		{"/workspace/connect/team-a/lab/jupyterlab/%252e%252e", "valid", 400},
		{"/workspace/connect/team-a/lab/jupyterlab", "valid", 404},
		{"/workspaces/api/v1/namespaces", "valid", 200},
	} {
		request := httptest.NewRequest("GET", test.path, nil)
		if test.token != "" {
			request.Header.Set(IAPHeader, test.token)
		}
		response := httptest.NewRecorder()
		proxy.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Fatalf("%s: got %d, want %d", test.path, response.Code, test.status)
		}
		if test.path == "/workspaces/api/v1/namespaces" && response.Body.String() != "{\"data\":[{\"name\":\"team-a\"}]}\n" {
			t.Fatal(response.Body.String())
		}
	}
	workspaces.failed = true
	request := httptest.NewRequest("GET", "/workspaces/api/v1/namespaces", nil)
	request.Header.Set(IAPHeader, "valid")
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	if response.Code != 503 {
		t.Fatal(response.Code)
	}
	request.Header.Add(IAPHeader, "valid")
	response = httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	if response.Code != 401 {
		t.Fatal(response.Code)
	}
}

func TestProxyWebSocket(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(request *http.Request) bool { return request.Header.Get("Origin") == "https://example.com" }}
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		connection, err := upgrader.Upgrade(response, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		kind, payload, err := connection.ReadMessage()
		if err == nil {
			_ = connection.WriteMessage(kind, payload)
		}
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	proxy, _ := NewProxy(fakeAuth{}, &fakeWorkspaces{target: target}, target, target, testPublicURL)
	server := httptest.NewServer(proxy)
	defer server.Close()
	headers := http.Header{"Host": {"example.com"}, "Origin": {"https://example.com"}, IAPHeader: {"valid"}}
	connection, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/workspace/connect/team-a/lab/jupyterlab/api/kernels/123/channels", headers)
	if err != nil {
		t.Fatalf("upgrade: %v, response: %v", err, response)
	}
	defer connection.Close()
	if err := connection.WriteMessage(websocket.TextMessage, []byte("kernel message")); err != nil {
		t.Fatal(err)
	}
	_, message, err := connection.ReadMessage()
	if err != nil || string(message) != "kernel message" {
		t.Fatalf("%s, %v", message, err)
	}
}

func TestProxyOriginsAndRewrite(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(response, request.URL.RequestURI())
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	proxy, _ := NewProxy(fakeAuth{}, &fakeWorkspaces{target: target, remove: true}, target, target, testPublicURL)
	for _, test := range []struct {
		method, host, origin, upgrade string
		want                          int
	}{
		{"POST", "example.com", "https://example.com", "", 200},
		{"POST", "example.com", "https://evil.example", "", 403},
		{"POST", "example.com", "", "", 403},
		{"GET", "evil.example", "", "", 421},
		{"GET", "example.com", "", "websocket", 403},
	} {
		request := httptest.NewRequest(test.method, "/workspace/connect/team-a/lab/jupyterlab/api/status?value=a%2Fb", nil)
		request.Host = test.host
		request.Header.Set(IAPHeader, "valid")
		request.Header.Set("Origin", test.origin)
		request.Header.Set("Upgrade", test.upgrade)
		response := httptest.NewRecorder()
		proxy.ServeHTTP(response, request)
		if response.Code != test.want {
			t.Fatalf("%+v: %d", test, response.Code)
		}
		if test.want == 200 && response.Body.String() != "/api/status?value=a%2Fb" {
			t.Fatal(response.Body.String())
		}
	}
}
