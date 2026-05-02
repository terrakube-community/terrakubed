package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// VCSCallbackHandler handles OAuth 2.0 callbacks from VCS providers.
// The flow is: UI redirects user to VCS authorization URL → VCS provider
// redirects back to /callback/v1/{vcsId}?code=xxx → we exchange the code
// for an access token and store it in the vcs table.
//
// Path: GET /callback/v1/{vcsId}
type VCSCallbackHandler struct {
	pool     *pgxpool.Pool
	uiURL    string
	hostname string
}

// NewVCSCallbackHandler creates a new handler.
func NewVCSCallbackHandler(pool *pgxpool.Pool, hostname, uiURL string) *VCSCallbackHandler {
	return &VCSCallbackHandler{pool: pool, hostname: hostname, uiURL: uiURL}
}

func (h *VCSCallbackHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract vcsId from path: /callback/v1/{vcsId}
	vcsID := strings.TrimPrefix(r.URL.Path, "/callback/v1/")
	if vcsID == "" || strings.Contains(vcsID, "/") {
		http.Error(w, "invalid callback path", http.StatusBadRequest)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code parameter", http.StatusBadRequest)
		return
	}

	// Load VCS record
	var vcsType, clientID, clientSecret, redirectURL string
	err := h.pool.QueryRow(r.Context(), `
		SELECT vcs_type, client_id, client_secret, COALESCE(redirect_url, '')
		FROM vcs WHERE id = $1
	`, vcsID).Scan(&vcsType, &clientID, &clientSecret, &redirectURL)
	if err != nil {
		log.Printf("VCS callback: VCS %s not found: %v", vcsID, err)
		http.Error(w, "VCS not found", http.StatusNotFound)
		return
	}

	// Build redirect URL if not stored
	if redirectURL == "" {
		redirectURL = fmt.Sprintf("https://%s/callback/v1/%s", h.hostname, vcsID)
	}

	// Exchange code for access token
	accessToken, refreshToken, expiry, err := h.exchangeCode(vcsType, clientID, clientSecret, redirectURL, code)
	if err != nil {
		log.Printf("VCS callback: token exchange failed for %s: %v", vcsID, err)
		http.Error(w, "token exchange failed", http.StatusInternalServerError)
		return
	}

	// Store the token
	if expiry.IsZero() {
		_, err = h.pool.Exec(r.Context(), `
			UPDATE vcs SET access_token = $1, refresh_token = $2, status = 'ACTIVE'
			WHERE id = $3
		`, accessToken, refreshToken, vcsID)
	} else {
		_, err = h.pool.Exec(r.Context(), `
			UPDATE vcs SET access_token = $1, refresh_token = $2,
			               token_expiration = $3, status = 'ACTIVE'
			WHERE id = $4
		`, accessToken, refreshToken, expiry, vcsID)
	}
	if err != nil {
		log.Printf("VCS callback: failed to save token for %s: %v", vcsID, err)
		http.Error(w, "failed to save token", http.StatusInternalServerError)
		return
	}

	log.Printf("VCS callback: OAuth complete for VCS %s (%s)", vcsID, vcsType)

	// Redirect user back to UI
	uiRedirect := h.uiURL
	if uiRedirect == "" {
		uiRedirect = "/"
	}
	// Redirect to the VCS settings page in the UI
	http.Redirect(w, r, uiRedirect, http.StatusFound)
}

// exchangeCode exchanges an OAuth authorization code for access/refresh tokens.
func (h *VCSCallbackHandler) exchangeCode(vcsType, clientID, clientSecret, redirectURL, code string) (accessToken, refreshToken string, expiry time.Time, err error) {
	switch {
	case strings.HasPrefix(vcsType, "GITHUB"):
		return h.exchangeGitHub(clientID, clientSecret, redirectURL, code)
	case strings.HasPrefix(vcsType, "GITLAB"):
		return h.exchangeGitLab(clientID, clientSecret, redirectURL, code)
	case strings.HasPrefix(vcsType, "BITBUCKET"):
		return h.exchangeBitbucket(clientID, clientSecret, redirectURL, code)
	default:
		err = fmt.Errorf("unsupported VCS type: %s", vcsType)
		return
	}
}

// ──────────────────────────────────────────────────
// GitHub OAuth
// ──────────────────────────────────────────────────

func (h *VCSCallbackHandler) exchangeGitHub(clientID, clientSecret, redirectURL, code string) (string, string, time.Time, error) {
	params := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURL},
	}

	req, err := http.NewRequest(http.MethodPost,
		"https://github.com/login/oauth/access_token",
		strings.NewReader(params.Encode()))
	if err != nil {
		return "", "", time.Time{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("GitHub token exchange: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Scope       string `json:"scope"`
		Error       string `json:"error"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &result); err != nil {
		return "", "", time.Time{}, fmt.Errorf("GitHub token parse: %w", err)
	}
	if result.Error != "" {
		return "", "", time.Time{}, fmt.Errorf("GitHub OAuth error: %s", result.Error)
	}

	return result.AccessToken, "", time.Time{}, nil
}

// ──────────────────────────────────────────────────
// GitLab OAuth
// ──────────────────────────────────────────────────

func (h *VCSCallbackHandler) exchangeGitLab(clientID, clientSecret, redirectURL, code string) (string, string, time.Time, error) {
	params := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURL},
		"grant_type":    {"authorization_code"},
	}

	req, err := http.NewRequest(http.MethodPost,
		"https://gitlab.com/oauth/token",
		bytes.NewBufferString(params.Encode()))
	if err != nil {
		return "", "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("GitLab token exchange: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", time.Time{}, fmt.Errorf("GitLab token parse: %w", err)
	}
	if result.Error != "" {
		return "", "", time.Time{}, fmt.Errorf("GitLab OAuth error: %s", result.Error)
	}

	var expiry time.Time
	if result.ExpiresIn > 0 {
		expiry = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	}

	return result.AccessToken, result.RefreshToken, expiry, nil
}

// ──────────────────────────────────────────────────
// Bitbucket OAuth
// ──────────────────────────────────────────────────

func (h *VCSCallbackHandler) exchangeBitbucket(clientID, clientSecret, redirectURL, code string) (string, string, time.Time, error) {
	params := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURL},
		"grant_type":    {"authorization_code"},
	}

	req, err := http.NewRequest(http.MethodPost,
		"https://bitbucket.org/site/oauth2/access_token",
		bytes.NewBufferString(params.Encode()))
	if err != nil {
		return "", "", time.Time{}, err
	}
	req.SetBasicAuth(clientID, clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("Bitbucket token exchange: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", time.Time{}, fmt.Errorf("Bitbucket token parse: %w", err)
	}
	if result.Error != "" {
		return "", "", time.Time{}, fmt.Errorf("Bitbucket OAuth error: %s", result.Error)
	}

	var expiry time.Time
	if result.ExpiresIn > 0 {
		expiry = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	}

	return result.AccessToken, result.RefreshToken, expiry, nil
}
