package status

import (
	"fmt"
	"log"
	"strings"

	"github.com/terrakube-community/terrakubed/internal/auth"
	"github.com/terrakube-community/terrakubed/internal/client"
	"github.com/terrakube-community/terrakubed/internal/config"
	"github.com/terrakube-community/terrakubed/internal/model"
	"github.com/terrakube-community/terrakubed/internal/storage"
)

type StatusService interface {
	SetRunning(job *model.TerraformJob) error
	SetCompleted(job *model.TerraformJob, success bool, output string) error
	SetNoChanges(job *model.TerraformJob, output string) error
	SetPending(job *model.TerraformJob, output string) error
	SetApprovalCompleted(job *model.TerraformJob, output string) error
	UpdateCommitId(job *model.TerraformJob, commitId string) error
	CreateHistory(job *model.TerraformJob, stateURL string) error
}

type Service struct {
	client  *client.TerrakubeClient
	storage storage.StorageService
	apiUrl  string
}

func NewStatusService(cfg *config.Config, storageService storage.StorageService) *Service {
	token, err := auth.GenerateTerrakubeToken(cfg.InternalSecret)
	if err != nil {
		log.Printf("Warning: failed to generate Terrakube token for API requests: %v", err)
	}
	return &Service{
		client:  client.NewTerrakubeClient(cfg.AzBuilderApiUrl, token),
		storage: storageService,
		apiUrl:  cfg.AzBuilderApiUrl,
	}
}

// getOutputPath returns the API URL path where the UI will fetch log output from.
// This matches the Java TerraformOutputPathService.getOutputPath() format.
func (s *Service) getOutputPath(orgId, jobId, stepId string) string {
	base := strings.TrimRight(s.apiUrl, "/")
	return fmt.Sprintf("%s/tfoutput/v1/organization/%s/job/%s/step/%s", base, orgId, jobId, stepId)
}

func (s *Service) SetRunning(job *model.TerraformJob) error {
	// Update step status to "running" with output path so the UI can start polling for logs
	outputPath := s.getOutputPath(job.OrganizationId, job.JobId, job.StepId)
	if err := s.client.UpdateStepStatus(job.OrganizationId, job.JobId, job.StepId, "running", outputPath); err != nil {
		log.Printf("Failed to update step to running: %v", err)
	}
	return s.client.UpdateJobStatus(job.OrganizationId, job.JobId, "running", "")
}

func (s *Service) SetCompleted(job *model.TerraformJob, success bool, output string) error {
	// Upload log output to storage
	outputPath := s.saveOutput(job.OrganizationId, job.JobId, job.StepId, output)

	status := "completed"
	if !success {
		status = "failed"
	}
	if err := s.client.UpdateStepStatus(job.OrganizationId, job.JobId, job.StepId, status, outputPath); err != nil {
		return fmt.Errorf("failed to update step status: %w", err)
	}
	return s.client.UpdateJobStatus(job.OrganizationId, job.JobId, status, "")
}

// SetNoChanges marks a plan step as completed and the job as "noChanges"
// (plan ran but detected no infrastructure drift). Distinct from "completed"
// so the UI can show "No Changes" and auto-apply skips the apply step.
func (s *Service) SetNoChanges(job *model.TerraformJob, output string) error {
	outputPath := s.saveOutput(job.OrganizationId, job.JobId, job.StepId, output)

	if err := s.client.UpdateStepStatus(job.OrganizationId, job.JobId, job.StepId, "completed", outputPath); err != nil {
		return fmt.Errorf("failed to update step status: %w", err)
	}
	return s.client.UpdateJobStatus(job.OrganizationId, job.JobId, "noChanges", "")
}

func (s *Service) SetPending(job *model.TerraformJob, output string) error {
	outputPath := s.saveOutput(job.OrganizationId, job.JobId, job.StepId, output)

	// Step is always "completed" (plan step finished successfully).
	// Only the JOB is set to "pending" (waiting for user approval to apply).
	// Matches Java UpdateJobStatusImpl: updateStepStatus sets "completed", updateJobStatus sets "pending".
	if err := s.client.UpdateStepStatus(job.OrganizationId, job.JobId, job.StepId, "completed", outputPath); err != nil {
		return fmt.Errorf("failed to update step status: %w", err)
	}
	return s.client.UpdateJobStatus(job.OrganizationId, job.JobId, "pending", "")
}

// SetApprovalCompleted marks the approval gate step as done and returns the job to "pending"
// so the Java API scheduler calls executePendingJob(), finds the next flow step (terraformApply /
// terraformDestroy) and dispatches it.
//
// Status must be "pending" — NOT "completed" (would mark remaining steps notExecuted)
// and NOT "queue" (falls to scheduler default → no action taken).
// The Java ScheduleJob switch: pending→executePendingJob, approved→executeApprovedJobs,
// queue→default (no-op).
func (s *Service) SetApprovalCompleted(job *model.TerraformJob, output string) error {
	outputPath := s.saveOutput(job.OrganizationId, job.JobId, job.StepId, output)
	if err := s.client.UpdateStepStatus(job.OrganizationId, job.JobId, job.StepId, "completed", outputPath); err != nil {
		return fmt.Errorf("failed to update approval step status: %w", err)
	}
	// "pending" triggers executePendingJob() in the Java scheduler, which finds the next
	// unexecuted flow step and dispatches the apply/destroy executor pod.
	return s.client.UpdateJobStatus(job.OrganizationId, job.JobId, "pending", "")
}

func (s *Service) UpdateCommitId(job *model.TerraformJob, commitId string) error {
	return s.client.UpdateJobCommitId(job.OrganizationId, job.JobId, commitId)
}

func (s *Service) CreateHistory(job *model.TerraformJob, stateURL string) error {
	return s.client.CreateHistory(job.OrganizationId, job.WorkspaceId, stateURL)
}

// saveOutput uploads the terraform log output to object storage and returns the output URL path.
// If upload fails, falls back to returning truncated raw output text so logs are still somewhat visible.
func (s *Service) saveOutput(orgId, jobId, stepId, output string) string {
	remotePath := fmt.Sprintf("tfoutput/%s/%s/%s.tfoutput", orgId, jobId, stepId)
	if err := s.storage.UploadFile(remotePath, strings.NewReader(output)); err != nil {
		log.Printf("Warning: failed to upload log output to storage: %v", err)
		// Fall back to truncated raw output so the API PATCH body stays small
		const maxFallbackLen = 4096
		if len(output) > maxFallbackLen {
			output = output[len(output)-maxFallbackLen:]
		}
		return output
	}
	return s.getOutputPath(orgId, jobId, stepId)
}
