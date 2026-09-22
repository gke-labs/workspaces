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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kubeflow/notebooks/gke/internal/connectionpolicy"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func connectionFixture(t *testing.T) (*Connections, *fake.Clientset, ConnectionGrant) {
	t.Helper()
	workspaces, client := kubeFixture(t)
	connections := &Connections{accounts: client.CoreV1().ServiceAccounts(connectionNamespace), auth: client.AuthenticationV1(), workspaces: workspaces, origin: "https://connect.example.com", audience: "https://connect.example.com/desktop", now: time.Now}
	grant := ConnectionGrant{ID: "connection-test", User: "alice@example.com", Namespace: "team-a", Workspace: "lab", WorkspaceUID: "current", Port: "jupyterlab", ExpiresAt: time.Now().Add(time.Hour)}
	encoded, _ := json.Marshal(grant)
	_, err := connections.accounts.Create(context.Background(), &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: grant.ID, Namespace: connectionNamespace, UID: "grant-uid", Labels: map[string]string{connectionLabel: "true"}, Annotations: map[string]string{connectionAnnotation: string(encoded)}}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("create", "tokenreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
		review := action.(clienttesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		if len(review.Spec.Audiences) != 1 || review.Spec.Audiences[0] != connections.audience {
			t.Fatal("TokenReview audience missing")
		}
		review.Status = authenticationv1.TokenReviewStatus{Authenticated: review.Spec.Token == "valid", Audiences: []string{connections.audience}, User: authenticationv1.UserInfo{Username: "system:serviceaccount:" + connectionNamespace + ":" + grant.ID, UID: "grant-uid"}}
		return true, review, nil
	})
	return connections, client, grant
}

func TestConnectionAuthorization(t *testing.T) {
	for _, scenario := range []string{"valid", "wrong audience", "missing audience", "invalid token", "deleted grant", "recreated grant", "expired", "changed workspace UID", "RBAC revoked", "other tenant", "other port", "verification failure", "unmanaged account"} {
		t.Run(scenario, func(t *testing.T) {
			connections, client, grant := connectionFixture(t)
			ctx := context.Background()
			token, namespace, port := "valid", "team-a", "jupyterlab"
			account, _ := connections.accounts.Get(ctx, grant.ID, metav1.GetOptions{})
			switch scenario {
			case "wrong audience", "missing audience", "verification failure":
				client.PrependReactor("create", "tokenreviews", func(clienttesting.Action) (bool, runtime.Object, error) {
					if scenario == "verification failure" {
						return true, nil, errors.New("unavailable")
					}
					audiences := []string{"https://wrong.example"}
					if scenario == "missing audience" {
						audiences = nil
					}
					return true, &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{Authenticated: true, Audiences: audiences, User: authenticationv1.UserInfo{Username: "system:serviceaccount:" + connectionNamespace + ":" + grant.ID, UID: "grant-uid"}}}, nil
				})
			case "invalid token":
				token = "forged"
			case "deleted grant":
				_ = connections.accounts.Delete(ctx, grant.ID, metav1.DeleteOptions{})
			case "recreated grant":
				account.UID = "different-uid"
				_, _ = connections.accounts.Update(ctx, account, metav1.UpdateOptions{})
			case "expired":
				connections.now = func() time.Time { return grant.ExpiresAt.Add(time.Second) }
			case "changed workspace UID":
				grant.WorkspaceUID = "old-workspace"
				encoded, _ := json.Marshal(grant)
				account.Annotations[connectionAnnotation] = string(encoded)
				_, _ = connections.accounts.Update(ctx, account, metav1.UpdateOptions{})
			case "RBAC revoked":
				client.PrependReactor("create", "subjectaccessreviews", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, &authorizationv1.SubjectAccessReview{}, nil
				})
			case "other tenant":
				namespace = "team-b"
			case "other port":
				port = "9999"
			case "unmanaged account":
				account.Labels = nil
				_, _ = connections.accounts.Update(ctx, account, metav1.UpdateOptions{})
			}
			_, target, err := connections.Authorize(ctx, token, namespace, "lab", port)
			if scenario == "valid" {
				if err != nil || target.WorkspaceUID != "current" {
					t.Fatalf("valid grant rejected: %v", err)
				}
			} else if err == nil {
				t.Fatal("unauthorized grant accepted")
			}
		})
	}
}

