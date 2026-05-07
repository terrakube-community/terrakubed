package middleware

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// buildJWT builds a minimal HS256 JWT for testing (unsigned or HMAC-signed).
// Use signSecret="" for an unsigned token (will fail verifyHMACToken).
func buildJWT(t *testing.T, payload map[string]interface{}) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payloadBytes, _ := json.Marshal(payload)
	pay := base64.RawURLEncoding.EncodeToString(payloadBytes)
	// Fake signature (token won't pass crypto verify — only used to test claim parsing)
	sig := base64.RawURLEncoding.EncodeToString([]byte("fakesig"))
	return header + "." + pay + "." + sig
}

func TestDecodeJWTClaims_BasicFields(t *testing.T) {
	token := buildJWT(t, map[string]interface{}{
		"iss": "TerrakubeTest",
		"sub": "user@example.com",
		"exp": time.Now().Add(time.Hour).Unix(),
		"email": "user@example.com",
	})

	claims, err := decodeJWTClaims(token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if claims.Issuer != "TerrakubeTest" {
		t.Errorf("issuer got %q want %q", claims.Issuer, "TerrakubeTest")
	}
	if claims.Email != "user@example.com" {
		t.Errorf("email got %q want %q", claims.Email, "user@example.com")
	}
}

func TestDecodeJWTClaims_GroupsNoDuplicates(t *testing.T) {
	// Groups is a []string in the JWT — verify it doesn't get appended twice.
	token := buildJWT(t, map[string]interface{}{
		"iss":    "Terrakube",
		"groups": []string{"admins", "devs"},
	})

	claims, err := decodeJWTClaims(token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have exactly 2 groups, not 4 (the bug was appending them again)
	if len(claims.Groups) != 2 {
		t.Errorf("groups len got %d want 2 (values: %v)", len(claims.Groups), claims.Groups)
	}
	if claims.Groups[0] != "admins" || claims.Groups[1] != "devs" {
		t.Errorf("unexpected groups: %v", claims.Groups)
	}
}

func TestDecodeJWTClaims_GroupsAsInterfaceArray(t *testing.T) {
	// Some OIDC providers use a raw JSON array of strings.
	// We encode the payload directly to ensure it's a proper JSON array.
	payloadMap := map[string]interface{}{
		"iss":    "SomeOIDC",
		"groups": []interface{}{"team-a", "team-b", "team-c"},
	}
	payloadBytes, _ := json.Marshal(payloadMap)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	pay := base64.RawURLEncoding.EncodeToString(payloadBytes)
	sig := base64.RawURLEncoding.EncodeToString([]byte("sig"))
	token := header + "." + pay + "." + sig

	claims, err := decodeJWTClaims(token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(claims.Groups) != 3 {
		t.Errorf("groups len got %d want 3 (values: %v)", len(claims.Groups), claims.Groups)
	}
}

func TestDecodeJWTClaims_InvalidFormat(t *testing.T) {
	_, err := decodeJWTClaims("notavalidtoken")
	if err == nil {
		t.Error("expected error for invalid token format")
	}
}

func TestVerifyHMACToken_WrongSignatureRejected(t *testing.T) {
	secret := []byte("test-secret-key")
	secretB64 := base64.URLEncoding.EncodeToString(secret)

	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"Terrakube"}`))
	signingInput := header + "." + payload

	// Wrong signature — should fail
	badToken := signingInput + "." + base64.RawURLEncoding.EncodeToString([]byte("wrong"))
	if err := verifyHMACToken(badToken, secretB64); err == nil {
		t.Error("expected error for wrong signature")
	}
}

func TestVerifyHMACToken_ValidSignatureAccepted(t *testing.T) {
	secret := []byte("test-secret-key")
	secretB64 := base64.URLEncoding.EncodeToString(secret)

	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"Terrakube"}`))
	signingInput := header + "." + payload

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signingInput))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	validToken := signingInput + "." + sig
	if err := verifyHMACToken(validToken, secretB64); err != nil {
		t.Errorf("expected valid token to pass, got: %v", err)
	}
}

func TestVerifyOIDCToken_RejectsAlgNone(t *testing.T) {
	// Build a token with alg: none
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"attacker"}`))
	token := header + "." + payload + "."

	err := verifyOIDCToken(token, "https://example.com")
	if err == nil {
		t.Error("expected error for alg:none token")
	}
	if !strings.Contains(err.Error(), "none") {
		t.Errorf("error should mention 'none', got: %v", err)
	}
}

func TestVerifyOIDCToken_RejectsEmptyAlg(t *testing.T) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"attacker"}`))
	token := header + "." + payload + "."

	err := verifyOIDCToken(token, "https://example.com")
	if err == nil {
		t.Error("expected error for empty alg token")
	}
}

func TestVerifyOIDCToken_SkipsWhenNoIssuer(t *testing.T) {
	// When issuerURI is empty, OIDC verification is disabled (all tokens accepted)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"anyone"}`))
	token := header + "." + payload + ".sig"

	if err := verifyOIDCToken(token, ""); err != nil {
		t.Errorf("expected nil error when issuerURI is empty, got: %v", err)
	}
}

func TestIsPublicPath(t *testing.T) {
	tests := []struct {
		path    string
		method  string
		public  bool
	}{
		{"/webhook/v1/github/abc", "POST", true},
		{"/health", "GET", true},
		{"/actuator/health", "GET", true},
		{"/api/v1/workspace", "GET", false},
		{"/api/v1/workspace", "OPTIONS", true},
		{"/callback/v1/github", "GET", true},
		{"/.well-known/terraform.json", "GET", true},
		{"/remote/tfe/v2/ping", "GET", true},
		// Terraform CLI plan/apply log endpoints — public GET only
		{"/remote/tfe/v2/plans/plan-123/log", "GET", true},
		{"/remote/tfe/v2/plans/plan-123/log", "POST", false},
		{"/remote/tfe/v2/applies/apply-456/logs", "GET", true},
		{"/remote/tfe/v2/applies/apply-456/logs", "DELETE", false},
		// apply/plan status (no suffix) — not public
		{"/remote/tfe/v2/plans/plan-123", "GET", false},
		{"/remote/tfe/v2/applies/apply-456", "GET", false},
	}

	for _, tt := range tests {
		got := isPublicPath(tt.path, tt.method)
		if got != tt.public {
			t.Errorf("isPublicPath(%q, %q) = %v, want %v", tt.path, tt.method, got, tt.public)
		}
	}
}
