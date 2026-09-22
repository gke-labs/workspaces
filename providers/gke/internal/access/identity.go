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
	"fmt"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

const (
	IAPHeader  = "X-Goog-Iap-Jwt-Assertion"
	iapIssuer  = "https://cloud.google.com/iap"
	iapKeysURL = "https://www.gstatic.com/iap/verify/public_key-jwk"
)

type Identity struct {
	Email   string
	Subject string
}

type Authenticator interface {
	Authenticate(context.Context, string) (Identity, error)
}

type cachedIdentity struct {
	identity  Identity
	expiresAt int64
}

type IAPAuthenticator struct {
	verifier *oidc.IDTokenVerifier
	audience string
	mu       sync.RWMutex
	cache    map[string]cachedIdentity
}

func NewIAPAuthenticator(ctx context.Context, audience string) (*IAPAuthenticator, error) {
	return newIAPAuthenticator(audience, oidc.NewRemoteKeySet(ctx, iapKeysURL))
}

func newIAPAuthenticator(audience string, keys oidc.KeySet) (*IAPAuthenticator, error) {
	parts := strings.Split(audience, "/")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "projects" || !decimal(parts[2]) || parts[3] != "global" || parts[4] != "backendServices" || !decimal(parts[5]) {
		return nil, fmt.Errorf("IAP audience must identify one global backend service by project number and service ID")
	}
	return &IAPAuthenticator{
		verifier: oidc.NewVerifier(iapIssuer, keys, &oidc.Config{
			ClientID:             audience,
			SupportedSigningAlgs: []string{"ES256"},
		}),
		audience: audience,
		cache:    make(map[string]cachedIdentity),
	}, nil
}

func (auth *IAPAuthenticator) Authenticate(ctx context.Context, assertion string) (Identity, error) {
	now := time.Now().Unix()
	auth.mu.RLock()
	if cached, ok := auth.cache[assertion]; ok && now < cached.expiresAt {
		auth.mu.RUnlock()
		return cached.identity, nil
	}
	auth.mu.RUnlock()

	token, err := auth.verifier.Verify(ctx, assertion)
	if err != nil {
		return Identity{}, fmt.Errorf("invalid IAP assertion: %w", err)
	}
	var claims struct {
		Audience  string `json:"aud"`
		Email     string `json:"email"`
		IssuedAt  int64  `json:"iat"`
		ExpiresAt int64  `json:"exp"`
	}
	if err := token.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("invalid IAP claims: %w", err)
	}
	if claims.Audience != auth.audience || claims.IssuedAt <= 0 || claims.IssuedAt > now+300 || claims.ExpiresAt <= claims.IssuedAt || claims.ExpiresAt-claims.IssuedAt > 86400 {
		return Identity{}, fmt.Errorf("invalid IAP audience or token lifetime (iat=%d exp=%d now=%d)", claims.IssuedAt, claims.ExpiresAt, now)
	}
	address, err := mail.ParseAddress(claims.Email)
	if err != nil || address.Address != claims.Email || strings.Contains(claims.Email, ":") || token.Subject == "" {
		return Identity{}, fmt.Errorf("IAP assertion must contain a subject and a Google account email")
	}
	identity := Identity{Email: claims.Email, Subject: token.Subject}
	exp := claims.ExpiresAt
	if now+60 < exp {
		exp = now + 60
	}
	auth.mu.Lock()
	if len(auth.cache) > 1024 {
		auth.cache = make(map[string]cachedIdentity)
	}
	auth.cache[assertion] = cachedIdentity{identity: identity, expiresAt: exp}
	auth.mu.Unlock()
	return identity, nil
}

func decimal(value string) bool {
	if value == "" {
		return false
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}
