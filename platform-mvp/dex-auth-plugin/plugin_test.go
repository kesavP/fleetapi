package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// stubHTTPClient records the request and returns a configurable response.
type stubHTTPClient struct {
	lastReq    *http.Request
	statusCode int
	body       string
}

func (s *stubHTTPClient) Do(req *http.Request) (*http.Response, error) {
	s.lastReq = req
	return &http.Response{
		StatusCode: s.statusCode,
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

func TestRun_MissingEnvVars(t *testing.T) {
	tests := []struct {
		name string
		cfg  config
	}{
		{"no issuer", config{clientID: "c", clientSecret: "s"}},
		{"no client ID", config{issuer: "i", clientSecret: "s"}},
		{"no client secret", config{issuer: "i", clientID: "c"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := run(&out, &errOut, tt.cfg, &stubHTTPClient{})
			if code != 1 {
				t.Errorf("expected exit code 1, got %d", code)
			}
			if !strings.Contains(errOut.String(), "AUTH_ERROR") {
				t.Errorf("expected AUTH_ERROR in stderr, got %q", errOut.String())
			}
		})
	}
}

func TestRun_SuccessfulTokenExchange(t *testing.T) {
	testToken := "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJ0ZXN0In0.signature"
	stub := &stubHTTPClient{
		statusCode: 200,
		body:       fmt.Sprintf(`{"access_token":"%s","token_type":"bearer","expires_in":900}`, testToken),
	}

	var out, errOut bytes.Buffer
	cfg := config{
		issuer:       "https://dex.example.com/dex",
		clientID:     "test-client",
		clientSecret: "test-secret",
	}

	code := run(&out, &errOut, cfg, stub)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d, stderr: %s", code, errOut.String())
	}

	// Validate HTTP request
	if stub.lastReq == nil {
		t.Fatal("expected an HTTP request to be made")
	}
	if stub.lastReq.URL.String() != "https://dex.example.com/dex/token" {
		t.Errorf("expected URL /dex/token, got %s", stub.lastReq.URL.String())
	}

	// Validate ExecCredential output
	var cred ExecCredential
	if err := json.NewDecoder(&out).Decode(&cred); err != nil {
		t.Fatalf("failed to decode ExecCredential: %v", err)
	}

	if cred.APIVersion != apiVersion {
		t.Errorf("expected apiVersion %q, got %q", apiVersion, cred.APIVersion)
	}
	if cred.Kind != kind {
		t.Errorf("expected kind %q, got %q", kind, cred.Kind)
	}
	if cred.Status.Token != testToken {
		t.Errorf("expected token %q, got %q", testToken, cred.Status.Token)
	}

	expiry, err := time.Parse(time.RFC3339, cred.Status.ExpirationTimestamp)
	if err != nil {
		t.Fatalf("invalid expiration timestamp: %v", err)
	}
	expectedMin := time.Now().Add(time.Duration(900-30) * time.Second)
	if expiry.Before(expectedMin.Add(-2*time.Second)) || expiry.After(expectedMin.Add(2*time.Second)) {
		t.Errorf("expected expiration around %v, got %v", expectedMin, expiry)
	}
}

func TestRun_ServerError(t *testing.T) {
	stub := &stubHTTPClient{statusCode: 502, body: `{"error":"upstream timeout"}`}
	var out, errOut bytes.Buffer
	cfg := config{issuer: "https://dex/dex", clientID: "c", clientSecret: "s"}

	code := run(&out, &errOut, cfg, stub)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(errOut.String(), "AUTH_ERROR") {
		t.Errorf("expected AUTH_ERROR in stderr, got %q", errOut.String())
	}
	if !strings.Contains(errOut.String(), "unavailable") {
		t.Errorf("expected 'unavailable' in error, got %q", errOut.String())
	}
}

func TestRun_ClientError(t *testing.T) {
	stub := &stubHTTPClient{statusCode: 401, body: `{"error":"invalid_client"}`}
	var out, errOut bytes.Buffer
	cfg := config{issuer: "https://dex/dex", clientID: "c", clientSecret: "bad-secret"}

	code := run(&out, &errOut, cfg, stub)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(errOut.String(), "not authorized") {
		t.Errorf("expected 'not authorized' in error, got %q", errOut.String())
	}
}

func TestRun_MalformedResponse(t *testing.T) {
	stub := &stubHTTPClient{statusCode: 200, body: `not json`}
	var out, errOut bytes.Buffer
	cfg := config{issuer: "https://dex/dex", clientID: "c", clientSecret: "s"}

	code := run(&out, &errOut, cfg, stub)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(errOut.String(), "malformed") {
		t.Errorf("expected 'malformed' in error, got %q", errOut.String())
	}
}

func TestRun_MissingToken(t *testing.T) {
	stub := &stubHTTPClient{statusCode: 200, body: `{"access_token":"","token_type":"bearer","expires_in":900}`}
	var out, errOut bytes.Buffer
	cfg := config{issuer: "https://dex/dex", clientID: "c", clientSecret: "s"}

	code := run(&out, &errOut, cfg, stub)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(errOut.String(), "no token") {
		t.Errorf("expected 'no token' in error, got %q", errOut.String())
	}
}

func TestBuildCredential(t *testing.T) {
	cred := buildCredential("abc.def.ghi", 600)

	if cred.APIVersion != apiVersion {
		t.Errorf("expected apiVersion %q, got %q", apiVersion, cred.APIVersion)
	}
	if cred.Kind != kind {
		t.Errorf("expected kind %q, got %q", kind, cred.Kind)
	}
	if cred.Status.Token != "abc.def.ghi" {
		t.Errorf("expected token %q, got %q", "abc.def.ghi", cred.Status.Token)
	}

	expiry, err := time.Parse(time.RFC3339, cred.Status.ExpirationTimestamp)
	if err != nil {
		t.Fatalf("invalid expiration timestamp: %v", err)
	}
	expected := time.Now().Add(time.Duration(600-30) * time.Second)
	diff := expiry.Sub(expected)
	if diff < -1*time.Second || diff > 1*time.Second {
		t.Errorf("expected expiration around %v, got %v (diff %v)", expected, expiry, diff)
	}
}

func TestBuildCredential_ShortExpiry(t *testing.T) {
	// Tokens with ≤60s expiry should NOT have the 30s safety buffer
	cred := buildCredential("short.token", 30)
	expiry, err := time.Parse(time.RFC3339, cred.Status.ExpirationTimestamp)
	if err != nil {
		t.Fatalf("invalid expiration: %v", err)
	}
	expected := time.Now().Add(30 * time.Second)
	diff := expiry.Sub(expected)
	if diff < -1*time.Second || diff > 1*time.Second {
		t.Errorf("short tokens should use exact expiry, got diff %v", diff)
	}
}
