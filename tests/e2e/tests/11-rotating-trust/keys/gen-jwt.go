// gen-jwt generates test keys and a pre-signed JWT for rotating-trust E2E tests.
// Usage: go run gen-jwt.go
// Output:
//
//	trust.jwks  — JWKS format public key (for dex-auth-plugin --trust-jwks)
//	test.jwt    — pre-signed JWT (valid for 1 year, for static test assertions)
//	test-key.pem — RSA private key
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func main() {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}

	// Write private key
	privBytes := x509.MarshalPKCS1PrivateKey(key)
	if err := os.WriteFile("test-key.pem", pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: privBytes,
	}), 0600); err != nil {
		panic(err)
	}

	// Write JWKS public key
	jwks := map[string]interface{}{
		"keys": []map[string]interface{}{
			{
				"alg": "RS256",
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
				"kid": "test-key-001",
				"kty": "RSA",
				"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"use": "sig",
			},
		},
	}
	jwksBytes, err := json.MarshalIndent(jwks, "", "  ")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile("trust.jwks", jwksBytes, 0644); err != nil {
		panic(err)
	}

	// Generate pre-signed JWT
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss":    "https://dex.monitoring.svc:5556/dex",
		"sub":    "chainsaw-test-client",
		"aud":    "kube-apiserver",
		"exp":    time.Now().Add(365 * 24 * time.Hour).Unix(),
		"iat":    time.Now().Unix(),
		"groups": []string{"test-tenant"},
	})
	token.Header["kid"] = "test-key-001"

	tokenString, err := token.SignedString(key)
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile("test.jwt", []byte(tokenString), 0644); err != nil {
		panic(err)
	}

	fmt.Println("Generated test keys:")
	fmt.Println("  trust.jwks   — JWKS public key (Dex-compatible)")
	fmt.Println("  test.jwt     — Pre-signed JWT (1yr validity)")
	fmt.Println("  test-key.pem — RSA 2048 private key")
}