func TestConnectionIssueAndRevoke(t *testing.T) {
	connections, client, _ := connectionFixture(t)
	client.PrependReactor("create", "serviceaccounts", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "token" {
			account := action.(clienttesting.CreateAction).GetObject().(*corev1.ServiceAccount)
			account.UID = "new-grant"
			return false, nil, nil
		}
		request := action.(clienttesting.CreateAction).GetObject().(*authenticationv1.TokenRequest)
		if len(request.Spec.Audiences) != 1 || request.Spec.Audiences[0] != connections.audience || *request.Spec.ExpirationSeconds != connectionpolicy.DefaultSeconds {
			t.Fatal("invalid token scope")
		}
		request.Status = authenticationv1.TokenRequestStatus{Token: "issued-token", ExpirationTimestamp: metav1.NewTime(time.Now().Add(20 * time.Minute))}
		return true, request, nil
	})
	ctx := context.Background()
	identity := Identity{Email: "alice@example.com"}
	grant, address, err := connections.Issue(ctx, identity, "team-a", "lab", "jupyterlab", 0)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(address)
	if parsed.Host != "connect.example.com" || parsed.Query().Get("token") != "issued-token" || time.Until(grant.ExpiresAt) > 21*time.Minute {
		t.Fatal("incorrect token URL or expiration")
	}
	account, _ := connections.accounts.Get(ctx, grant.ID, metav1.GetOptions{})
	if account.AutomountServiceAccountToken == nil || *account.AutomountServiceAccountToken || len(account.Secrets) != 0 || account.Annotations[connectionAnnotation] == "" {
		t.Fatal("grant must be tokenless and scoped")
	}
	if err := connections.Revoke(ctx, Identity{Email: "other@example.com"}, grant.ID); !errors.Is(err, ErrDenied) {
		t.Fatal("another user revoked a grant")
	}
	if err := connections.Revoke(ctx, identity, grant.ID); err != nil {
		t.Fatal(err)
	}
}

