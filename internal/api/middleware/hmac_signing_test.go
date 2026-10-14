package middleware

import (
	"encoding/base64"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

// Tokens minted for the PAT/team-token endpoints must verify with
// verifyHMACToken. They used to be signed with the raw secret string while
// verification base64url-decodes it first, so every Go-minted token failed
// with "signature mismatch".
func TestSignedTokenVerifies(t *testing.T) {
	secret := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

	key, err := HMACSigningKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"iss": "Terrakube"}).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyHMACToken(signed, secret); err != nil {
		t.Fatalf("token signed with HMACSigningKey should verify: %v", err)
	}

	// The old behavior (raw secret bytes) must NOT verify.
	bad, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"iss": "Terrakube"}).SignedString([]byte(secret))
	if err := verifyHMACToken(bad, secret); err == nil {
		t.Fatal("token signed with the raw secret string unexpectedly verified")
	}
}
