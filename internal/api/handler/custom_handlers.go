package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/redis/go-redis/v9"
	"github.com/terrakube-community/terrakubed/internal/api/repository"
	"github.com/terrakube-community/terrakubed/internal/api/streaming"
	"github.com/terrakube-community/terrakubed/internal/storage"
)

// LogsHandler handles the /logs endpoint for Redis log streaming.
type LogsHandler struct {
	repo  *repository.GenericRepository
	redis *redis.Client // optional; live streaming disabled when nil
}

// NewLogsHandler creates a new LogsHandler.
func NewLogsHandler(repo *repository.GenericRepository) *LogsHandler {
	return &LogsHandler{repo: repo}
}

// WithRedis wires an optional Redis client for live log streaming.
func (h *LogsHandler) WithRedis(client *redis.Client) *LogsHandler {
	h.redis = client
	return h
}

// SetupConsumerGroups handles POST /logs/{jobId}/setup-consumer-groups
// Creates the Redis consumer groups that the Java streaming service expects.
func (h *LogsHandler) SetupConsumerGroups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract jobId from path
	path := strings.TrimPrefix(r.URL.Path, "/logs/")
	jobID := strings.TrimSuffix(path, "/setup-consumer-groups")

	if h.redis != nil && jobID != "" {
		streamKey := streaming.JDKSerialize(jobID)
		ctx := r.Context()
		_ = h.redis.XGroupCreateMkStream(ctx, streamKey, "CLI", "0").Err()
		_ = h.redis.XGroupCreateMkStream(ctx, streamKey, "UI", "0").Err()
		log.Printf("LogsHandler: created consumer groups for job %s", jobID)
	}

	w.WriteHeader(http.StatusOK)
}

// AppendLogs handles POST /logs — receives log lines from executors and writes to Redis.
// The Java executor posts here; the Go executor writes directly to Redis.
// Path: POST /logs (body: JSON with data array)
func (h *LogsHandler) AppendLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var req struct {
		Data []struct {
			JobID      interface{} `json:"jobId"`
			StepID     string      `json:"stepId"`
			LineNumber string      `json:"lineNumber"`
			Output     string      `json:"output"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if h.redis != nil {
		ctx := r.Context()
		for _, entry := range req.Data {
			jobID := fmt.Sprintf("%v", entry.JobID)
			streamKey := streaming.JDKSerialize(jobID)
			_ = h.redis.XAdd(ctx, &redis.XAddArgs{
				Stream: streamKey,
				Values: map[string]interface{}{
					streaming.JDKSerialize("jobId"):      streaming.JDKSerialize(jobID),
					streaming.JDKSerialize("stepId"):     streaming.JDKSerialize(entry.StepID),
					streaming.JDKSerialize("lineNumber"): streaming.JDKSerialize(entry.LineNumber),
					streaming.JDKSerialize("output"):     streaming.JDKSerialize(entry.Output),
				},
			}).Err()
		}
	}

	log.Printf("AppendLogs: %d entries (redis=%v)", len(req.Data), h.redis != nil)
	w.WriteHeader(http.StatusOK)
}

// TerraformOutputHandler serves /tfoutput/v1 — returns job step output.
// Path: /tfoutput/v1/organization/{orgId}/job/{jobId}/step/{stepId}
type TerraformOutputHandler struct {
	repo      *repository.GenericRepository
	streaming *streaming.LogStreamReader
}

// NewTerraformOutputHandler creates a new TerraformOutputHandler.
func NewTerraformOutputHandler(repo *repository.GenericRepository, streaming *streaming.LogStreamReader) *TerraformOutputHandler {
	return &TerraformOutputHandler{repo: repo, streaming: streaming}
}

// GetOutput handles GET /tfoutput/v1/organization/{orgId}/job/{jobId}/step/{stepId}
func (h *TerraformOutputHandler) GetOutput(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	orgID, jobID, stepID, ok := parseTfOutputPath(r.URL.Path)
	if !ok {
		http.Error(w, "invalid path — expected /tfoutput/v1/organization/{orgId}/job/{jobId}/step/{stepId}", http.StatusBadRequest)
		return
	}

	data, err := h.streaming.GetStepOutput(context.Background(), orgID, jobID, stepID)
	if err != nil {
		log.Printf("GetOutput failed (org=%s job=%s step=%s): %v", orgID, jobID, stepID, err)
		http.Error(w, "output not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// parseTfOutputPath extracts orgId, jobId, stepId from the URL path.
// Expected: /tfoutput/v1/organization/{orgId}/job/{jobId}/step/{stepId}
func parseTfOutputPath(path string) (orgID, jobID, stepID string, ok bool) {
	// Strip prefix
	path = strings.TrimPrefix(path, "/tfoutput/v1/organization/")
	// orgId/job/{jobId}/step/{stepId}
	parts := strings.Split(path, "/")
	// parts: [orgId, "job", jobId, "step", stepId]
	if len(parts) != 5 || parts[1] != "job" || parts[3] != "step" {
		return "", "", "", false
	}
	return parts[0], parts[2], parts[4], true
}

// ContextHandler serves /context/v1 — provides execution context for jobs.
type ContextHandler struct {
	repo    *repository.GenericRepository
	storage storage.StorageService
}

// NewContextHandler creates a new ContextHandler.
func NewContextHandler(repo *repository.GenericRepository, storage storage.StorageService) *ContextHandler {
	return &ContextHandler{repo: repo, storage: storage}
}

// GetContext handles GET /context/v1/{jobId}
func (h *ContextHandler) GetContext(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract jobId from path: /context/v1/{jobId}
	jobId := strings.TrimPrefix(r.URL.Path, "/context/v1/")
	if jobId == "" || strings.Contains(jobId, "/") {
		http.Error(w, "invalid path — expected /context/v1/{jobId}", http.StatusBadRequest)
		return
	}

	remotePath := fmt.Sprintf("tfplan/%s/context.json", jobId)
	reader, err := h.storage.DownloadFile(remotePath)
	if err != nil {
		log.Printf("Plan context not found for job %s: %v", jobId, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{}"))
		return
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		log.Printf("Failed to read plan context for job %s: %v", jobId, err)
		http.Error(w, "failed to read context", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}
