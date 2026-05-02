package handler

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/terrakube-community/terrakubed/internal/api/tcl"
	"github.com/terrakube-community/terrakubed/internal/storage"
)

// TerraformStateHandler handles /tfstate/v1/ endpoints.
type TerraformStateHandler struct {
	pool     *pgxpool.Pool
	hostname string
	storage  storage.StorageService
}

// NewTerraformStateHandler creates a new handler.
func NewTerraformStateHandler(pool *pgxpool.Pool, hostname string, storage storage.StorageService) *TerraformStateHandler {
	return &TerraformStateHandler{pool: pool, hostname: hostname, storage: storage}
}

// ServeHTTP routes /tfstate/v1/ requests.
func (h *TerraformStateHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/tfstate/v1/")

	// PUT /tfstate/v1/archive/{archiveId}/terraform.tfstate
	if r.Method == http.MethodPut && strings.HasPrefix(path, "archive/") {
		h.uploadHostedState(w, r, path)
		return
	}

	// GET /tfstate/v1/organization/{orgId}/workspace/{wsId}/state/terraform.tfstate
	// GET /tfstate/v1/organization/{orgId}/workspace/{wsId}/state/{filename}.json
	if r.Method == http.MethodGet && strings.HasPrefix(path, "organization/") {
		h.getState(w, r, path)
		return
	}

	http.Error(w, "Not found", http.StatusNotFound)
}

