package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/terrakube-community/terrakubed/internal/api/middleware"
)

// This file ports the state-version routes of Java's RemoteTfeController that
// the Terraform CLI uses for `state` commands and remote state reads:
//
//	GET  /workspaces/{id}/current-state-version
//	GET  /state-versions/{historyId}
//	POST /workspaces/{id}/state-versions
//
// Without them `terraform state rm` says "No state file was found!" because
// the CLI's read of the current state version 404s.

// workspaceSubPath matches workspaces/{id}/{sub}.
func workspaceSubPath(parts []string, sub string) (wsID string, ok bool) {
	if len(parts) == 3 && parts[0] == "workspaces" && parts[1] != "" && parts[2] == sub {
		return parts[1], true
	}
	return "", false
}

// decodeStateAttribute decodes a base64 attribute the way Java's MIME decoder
// does: tolerant of embedded line breaks.
func decodeStateAttribute(s string) ([]byte, error) {
	cleaned := strings.NewReplacer("\r", "", "\n", "", " ", "").Replace(s)
	return base64.StdEncoding.DecodeString(cleaned)
}

type stateSummary struct {
	Serial           int64
	Lineage          string
	TerraformVersion string
}

// parseStateSummary validates that raw is a Terraform state document before we
// let it near the live state, and extracts the fields the TFE API reports.
func parseStateSummary(raw []byte) (stateSummary, error) {
	var doc struct {
		Serial           *int64 `json:"serial"`
		Lineage          string `json:"lineage"`
		TerraformVersion string `json:"terraform_version"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return stateSummary{}, fmt.Errorf("state is not valid JSON: %w", err)
	}
	if doc.Serial == nil {
		return stateSummary{}, fmt.Errorf("state has no serial")
	}
	return stateSummary{Serial: *doc.Serial, Lineage: doc.Lineage, TerraformVersion: doc.TerraformVersion}, nil
}

func (h *RemoteTFEHandler) stateDownloadURL(orgID, wsID string) string {
	return fmt.Sprintf("https://%s/tfstate/v1/organization/%s/workspace/%s/state/terraform.tfstate", h.hostname, orgID, wsID)
}

func (h *RemoteTFEHandler) stateJSONURL(orgID, wsID, historyID string) string {
	return fmt.Sprintf("https://%s/tfstate/v1/organization/%s/workspace/%s/state/%s.json", h.hostname, orgID, wsID, historyID)
}

func (h *RemoteTFEHandler) workspaceOrg(ctx context.Context, wsID string) (string, bool) {
	var orgID string
	if err := h.pool.QueryRow(ctx,
		`SELECT organization_id::text FROM workspace WHERE id = $1 AND deleted = false`, wsID).Scan(&orgID); err != nil {
		return "", false
	}
	return orgID, true
}

// getCurrentStateVersion serves GET /workspaces/{id}/current-state-version.
// A workspace with no stored state is a 404, which is what the CLI expects for
// a brand-new workspace.
func (h *RemoteTFEHandler) getCurrentStateVersion(w http.ResponseWriter, r *http.Request, wsID string) {
	orgID, ok := h.workspaceOrg(r.Context(), wsID)
	if !ok {
		http.Error(w, "Workspace not found", http.StatusNotFound)
		return
	}

	reader, err := h.storage.DownloadFile(fmt.Sprintf("tfstate/%s/%s/terraform.tfstate", orgID, wsID))
	if err != nil {
		http.Error(w, "No state", http.StatusNotFound)
		return
	}
	defer reader.Close()
	raw, err := io.ReadAll(reader)
	if err != nil || len(raw) == 0 {
		http.Error(w, "No state", http.StatusNotFound)
		return
	}

	sum, err := parseStateSummary(raw)
	if err != nil {
		log.Printf("TFE current-state-version: workspace %s has unreadable state: %v", wsID, err)
		http.Error(w, "stored state is not readable", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"data": map[string]interface{}{
			"id":   uuid.New().String(),
			"type": "state-versions",
			"attributes": map[string]interface{}{
				"vcs-commit-url":            nil,
				"vcs-commit-sha":            nil,
				"hosted-state-download-url": h.stateDownloadURL(orgID, wsID),
				"serial":                    sum.Serial,
				"terraform-version":         sum.TerraformVersion,
				"status":                    "finalized",
			},
		},
	})
}

// getStateVersion serves GET /state-versions/{historyId}.
func (h *RemoteTFEHandler) getStateVersion(w http.ResponseWriter, r *http.Request, historyID string) {
	var orgID, wsID, output string
	err := h.pool.QueryRow(r.Context(),
		`SELECT w.organization_id::text, h.workspace_id::text, COALESCE(h.output, '')
		 FROM history h JOIN workspace w ON h.workspace_id = w.id WHERE h.id = $1`, historyID,
	).Scan(&orgID, &wsID, &output)
	if err != nil {
		http.Error(w, "State version not found", http.StatusNotFound)
		return
	}
	h.writeStateVersionDoc(w, r.Context(), http.StatusOK, historyID, orgID, wsID, output != "")
}

func (h *RemoteTFEHandler) writeStateVersionDoc(w http.ResponseWriter, ctx context.Context, code int, historyID, orgID, wsID string, finalized bool) {
	attrs := map[string]interface{}{
		"vcs-commit-sha":                 nil,
		"vcs-commit-url":                 nil,
		"hosted-state-download-url":      h.stateDownloadURL(orgID, wsID),
		"hosted-json-state-download-url": h.stateJSONURL(orgID, wsID, historyID),
		"status":                         "pending",
	}
	if finalized {
		attrs["status"] = "finalized"
	} else {
		var archiveID string
		if err := h.pool.QueryRow(ctx, `SELECT id::text FROM temp_archive WHERE history_id = $1 LIMIT 1`, historyID).Scan(&archiveID); err == nil {
			attrs["hosted-state-upload-url"] = fmt.Sprintf("https://%s/tfstate/v1/archive/%s/terraform.tfstate", h.hostname, archiveID)
		}
	}
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"data": map[string]interface{}{"id": historyID, "type": "state-versions", "attributes": attrs},
	})
}

// canManageState mirrors Java's validateUserManageState: internal service
// tokens and the owner group may always write state; anyone else needs a team
// in the workspace's organization with manage_state, or a workspace access
// record granting it.
//
// Deliberately NOT middleware's IsServiceAccount(): that also covers every
// PAT, which would make this check pass for any token. A PAT is judged by the
// groups claim it carries, like an OIDC user.
func (h *RemoteTFEHandler) canManageState(ctx context.Context, orgID, wsID string) bool {
	user := middleware.GetUser(ctx)
	if user == nil {
		return false
	}
	if user.IsInternal() || (h.ownerGroup != "" && user.IsMember(h.ownerGroup)) {
		return true
	}
	if len(user.Groups) == 0 {
		return false
	}
	var ok bool
	if err := h.pool.QueryRow(ctx,
		`SELECT COALESCE(bool_or(manage_state), false) FROM team
		 WHERE organization_id = $1 AND name = ANY($2::text[])`, orgID, user.Groups).Scan(&ok); err == nil && ok {
		return true
	}
	return middleware.UserHasWorkspaceAccess(ctx, h.pool, wsID, "manage_state", user.Groups)
}

// createWorkspaceStateVersion serves POST /workspaces/{id}/state-versions,
// mirroring Java's createWorkspaceState: the CLI sends the new state inline
// (base64 "state"), or without it and uploads to the returned upload URL.
//
// This writes the live state, so: the state is validated before anything is
// touched, the history copies are written first, and the canonical
// terraform.tfstate (what remote runs read) is written last, after the
// database rows are committed. A failure therefore never leaves the live
// state half-written.
func (h *RemoteTFEHandler) createWorkspaceStateVersion(w http.ResponseWriter, r *http.Request, wsID string) {
	orgID, ok := h.workspaceOrg(r.Context(), wsID)
	if !ok {
		http.Error(w, "Workspace not found", http.StatusNotFound)
		return
	}
	if !h.canManageState(r.Context(), orgID, wsID) {
		log.Printf("TFE create state version: denied for workspace %s (no manage_state)", wsID)
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"errors": []map[string]string{{"status": "403", "title": "forbidden",
				"detail": "You do not have permission to manage state in this workspace."}},
		})
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	var req struct {
		Data struct {
			Attributes struct {
				Serial    *int64 `json:"serial"`
				MD5       string `json:"md5"`
				Lineage   string `json:"lineage"`
				State     string `json:"state"`
				JSONState string `json:"json-state"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	attrs := req.Data.Attributes

	var rawState []byte
	if attrs.State != "" {
		if rawState, err = decodeStateAttribute(attrs.State); err != nil {
			http.Error(w, "state is not valid base64", http.StatusUnprocessableEntity)
			return
		}
		if _, err := parseStateSummary(rawState); err != nil {
			log.Printf("TFE create state version: rejecting state for workspace %s: %v", wsID, err)
			http.Error(w, "state is not a valid Terraform state: "+err.Error(), http.StatusUnprocessableEntity)
			return
		}
	}
	jsonState := []byte("{}")
	if attrs.JSONState != "" {
		if jsonState, err = decodeStateAttribute(attrs.JSONState); err != nil {
			http.Error(w, "json-state is not valid base64", http.StatusUnprocessableEntity)
			return
		}
	}
	serial := int64(1)
	if attrs.Serial != nil {
		serial = *attrs.Serial
	}

	historyID := uuid.New().String()
	prefix := fmt.Sprintf("tfstate/%s/%s", orgID, wsID)

	if rawState != nil {
		if err := h.storage.UploadFile(fmt.Sprintf("%s/state/%s.raw.json", prefix, historyID), bytes.NewReader(rawState)); err != nil {
			log.Printf("TFE create state version: raw copy upload failed: %v", err)
			http.Error(w, "failed to store state", http.StatusInternalServerError)
			return
		}
	}
	if err := h.storage.UploadFile(fmt.Sprintf("%s/state/%s.json", prefix, historyID), bytes.NewReader(jsonState)); err != nil {
		log.Printf("TFE create state version: json upload failed: %v", err)
		http.Error(w, "failed to store state", http.StatusInternalServerError)
		return
	}

	tx, err := h.pool.Begin(r.Context())
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(r.Context())

	// Like Java, record the CLI write as a completed placeholder job so the
	// history row has something to point at.
	var jobID int
	if err := tx.QueryRow(r.Context(), `
		INSERT INTO job (status, output, comments, commit_id, template_reference, via,
		                 refresh, refresh_only, plan_changes, terraform_plan, approval_team,
		                 organization_id, workspace_id)
		VALUES ('completed', '', '', '', '', 'cli', true, false, true, '', '', $1, $2)
		RETURNING id`, orgID, wsID).Scan(&jobID); err != nil {
		log.Printf("TFE create state version: placeholder job failed: %v", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(r.Context(),
		`INSERT INTO step (id, step_number, name, status, output, job_id)
		 VALUES ($1, 100, 'Dummy State Uploaded', 'completed', '', $2)`, uuid.New(), jobID); err != nil {
		log.Printf("TFE create state version: placeholder step failed: %v", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	output := ""
	if rawState != nil {
		output = h.stateJSONURL(orgID, wsID, historyID)
	}
	if _, err := tx.Exec(r.Context(),
		`INSERT INTO history (id, workspace_id, serial, md5, lineage, job_reference, output)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		historyID, wsID, serial, attrs.MD5, attrs.Lineage, fmt.Sprint(jobID), output); err != nil {
		log.Printf("TFE create state version: history failed: %v", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if rawState == nil {
		if _, err := tx.Exec(r.Context(),
			`INSERT INTO temp_archive (id, type, history_id) VALUES ($1, 'state', $2)`, uuid.New(), historyID); err != nil {
			log.Printf("TFE create state version: archive failed: %v", err)
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	// Last: the canonical state that remote runs read.
	if rawState != nil {
		if err := h.storage.UploadFile(prefix+"/terraform.tfstate", bytes.NewReader(rawState)); err != nil {
			log.Printf("TFE create state version: CANONICAL STATE WRITE FAILED for workspace %s (history %s recorded): %v", wsID, historyID, err)
			http.Error(w, "failed to store state", http.StatusInternalServerError)
			return
		}
		log.Printf("TFE: workspace %s state updated via CLI (serial %d, history %s)", wsID, serial, historyID)
	}

	h.writeStateVersionDoc(w, r.Context(), http.StatusCreated, historyID, orgID, wsID, rawState != nil)
}
