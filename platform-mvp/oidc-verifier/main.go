package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func main() {
	issuer := flag.String("issuer", os.Getenv("OIDC_ISSUER"), "Dex issuer URL (e.g. https://host/dex)")
	audience := flag.String("audience", os.Getenv("OIDC_AUDIENCE"), "Expected audience claim")
	port := flag.String("port", "8080", "Listen port")
	insecureSkipVerify := flag.Bool("insecure-skip-verify", false, "Skip TLS certificate verification (for self-signed certs)")
	trustJWKS := flag.String("trust-jwks", "", "Path to a JSON JWKS file containing additional trusted keys")
	flag.Parse()

	if *issuer == "" {
		log.Fatal("--issuer or OIDC_ISSUER is required")
	}

	jwksURL := strings.TrimRight(*issuer, "/") + "/keys"

	keySet := map[string]interface{}{}

	if *trustJWKS != "" {
		data, err := os.ReadFile(*trustJWKS)
		if err != nil {
			log.Fatalf("failed to read trust-jwks file: %v", err)
		}
		loaded, err := parseJWKS(data)
		if err != nil {
			log.Fatalf("failed to parse trust-jwks file: %v", err)
		}
		for k, v := range loaded {
			keySet[k] = v
		}
	}

	s := &server{
		issuer:             *issuer,
		audience:           *audience,
		jwksURL:            jwksURL,
		insecureSkipVerify: *insecureSkipVerify,
		keySet:             keySet,
	}

	if err := s.refreshJWKS(); err != nil {
		log.Printf("WARNING: initial JWKS fetch failed: %v (will retry)", err)
	}

	go func() {
		for {
			time.Sleep(5 * time.Minute)
			if err := s.refreshJWKS(); err != nil {
				log.Printf("WARNING: JWKS refresh failed: %v", err)
			}
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/verify", s.handleVerify)
	mux.HandleFunc("/healthz", s.handleHealthz)

	addr := ":" + *port
	log.Printf("oidc-verifier listening on %s, issuer=%s", addr, *issuer)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

type server struct {
	issuer   string
	audience string
	jwksURL  string

	mu       sync.RWMutex
	keySet   map[string]interface{}
	lastLoad time.Time

	insecureSkipVerify bool
}

func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	loaded := len(s.keySet) > 0
	s.mu.RUnlock()

	if !loaded {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"status": "no_keys_loaded"})
		return
	}
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *server) handleVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	authHeader := r.Header.Get("Authorization")
	if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
		logAudit("jwt_verify", "unknown", "failed", "missing Authorization header")
		writeJSON(w, http.StatusUnauthorized, map[string]interface{}{
			"error": "missing Authorization header",
		})
		return
	}

	tokenString := strings.TrimPrefix(authHeader, "Bearer ")

	s.mu.RLock()
	keySet := s.keySet
	s.mu.RUnlock()

	opts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512"}),
		jwt.WithIssuer(s.issuer),
	}
	if s.audience != "" {
		opts = append(opts, jwt.WithAudience(s.audience))
	}

	token, err := jwt.NewParser(opts...).Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		kid, ok := token.Header["kid"].(string)
		if !ok {
			return nil, fmt.Errorf("missing kid in token header")
		}
		key, ok := keySet[kid]
		if !ok {
			return nil, fmt.Errorf("unknown kid: %s", kid)
		}
		return key, nil
	})

	if err != nil {
		logAudit("jwt_verify", extractSub(tokenString), "failed", err.Error())
		writeJSON(w, http.StatusUnauthorized, map[string]interface{}{
			"error":  "token validation failed",
			"detail": err.Error(),
		})
		return
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		logAudit("jwt_verify", "unknown", "failed", "invalid claims type")
		writeJSON(w, http.StatusUnauthorized, map[string]interface{}{
			"error": "invalid token claims",
		})
		return
	}

	sub, _ := claims.GetSubject()
	logAudit("jwt_verify", sub, "success", "token validated")

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "verified",
		"claims": claims,
	})
}