func TestConnectionRequestedLifetimes(t *testing.T) {
	for _, test := range []struct {
		name      string
		requested int64
		maximum   int64
		issued    int64
		want      int64
	}{
		{"default", 0, 0, 86400, 86400},
		{"minimum", 600, 0, 600, 600},
		{"seven days", 604800, 0, 604800, 604800},
		{"thirty days opt in", 2592000, 2592000, 2592000, 2592000},
		{"GKE shorter", 604800, 0, 172800, 172800},
		{"issuer longer", 3600, 0, 172800, 3600},
	} {
		t.Run(test.name, func(t *testing.T) {
			connections, client, existing := connectionFixture(t)
			now := time.Now().UTC().Truncate(time.Second)
			connections.now = func() time.Time { return now }
			connections.policy.MaxSeconds = test.maximum
			client.PrependReactor("create", "serviceaccounts", func(action clienttesting.Action) (bool, runtime.Object, error) {
				if action.GetSubresource() != "token" {
					account := action.(clienttesting.CreateAction).GetObject().(*corev1.ServiceAccount)
					account.UID = "lifetime-grant"
					return false, nil, nil
				}
				request := action.(clienttesting.CreateAction).GetObject().(*authenticationv1.TokenRequest)
				wantRequest := test.requested
				if wantRequest == 0 {
					wantRequest = connectionpolicy.DefaultSeconds
				}
				if *request.Spec.ExpirationSeconds != wantRequest {
					t.Fatalf("TokenRequest duration = %d, want %d", *request.Spec.ExpirationSeconds, wantRequest)
				}
				request.Status = authenticationv1.TokenRequestStatus{Token: "issued-token", ExpirationTimestamp: metav1.NewTime(now.Add(time.Duration(test.issued) * time.Second))}
				return true, request, nil
			})
			grant, _, err := connections.Issue(context.Background(), Identity{Email: "alice@example.com"}, "team-a", "lab", "jupyterlab", test.requested)
			if err != nil || !grant.ExpiresAt.Equal(now.Add(time.Duration(test.want)*time.Second)) {
				t.Fatalf("effective expiration = %v, err = %v", grant.ExpiresAt, err)
			}
			stored, err := connections.accounts.Get(context.Background(), grant.ID, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			storedGrant, err := connections.readGrant(stored)
			if err != nil || !storedGrant.ExpiresAt.Equal(grant.ExpiresAt) {
				t.Fatalf("stored expiry differs from response: %v", err)
			}
			connections.now = func() time.Time { return grant.ExpiresAt }
			if _, err := connections.readGrant(stored); !errors.Is(err, ErrDenied) {
				t.Fatal("grant accepted at its expiry")
			}
			connections.now = func() time.Time { return now }
			connections.policy = connectionpolicy.Policy{DefaultSeconds: 600, MaxSeconds: 600}
			old, _ := connections.accounts.Get(context.Background(), existing.ID, metav1.GetOptions{})
			oldGrant, err := connections.readGrant(old)
			if err != nil || !oldGrant.ExpiresAt.Equal(existing.ExpiresAt) {
				t.Fatal("new issuance policy changed an existing grant")
			}
		})
	}
}

func TestConnectionDurationAPI(t *testing.T) {
	for _, value := range []string{"-1", "599", "604801", "9223372036854775807", "2592000", "1.5", "\"86400\""} {
		connections, client, _ := connectionFixture(t)
		target, _ := url.Parse("http://unused.invalid")
		proxy, _ := NewProxy(fakeAuth{}, connections.workspaces, target, target, testPublicURL)
		proxy.EnableConnections(connections)
		client.ClearActions()
		request := httptest.NewRequest("POST", "https://example.com/workspaces/connections/api", strings.NewReader(`{"namespace":"team-a","workspace":"lab","port":"jupyterlab","durationSeconds":`+value+`}`))
		request.Header.Set(IAPHeader, "valid")
		request.Header.Set("Origin", "https://example.com")
		response := httptest.NewRecorder()
		proxy.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || len(client.Actions()) != 0 {
			t.Fatalf("invalid duration %s: status %d, Kubernetes actions %v", value, response.Code, client.Actions())
		}
	}
	connections, _, _ := connectionFixture(t)
	connections.policy = connectionpolicy.Policy{DefaultSeconds: 3600, MaxSeconds: 2592000}
	target, _ := url.Parse("http://unused.invalid")
	proxy, _ := NewProxy(fakeAuth{}, connections.workspaces, target, target, testPublicURL)
	proxy.EnableConnections(connections)
	request := httptest.NewRequest("GET", "https://example.com/workspaces/connections/api", nil)
	request.Header.Set(IAPHeader, "valid")
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	var result struct {
		DurationPolicy connectionpolicy.Policy `json:"durationPolicy"`
		Minimum        int64                   `json:"minimumDurationSeconds"`
	}
	if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.DurationPolicy != connections.policy || result.Minimum != 600 {
		t.Fatalf("incorrect public lifetime policy: %s", response.Body.String())
	}
}

func TestDesktopHTTPBoundary(t *testing.T) {
	connections, _, _ := connectionFixture(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "" || request.Header.Get(IAPHeader) != "" || request.Header.Get("Kubeflow-Userid") != "" || request.URL.Query().Has("token") || request.Header.Get("Referer") != "" || strings.Contains(request.Header.Get("Cookie"), "GCP_IAP") {
			t.Error("desktop credentials leaked to notebook")
		}
		xsrf, err := request.Cookie("_xsrf")
		if err != nil || len(xsrf.Value) != 32 || request.Header.Get("X-XSRFToken") != xsrf.Value {
			t.Error("verified desktop request lacks a matching Jupyter XSRF pair")
		}
		response.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	workspaces := &fakeWorkspaces{target: target, uid: "current"}
	connections.workspaces = workspaces
	proxy, _ := NewProxy(fakeAuth{}, workspaces, target, target, testPublicURL)
	handler := proxy.EnableConnections(connections)
	for _, scenario := range []struct {
		name, method, path, auth, origin string
		status                           int
	}{
		{"header", "POST", "/workspace/connect/team-a/lab/jupyterlab/api/kernels", "token valid", "", 200},
		{"query", "GET", "/workspace/connect/team-a/lab/jupyterlab/api/kernels?token=valid", "", "", 200},
		{"same tokens", "GET", "/workspace/connect/team-a/lab/jupyterlab/?token=valid", "token valid", "", 200},
		{"missing", "GET", "/workspace/connect/team-a/lab/jupyterlab/", "", "", 401},
		{"invalid", "GET", "/workspace/connect/team-a/lab/jupyterlab/", "token invalid", "", 403},
		{"conflicting", "GET", "/workspace/connect/team-a/lab/jupyterlab/?token=other", "token valid", "", 401},
		{"duplicates", "GET", "/workspace/connect/team-a/lab/jupyterlab/?token=valid&token=valid", "", "", 401},
		{"origin", "POST", "/workspace/connect/team-a/lab/jupyterlab/api/kernels", "token valid", "https://evil.example", 403},
		{"tenant", "GET", "/workspace/connect/team-b/lab/jupyterlab/", "token valid", "", 403},
		{"backend", "GET", "/workspaces/api/v1/namespaces", "token valid", "", 404},
		{"issuer", "POST", "/workspaces/connections/api", "token valid", "", 404},
		{"encoding", "GET", "/workspace/connect/team-a/lab/jupyterlab/%2fsecret", "token valid", "", 400},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			request := httptest.NewRequest(scenario.method, "https://connect.example.com"+scenario.path, nil)
			request.Header.Set("Authorization", scenario.auth)
			request.Header.Set("Origin", scenario.origin)
			request.Header.Set(IAPHeader, "forged")
			request.Header.Set("Cookie", "GCP_IAP_AUTH_TOKEN_test=secret")
			request.Header.Set("Referer", "https://connect.example.com/?token=valid")
			request.Header.Set("Kubeflow-Userid", "admin")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != scenario.status {
				t.Fatalf("got %d, want %d", response.Code, scenario.status)
			}
		})
	}
	request := httptest.NewRequest("GET", "/workspaces/connections/api", nil)
	request.Header.Set("Authorization", "token valid")
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	if response.Code != 401 {
		t.Fatal("desktop token bypassed browser IAP")
	}
	request = httptest.NewRequest("GET", "/workspaces/connections/api", nil)
	request.Header.Set(IAPHeader, "valid")
	response = httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	if response.Code != 200 || strings.Contains(response.Body.String(), "issued-token") {
		t.Fatal("grant listing failed or exposed a token")
	}
}

