package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"time"
)

func main() {
	privPEM, _ := os.ReadFile("test-key.pem")
	block, _ := pem.Decode(privPEM)
	priv, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		panic(err)
	}
	rsaPriv := priv.(*rsa.PrivateKey)
	pub := &rsaPriv.PublicKey

	pubDER, _ := x509.MarshalPKIXPublicKey(pub)
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	os.WriteFile("test-key.pub", pubPEM, 0644)

	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())

	jwks := map[string]interface{}{
		"keys": []map[string]interface{}{
			{
				"kty": "RSA",
				"kid": "test-key-001",
				"use": "sig",
				"alg": "RS256",
				"n":   n,
				"e":   e,
			},
		},
	}
	jwksJSON, _ := json.MarshalIndent(jwks, "", "  ")
	os.WriteFile("trust.jwks", jwksJSON, 0644)

	now := time.Now()
	header := map[string]string{"alg": "RS256", "typ": "JWT", "kid": "test-key-001"}
	claims := map[string]interface{}{
		"iss": "https://bm4080.taildf7067.ts.net/dex",
		"sub": "widget-controller",
		"aud": "test",
		"iat": now.Unix(),
		"exp": now.Add(365 * 24 * time.Hour).Unix(),
	}

	headerJSON, _ := json.Marshal(header)
	claimsJSON, _ := json.Marshal(claims)
	h := base64.RawURLEncoding.EncodeToString(headerJSON)
	c := base64.RawURLEncoding.EncodeToString(claimsJSON)
	signingInput := h + "." + c
	hash := sha256.Sum256([]byte(signingInput))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, rsaPriv, crypto.SHA256, hash[:])
	s := base64.RawURLEncoding.EncodeToString(sig)
	jwt := signingInput + "." + s
	os.WriteFile("test.jwt", []byte(jwt), 0644)

	fmt.Printf("JWT: %s\nJWKS: %s\n", jwt, string(jwksJSON))
}
