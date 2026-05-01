// Package vcs handles VCS provider interactions: commit status updates,
// webhook processing helpers, and OAuth token management.
package vcs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// StatusState represents a VCS commit status state.
type StatusState string

const (
	StatePending StatusState = "pending"
	StateSuccess StatusState = "success"
	StateFailure StatusState = "failure"
	StateError   StatusState = "error"
)

// CommitStatus holds the information needed to post a commit status.
type CommitStatus struct {
	// VCS provider type: "GITHUB", "GITLAB", "BITBUCKET"
	VCSType string
	// OAuth or PAT token
	AccessToken string
	// Repository identifier:
	//   GitHub/Bitbucket: "owner/repo"
	//   GitLab: numeric project ID or "namespace/project"
	RepoRef string
	// Git commit SHA
	CommitSHA string
	// Current status
	State StatusState
	// Optional target URL (link to the job in Terrakube UI)
	TargetURL string
	// Short description shown in VCS UI
	Description string
	// Context label shown in VCS UI (e.g. "terrakube/plan")
	Context string
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

// PostStatus posts a commit status to the appropriate VCS provider.
// Returns an error if the provider type is unsupported or the request fails.
func PostStatus(cs CommitStatus) error {
	if cs.CommitSHA == "" || cs.AccessToken == "" {
		return nil // Nothing to post
	}

	switch {
	case strings.HasPrefix(cs.VCSType, "GITHUB"):
		return postGitHubStatus(cs)
	case strings.HasPrefix(cs.VCSType, "GITLAB"):
		return postGitLabStatus(cs)
	case strings.HasPrefix(cs.VCSType, "BITBUCKET"):
		return postBitbucketStatus(cs)
	default:
		log.Printf("CommitStatus: unsupported VCS type %q — skipping", cs.VCSType)
		return nil
	}
}

// postGitHubStatus posts a status to the GitHub Statuses API.
// POST /repos/{owner}/{repo}/statuses/{sha}
func postGitHubStatus(cs CommitStatus) error {
	// RepoRef may come from the clone URL — extract "owner/repo" if needed
	repoPath := extractGitHubRepoPath(cs.RepoRef)
	if repoPath == "" {
		return fmt.Errorf("GitHub: could not extract owner/repo from %q", cs.RepoRef)
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/statuses/%s", repoPath, cs.CommitSHA)

	payload := map[string]string{
		"state":       string(githubState(cs.State)),
		"description": cs.Description,
		"context":     cs.Context,
	}
	if cs.TargetURL != "" {
		payload["target_url"] = cs.TargetURL
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+cs.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub status POST: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GitHub status POST: HTTP %d for %s@%s", resp.StatusCode, repoPath, cs.CommitSHA)
	}

	log.Printf("CommitStatus: GitHub %s/%s %s → %s", repoPath, cs.CommitSHA[:8], cs.Context, cs.State)
	return nil
}

// postGitLabStatus posts a status to the GitLab Commits API.
// POST /api/v4/projects/{id}/statuses/{sha}
func postGitLabStatus(cs CommitStatus) error {
	// GitLab expects the project path URL-encoded
	projectID := extractGitLabProjectID(cs.RepoRef)
	if projectID == "" {
		return fmt.Errorf("GitLab: could not extract project from %q", cs.RepoRef)
	}

	glState := gitlabState(cs.State)
	url := fmt.Sprintf("https://gitlab.com/api/v4/projects/%s/statuses/%s", projectID, cs.CommitSHA)

	payload := map[string]string{
		"state":       string(glState),
		"name":        cs.Context,
		"description": cs.Description,
	}
	if cs.TargetURL != "" {
		payload["target_url"] = cs.TargetURL
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cs.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("GitLab status POST: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GitLab status POST: HTTP %d", resp.StatusCode)
	}

	log.Printf("CommitStatus: GitLab %s@%s %s → %s", projectID, cs.CommitSHA[:8], cs.Context, cs.State)
	return nil
}

// postBitbucketStatus posts a build status to the Bitbucket Commits API.
// POST /2.0/repositories/{workspace}/{repo_slug}/commit/{sha}/statuses/build
func postBitbucketStatus(cs CommitStatus) error {
	repoPath := extractBitbucketRepoPath(cs.RepoRef)
	if repoPath == "" {
		return fmt.Errorf("Bitbucket: could not extract repo path from %q", cs.RepoRef)
	}

	state := bitbucketState(cs.State)
	url := fmt.Sprintf("https://api.bitbucket.org/2.0/repositories/%s/commit/%s/statuses/build",
		repoPath, cs.CommitSHA)

	payload := map[string]string{
		"state":       state,
		"key":         cs.Context,
		"name":        cs.Context,
		"description": cs.Description,
	}
	if cs.TargetURL != "" {
		payload["url"] = cs.TargetURL
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cs.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("Bitbucket status POST: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Bitbucket status POST: HTTP %d", resp.StatusCode)
	}

	log.Printf("CommitStatus: Bitbucket %s@%s %s → %s", repoPath, cs.CommitSHA[:8], cs.Context, cs.State)
	return nil
}

// ──────────────────────────────────────────────────
// State mapping helpers
// ──────────────────────────────────────────────────

func githubState(s StatusState) string {
	switch s {
	case StateSuccess:
		return "success"
	case StateFailure, StateError:
		return "failure"
	default:
		return "pending"
	}
}

func gitlabState(s StatusState) string {
	switch s {
	case StateSuccess:
		return "success"
	case StateFailure:
		return "failed"
	case StateError:
		return "failed"
	default:
		return "pending"
	}
}

func bitbucketState(s StatusState) string {
	switch s {
	case StateSuccess:
		return "SUCCESSFUL"
	case StateFailure, StateError:
		return "FAILED"
	default:
		return "INPROGRESS"
	}
}

// ──────────────────────────────────────────────────
// Repository path extraction helpers
// ──────────────────────────────────────────────────

// extractGitHubRepoPath extracts "owner/repo" from a git URL or returns it directly.
// Handles:
//   - "git@github.com:owner/repo.git"
//   - "https://github.com/owner/repo.git"
//   - "https://github.com/owner/repo"
//   - "owner/repo"
func extractGitHubRepoPath(s string) string {
	// SSH format
	if strings.HasPrefix(s, "git@github.com:") {
		path := strings.TrimPrefix(s, "git@github.com:")
		return strings.TrimSuffix(path, ".git")
	}
	// HTTPS format
	if idx := strings.Index(s, "github.com/"); idx >= 0 {
		path := s[idx+len("github.com/"):]
		return strings.TrimSuffix(path, ".git")
	}
	// Assume it's already "owner/repo"
	if strings.Contains(s, "/") && !strings.HasPrefix(s, "http") {
		return strings.TrimSuffix(s, ".git")
	}
	return ""
}

// extractGitLabProjectID extracts the URL-encoded project path from a GitLab URL.
func extractGitLabProjectID(s string) string {
	// SSH: git@gitlab.com:namespace/project.git
	if strings.HasPrefix(s, "git@gitlab.com:") {
		path := strings.TrimPrefix(s, "git@gitlab.com:")
		path = strings.TrimSuffix(path, ".git")
		return strings.ReplaceAll(path, "/", "%2F")
	}
	// HTTPS: https://gitlab.com/namespace/project
	if idx := strings.Index(s, "gitlab.com/"); idx >= 0 {
		path := s[idx+len("gitlab.com/"):]
		path = strings.TrimSuffix(path, ".git")
		return strings.ReplaceAll(path, "/", "%2F")
	}
	// Already "namespace/project"
	if strings.Contains(s, "/") && !strings.HasPrefix(s, "http") {
		path := strings.TrimSuffix(s, ".git")
		return strings.ReplaceAll(path, "/", "%2F")
	}
	return ""
}

// extractBitbucketRepoPath extracts "workspace/repo_slug" from a Bitbucket URL.
func extractBitbucketRepoPath(s string) string {
	if strings.Contains(s, "bitbucket.org/") {
		idx := strings.Index(s, "bitbucket.org/")
		path := s[idx+len("bitbucket.org/"):]
		path = strings.TrimSuffix(path, ".git")
		return path
	}
	if strings.HasPrefix(s, "git@bitbucket.org:") {
		path := strings.TrimPrefix(s, "git@bitbucket.org:")
		return strings.TrimSuffix(path, ".git")
	}
	return ""
}