func TestDesktopWebSocketRevocation(t *testing.T) {
	connections, client, grant := connectionFixture(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Has("token") || request.Header.Get("Authorization") != "" {
			t.Error("WebSocket token leaked")
		}
		upgrader := websocket.Upgrader{}
		connection, err := upgrader.Upgrade(response, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		for {
			messageType, message, err := connection.ReadMessage()
			if err != nil {
				return
			}
			if connection.WriteMessage(messageType, message) != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	workspaces := &fakeWorkspaces{target: target, uid: "current"}
	connections.workspaces = workspaces
	proxy, _ := NewProxy(fakeAuth{}, workspaces, target, target, testPublicURL)
	handler := proxy.EnableConnections(connections).(*desktopHandler)
	handler.checkInterval = 10 * time.Millisecond
	server := httptest.NewServer(handler)
	defer server.Close()
	header := http.Header{"Host": {"connect.example.com"}}
	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/workspace/connect/team-a/lab/jupyterlab/api/kernels/test/channels?token=valid", header)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := connection.WriteMessage(websocket.TextMessage, []byte("roundtrip")); err != nil {
		t.Fatal(err)
	}
	if _, message, err := connection.ReadMessage(); err != nil || string(message) != "roundtrip" {
		t.Fatalf("WebSocket did not proxy: %v", err)
	}
	if err := client.CoreV1().ServiceAccounts(connectionNamespace).Delete(context.Background(), grant.ID, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, _, err = connection.ReadMessage()
	if err == nil || strings.Contains(err.Error(), "timeout") {
		t.Fatalf("revocation did not close active WebSocket: %v", err)
	}
}
