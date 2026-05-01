package handler

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ApprovalHandler handles plan approval / rejection from the UI.
//
//	POST /approval/v1/{jobId}/approve
//	POST /approval/v1/{jobId}/reject
type ApprovalHandler struct {
	pool *pgxpool.Pool
}

// NewApprovalHandler creates a new ApprovalHandler.
func NewApprovalHandler(pool *pgxpool.Pool) *ApprovalHandler {
	return &ApprovalHandler{pool: pool}
}

func (h *ApprovalHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// /approval/v1/{jobId}/approve  or  /approval/v1/{jobId}/reject
	path := strings.TrimPrefix(r.URL.Path, "/approval/v1/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 {
		http.Error(w, "invalid path — expected /approval/v1/{jobId}/{approve|reject}", http.StatusBadRequest)
		return
	}

	jobID := parts[0]
	action := parts[1]

	switch action {
	case "approve":
		h.approve(w, r, jobID)
	case "reject":
		h.reject(w, r, jobID)
	default:
		http.Error(w, "unknown action — use approve or reject", http.StatusBadRequest)
	}
}

// approve transitions the waitingApproval step → completed and re-queues the job.
func (h *ApprovalHandler) approve(w http.ResponseWriter, r *http.Request, jobID string) {
	ctx := r.Context()

	// Find the waitingApproval step
	var stepID string
	err := h.pool.QueryRow(ctx,
		`SELECT id FROM step WHERE job_id = $1 AND status = 'waitingApproval' LIMIT 1`,
		jobID,
	).Scan(&stepID)
	if err != nil {
		http.Error(w, "no waitingApproval step found for job", http.StatusNotFound)
		return
	}

	// Mark approval step as completed
	if _, err := h.pool.Exec(ctx,
		"UPDATE step SET status = 'completed' WHERE id = $1", stepID); err != nil {
		log.Printf("Approval: failed to complete step %s: %v", stepID, err)
		http.Error(w, "failed to approve", http.StatusInternalServerError)
		return
	}

	// Re-queue job so scheduler picks up the next step
	if _, err := h.pool.Exec(ctx,
		"UPDATE job SET status = 'queue' WHERE id = $1", jobID); err != nil {
		log.Printf("Approval: failed to re-queue job %s: %v", jobID, err)
		http.Error(w, "failed to re-queue job", http.StatusInternalServerError)
		return
	}

	log.Printf("Job %s approved — re-queued for next step", jobID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "approved"})
}

// reject marks the waitingApproval step + job as rejected/cancelled.
func (h *ApprovalHandler) reject(w http.ResponseWriter, r *http.Request, jobID string) {
	ctx := r.Context()

	var stepID string
	err := h.pool.QueryRow(ctx,
		`SELECT id FROM step WHERE job_id = $1 AND status = 'waitingApproval' LIMIT 1`,
		jobID,
	).Scan(&stepID)
	if err != nil {
		http.Error(w, "no waitingApproval step found for job", http.StatusNotFound)
		return
	}

	h.pool.Exec(ctx, "UPDATE step SET status = 'rejected' WHERE id = $1", stepID)
	// Mark remaining pending steps as notExecuted
	h.pool.Exec(ctx,
		"UPDATE step SET status = 'notExecuted' WHERE job_id = $1 AND status = 'pending'", jobID)
	h.pool.Exec(ctx, "UPDATE job SET status = 'rejected' WHERE id = $1", jobID)

	log.Printf("Job %s rejected", jobID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "rejected"})
}
