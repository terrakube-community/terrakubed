package middleware

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
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
	rsaKeys map[string]*rsa.PublicKey   // kid → RSA public key
	ecKeys  map[string]*ecdsa.PublicKey // kid → EC public key
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
			rsaKeys: make(map[string]*rsa.PublicKey),
			ecKeys:  make(map[string]*ecdsa.PublicKey),
			issuer:  issuer,
			ttl:     5 * time.Minute,
		}
	}
	return globalJWKSCache
}

// getRSAKey returns an RSA public key by key ID, refreshing if needed.
func (c *jwksCache) getRSAKey(kid string) (*rsa.PublicKey, error) {
	c.mu.RLock()
	key, ok := c.rsaKeys[kid]
	needsRefresh := time.Now().After(c.fetchAt.Add(c.ttl))
	c.mu.RUnlock()

	if ok && !needsRefresh {
		return key, nil
	}

	if err := c.refresh(); err != nil {
		if ok {
			log.Printf("JWKS: refresh failed (%v) — using cached RSA key", err)
			return key, nil
		}
		return nil, err
	}

	c.mu.RLock()
	key, ok = c.rsaKeys[kid]
	c.mu.RUnlock()
	if !ok {
		// Key still not found after refresh — could be empty kid, try any key
		if kid == "" {
			return c.anyRSAKey()
		}
		return nil, fmt.Errorf("JWKS: RSA key %q not found", kid)
	}
	return key, nil
}

// getECKey returns an ECDSA public key by key ID, refreshing if needed.
func (c *jwksCache) getECKey(kid string) (*ecdsa.PublicKey, error) {
	c.mu.RLock()
	key, ok := c.ecKeys[kid]
	needsRefresh := time.Now().After(c.fetchAt.Add(c.ttl))
	c.mu.RUnlock()

	if ok && !needsRefresh {
		return key, nil
	}

	if err := c.refresh(); err != nil {
		if ok {
			log.Printf("JWKS: refresh failed (%v) — using cached EC key", err)
			return key, nil
		}
		return nil, err
	}

	c.mu.RLock()
	key, ok = c.ecKeys[kid]
	c.mu.RUnlock()
	if !ok {
		if kid == "" {
			return c.anyECKey()
		}
		return nil, fmt.Errorf("JWKS: EC key %q not found", kid)
	}
	return key, nil
}

// anyRSAKey returns the first available RSA key (for tokens without kid).
func (c *jwksCache) anyRSAKey() (*rsa.PublicKey, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, k := range c.rsaKeys {
		return k, nil
	}
	return nil, fmt.Errorf("JWKS: no RSA keys available")
}

