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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/kubeflow/notebooks/gke/internal/connectionpolicy"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authenticationclient "k8s.io/client-go/kubernetes/typed/authentication/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

const connectionLabel = "notebooks.kubeflow.org/desktop-connection"
const connectionAnnotation = "notebooks.kubeflow.org/connection"
const connectionNamespace = "notebooks-connections"

type ConnectionGrant struct {
	ID           string    `json:"id"`
	User         string    `json:"user"`
	Namespace    string    `json:"namespace"`
	Workspace    string    `json:"workspace"`
	WorkspaceUID string    `json:"workspaceUID"`
	Port         string    `json:"port"`
	ExpiresAt    time.Time `json:"expiresAt"`
}

type Connections struct {
	accounts   coreclient.ServiceAccountInterface
	auth       authenticationclient.AuthenticationV1Interface
	workspaces WorkspaceAccess
	origin     string
	audience   string
	now        func() time.Time
	policy     connectionpolicy.Policy
}

func NewConnections(config *rest.Config, workspaces WorkspaceAccess, publicURL *url.URL, policy connectionpolicy.Policy) (*Connections, error) {
	if !validTarget(publicURL) || publicURL.Scheme != "https" || workspaces == nil {
		return nil, fmt.Errorf("desktop URL must be an HTTPS origin")
	}
	policy, err := policy.Resolve()
	if err != nil {
		return nil, err
	}
	core, err := coreclient.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	auth, err := authenticationclient.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	origin := "https://" + publicURL.Host
	return &Connections{accounts: core.ServiceAccounts(connectionNamespace), auth: auth, workspaces: workspaces, origin: origin, audience: origin + "/desktop", now: time.Now, policy: policy}, nil
}

func (connections *Connections) Issue(ctx context.Context, identity Identity, namespace, workspace, port string, durationSeconds int64) (ConnectionGrant, string, error) {
	lifetime, err := connections.policy.Duration(durationSeconds)
	if err != nil {
		return ConnectionGrant{}, "", err
	}
	if !validName(namespace) || !validName(workspace) || !validName(port) {
		return ConnectionGrant{}, "", ErrDenied
	}
	target, err := connections.workspaces.Resolve(ctx, identity, namespace, workspace, port)
	if err != nil {
		return ConnectionGrant{}, "", err
	}
	if target.WorkspaceUID == "" {
		return ConnectionGrant{}, "", fmt.Errorf("workspace UID required")
	}
	grants, err := connections.List(ctx, identity)
	if err != nil {
		return ConnectionGrant{}, "", err
	}
	if len(grants) >= 10 {
		return ConnectionGrant{}, "", fmt.Errorf("revoke an existing connection first")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return ConnectionGrant{}, "", err
	}
	grant := ConnectionGrant{ID: "connection-" + hex.EncodeToString(random[:]), User: identity.Email, Namespace: namespace, Workspace: workspace, WorkspaceUID: target.WorkspaceUID, Port: port, ExpiresAt: connections.now().Add(lifetime).UTC().Truncate(time.Second)}
	encoded, err := json.Marshal(grant)
	if err != nil {
		return ConnectionGrant{}, "", err
	}
	automount := false
	account, err := connections.accounts.Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: grant.ID, Labels: map[string]string{connectionLabel: "true"}, Annotations: map[string]string{connectionAnnotation: string(encoded)}}, AutomountServiceAccountToken: &automount}, metav1.CreateOptions{})
	if err != nil {
		return ConnectionGrant{}, "", err
	}
	expiration := int64(lifetime.Seconds())
	token, err := connections.accounts.CreateToken(ctx, account.Name, &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{Audiences: []string{connections.audience}, ExpirationSeconds: &expiration}}, metav1.CreateOptions{})
	if err != nil || token.Status.Token == "" || !token.Status.ExpirationTimestamp.After(connections.now()) {
		connections.discard(account)
		return ConnectionGrant{}, "", fmt.Errorf("connection token issuance failed")
	}
	if token.Status.ExpirationTimestamp.Before(&metav1.Time{Time: grant.ExpiresAt}) {
		grant.ExpiresAt = token.Status.ExpirationTimestamp.Time
		encoded, _ = json.Marshal(grant)
		account.Annotations[connectionAnnotation] = string(encoded)
		if _, err := connections.accounts.Update(ctx, account, metav1.UpdateOptions{}); err != nil {
			connections.discard(account)
			return ConnectionGrant{}, "", err
		}
	}
	address := connections.origin + "/workspace/connect/" + namespace + "/" + workspace + "/" + port + "/?" + url.Values{"token": {token.Status.Token}}.Encode()
	return grant, address, nil
}