func (s *server) refreshJWKS() error {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: s.insecureSkipVerify},
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: transport}
	resp, err := client.Get(s.jwksURL)
	if err != nil {
		return fmt.Errorf("fetching JWKS: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("JWKS endpoint returned %d", resp.StatusCode)
	}

	var response struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return fmt.Errorf("decoding JWKS: %w", err)
	}

	newKeySet, err := parseJWKSKeys(response.Keys)
	if err != nil {
		return err
	}

	s.mu.Lock()
	for k, v := range newKeySet {
		s.keySet[k] = v
	}
	s.lastLoad = time.Now()
	s.mu.Unlock()

	logAudit("jwks_refresh", "system", "success", fmt.Sprintf("loaded %d keys", len(newKeySet)))
	return nil
}

func parseJWKS(data []byte) (map[string]interface{}, error) {
	var response struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("decoding JWKS: %w", err)
	}
	return parseJWKSKeys(response.Keys)
}

func parseJWKSKeys(keys []json.RawMessage) (map[string]interface{}, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("JWKS contains no keys")
	}
	newKeySet := map[string]interface{}{}
	for _, rawKey := range keys {
		var header struct {
			Kid string `json:"kid"`
		}
		if err := json.Unmarshal(rawKey, &header); err != nil {
			continue
		}
		if header.Kid == "" {
			continue
		}

		parsedKey, err := parseJWK(rawKey)
		if err != nil {
			return nil, fmt.Errorf("failed to parse key %s: %w", header.Kid, err)
		}
		newKeySet[header.Kid] = parsedKey
	}

	return newKeySet, nil
}

func extractSub(tokenString string) string {
	parser := jwt.NewParser()
	token, _, err := parser.ParseUnverified(tokenString, jwt.MapClaims{})
	if err != nil {
		return "unknown"
	}
	if claims, ok := token.Claims.(jwt.MapClaims); ok {
		if sub, ok := claims["sub"].(string); ok {
			return sub
		}
		if cid, ok := claims["client_id"].(string); ok {
			return cid
		}
	}
	return "unknown"
}

func logAudit(action, subject, result, detail string) {
	entry := map[string]interface{}{
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"action":    action,
		"subject":   subject,
		"result":    result,
		"detail":    detail,
		"component": "oidc-verifier",
	}
	b, _ := json.Marshal(entry)
	fmt.Printf("AUDIT %s\n", string(b))
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// ── JWK Parsing ──────────────────────────────────────────────────────────────

type jwkFields struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
	X   string `json:"x"`
	Y   string `json:"y"`
	Crv string `json:"crv"`
}

func parseJWK(raw json.RawMessage) (interface{}, error) {
	var f jwkFields
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}

	switch f.Kty {
	case "RSA":
		return parseRSAJWK(&f)
	case "EC":
		return parseECJWK(&f)
	default:
		return nil, fmt.Errorf("unsupported kty: %s", f.Kty)
	}
}

func parseRSAJWK(f *jwkFields) (*rsa.PublicKey, error) {
	if f.N == "" || f.E == "" {
		return nil, fmt.Errorf("RSA JWK missing 'n' or 'e'")
	}

	n := new(big.Int).SetBytes(base64URLDecode(f.N))

	eBytes := base64URLDecode(f.E)
	e := 0
	for _, b := range eBytes {
		e = e<<8 | int(b)
	}

	return &rsa.PublicKey{N: n, E: e}, nil
}

func parseECJWK(f *jwkFields) (*ecdsa.PublicKey, error) {
	if f.X == "" || f.Y == "" || f.Crv == "" {
		return nil, fmt.Errorf("EC JWK missing 'x', 'y', or 'crv'")
	}

	var curve elliptic.Curve
	switch f.Crv {
	case "P-256":
		curve = elliptic.P256()
	case "P-384":
		curve = elliptic.P384()
	case "P-521":
		curve = elliptic.P521()
	default:
		return nil, fmt.Errorf("unsupported EC curve: %s", f.Crv)
	}

	x := new(big.Int).SetBytes(base64URLDecode(f.X))
	y := new(big.Int).SetBytes(base64URLDecode(f.Y))

	return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
}

func base64URLDecode(s string) []byte {
	s = strings.TrimRight(s, "=")
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}
	raw, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
		if err != nil {
			return nil
		}
	}
	return raw
}