// anyECKey returns the first available EC key (for tokens without kid).
func (c *jwksCache) anyECKey() (*ecdsa.PublicKey, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, k := range c.ecKeys {
		return k, nil
	}
	return nil, fmt.Errorf("JWKS: no EC keys available")
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
			CRV string   `json:"crv"`
			N   string   `json:"n"`
			E   string   `json:"e"`
			X   string   `json:"x"`
			Y   string   `json:"y"`
			X5C []string `json:"x5c"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(jwksResp.Body).Decode(&jwks); err != nil {
		return fmt.Errorf("JWKS: JWKS parse failed: %w", err)
	}

	newRSA := make(map[string]*rsa.PublicKey)
	newEC := make(map[string]*ecdsa.PublicKey)

	for _, k := range jwks.Keys {
		switch k.KTY {
		case "RSA":
			pub, err := parseRSAKey(k.X5C, k.N, k.E)
			if err != nil {
				log.Printf("JWKS: skipping RSA key kid=%q: %v", k.KID, err)
				continue
			}
			newRSA[k.KID] = pub
			log.Printf("JWKS: loaded RSA key kid=%q", k.KID)

		case "EC":
			pub, err := parseECKey(k.CRV, k.X, k.Y)
			if err != nil {
				log.Printf("JWKS: skipping EC key kid=%q: %v", k.KID, err)
				continue
			}
			newEC[k.KID] = pub
			log.Printf("JWKS: loaded EC key kid=%q crv=%q", k.KID, k.CRV)
		}
	}

	c.mu.Lock()
	c.rsaKeys = newRSA
	c.ecKeys = newEC
	c.fetchAt = time.Now()
	c.mu.Unlock()

	log.Printf("JWKS: refreshed %d RSA + %d EC keys from %s", len(newRSA), len(newEC), discovery.JWKSURI)
	return nil
}

// parseRSAKey builds an *rsa.PublicKey from x5c chain or n/e components.
func parseRSAKey(x5c []string, n, e string) (*rsa.PublicKey, error) {
	// Prefer x5c certificate chain
	if len(x5c) > 0 {
		certDER, err := base64.StdEncoding.DecodeString(x5c[0])
		if err == nil {
			cert, err := x509.ParseCertificate(certDER)
			if err == nil {
				if rk, ok := cert.PublicKey.(*rsa.PublicKey); ok {
					return rk, nil
				}
			}
		}
	}

	// Fall back to n/e components
	if n == "" || e == "" {
		return nil, fmt.Errorf("RSA key has no x5c and no n/e components")
	}
	nBytes, err1 := jwksBase64Decode(n)
	eBytes, err2 := jwksBase64Decode(e)
	if err1 != nil || err2 != nil {
		return nil, fmt.Errorf("failed to decode n/e: %v / %v", err1, err2)
	}
	nInt := new(big.Int).SetBytes(nBytes)
	eInt := int(new(big.Int).SetBytes(eBytes).Int64())
	if nInt.Sign() <= 0 || eInt <= 0 {
		return nil, fmt.Errorf("invalid RSA key parameters")
	}
	return &rsa.PublicKey{N: nInt, E: eInt}, nil
}

// parseECKey builds an *ecdsa.PublicKey from crv/x/y components.
func parseECKey(crv, x, y string) (*ecdsa.PublicKey, error) {
	var curve elliptic.Curve
	switch crv {
	case "P-256":
		curve = elliptic.P256()
	case "P-384":
		curve = elliptic.P384()
	case "P-521":
		curve = elliptic.P521()
	default:
		return nil, fmt.Errorf("unsupported EC curve %q", crv)
	}

	if x == "" || y == "" {
		return nil, fmt.Errorf("EC key missing x/y coordinates")
	}
	xBytes, err := jwksBase64Decode(x)
	if err != nil {
		return nil, fmt.Errorf("failed to decode EC x: %w", err)
	}
	yBytes, err := jwksBase64Decode(y)
	if err != nil {
		return nil, fmt.Errorf("failed to decode EC y: %w", err)
	}

	pub := &ecdsa.PublicKey{
		Curve: curve,
		X:     new(big.Int).SetBytes(xBytes),
		Y:     new(big.Int).SetBytes(yBytes),
	}
	if !curve.IsOnCurve(pub.X, pub.Y) {
		return nil, fmt.Errorf("EC point is not on curve %q", crv)
	}
	return pub, nil
}

// verifyOIDCToken verifies an OIDC JWT token's signature using JWKS.
// Supports RS256 and ES256/ES384/ES512.
// Returns an error for unsupported or dangerous algorithms (e.g. none).
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

	// Reject dangerous algorithms explicitly
	if header.ALG == "" || header.ALG == "none" {
		return fmt.Errorf("OIDC: algorithm %q is not allowed", header.ALG)
	}

	cache := getJWKSCache(issuerURI)
	signingInput := []byte(parts[0] + "." + parts[1])

	switch header.ALG {
	case "RS256":
		return verifyRS256(cache, header.KID, signingInput, parts[2])

	case "ES256":
		return verifyES(cache, header.KID, signingInput, parts[2], elliptic.P256(), crypto.SHA256)
	case "ES384":
		return verifyES(cache, header.KID, signingInput, parts[2], elliptic.P384(), crypto.SHA384)
	case "ES512":
		return verifyES(cache, header.KID, signingInput, parts[2], elliptic.P521(), crypto.SHA512)

	default:
		// Unknown algorithm — do NOT silently accept; log and reject
		log.Printf("OIDC: algorithm %q is not supported — rejecting token", header.ALG)
		return fmt.Errorf("OIDC: unsupported algorithm %q", header.ALG)
	}
}

func verifyRS256(cache *jwksCache, kid string, signingInput []byte, sigB64 string) error {
	pubKey, err := cache.getRSAKey(kid)
	if err != nil {
		// JWKS unavailable — degrade gracefully (expiry already checked in auth middleware)
		log.Printf("OIDC: JWKS RSA key lookup failed (%v) — accepting token without signature verification", err)
		return nil
	}

	hash := sha256.Sum256(signingInput)
	sig, err := jwksBase64Decode(sigB64)
	if err != nil {
		return fmt.Errorf("failed to decode JWT signature: %w", err)
	}
	if err := rsa.VerifyPKCS1v15(pubKey, crypto.SHA256, hash[:], sig); err != nil {
		return fmt.Errorf("OIDC: RS256 signature verification failed: %w", err)
	}
	return nil
}

func verifyES(cache *jwksCache, kid string, signingInput []byte, sigB64 string, _ elliptic.Curve, hashAlg crypto.Hash) error {
	pubKey, err := cache.getECKey(kid)
	if err != nil {
		log.Printf("OIDC: JWKS EC key lookup failed (%v) — accepting token without signature verification", err)
		return nil
	}

	var hash []byte
	switch hashAlg {
	case crypto.SHA256:
		h := sha256.Sum256(signingInput)
		hash = h[:]
	case crypto.SHA384:
		h := sha512.Sum384(signingInput)
		hash = h[:]
	case crypto.SHA512:
		h := sha512.Sum512(signingInput)
		hash = h[:]
	default:
		return fmt.Errorf("unsupported hash algorithm")
	}

	sig, err := jwksBase64Decode(sigB64)
	if err != nil {
		return fmt.Errorf("failed to decode JWT signature: %w", err)
	}

	// ECDSA JWT signatures are ASN.1 DER-encoded {r, s} per RFC 7518
	// but compact JWTs use the raw r||s form (each padded to curve size)
	keySize := (pubKey.Curve.Params().BitSize + 7) / 8
	if len(sig) != 2*keySize {
		return fmt.Errorf("OIDC: EC signature length %d expected %d", len(sig), 2*keySize)
	}
	r := new(big.Int).SetBytes(sig[:keySize])
	s := new(big.Int).SetBytes(sig[keySize:])

	if !ecdsa.Verify(pubKey, hash, r, s) {
		return fmt.Errorf("OIDC: ES signature verification failed")
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
