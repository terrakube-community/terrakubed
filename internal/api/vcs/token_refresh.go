package vcs

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TokenRefresher periodically checks for expiring VCS OAuth tokens and refreshes them.
// GitLab and Bitbucket issue short-lived tokens (2h) with refresh tokens.
// GitHub personal access tokens do not expire by default.
type TokenRefresher struct {
	pool     *pgxpool.Pool
	interval time.Duration
}

// NewTokenRefresher creates a new refresher. Call Start() to begin the refresh loop.
func NewTokenRefresher(pool *pgxpool.Pool) *TokenRefresher {
	return &TokenRefresher{
		pool:     pool,
		interval: 10 * time.Minute,
	}
}

// Start runs the refresh loop until ctx is cancelled.
func (r *TokenRefresher) Start(ctx context.Context) {
	log.Printf("VCS token refresher started (interval: %s)", r.interval)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	// Run immediately on start
	r.refreshExpiring(ctx)

	for {
		select {
		case <-ctx.Done():
			log.Println("VCS token refresher stopped")
			return
		case <-ticker.C:
			r.refreshExpiring(ctx)
		}
	}
}

// refreshExpiring finds VCS records whose tokens expire within the next 30 minutes
// and refreshes them using the stored refresh token.
func (r *TokenRefresher) refreshExpiring(ctx context.Context) {
	rows, err := r.pool.Query(ctx, `
		SELECT id::text, vcs_type, client_id, client_secret,
		       refresh_token, COALESCE(redirect_url, '')
		FROM vcs
		WHERE refresh_token IS NOT NULL AND refresh_token != ''
		  AND (token_expiration IS NULL OR token_expiration < NOW() + INTERVAL '30 minutes')
		  AND status = 'ACTIVE'
	`)
	if err != nil {
		log.Printf("TokenRefresher: query error: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var vcsID, vcsType, clientID, clientSecret, refreshToken, redirectURL string
		if err := rows.Scan(&vcsID, &vcsType, &clientID, &clientSecret, &refreshToken, &redirectURL); err != nil {
			continue
		}

		accessToken, newRefresh, expiry, err := refreshOAuthToken(vcsType, clientID, clientSecret, refreshToken, redirectURL)
		if err != nil {
			log.Printf("TokenRefresher: failed to refresh token for VCS %s (%s): %v", vcsID, vcsType, err)
			continue
		}

		// Update the stored token
		if newRefresh == "" {
			newRefresh = refreshToken // Some providers don't return a new refresh token
		}
		if expiry.IsZero() {
			_, err = r.pool.Exec(ctx, `
				UPDATE vcs SET access_token = $1, refresh_token = $2 WHERE id = $3
			`, accessToken, newRefresh, vcsID)
		} else {
			_, err = r.pool.Exec(ctx, `
				UPDATE vcs SET access_token = $1, refresh_token = $2, token_expiration = $3 WHERE id = $4
			`, accessToken, newRefresh, expiry, vcsID)
		}
		if err != nil {
			log.Printf("TokenRefresher: failed to save refreshed token for VCS %s: %v", vcsID, err)
			continue
		}

		log.Printf("TokenRefresher: refreshed token for VCS %s (%s)", vcsID, vcsType)
	}
}

// refreshOAuthToken exchanges a refresh token for a new access token.
func refreshOAuthToken(vcsType, clientID, clientSecret, refreshToken, redirectURL string) (accessToken, newRefresh string, expiry time.Time, err error) {
	switch {
	case strings.HasPrefix(vcsType, "GITLAB"):
		return refreshGitLabToken(clientID, clientSecret, refreshToken, redirectURL)
	case strings.HasPrefix(vcsType, "BITBUCKET"):
		return refreshBitbucketToken(clientID, clientSecret, refreshToken)
	default:
		err = fmt.Errorf("token refresh not supported for VCS type %q", vcsType)
		return
	}
}

func refreshGitLabToken(clientID, clientSecret, refreshToken, redirectURL string) (string, string, time.Time, error) {
	params := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
		"redirect_uri":  {redirectURL},
	}

	req, err := http.NewRequest(http.MethodPost,
		"https://gitlab.com/oauth/token",
		strings.NewReader(params.Encode()))
	if err != nil {
		return "", "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("GitLab refresh request: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", time.Time{}, fmt.Errorf("GitLab refresh parse: %w", err)
	}
	if result.Error != "" {
		return "", "", time.Time{}, fmt.Errorf("GitLab refresh error: %s", result.Error)
	}

	var expiry time.Time
	if result.ExpiresIn > 0 {
		expiry = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	}
	return result.AccessToken, result.RefreshToken, expiry, nil
}

func refreshBitbucketToken(clientID, clientSecret, refreshToken string) (string, string, time.Time, error) {
	params := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}

	req, err := http.NewRequest(http.MethodPost,
		"https://bitbucket.org/site/oauth2/access_token",
		strings.NewReader(params.Encode()))
	if err != nil {
		return "", "", time.Time{}, err
	}
	req.SetBasicAuth(clientID, clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("Bitbucket refresh request: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", time.Time{}, fmt.Errorf("Bitbucket refresh parse: %w", err)
	}
	if result.Error != "" {
		return "", "", time.Time{}, fmt.Errorf("Bitbucket refresh error: %s", result.Error)
	}

	var expiry time.Time
	if result.ExpiresIn > 0 {
		expiry = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	}
	return result.AccessToken, result.RefreshToken, expiry, nil
}

// GetFreshToken returns a fresh access token for a VCS, refreshing if expired.
// This can be called just before making API calls to ensure the token is valid.
func GetFreshToken(ctx context.Context, pool *pgxpool.Pool, vcsID string) (string, error) {
	var accessToken, refreshToken, vcsType, clientID, clientSecret, redirectURL string
	var tokenExpiry *time.Time

	err := pool.QueryRow(ctx, `
		SELECT access_token, COALESCE(refresh_token,''), vcs_type,
		       client_id, client_secret, COALESCE(redirect_url,''), token_expiration
		FROM vcs WHERE id = $1
	`, vcsID).Scan(&accessToken, &refreshToken, &vcsType, &clientID, &clientSecret, &redirectURL, &tokenExpiry)
	if err != nil {
		return "", fmt.Errorf("VCS %s not found: %w", vcsID, err)
	}

	// Check if token is still valid (at least 5 minutes left)
	if tokenExpiry != nil && time.Now().Add(5*time.Minute).After(*tokenExpiry) {
		if refreshToken == "" {
			return accessToken, nil // No refresh token — use as-is
		}
		newAccess, newRefresh, newExpiry, err := refreshOAuthToken(vcsType, clientID, clientSecret, refreshToken, redirectURL)
		if err != nil {
			log.Printf("GetFreshToken: refresh failed for VCS %s: %v — using stale token", vcsID, err)
			return accessToken, nil
		}
		if newRefresh == "" {
			newRefresh = refreshToken
		}
		if newExpiry.IsZero() {
			pool.Exec(ctx, `UPDATE vcs SET access_token=$1, refresh_token=$2 WHERE id=$3`,
				newAccess, newRefresh, vcsID)
		} else {
			pool.Exec(ctx, `UPDATE vcs SET access_token=$1, refresh_token=$2, token_expiration=$3 WHERE id=$4`,
				newAccess, newRefresh, newExpiry, vcsID)
		}
		return newAccess, nil
	}

	return accessToken, nil
}
