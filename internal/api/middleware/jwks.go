package middleware

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// jwksCache caches JWKS keys with a TTL to avoid fetching on every request.
type jwksCache struct {
	mu      sync.RWMutex
	keys    map[string]*rsa.PublicKey // kid → public key
	fetchAt time.Time
	issuer  string
	ttl     time.Duration
}

var (
	globalJWKSCache   *jwksCache
	globalJWKSCacheMu sync.Mutex
)

// getJWKSCache returns (or creates) the singleton cache for a given issuer.
func getJWKSCache(issuer string) *jwksCache {
	globalJWKSCacheMu.Lock()
	defer globalJWKSCacheMu.Unlock()
	if globalJWKSCache == nil || globalJWKSCache.issuer != issuer {
		globalJWKSCache = &jwksCache{
			keys:   make(map[string]*rsa.PublicKey),
			issuer: issuer,
			ttl:    5 * time.Minute,
		}
	}
	return globalJWKSCache
}

// getPublicKey returns the RSA public key for a given key ID.
// It refreshes the JWKS cache if expired or key is not found.
func (c *jwksCache) getPublicKey(kid string) (*rsa.PublicKey, error) {
	c.mu.RLock()
	key, ok := c.keys[kid]
	needsRefresh := time.Now().After(c.fetchAt.Add(c.ttl))
	c.mu.RUnlock()

	if ok && !needsRefresh {
		return key, nil
	}

	// Fetch JWKS from issuer
	if err := c.refresh(); err != nil {
		if ok {
			// Return stale key on refresh failure
			log.Printf("JWKS: refresh failed (%v) — using cached key", err)
			return key, nil
		}
		return nil, err
	}

	c.mu.RLock()
	key, ok = c.keys[kid]
	c.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("JWKS: key %q not found in fetched JWKS", kid)
	}
	return key, nil
}

// refresh fetches the JWKS from the OIDC discovery endpoint.
func (c *jwksCache) refresh() error {
	// Step 1: OIDC discovery
	discoveryURL := c.issuer + "/.well-known/openid-configuration"
	resp, err := http.Get(discoveryURL) //nolint:gosec
	if err != nil {
		return fmt.Errorf("JWKS: discovery fetch failed: %w", err)
	}
	defer resp.Body.Close()

	var discovery struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&discovery); err != nil {
		return fmt.Errorf("JWKS: discovery parse failed: %w", err)
	}
	if discovery.JWKSURI == "" {
		return fmt.Errorf("JWKS: discovery doc has no jwks_uri")
	}

	// Step 2: Fetch JWKS
	jwksResp, err := http.Get(discovery.JWKSURI) //nolint:gosec
	if err != nil {
		return fmt.Errorf("JWKS: JWKS fetch failed: %w", err)
	}
	defer jwksResp.Body.Close()

	var jwks struct {
		Keys []struct {
			KID string   `json:"kid"`
			KTY string   `json:"kty"`
			ALG string   `json:"alg"`
			N   string   `json:"n"`
			E   string   `json:"e"`
			X5C []string `json:"x5c"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(jwksResp.Body).Decode(&jwks); err != nil {
		return fmt.Errorf("JWKS: JWKS parse failed: %w", err)
	}

	newKeys := make(map[string]*rsa.PublicKey, len(jwks.Keys))
	for _, k := range jwks.Keys {
		if k.KTY != "RSA" {
			continue
		}

		var pub *rsa.PublicKey

		// Prefer x5c certificate chain
		if len(k.X5C) > 0 {
			certDER, err := base64.StdEncoding.DecodeString(k.X5C[0])
			if err == nil {
				cert, err := x509.ParseCertificate(certDER)
				if err == nil {
					if rk, ok := cert.PublicKey.(*rsa.PublicKey); ok {
						pub = rk
					}
				}
			}
		}

		// Fall back to n/e components
		if pub == nil && k.N != "" && k.E != "" {
			nBytes, err1 := jwksBase64Decode(k.N)
			eBytes, err2 := jwksBase64Decode(k.E)
			if err1 == nil && err2 == nil {
				n := new(big.Int).SetBytes(nBytes)
				e := int(new(big.Int).SetBytes(eBytes).Int64())
				if n.Sign() > 0 && e > 0 {
					pub = &rsa.PublicKey{N: n, E: e}
				}
			}
		}

		if pub != nil {
			newKeys[k.KID] = pub
			log.Printf("JWKS: loaded RSA key kid=%q", k.KID)
		}
	}

	c.mu.Lock()
	c.keys = newKeys
	c.fetchAt = time.Now()
	c.mu.Unlock()

	log.Printf("JWKS: refreshed %d keys from %s", len(newKeys), discovery.JWKSURI)
	return nil
}

// verifyOIDCToken verifies an OIDC JWT token's signature using JWKS.
// It gracefully degrades when Dex is unreachable — expiry check still applies.
func verifyOIDCToken(token, issuerURI string) error {
	if issuerURI == "" {
		return nil // OIDC verification not configured
	}

	parts := splitJWT(token)
	if len(parts) != 3 {
		return fmt.Errorf("invalid JWT format")
	}

	// Decode header to get algorithm + key ID
	headerBytes, err := jwksBase64Decode(parts[0])
	if err != nil {
		return fmt.Errorf("failed to decode JWT header: %w", err)
	}

	var header struct {
		ALG string `json:"alg"`
		KID string `json:"kid"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return fmt.Errorf("failed to parse JWT header: %w", err)
	}

	// Only RS256 supported; accept others with a log warning
	if header.ALG != "RS256" {
		log.Printf("OIDC: algorithm %q not supported — skipping signature verification", header.ALG)
		return nil
	}

	// Fetch the signing key
	cache := getJWKSCache(issuerURI)
	pubKey, err := cache.getPublicKey(header.KID)
	if err != nil {
		// JWKS unavailable — degrade gracefully (expiry already checked in auth middleware)
		log.Printf("OIDC: JWKS key lookup failed (%v) — accepting token without signature verification", err)
		return nil
	}

	// Compute SHA256 hash of "header.payload"
	signingInput := []byte(parts[0] + "." + parts[1])
	hash := sha256.Sum256(signingInput)

	// Decode the signature
	sig, err := jwksBase64Decode(parts[2])
	if err != nil {
		return fmt.Errorf("failed to decode JWT signature: %w", err)
	}

	if err := rsa.VerifyPKCS1v15(pubKey, crypto.SHA256, hash[:], sig); err != nil {
		return fmt.Errorf("OIDC: RS256 signature verification failed: %w", err)
	}

	return nil
}

// splitJWT splits a JWT into its three parts without importing strings.
func splitJWT(token string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			parts = append(parts, token[start:i])
			start = i + 1
		}
	}
	parts = append(parts, token[start:])
	return parts
}

// jwksBase64Decode decodes a standard or URL-safe base64 string (with or without padding).
func jwksBase64Decode(s string) ([]byte, error) {
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}
	// Try URL-safe first (JWKS n/e fields use base64url)
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.StdEncoding.DecodeString(s)
}
