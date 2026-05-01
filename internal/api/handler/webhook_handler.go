package handler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/terrakube-community/terrakubed/internal/api/tcl"
)

// WebhookHandler handles VCS webhook events from GitHub, GitLab and Bitbucket.
// On a matching push event it creates a Job + Steps and sets status=pending
// so the scheduler picks it up.
type WebhookHandler struct {
	pool         *pgxpool.Pool
	tclProcessor *tcl.Processor
}

// NewWebhookHandler creates a new WebhookHandler.
func NewWebhookHandler(pool *pgxpool.Pool) *WebhookHandler {
	return &WebhookHandler{
		pool:         pool,
		tclProcessor: tcl.NewProcessor(pool),
	}
}

// ServeHTTP routes webhook requests by provider.
//
//	POST /webhook/v1/github/{webhookId}
//	POST /webhook/v1/gitlab/{webhookId}
//	POST /webhook/v1/bitbucket/{webhookId}
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/webhook/v1/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 {
		http.Error(w, "invalid webhook path", http.StatusBadRequest)
		return
	}

	provider := parts[0]
	webhookID := parts[1]

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	switch provider {
	case "github":
		h.handleGitHub(w, r, webhookID, body)
	case "gitlab":
		h.handleGitLab(w, r, webhookID, body)
	case "bitbucket":
		h.handleBitbucket(w, r, webhookID, body)
	default:
		http.Error(w, "unknown provider", http.StatusBadRequest)
	}
}

// ──────────────────────────────────────────────────
// GitHub
// ──────────────────────────────────────────────────

type githubPushEvent struct {
	Ref        string `json:"ref"` // "refs/heads/main"
	HeadCommit struct {
		ID string `json:"id"`
	} `json:"head_commit"`
	Repository struct {
		CloneURL string `json:"clone_url"`
		SSHURL   string `json:"ssh_url"`
	} `json:"repository"`
}

