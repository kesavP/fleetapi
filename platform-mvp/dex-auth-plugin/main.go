// dex-auth-plugin is a client-go exec credential plugin that authenticates
// to Dex via client_credentials OAuth2 grant and outputs an ExecCredential
// on stdout for use with Kubernetes client-go's native credential rotation.
//
// Protocol: client.authentication.k8s.io/v1 ExecCredential
// Input:    DEX_ISSUER, DEX_CLIENT_ID, DEX_CLIENT_SECRET (env vars)
// Output:   ExecCredential JSON to stdout
//
// client-go caches the token and re-invokes this binary before expiry.
// The binary is stateless between invocations.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const apiVersion = "client.authentication.k8s.io/v1"
const kind = "ExecCredential"

type ExecCredential struct {
	APIVersion string               `json:"apiVersion"`
	Kind       string               `json:"kind"`
	Status     ExecCredentialStatus `json:"status"`
}

type ExecCredentialStatus struct {
	Token               string `json:"token"`
	ExpirationTimestamp string `json:"expirationTimestamp"`
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope"`
}

type config struct {
	issuer       string
	clientID     string
	clientSecret string
}

func main() {
	cfg := config{
		issuer:       os.Getenv("DEX_ISSUER"),
		clientID:     os.Getenv("DEX_CLIENT_ID"),
		clientSecret: os.Getenv("DEX_CLIENT_SECRET"),
	}
	os.Exit(run(os.Stdout, os.Stderr, cfg, http.DefaultClient))
}

// run is the testable entry point. It fetches a token from Dex and writes
// ExecCredential JSON to out. Returns 0 on success, 1 on failure.
func run(out, errOut io.Writer, cfg config, client interface {
	Do(req *http.Request) (*http.Response, error)
}) int {
	if cfg.issuer == "" || cfg.clientID == "" || cfg.clientSecret == "" {
		fmt.Fprintf(errOut, "AUTH_ERROR: missing required env vars (DEX_ISSUER, DEX_CLIENT_ID, DEX_CLIENT_SECRET)\n")
		return 1
	}

	tokenURL := strings.TrimRight(cfg.issuer, "/") + "/token"

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", cfg.clientID)
	form.Set("client_secret", cfg.clientSecret)
	form.Set("scope", "openid")

	req, err := http.NewRequest("POST", tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		fmt.Fprintf(errOut, "AUTH_ERROR: failed to create request: %v\n", err)
		return 1
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(errOut, "AUTH_ERROR: token request failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		fmt.Fprintf(errOut, "AUTH_ERROR: IDP unavailable (status %d)\n", resp.StatusCode)
		return 1
	}
	if resp.StatusCode >= 400 {
		fmt.Fprintf(errOut, "AUTH_ERROR: client not authorized (status %d)\n", resp.StatusCode)
		return 1
	}

	var tokenResp tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		fmt.Fprintf(errOut, "AUTH_ERROR: malformed response: %v\n", err)
		return 1
	}

	if tokenResp.AccessToken == "" {
		fmt.Fprintf(errOut, "AUTH_ERROR: no token in response\n")
		return 1
	}

	cred := buildCredential(tokenResp.AccessToken, tokenResp.ExpiresIn)
	if err := json.NewEncoder(out).Encode(cred); err != nil {
		fmt.Fprintf(errOut, "AUTH_ERROR: failed to encode credential: %v\n", err)
		return 1
	}

	return 0
}

func buildCredential(token string, expiresIn int) ExecCredential {
	expiry := time.Now().Add(time.Duration(expiresIn) * time.Second)
	if expiresIn > 60 {
		expiry = time.Now().Add(time.Duration(expiresIn-30) * time.Second)
	}
	return ExecCredential{
		APIVersion: apiVersion,
		Kind:       kind,
		Status: ExecCredentialStatus{
			Token:               token,
			ExpirationTimestamp: expiry.Format(time.RFC3339),
		},
	}
}
