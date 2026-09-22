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
	"bufio"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kubeflow/notebooks/gke/internal/connectionpolicy"
	"golang.org/x/time/rate"
)

//go:embed connections.html
var connectionsPage string

func (proxy *Proxy) EnableConnections(connections *Connections) http.Handler {
	proxy.connections = connections
	proxy.issueLimit = rate.NewLimiter(rate.Every(time.Second), 5)
	publicURL, _ := url.Parse(connections.origin)
	return &desktopHandler{connections: connections, proxy: &Proxy{publicHost: publicURL.Host, publicOrigin: connections.origin, transport: proxy.transport}, limit: rate.NewLimiter(50, 100), checkInterval: 15 * time.Second}
}

func (proxy *Proxy) connectionManagement(response http.ResponseWriter, request *http.Request, identity Identity) {
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Referrer-Policy", "no-referrer")
	response.Header().Set("X-Content-Type-Options", "nosniff")
	if request.URL.Path == "/workspaces/connections" && request.Method == http.MethodGet {
		response.Header().Set("Content-Type", "text/html; charset=utf-8")
		response.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		_, _ = io.WriteString(response, connectionsPage)
		return
	}
	if request.URL.Path == "/workspaces/connections/client.js" && request.Method == http.MethodGet {
		response.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = io.WriteString(response, connectionsScript)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	response.Header().Set("Content-Type", "application/json")
	api := "/workspaces/connections/api"
	switch {
	case request.URL.Path == api && request.Method == http.MethodGet:
		grants, err := proxy.connections.List(ctx, identity)
		if err != nil {
			accessError(response, err)
			return
		}
		policy, err := proxy.connections.policy.Resolve()
		if err != nil {
			accessError(response, err)
			return
		}
		_ = json.NewEncoder(response).Encode(map[string]any{"user": identity.Email, "grants": grants, "durationPolicy": policy, "minimumDurationSeconds": connectionpolicy.MinimumSeconds})
	case request.URL.Path == api && request.Method == http.MethodPost:
		if !proxy.issueLimit.Allow() {
			http.Error(response, "too many connection requests", http.StatusTooManyRequests)
			return
		}
		var input struct {
			Namespace       string `json:"namespace"`
			Workspace       string `json:"workspace"`
			Port            string `json:"port"`
			DurationSeconds int64  `json:"durationSeconds"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(response, request.Body, 4096))
		decoder.DisallowUnknownFields()
		var extra any
		if decoder.Decode(&input) != nil || decoder.Decode(&extra) != io.EOF {
			http.Error(response, "invalid request", http.StatusBadRequest)
			return
		}
		grant, address, err := proxy.connections.Issue(ctx, identity, input.Namespace, input.Workspace, input.Port, input.DurationSeconds)
		if err != nil {
			if errors.Is(err, connectionpolicy.ErrDuration) {
				http.Error(response, err.Error(), http.StatusBadRequest)
				return
			}
			accessError(response, err)
			return
		}
		response.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(response).Encode(map[string]any{"grant": grant, "url": address})
	case strings.HasPrefix(request.URL.Path, api+"/") && request.Method == http.MethodDelete:
		if err := proxy.connections.Revoke(ctx, identity, strings.TrimPrefix(request.URL.Path, api+"/")); err != nil {
			accessError(response, err)
			return
		}
		response.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(response, request)
	}
}

type desktopHandler struct {
	connections   *Connections
	proxy         *Proxy
	limit         *rate.Limiter
	checkInterval time.Duration
}

func desktopToken(request *http.Request) (string, error) {
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil || len(query["token"]) > 1 || len(request.Header.Values("Authorization")) > 1 {
		return "", ErrDenied
	}
	token := query.Get("token")
	if authorization := request.Header.Get("Authorization"); authorization != "" {
		parts := strings.SplitN(authorization, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "token") || (token != "" && token != parts[1]) {
			return "", ErrDenied
		}
		token = parts[1]
	}
	if token == "" || len(token) > 8192 || strings.ContainsAny(token, " \t\r\n") {
		return "", ErrDenied
	}
	query.Del("token")
	request.URL.RawQuery = query.Encode()
	request.RequestURI = request.URL.RequestURI()
	request.Header.Del("Referer")
	return token, nil
}

func (handler *desktopHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Referrer-Policy", "no-referrer")
	if request.URL.Path == "/healthz" && request.Method == http.MethodGet {
		response.WriteHeader(http.StatusOK)
		return
	}
	if request.Host != handler.proxy.publicHost {
		http.Error(response, "unknown host", http.StatusMisdirectedRequest)
		return
	}
	if origin := request.Header.Get("Origin"); origin != "" && origin != handler.proxy.publicOrigin {
		http.Error(response, "origin denied", http.StatusForbidden)
		return
	}
	if !safePath(request.URL) {
		http.Error(response, "invalid path", http.StatusBadRequest)
		return
	}
	parts := strings.SplitN(request.URL.Path, "/", 7)
	if len(parts) != 7 || parts[1] != "workspace" || parts[2] != "connect" || !validName(parts[3]) || !validName(parts[4]) || !validName(parts[5]) {
		http.NotFound(response, request)
		return
	}
	if !handler.limit.Allow() {
		http.Error(response, "too many requests", http.StatusTooManyRequests)
		return
	}
	token, err := desktopToken(request)
	if err != nil {
		http.Error(response, "connection token required", http.StatusUnauthorized)
		return
	}
	check := func() (ConnectionGrant, Target, error) {
		ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
		defer cancel()
		return handler.connections.Authorize(ctx, token, parts[3], parts[4], parts[5])
	}
	grant, target, err := check()
	if err != nil {
		accessError(response, err)
		return
	}
	if !validTarget(target.URL) {
		http.Error(response, "invalid target", http.StatusServiceUnavailable)
		return
	}
	var xsrf [16]byte
	if _, err := rand.Read(xsrf[:]); err != nil {
		http.Error(response, "request protection unavailable", http.StatusServiceUnavailable)
		return
	}
	cookies := request.Cookies()
	request.Header.Del("Cookie")
	for _, cookie := range cookies {
		if cookie.Name != "_xsrf" {
			request.AddCookie(cookie)
		}
	}
	request.AddCookie(&http.Cookie{Name: "_xsrf", Value: hex.EncodeToString(xsrf[:])})
	request.Header.Set("X-XSRFToken", hex.EncodeToString(xsrf[:]))
	ctx, cancel := context.WithDeadline(request.Context(), grant.ExpiresAt)
	defer cancel()
	request = request.WithContext(ctx)
	path := request.URL.Path
	if target.RemovePrefix {
		path = "/" + parts[6]
	}
	guard := &connectionWriter{ResponseWriter: response, expires: grant.ExpiresAt, checkInterval: handler.checkInterval, check: func() bool {
		_, current, err := check()
		return err == nil && validTarget(current.URL) && current.WorkspaceUID == target.WorkspaceUID && current.URL.String() == target.URL.String()
	}, done: make(chan struct{})}
	defer close(guard.done)
	handler.proxy.forward(guard, request, target.URL, path, "")
}

type connectionWriter struct {
	http.ResponseWriter
	expires       time.Time
	check         func() bool
	checkInterval time.Duration
	done          chan struct{}
}

func (writer *connectionWriter) Unwrap() http.ResponseWriter { return writer.ResponseWriter }

func (writer *connectionWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := writer.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("websocket upgrade unavailable")
	}
	connection, buffer, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, err
	}
	_ = connection.SetDeadline(writer.expires)
	go func() {
		ticker := time.NewTicker(writer.checkInterval)
		defer ticker.Stop()
		for {
			select {
			case <-writer.done:
				return
			case <-ticker.C:
				if !time.Now().Before(writer.expires) || !writer.check() {
					_ = connection.Close()
					return
				}
			}
		}
	}()
	return connection, buffer, nil
}

//go:embed connections.js
var connectionsScript string