func (h *WebhookHandler) handleGitHub(w http.ResponseWriter, r *http.Request, webhookID string, body []byte) {
	event := r.Header.Get("X-GitHub-Event")
	if event != "push" {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message":"event ignored"}`))
		return
	}

	// Load webhook record to get secret and workspace
	secret, workspaceID, err := h.loadWebhook(r.Context(), webhookID)
	if err != nil {
		log.Printf("GitHub webhook %s not found: %v", webhookID, err)
		http.Error(w, "webhook not found", http.StatusNotFound)
		return
	}

	// Verify HMAC signature
	if secret != "" {
		sig := r.Header.Get("X-Hub-Signature-256")
		if !verifyGitHubSignature(secret, sig, body) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
	}

	var push githubPushEvent
	if err := json.Unmarshal(body, &push); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	// Extract branch from ref (refs/heads/main → main)
	branch := strings.TrimPrefix(push.Ref, "refs/heads/")
	commitID := push.HeadCommit.ID

	if err := h.triggerJob(r.Context(), workspaceID, branch, commitID, "github"); err != nil {
		log.Printf("Failed to trigger job for workspace %s: %v", workspaceID, err)
		http.Error(w, "failed to trigger job", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"message":"job triggered"}`))
}

func verifyGitHubSignature(secret, signature string, body []byte) bool {
	if !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}

// ──────────────────────────────────────────────────
// GitLab
// ──────────────────────────────────────────────────

type gitlabPushEvent struct {
	Ref        string `json:"ref"`
	CheckoutSHA string `json:"checkout_sha"`
}

func (h *WebhookHandler) handleGitLab(w http.ResponseWriter, r *http.Request, webhookID string, body []byte) {
	event := r.Header.Get("X-Gitlab-Event")
	if event != "Push Hook" && event != "Tag Push Hook" {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message":"event ignored"}`))
		return
	}

	secret, workspaceID, err := h.loadWebhook(r.Context(), webhookID)
	if err != nil {
		http.Error(w, "webhook not found", http.StatusNotFound)
		return
	}

	// GitLab sends secret token in X-Gitlab-Token header
	if secret != "" {
		token := r.Header.Get("X-Gitlab-Token")
		if token != secret {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
	}

	var push gitlabPushEvent
	if err := json.Unmarshal(body, &push); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	branch := strings.TrimPrefix(push.Ref, "refs/heads/")
	if err := h.triggerJob(r.Context(), workspaceID, branch, push.CheckoutSHA, "gitlab"); err != nil {
		log.Printf("Failed to trigger job for workspace %s: %v", workspaceID, err)
		http.Error(w, "failed to trigger job", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"message":"job triggered"}`))
}

// ──────────────────────────────────────────────────
// Bitbucket
// ──────────────────────────────────────────────────

type bitbucketPushEvent struct {
	Push struct {
		Changes []struct {
			New struct {
				Name   string `json:"name"`
				Target struct {
					Hash string `json:"hash"`
				} `json:"target"`
			} `json:"new"`
		} `json:"changes"`
	} `json:"push"`
}

func (h *WebhookHandler) handleBitbucket(w http.ResponseWriter, r *http.Request, webhookID string, body []byte) {
	event := r.Header.Get("X-Event-Key")
	if event != "repo:push" {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message":"event ignored"}`))
		return
	}

	_, workspaceID, err := h.loadWebhook(r.Context(), webhookID)
	if err != nil {
		http.Error(w, "webhook not found", http.StatusNotFound)
		return
	}

	var push bitbucketPushEvent
	if err := json.Unmarshal(body, &push); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	for _, change := range push.Push.Changes {
		branch := change.New.Name
		commitID := change.New.Target.Hash
		if branch == "" {
			continue
		}
		if err := h.triggerJob(r.Context(), workspaceID, branch, commitID, "bitbucket"); err != nil {
			log.Printf("Failed to trigger job for workspace %s branch %s: %v", workspaceID, branch, err)
		}
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"message":"job triggered"}`))
}

// ──────────────────────────────────────────────────
// Common helpers
// ──────────────────────────────────────────────────

// loadWebhook looks up a webhook by ID and returns its secret + workspace ID.
func (h *WebhookHandler) loadWebhook(ctx context.Context, webhookID string) (secret, workspaceID string, err error) {
	err = h.pool.QueryRow(ctx,
		`SELECT COALESCE(webhook_token,''), workspace_id::text FROM webhook WHERE id = $1`,
		webhookID,
	).Scan(&secret, &workspaceID)
	if err != nil {
		return "", "", fmt.Errorf("webhook %s: %w", webhookID, err)
	}
	return secret, workspaceID, nil
}

// triggerJob creates a job + steps for the given workspace if the branch matches.
// It mirrors Java's WebhookServiceImpl.createJob().
func (h *WebhookHandler) triggerJob(ctx context.Context, workspaceID, pushBranch, commitID, via string) error {
	// Load workspace — check branch matches and workspace is not locked
	var (
		orgID            string
		wsBranch         string
		locked           bool
		templateRef      string
		defaultTemplate  string
		terraformVersion string
	)
	err := h.pool.QueryRow(ctx, `
		SELECT w.organization_id::text, COALESCE(w.branch,''), w.locked,
		       COALESCE(w.default_template,''), COALESCE(o.default_template,''),
		       COALESCE(w.terraform_version,'')
		FROM workspace w
		JOIN organization o ON w.organization_id = o.id
		WHERE w.id = $1 AND w.deleted = false
	`, workspaceID).Scan(&orgID, &wsBranch, &locked, &templateRef, &defaultTemplate, &terraformVersion)
	if err != nil {
		return fmt.Errorf("workspace %s not found: %w", workspaceID, err)
	}

	// Only trigger if the push branch matches the workspace branch
	if wsBranch != "" && wsBranch != pushBranch {
		log.Printf("Webhook: workspace %s branch=%s does not match push branch=%s, skipping", workspaceID, wsBranch, pushBranch)
		return nil
	}

	if locked {
		log.Printf("Webhook: workspace %s is locked, skipping", workspaceID)
		return nil
	}

	// Resolve template reference
	resolvedTemplate := templateRef
	if resolvedTemplate == "" {
		resolvedTemplate = defaultTemplate
	}

	// Create job
	orgUUID, err := uuid.Parse(orgID)
	if err != nil {
		return fmt.Errorf("invalid org id: %w", err)
	}
	wsUUID, err := uuid.Parse(workspaceID)
	if err != nil {
		return fmt.Errorf("invalid workspace id: %w", err)
	}

	var jobID int
	err = h.pool.QueryRow(ctx, `
		INSERT INTO job (status, output, comments, commit_id, template_reference, via,
		                 refresh, refresh_only, plan_changes, terraform_plan, approval_team,
		                 organization_id, workspace_id)
		VALUES ('pending', '', '', $1, $2, $3,
		        false, false, false, '', '',
		        $4, $5)
		RETURNING id
	`, commitID, resolvedTemplate, via, orgUUID, wsUUID,
	).Scan(&jobID)
	if err != nil {
		return fmt.Errorf("create job: %w", err)
	}

	log.Printf("Webhook (%s): created job %d for workspace %s (branch=%s commit=%s)",
		via, jobID, workspaceID, pushBranch, commitID)

	// Create steps from TCL
	if err := h.tclProcessor.InitJobSteps(ctx, jobID); err != nil {
		log.Printf("Warning: failed to init steps for job %d: %v", jobID, err)
	}

	return nil
}