func (h *TerraformStateHandler) getState(w http.ResponseWriter, r *http.Request, path string) {
	// Parse: organization/{orgId}/workspace/{wsId}/state/{filename}
	parts := strings.Split(path, "/")
	if len(parts) < 6 {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	orgID := parts[1]
	wsID := parts[3]
	stateFile := parts[5]

	log.Printf("Get state: org=%s ws=%s file=%s", orgID, wsID, stateFile)

	// Read from storage backend
	storagePath := fmt.Sprintf("tfstate/%s/%s/%s", orgID, wsID, stateFile)
	reader, err := h.storage.DownloadFile(storagePath)
	if err != nil {
		log.Printf("Error reading state: %v", err)
		http.Error(w, "State not found", http.StatusNotFound)
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	io.Copy(w, reader)
}

func (h *TerraformStateHandler) uploadHostedState(w http.ResponseWriter, r *http.Request, path string) {
	// Parse: archive/{archiveId}/terraform.tfstate
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	archiveID := parts[1]
	log.Printf("Upload hosted state for archive: %s", archiveID)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// Look up archive to find the workspace/org context
	var historyID, orgID, wsID string
	err = h.pool.QueryRow(r.Context(), `
		SELECT a.history_id, o.id, w.id
		FROM temp_archive a
		JOIN history h ON a.history_id = h.id
		JOIN workspace w ON h.workspace_id = w.id
		JOIN organization o ON w.organization_id = o.id
		WHERE a.id = $1
	`, archiveID).Scan(&historyID, &orgID, &wsID)
	if err != nil {
		log.Printf("Archive %s not found: %v", archiveID, err)
		http.Error(w, "Archive not found", http.StatusForbidden)
		return
	}

	// Upload to storage backend
	storagePath := fmt.Sprintf("tfstate/%s/%s/%s.tfstate", orgID, wsID, historyID)
	if err := h.storage.UploadFile(storagePath, bytes.NewReader(body)); err != nil {
		log.Printf("Error uploading state to storage: %v", err)
		http.Error(w, "Failed to upload state", http.StatusInternalServerError)
		return
	}
	log.Printf("Upload state: org=%s ws=%s history=%s (%d bytes)", orgID, wsID, historyID, len(body))

	// Update history with output URL and MD5
	stateHash := fmt.Sprintf("%x", md5.Sum(body))
	outputURL := fmt.Sprintf("https://%s/tfstate/v1/organization/%s/workspace/%s/state/%s.json",
		h.hostname, orgID, wsID, historyID)

	_, err = h.pool.Exec(r.Context(),
		"UPDATE history SET output = $1, md5 = $2 WHERE id = $3",
		outputURL, stateHash, historyID)
	if err != nil {
		log.Printf("Error updating history: %v", err)
	}

	// Delete the archive entry
	_, _ = h.pool.Exec(r.Context(), "DELETE FROM temp_archive WHERE id = $1", archiveID)

	w.WriteHeader(http.StatusCreated)
}

// ──────────────────────────────────────────────────
// TFE Remote State API (/remote/tfe/v2/)
// ──────────────────────────────────────────────────

// RemoteTFEHandler handles TFE-compatible API endpoints.
type RemoteTFEHandler struct {
	pool     *pgxpool.Pool
	hostname string
	storage  storage.StorageService
}

// NewRemoteTFEHandler creates a new handler.
func NewRemoteTFEHandler(pool *pgxpool.Pool, hostname string, storageSvc storage.StorageService) *RemoteTFEHandler {
	return &RemoteTFEHandler{pool: pool, hostname: hostname, storage: storageSvc}
}

// ServeHTTP routes /remote/tfe/v2/ requests.
func (h *RemoteTFEHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/remote/tfe/v2/")
	w.Header().Set("Content-Type", "application/vnd.api+json")

	switch {
	case path == "ping":
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	case strings.HasPrefix(path, "workspaces"):
		h.handleWorkspaces(w, r, path)

	case strings.HasPrefix(path, "state-versions"):
		h.handleStateVersions(w, r, path)

	case strings.HasPrefix(path, "runs"):
		h.handleRuns(w, r, path)

	case strings.HasPrefix(path, "plans"):
		h.handlePlans(w, r, path)

	case strings.HasPrefix(path, "configuration-versions"):
		h.handleConfigVersions(w, r, path)

	default:
		http.Error(w, "Not found", http.StatusNotFound)
	}
}

func (h *RemoteTFEHandler) handleWorkspaces(w http.ResponseWriter, r *http.Request, path string) {
	// GET /remote/tfe/v2/workspaces?search[name]=xxx
	// This is used by terraform CLI to find workspace by name
	parts := strings.Split(path, "/")
	if r.Method == http.MethodGet && len(parts) == 1 {
		name := r.URL.Query().Get("search[name]")
		if name == "" {
			http.Error(w, "search[name] required", http.StatusBadRequest)
			return
		}

		var wsID, orgID string
		var locked bool
		var terraformVersion *string
		err := h.pool.QueryRow(r.Context(),
			"SELECT w.id, w.organization_id, w.locked, w.terraform_version FROM workspace w WHERE w.name = $1 AND w.deleted = false LIMIT 1",
			name,
		).Scan(&wsID, &orgID, &locked, &terraformVersion)
		if err != nil {
			http.Error(w, "Workspace not found", http.StatusNotFound)
			return
		}

		tfVer := ""
		if terraformVersion != nil {
			tfVer = *terraformVersion
		}

		doc := map[string]interface{}{
			"data": []map[string]interface{}{
				{
					"id":   wsID,
					"type": "workspaces",
					"attributes": map[string]interface{}{
						"name":              name,
						"locked":            locked,
						"terraform-version": tfVer,
						"permissions": map[string]bool{
							"can-queue-run":  true,
							"can-lock":       true,
							"can-unlock":     true,
							"can-read-state": true,
						},
					},
				},
			},
		}
		json.NewEncoder(w).Encode(doc)
		return
	}
	http.Error(w, "Not found", http.StatusNotFound)
}

func (h *RemoteTFEHandler) handleStateVersions(w http.ResponseWriter, r *http.Request, path string) {
	// POST /remote/tfe/v2/state-versions — create a new state version
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Failed to read body", http.StatusBadRequest)
			return
		}
		defer r.Body.Close()

		log.Printf("Create state version: %d bytes", len(body))

		// Parse the incoming state version request
		var req struct {
			Data struct {
				Type       string `json:"type"`
				Attributes struct {
					Serial  int    `json:"serial"`
					MD5     string `json:"md5"`
					Lineage string `json:"lineage"`
					State   string `json:"state"`
				} `json:"attributes"`
				Relationships struct {
					Workspace struct {
						Data struct {
							ID string `json:"id"`
						} `json:"data"`
					} `json:"workspace"`
				} `json:"relationships"`
			} `json:"data"`
		}

		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		// Create history entry and archive for state upload
		historyID := uuid.New()
		archiveID := uuid.New()

		_, err = h.pool.Exec(r.Context(), `
			INSERT INTO history (id, workspace_id, serial, md5, lineage, job_reference, output)
			VALUES ($1, $2, $3, $4, $5, '', '')
		`, historyID, req.Data.Relationships.Workspace.Data.ID,
			req.Data.Attributes.Serial, req.Data.Attributes.MD5, req.Data.Attributes.Lineage)
		if err != nil {
			log.Printf("Error creating history: %v", err)
			http.Error(w, "Failed to create state version", http.StatusInternalServerError)
			return
		}

		_, err = h.pool.Exec(r.Context(), `
			INSERT INTO temp_archive (id, type, history_id) VALUES ($1, 'state', $2)
		`, archiveID, historyID)
		if err != nil {
			log.Printf("Error creating archive: %v", err)
		}

		uploadURL := fmt.Sprintf("https://%s/tfstate/v1/archive/%s/terraform.tfstate", h.hostname, archiveID)

		doc := map[string]interface{}{
			"data": map[string]interface{}{
				"id":   historyID.String(),
				"type": "state-versions",
				"attributes": map[string]interface{}{
					"upload-url":              uploadURL,
					"hosted-state-upload-url": uploadURL,
					"serial":                  req.Data.Attributes.Serial,
				},
			},
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(doc)
		return
	}
	http.Error(w, "Not found", http.StatusNotFound)
}

// ──────────────────────────────────────────────────
// Runs API (used by terraform CLI remote backend)
// ──────────────────────────────────────────────────

// jobStatusToTFE maps internal Terrakube job statuses to TFE run statuses.
// The terraform CLI uses TFE status to know when to prompt / poll / error.
func jobStatusToTFE(jobStatus, currentStepType string) string {
	switch jobStatus {
	case "pending":
		return "pending"
	case "queue":
		return "plan_queued"
	case "running":
		if strings.Contains(currentStepType, "Apply") || strings.Contains(currentStepType, "Destroy") {
			return "applying"
		}
		return "planning"
	case "waitingApproval":
		return "planned"
	case "noChanges":
		return "planned_and_finished"
	case "completed":
		return "applied"
	case "failed":
		return "errored"
	case "cancelled", "rejected":
		return "discarded"
	default:
		return "pending"
	}
}

func (h *RemoteTFEHandler) handleRuns(w http.ResponseWriter, r *http.Request, path string) {
	// Strip "runs" prefix to get sub-path
	sub := strings.TrimPrefix(path, "runs")
	sub = strings.TrimPrefix(sub, "/")

	switch {
	// POST /runs — create a new run
	case r.Method == http.MethodPost && sub == "":
		h.createRun(w, r)

	// GET /runs/{runId} — get run status
	case r.Method == http.MethodGet && sub != "" && !strings.Contains(sub, "/"):
		h.getRun(w, r, sub)

	// POST /runs/{runId}/actions/apply — approve the run
	case r.Method == http.MethodPost && strings.HasSuffix(sub, "/actions/apply"):
		runID := strings.TrimSuffix(sub, "/actions/apply")
		h.applyRun(w, r, runID)

	// POST /runs/{runId}/actions/discard — discard/cancel the run
	case r.Method == http.MethodPost && strings.HasSuffix(sub, "/actions/discard"):
		runID := strings.TrimSuffix(sub, "/actions/discard")
		h.discardRun(w, r, runID)

	// GET /runs/{runId}/plan — get plan details for a run
	case r.Method == http.MethodGet && strings.HasSuffix(sub, "/plan"):
		runID := strings.TrimSuffix(sub, "/plan")
		h.getRunPlan(w, r, runID)

	default:
		log.Printf("TFE runs: unhandled %s %s", r.Method, path)
		http.Error(w, "Not found", http.StatusNotFound)
	}
}

// createRun handles POST /remote/tfe/v2/runs
// Terraform CLI sends this after uploading the configuration version.
func (h *RemoteTFEHandler) createRun(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var req struct {
		Data struct {
			Type       string `json:"type"`
			Attributes struct {
				Message   string `json:"message"`
				IsDestroy bool   `json:"is-destroy"`
			} `json:"attributes"`
			Relationships struct {
				Workspace struct {
					Data struct{ ID string `json:"id"` } `json:"data"`
				} `json:"workspace"`
				ConfigurationVersion struct {
					Data struct{ ID string `json:"id"` } `json:"data"`
				} `json:"configuration-version"`
			} `json:"relationships"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	wsID := req.Data.Relationships.Workspace.Data.ID
	cvID := req.Data.Relationships.ConfigurationVersion.Data.ID

	log.Printf("TFE createRun: workspace=%s configVersion=%s message=%q",
		wsID, cvID, req.Data.Attributes.Message)

	// Look up workspace → org, template
	var orgID, defaultTemplate string
	var locked bool
	err = h.pool.QueryRow(r.Context(), `
		SELECT w.organization_id::text, COALESCE(w.default_template,''), w.locked
		FROM workspace w WHERE w.id = $1 AND w.deleted = false
	`, wsID).Scan(&orgID, &defaultTemplate, &locked)
	if err != nil {
		log.Printf("TFE createRun: workspace %s not found: %v", wsID, err)
		http.Error(w, "workspace not found", http.StatusNotFound)
		return
	}
	if locked {
		http.Error(w, "workspace is locked", http.StatusConflict)
		return
	}

	// override_source: the download URL the executor fetches the uploaded tar.gz from.
	// override_branch: "remote-content" tells the executor this is a CLI upload (not git).
	overrideSource := fmt.Sprintf("https://%s/remote/tfe/v2/configuration-versions/%s/terraformContent.tar.gz", h.hostname, cvID)
	overrideBranch := "remote-content"

	var jobID int
	err = h.pool.QueryRow(r.Context(), `
		INSERT INTO job (status, output, comments, commit_id, template_reference, via,
		                 refresh, refresh_only, plan_changes, terraform_plan, approval_team,
		                 organization_id, workspace_id, override_source, override_branch)
		VALUES ('pending', '', $1, '', $2, 'cli',
		        false, false, false, '', '',
		        $3, $4, $5, $6)
		RETURNING id
	`, req.Data.Attributes.Message, defaultTemplate, orgID, wsID, overrideSource, overrideBranch).Scan(&jobID)
	if err != nil {
		log.Printf("TFE createRun: failed to create job: %v", err)
		http.Error(w, "failed to create run", http.StatusInternalServerError)
		return
	}

	// Init TCL steps async
	if defaultTemplate != "" {
		go func() {
			proc := tcl.NewProcessor(h.pool)
			if err := proc.InitJobSteps(context.Background(), jobID); err != nil {
				log.Printf("TFE createRun: failed to init steps for job %d: %v", jobID, err)
			}
		}()
	}

	runID := fmt.Sprintf("run-%d", jobID)
	doc := h.buildRunDoc(runID, jobID, "pending", wsID, "planning")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(doc)
}

// getRun handles GET /remote/tfe/v2/runs/{runId}
func (h *RemoteTFEHandler) getRun(w http.ResponseWriter, r *http.Request, runID string) {
	jobID, err := parseRunID(runID)
	if err != nil {
		http.Error(w, "invalid run id", http.StatusBadRequest)
		return
	}

	var jobStatus, wsID string
	var jobTCL *string
	err = h.pool.QueryRow(r.Context(), `
		SELECT j.status, j.workspace_id::text, j.tcl
		FROM job j WHERE j.id = $1
	`, jobID).Scan(&jobStatus, &wsID, &jobTCL)
	if err != nil {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}

	// Determine current step type for accurate status mapping
	currentStepType := h.currentStepType(r.Context(), jobID, derefStr(jobTCL))
	tfeStatus := jobStatusToTFE(jobStatus, currentStepType)

	doc := h.buildRunDoc(runID, jobID, tfeStatus, wsID, currentStepType)
	json.NewEncoder(w).Encode(doc)
}

// applyRun handles POST /remote/tfe/v2/runs/{runId}/actions/apply
func (h *RemoteTFEHandler) applyRun(w http.ResponseWriter, r *http.Request, runID string) {
	jobID, err := parseRunID(runID)
	if err != nil {
		http.Error(w, "invalid run id", http.StatusBadRequest)
		return
	}

	// Find waitingApproval step and approve it
	var stepID string
	err = h.pool.QueryRow(r.Context(),
		`SELECT id FROM step WHERE job_id = $1 AND status = 'waitingApproval' LIMIT 1`, jobID,
	).Scan(&stepID)
	if err != nil {
		// No waitingApproval step — job may already be running/applying
		log.Printf("TFE applyRun: no waitingApproval step for job %d: %v", jobID, err)
		w.WriteHeader(http.StatusOK)
		return
	}

	h.pool.Exec(r.Context(), "UPDATE step SET status = 'completed' WHERE id = $1", stepID)
	h.pool.Exec(r.Context(), "UPDATE job SET status = 'queue' WHERE id = $1", jobID)

	log.Printf("TFE: run %s (job %d) approved via API", runID, jobID)
	w.WriteHeader(http.StatusOK)
}

// discardRun handles POST /remote/tfe/v2/runs/{runId}/actions/discard
func (h *RemoteTFEHandler) discardRun(w http.ResponseWriter, r *http.Request, runID string) {
	jobID, err := parseRunID(runID)
	if err != nil {
		http.Error(w, "invalid run id", http.StatusBadRequest)
		return
	}

	h.pool.Exec(r.Context(), "UPDATE step SET status = 'rejected' WHERE job_id = $1 AND status = 'waitingApproval'", jobID)
	h.pool.Exec(r.Context(), "UPDATE step SET status = 'notExecuted' WHERE job_id = $1 AND status = 'pending'", jobID)
	h.pool.Exec(r.Context(), "UPDATE job SET status = 'cancelled' WHERE id = $1", jobID)

	log.Printf("TFE: run %s (job %d) discarded", runID, jobID)
	w.WriteHeader(http.StatusOK)
}

// getRunPlan handles GET /remote/tfe/v2/runs/{runId}/plan
func (h *RemoteTFEHandler) getRunPlan(w http.ResponseWriter, r *http.Request, runID string) {
	jobID, err := parseRunID(runID)
	if err != nil {
		http.Error(w, "invalid run id", http.StatusBadRequest)
		return
	}
	planID := fmt.Sprintf("plan-%d", jobID)
	h.writePlanDoc(w, r.Context(), planID, jobID)
}

// buildRunDoc builds a TFE-compatible run response document.
func (h *RemoteTFEHandler) buildRunDoc(runID string, jobID int, tfeStatus, wsID, currentStepType string) map[string]interface{} {
	planID := fmt.Sprintf("plan-%d", jobID)

	var hasChanges bool
	if tfeStatus == "planned" || tfeStatus == "applying" || tfeStatus == "applied" {
		hasChanges = true
	}

	return map[string]interface{}{
		"data": map[string]interface{}{
			"id":   runID,
			"type": "runs",
			"attributes": map[string]interface{}{
				"status":       tfeStatus,
				"has-changes":  hasChanges,
				"is-destroy":   strings.Contains(currentStepType, "Destroy"),
				"message":      "",
				"status-timestamps": map[string]string{},
				"actions": map[string]interface{}{
					"is-confirmable": tfeStatus == "planned",
					"is-discardable": tfeStatus == "planned" || tfeStatus == "pending",
					"is-cancelable":  tfeStatus == "planning" || tfeStatus == "applying",
				},
				"permissions": map[string]bool{
					"can-apply":   true,
					"can-cancel":  true,
					"can-discard": true,
					"can-force-execute": true,
				},
			},
			"relationships": map[string]interface{}{
				"workspace": map[string]interface{}{
					"data": map[string]interface{}{"id": wsID, "type": "workspaces"},
				},
				"plan": map[string]interface{}{
					"data": map[string]interface{}{"id": planID, "type": "plans"},
					"links": map[string]string{
						"related": fmt.Sprintf("/remote/tfe/v2/plans/%s", planID),
					},
				},
			},
		},
	}
}

// ──────────────────────────────────────────────────
// Plans API
// ──────────────────────────────────────────────────

func (h *RemoteTFEHandler) handlePlans(w http.ResponseWriter, r *http.Request, path string) {
	sub := strings.TrimPrefix(path, "plans")
	sub = strings.TrimPrefix(sub, "/")

	parts := strings.SplitN(sub, "/", 2)
	planID := parts[0]
	var suffix string
	if len(parts) == 2 {
		suffix = parts[1]
	}

	if planID == "" {
		http.Error(w, "plan id required", http.StatusBadRequest)
		return
	}

	jobID, err := parsePlanID(planID)
	if err != nil {
		http.Error(w, "invalid plan id", http.StatusBadRequest)
		return
	}

	// GET /plans/{planId}/log — serve plan log
	if r.Method == http.MethodGet && suffix == "log" {
		h.getPlanLog(w, r, jobID)
		return
	}

	// GET /plans/{planId} — return plan status
	if r.Method == http.MethodGet {
		h.writePlanDoc(w, r.Context(), planID, jobID)
		return
	}

	http.Error(w, "Not found", http.StatusNotFound)
}

// writePlanDoc writes a TFE plan response.
func (h *RemoteTFEHandler) writePlanDoc(w http.ResponseWriter, ctx context.Context, planID string, jobID int) {
	// Determine plan status from job / step status
	var jobStatus string
	var orgID, wsID string
	err := h.pool.QueryRow(ctx,
		"SELECT status, organization_id::text, workspace_id::text FROM job WHERE id = $1", jobID,
	).Scan(&jobStatus, &orgID, &wsID)
	if err != nil {
		http.Error(w, "plan not found", http.StatusNotFound)
		return
	}

	// Look up plan step (first terraform step)
	var planStepID, planStepStatus string
	_ = h.pool.QueryRow(ctx,
		`SELECT id::text, status FROM step WHERE job_id = $1 ORDER BY step_number ASC LIMIT 1`,
		jobID,
	).Scan(&planStepID, &planStepStatus)

	planStatus := "pending"
	switch planStepStatus {
	case "running":
		planStatus = "running"
	case "completed":
		planStatus = "finished"
	case "failed":
		planStatus = "errored"
	}

	logReadURL := fmt.Sprintf("https://%s/remote/tfe/v2/plans/%s/log", h.hostname, planID)

	doc := map[string]interface{}{
		"data": map[string]interface{}{
			"id":   planID,
			"type": "plans",
			"attributes": map[string]interface{}{
				"status":       planStatus,
				"log-read-url": logReadURL,
				"resource-additions":    0,
				"resource-changes":      0,
				"resource-destructions": 0,
			},
			"links": map[string]string{
				"self": fmt.Sprintf("/remote/tfe/v2/plans/%s", planID),
			},
		},
	}
	json.NewEncoder(w).Encode(doc)
}

// getPlanLog serves the plan step log — tries Redis then storage.
func (h *RemoteTFEHandler) getPlanLog(w http.ResponseWriter, r *http.Request, jobID int) {
	// Get plan step details
	var stepID, orgID, wsID string
	err := h.pool.QueryRow(r.Context(), `
		SELECT s.id::text, j.organization_id::text, j.workspace_id::text
		FROM step s JOIN job j ON s.job_id = j.id
		WHERE s.job_id = $1 ORDER BY s.step_number ASC LIMIT 1
	`, jobID).Scan(&stepID, &orgID, &wsID)
	if err != nil {
		// No steps yet — return empty log
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		return
	}

	// Try storage path: tfoutput/{orgId}/{jobId}/{stepId}.tfoutput
	storagePath := fmt.Sprintf("tfoutput/%s/%d/%s.tfoutput", orgID, jobID, stepID)
	reader, err := h.storage.DownloadFile(storagePath)
	if err == nil {
		defer reader.Close()
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		io.Copy(w, reader)
		return
	}

	// Not yet available — return empty body (CLI will retry)
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
}

// ──────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────

// parseRunID extracts the numeric job ID from "run-{jobId}" or plain "{jobId}".
func parseRunID(runID string) (int, error) {
	s := strings.TrimPrefix(runID, "run-")
	return strconv.Atoi(s)
}

// parsePlanID extracts the numeric job ID from "plan-{jobId}" or plain "{jobId}".
func parsePlanID(planID string) (int, error) {
	s := strings.TrimPrefix(planID, "plan-")
	return strconv.Atoi(s)
}

// currentStepType returns the type of the currently running (or next pending) step.
func (h *RemoteTFEHandler) currentStepType(ctx context.Context, jobID int, jobTCL string) string {
	var stepNumber int
	err := h.pool.QueryRow(ctx,
		`SELECT step_number FROM step WHERE job_id = $1 AND status IN ('running','pending') ORDER BY step_number ASC LIMIT 1`,
		jobID,
	).Scan(&stepNumber)
	if err != nil {
		return "terraformPlan"
	}
	if jobTCL == "" {
		return "terraformPlan"
	}
	flow, err := tcl.ParseFlow(jobTCL)
	if err != nil {
		return "terraformPlan"
	}
	for _, f := range flow {
		if f.Step == stepNumber {
			return f.Type
		}
	}
	return "terraformPlan"
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (h *RemoteTFEHandler) handleConfigVersions(w http.ResponseWriter, r *http.Request, path string) {
	// path examples:
	//   "configuration-versions"                              → POST  (create)
	//   "configuration-versions/<id>"                         → PUT   (upload tar.gz)
	//   "configuration-versions/<id>/terraformContent.tar.gz" → GET   (download tar.gz)
	log.Printf("TFE config-versions: %s %s", r.Method, path)

	parts := strings.SplitN(strings.TrimPrefix(path, "configuration-versions"), "/", 3)
	// parts[0] is always "" (prefix stripped), parts[1] is the ID (if present), parts[2] is suffix

	// POST /remote/tfe/v2/configuration-versions — create a new config version
	if r.Method == http.MethodPost && (len(parts) < 2 || parts[1] == "") {
		id := uuid.New().String()
		uploadURL := fmt.Sprintf("https://%s/remote/tfe/v2/configuration-versions/%s", h.hostname, id)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{
				"id":   id,
				"type": "configuration-versions",
				"attributes": map[string]interface{}{
					"status":     "pending",
					"upload-url": uploadURL,
				},
			},
		})
		return
	}

	if len(parts) < 2 || parts[1] == "" {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	id := parts[1]
	suffix := ""
	if len(parts) == 3 {
		suffix = parts[2]
	}

	storageKey := fmt.Sprintf("cli-uploads/%s/content.tar.gz", id)

	// PUT /remote/tfe/v2/configuration-versions/<id> — receive and store the tar.gz
	if r.Method == http.MethodPut && suffix == "" {
		body, err := io.ReadAll(r.Body)
		defer r.Body.Close()
		if err != nil {
			http.Error(w, "Failed to read body", http.StatusBadRequest)
			return
		}
		if err := h.storage.UploadFile(storageKey, bytes.NewReader(body)); err != nil {
			log.Printf("Config version upload failed (%s): %v", id, err)
			http.Error(w, "Failed to store config", http.StatusInternalServerError)
			return
		}
		log.Printf("Config version stored: id=%s (%d bytes) → %s", id, len(body), storageKey)
		w.WriteHeader(http.StatusOK)
		return
	}

	// GET /remote/tfe/v2/configuration-versions/<id>/terraformContent.tar.gz — serve tar.gz
	if r.Method == http.MethodGet && suffix == "terraformContent.tar.gz" {
		reader, err := h.storage.DownloadFile(storageKey)
		if err != nil {
			log.Printf("Config version not found (%s): %v", id, err)
			http.Error(w, "Config version not found", http.StatusNotFound)
			return
		}
		defer reader.Close()
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s.tar.gz\"", id))
		w.WriteHeader(http.StatusOK)
		io.Copy(w, reader)
		return
	}

	// GET /remote/tfe/v2/configuration-versions/<id> — return status
	if r.Method == http.MethodGet {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{
				"id":   id,
				"type": "configuration-versions",
				"attributes": map[string]interface{}{
					"status": "uploaded",
				},
			},
		})
		return
	}

	http.Error(w, "Not found", http.StatusNotFound)
}

// ──────────────────────────────────────────────────
// .well-known/terraform.json
// ──────────────────────────────────────────────────

// WellKnownHandler serves /.well-known/terraform.json.
type WellKnownHandler struct {
	hostname string
}

// NewWellKnownHandler creates a new handler.
func NewWellKnownHandler(hostname string) *WellKnownHandler {
	return &WellKnownHandler{hostname: hostname}
}

func (h *WellKnownHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	doc := map[string]interface{}{
		"modules.v1":   fmt.Sprintf("https://%s/registry/v1/modules/", h.hostname),
		"providers.v1": fmt.Sprintf("https://%s/registry/v1/providers/", h.hostname),
		"tfe.v2":       fmt.Sprintf("https://%s/remote/tfe/v2/", h.hostname),
		"tfe.v2.1":     fmt.Sprintf("https://%s/remote/tfe/v2/", h.hostname),
		"motd.v1":      fmt.Sprintf("https://%s/remote/tfe/v2/motd", h.hostname),
	}
	json.NewEncoder(w).Encode(doc)
}
