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
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

var ErrDenied = errors.New("access denied")

var resourceName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

type Namespace struct {
	Name string `json:"name"`
}

type Target struct {
	URL          *url.URL
	RemovePrefix bool
	WorkspaceUID string
}

type WorkspaceAccess interface {
	Namespaces(context.Context, Identity) ([]Namespace, error)
	Resolve(context.Context, Identity, string, string, string) (Target, error)
}

type Proxy struct {
	auth         Authenticator
	workspaces   WorkspaceAccess
	frontend     *url.URL
	backend      *url.URL
	publicOrigin string
	publicHost   string
	transport    http.RoundTripper
	connections  *Connections
	issueLimit   *rate.Limiter
}

func NewProxy(auth Authenticator, workspaces WorkspaceAccess, frontend, backend, publicURL *url.URL) (*Proxy, error) {
	if auth == nil || workspaces == nil || !validTarget(frontend) || !validTarget(backend) || !validTarget(publicURL) || publicURL.Scheme != "https" {
		return nil, fmt.Errorf("authenticator, workspace access, and HTTP upstream origins are required")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxIdleConns = 256
	transport.MaxIdleConnsPerHost = 64
	transport.IdleConnTimeout = 300 * time.Second
	transport.ResponseHeaderTimeout = 120 * time.Second
	return &Proxy{auth: auth, workspaces: workspaces, frontend: frontend, backend: backend, publicOrigin: "https://" + publicURL.Host, publicHost: publicURL.Host, transport: transport}, nil
}

func validTarget(target *url.URL) bool {
	return target != nil && (target.Scheme == "http" || target.Scheme == "https") && target.Host != "" && target.User == nil && target.RawQuery == "" && target.Fragment == "" && (target.Path == "" || target.Path == "/")
}

func (proxy *Proxy) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/healthz" && request.Method == http.MethodGet {
		response.WriteHeader(http.StatusOK)
		return
	}
	if request.Host != proxy.publicHost {
		log.Printf("rejected request for unknown host %q (expected %q) on path %s", request.Host, proxy.publicHost, request.URL.Path)
		http.Error(response, "unknown host", http.StatusMisdirectedRequest)
		return
	}
	origin := request.Header.Get("Origin")
	unsafe := request.Method != http.MethodGet && request.Method != http.MethodHead && request.Method != http.MethodOptions
	upgrade := strings.EqualFold(request.Header.Get("Upgrade"), "websocket")
	if (origin != "" || unsafe || upgrade) && origin != proxy.publicOrigin {
		log.Printf("rejected origin %q (expected %q) for %s %s", origin, proxy.publicOrigin, request.Method, request.URL.Path)
		http.Error(response, "origin denied", http.StatusForbidden)
		return
	}
	assertions := request.Header.Values(IAPHeader)
	if len(assertions) != 1 || assertions[0] == "" {
		log.Printf("missing or multiple IAP assertions (%d) for %s %s", len(assertions), request.Method, request.URL.Path)
		http.Error(response, "authentication required", http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	identity, err := proxy.auth.Authenticate(ctx, assertions[0])
	cancel()
	if err != nil {
		log.Printf("IAP authentication failed for %s %s: %v", request.Method, request.URL.Path, err)
		http.Error(response, "invalid identity", http.StatusUnauthorized)
		return
	}
	if !safePath(request.URL) {
		log.Printf("rejected unsafe path %q", request.URL.Path)
		http.Error(response, "invalid path", http.StatusBadRequest)
		return
	}
	path := request.URL.Path
	if proxy.connections != nil && (path == "/workspaces/connections" || strings.HasPrefix(path, "/workspaces/connections/")) {
		proxy.connectionManagement(response, request, identity)
		return
	}
	if path == "/" {
		http.Redirect(response, request, "/workspaces/", http.StatusFound)
		return
	}
	if path == "/workspaces/api/v1/namespaces" {
		if request.Method != http.MethodGet {
			response.Header().Set("Allow", "GET")
			http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
		defer cancel()
		namespaces, err := proxy.workspaces.Namespaces(ctx, identity)
		if err != nil {
			accessError(response, err)
			return
		}
		if namespaces == nil {
			namespaces = []Namespace{}
		}
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(response).Encode(struct {
			Data []Namespace `json:"data"`
		}{namespaces})
		return
	}
	if strings.HasPrefix(path, "/workspaces/api/") {
		proxy.forward(response, request, proxy.backend, strings.TrimPrefix(path, "/workspaces"), identity.Email)
		return
	}
	if strings.HasPrefix(path, "/workspace/") {
		parts := strings.SplitN(path, "/", 7)
		if len(parts) != 7 || parts[1] != "workspace" || parts[2] != "connect" || !validName(parts[3]) || !validName(parts[4]) || !validName(parts[5]) {
			http.Error(response, "unknown workspace path", http.StatusNotFound)
			return
		}
		ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
		target, err := proxy.workspaces.Resolve(ctx, identity, parts[3], parts[4], parts[5])
		cancel()
		if err != nil {
			accessError(response, err)
			return
		}
		if !validTarget(target.URL) {
			accessError(response, fmt.Errorf("invalid workspace target"))
			return
		}
		if target.RemovePrefix {
			path = "/" + parts[6]
		}
		proxy.forward(response, request, target.URL, path, "")
		return
	}
	proxy.forward(response, request, proxy.frontend, path, "")
}

func validName(name string) bool {
	return len(name) <= 253 && resourceName.MatchString(name) && !strings.Contains(name, "..")
}

func safePath(address *url.URL) bool {
	if strings.ContainsAny(address.Path, "\\\x00\r\n%") || strings.Contains(address.Path, "//") {
		return false
	}
	for _, segment := range strings.Split(address.Path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	escaped := strings.ToLower(address.EscapedPath())
	return !strings.Contains(escaped, "%2f") && !strings.Contains(escaped, "%5c")
}

func accessError(response http.ResponseWriter, err error) {
	if errors.Is(err, ErrDenied) {
		http.Error(response, "access denied", http.StatusForbidden)
		return
	}
	log.Printf("workspace access error: %v", err)
	http.Error(response, "workspace access unavailable", http.StatusServiceUnavailable)
}

func stripCredentials(headers http.Header) {
	for name := range headers {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "kubeflow-") || strings.HasPrefix(lower, "x-goog-") || strings.HasPrefix(lower, "x-auth-request-") || strings.HasPrefix(lower, "x-forwarded-") || strings.HasPrefix(lower, "impersonate-") || lower == "authorization" || lower == "proxy-authorization" || lower == "forwarded" {
			headers.Del(name)
		}
	}
	cookies := (&http.Request{Header: headers}).Cookies()
	headers.Del("Cookie")
	for _, cookie := range cookies {
		if strings.HasPrefix(cookie.Name, "GCP_IAP_AUTH_TOKEN") {
			continue
		}
		(&http.Request{Header: headers}).AddCookie(cookie)
	}
}

func (proxy *Proxy) forward(response http.ResponseWriter, request *http.Request, target *url.URL, path, email string) {
	reverse := &httputil.ReverseProxy{
		Transport:     proxy.transport,
		FlushInterval: -1,
		Rewrite: func(outbound *httputil.ProxyRequest) {
			outbound.SetURL(target)
			outbound.Out.URL.Path = path
			outbound.Out.URL.RawPath = ""
			outbound.Out.Host = outbound.In.Host
			stripCredentials(outbound.Out.Header)
			outbound.Out.Header.Set("X-Forwarded-Proto", "https")
			outbound.Out.Header.Set("X-Forwarded-Host", proxy.publicHost)
			if email != "" {
				outbound.Out.Header.Set("Kubeflow-Userid", email)
			}
		},
		ErrorHandler: func(response http.ResponseWriter, request *http.Request, err error) {
			log.Printf("upstream proxy error for %s %s -> %s: %v", request.Method, request.URL.Path, target.String(), err)
			http.Error(response, "upstream unavailable", http.StatusBadGateway)
		},
	}
	reverse.ServeHTTP(response, request)
}