func (connections *Connections) discard(account *corev1.ServiceAccount) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = connections.accounts.Delete(ctx, account.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &account.UID}})
}

func (connections *Connections) readGrant(account *corev1.ServiceAccount) (ConnectionGrant, error) {
	var grant ConnectionGrant
	if account.UID == "" || account.DeletionTimestamp != nil || account.Labels[connectionLabel] != "true" || !strings.HasPrefix(account.Name, "connection-") {
		return grant, ErrDenied
	}
	if err := json.Unmarshal([]byte(account.Annotations[connectionAnnotation]), &grant); err != nil {
		return grant, ErrDenied
	}
	address, err := mail.ParseAddress(grant.User)
	if err != nil || address.Address != grant.User || strings.Contains(grant.User, ":") || grant.ID != account.Name || grant.WorkspaceUID == "" || !validName(grant.Namespace) || !validName(grant.Workspace) || !validName(grant.Port) || !connections.now().Before(grant.ExpiresAt) {
		return grant, ErrDenied
	}
	return grant, nil
}

func (connections *Connections) Authorize(ctx context.Context, token, namespace, workspace, port string) (ConnectionGrant, Target, error) {
	review, err := connections.auth.TokenReviews().Create(ctx, &authenticationv1.TokenReview{Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: []string{connections.audience}}}, metav1.CreateOptions{})
	if err != nil {
		return ConnectionGrant{}, Target{}, fmt.Errorf("token verification unavailable")
	}
	if !review.Status.Authenticated || review.Status.Error != "" || len(review.Status.Audiences) != 1 || review.Status.Audiences[0] != connections.audience || review.Status.User.UID == "" {
		return ConnectionGrant{}, Target{}, ErrDenied
	}
	prefix := "system:serviceaccount:" + connectionNamespace + ":"
	if !strings.HasPrefix(review.Status.User.Username, prefix) {
		return ConnectionGrant{}, Target{}, ErrDenied
	}
	name := strings.TrimPrefix(review.Status.User.Username, prefix)
	account, err := connections.accounts.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return ConnectionGrant{}, Target{}, ErrDenied
	}
	if err != nil {
		return ConnectionGrant{}, Target{}, fmt.Errorf("grant lookup unavailable")
	}
	if string(account.UID) != review.Status.User.UID {
		return ConnectionGrant{}, Target{}, ErrDenied
	}
	grant, err := connections.readGrant(account)
	if err != nil || grant.Namespace != namespace || grant.Workspace != workspace || grant.Port != port {
		return ConnectionGrant{}, Target{}, ErrDenied
	}
	target, err := connections.workspaces.Resolve(ctx, Identity{Email: grant.User}, namespace, workspace, port)
	if err != nil {
		return ConnectionGrant{}, Target{}, err
	}
	if target.WorkspaceUID != grant.WorkspaceUID {
		return ConnectionGrant{}, Target{}, ErrDenied
	}
	return grant, target, nil
}

func (connections *Connections) List(ctx context.Context, identity Identity) ([]ConnectionGrant, error) {
	accounts, err := connections.accounts.List(ctx, metav1.ListOptions{LabelSelector: connectionLabel + "=true", Limit: 1000})
	if err != nil || accounts == nil || accounts.Continue != "" {
		return nil, fmt.Errorf("grant listing unavailable")
	}
	grants := []ConnectionGrant{}
	for index := range accounts.Items {
		grant, err := connections.readGrant(&accounts.Items[index])
		if err == nil && grant.User == identity.Email {
			grants = append(grants, grant)
		}
	}
	return grants, nil
}

func (connections *Connections) Revoke(ctx context.Context, identity Identity, name string) error {
	if !validName(name) || !strings.HasPrefix(name, "connection-") {
		return ErrDenied
	}
	account, err := connections.accounts.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return ErrDenied
	}
	if err != nil {
		return err
	}
	var grant ConnectionGrant
	if json.Unmarshal([]byte(account.Annotations[connectionAnnotation]), &grant) != nil || grant.User != identity.Email || account.Labels[connectionLabel] != "true" {
		return ErrDenied
	}
	return connections.accounts.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &account.UID}})
}

func (connections *Connections) Cleanup(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		accounts, err := connections.accounts.List(requestCtx, metav1.ListOptions{LabelSelector: connectionLabel + "=true", Limit: 1000})
		if err == nil {
			for index := range accounts.Items {
				account := &accounts.Items[index]
				if _, err := connections.readGrant(account); err != nil {
					_ = connections.accounts.Delete(requestCtx, account.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &account.UID}})
				}
			}
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
