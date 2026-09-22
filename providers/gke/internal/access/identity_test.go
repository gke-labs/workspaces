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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
)

const testAudience = "/projects/123/global/backendServices/456"

func signingKey(t *testing.T, name string) jose.JSONWebKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return jose.JSONWebKey{Key: key, KeyID: name, Algorithm: "ES256", Use: "sig"}
}

func signClaims(t *testing.T, key jose.JSONWebKey, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	serialized, err := signed.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return serialized
}

func validClaims() map[string]any {
	now := time.Now().Unix()
	return map[string]any{"iss": iapIssuer, "aud": testAudience, "iat": now - 10, "exp": now + 300, "sub": "accounts.google.com:123", "email": "user@example.com"}
}

func TestIAPAuthentication(t *testing.T) {
	key := signingKey(t, "first")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(response).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{key.Public()}})
	}))
	defer server.Close()
	auth, err := newIAPAuthenticator(testAudience, oidc.NewRemoteKeySet(context.Background(), server.URL))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		change  func(map[string]any)
		allowed bool
	}{
		{"valid", func(claims map[string]any) {}, true},
		{"wrong audience", func(claims map[string]any) { claims["aud"] = "/projects/123/global/backendServices/789" }, false},
		{"audience array", func(claims map[string]any) { claims["aud"] = []string{testAudience} }, false},
		{"wrong issuer", func(claims map[string]any) { claims["iss"] = "https://accounts.google.com" }, false},
		{"expired", func(claims map[string]any) { claims["exp"] = time.Now().Unix() - 60 }, false},
		{"future issuance", func(claims map[string]any) { claims["iat"] = time.Now().Unix() + 600 }, false},
		{"missing issuance", func(claims map[string]any) { delete(claims, "iat") }, false},
		{"missing expiry", func(claims map[string]any) { delete(claims, "exp") }, false},
		{"excessive lifetime", func(claims map[string]any) { claims["exp"] = time.Now().Unix() + 90000 }, false},
		{"missing subject", func(claims map[string]any) { delete(claims, "sub") }, false},
		{"missing email", func(claims map[string]any) { delete(claims, "email") }, false},
		{"external identity", func(claims map[string]any) { claims["email"] = "provider:tenant:user@example.com" }, false},
		{"header injection", func(claims map[string]any) { claims["email"] = "user@example.com\r\nX-Evil: true" }, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			claims := validClaims()
			test.change(claims)
			identity, err := auth.Authenticate(context.Background(), signClaims(t, key, claims))
			if (err == nil) != test.allowed {
				t.Fatalf("allowed=%v, err=%v", test.allowed, err)
			}
			if test.allowed && identity.Email != "user@example.com" {
				t.Fatalf("unexpected identity: %+v", identity)
			}
		})
	}
	for _, assertion := range []string{"", "not-a-token", signClaims(t, signingKey(t, "forged"), validClaims())} {
		if _, err := auth.Authenticate(context.Background(), assertion); err == nil {
			t.Fatal("invalid signature accepted")
		}
	}
}

func TestIAPKeyRotation(t *testing.T) {
	key := signingKey(t, "first")
	var mutex sync.Mutex
	public := key.Public()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		_ = json.NewEncoder(response).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{public}})
	}))
	defer server.Close()
	auth, err := newIAPAuthenticator(testAudience, oidc.NewRemoteKeySet(context.Background(), server.URL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Authenticate(context.Background(), signClaims(t, key, validClaims())); err != nil {
		t.Fatal(err)
	}
	key = signingKey(t, "second")
	mutex.Lock()
	public = key.Public()
	mutex.Unlock()
	if _, err := auth.Authenticate(context.Background(), signClaims(t, key, validClaims())); err != nil {
		t.Fatal(err)
	}
}

func TestIAPAudienceConfiguration(t *testing.T) {
	for _, audience := range []string{"", "*", "/projects/123", "/projects/name/global/backendServices/456", "/projects/123/global/backendServices/*"} {
		if _, err := NewIAPAuthenticator(context.Background(), audience); err == nil {
			t.Fatalf("accepted %q", audience)
		}
	}
}
