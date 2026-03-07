package core

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/ilkerispir/terrakubed/internal/executor/terraform"
	"github.com/ilkerispir/terrakubed/internal/model"
)

// downloadState downloads the most recent terraform state to workingDir/terraform.tfstate.
// It tries multiple paths in order, skipping empty (0-byte) files:
//  1. tfstate/{orgId}/{wsId}/terraform.tfstate   — Java API / TFC migration / Go executor v0.0.44+
//  2. tfstate/{orgId}/{wsId}/state/state.raw.json — Go executor post-apply raw state fallback
//  3. organization/{orgId}/workspace/{wsId}/state/terraform.tfstate — legacy Go executor path
func (p *JobProcessor) downloadState(job *model.TerraformJob, workingDir string) {
	candidates := []string{
		fmt.Sprintf("tfstate/%s/%s/terraform.tfstate", job.OrganizationId, job.WorkspaceId),
		fmt.Sprintf("tfstate/%s/%s/state/state.raw.json", job.OrganizationId, job.WorkspaceId),
		fmt.Sprintf("organization/%s/workspace/%s/state/terraform.tfstate", job.OrganizationId, job.WorkspaceId),
	}

	for _, remotePath := range candidates {
		reader, err := p.Storage.DownloadFile(remotePath)
		if err != nil {
			log.Printf("State not found at %s: %v", remotePath, err)
			continue
		}

		localStatePath := filepath.Join(workingDir, "terraform.tfstate")
		f, err := os.Create(localStatePath)
		if err != nil {
			reader.Close()
			log.Printf("Failed to create local state file: %v", err)
			return
		}

		n, err := io.Copy(f, reader)
		f.Close()
		reader.Close()

		if err != nil {
			log.Printf("Failed to write state file from %s: %v", remotePath, err)
			os.Remove(localStatePath)
			continue
		}

		if n == 0 {
			log.Printf("State file at %s is empty (0 bytes), trying next candidate", remotePath)
			os.Remove(localStatePath)
			continue
		}

		log.Printf("Downloaded existing state (%d bytes) from %s to %s", n, remotePath, localStatePath)
		return
	}

	log.Printf("No existing state found (this is normal for new workspaces)")
}

func (p *JobProcessor) uploadStateAndOutput(job *model.TerraformJob, workingDir string) {
	// Upload terraform.tfstate only if it exists and is non-empty.
	// Never overwrite a valid state with a 0-byte file (e.g. from a plan that found no prior state).
	statePath := filepath.Join(workingDir, "terraform.tfstate")
	if info, err := os.Stat(statePath); err == nil && info.Size() > 0 {
		f, err := os.Open(statePath)
		if err == nil {
			defer f.Close()
			// Use same path as Java API / TFC migration protocol
			remotePath := fmt.Sprintf("tfstate/%s/%s/terraform.tfstate", job.OrganizationId, job.WorkspaceId)
			if err := p.Storage.UploadFile(remotePath, f); err != nil {
				log.Printf("Failed to upload state: %v", err)
			}
		}
	}

	// Upload Plan if exists (terraformPlan)
	planPath := filepath.Join(workingDir, "terraform.tfplan")
	if _, err := os.Stat(planPath); err == nil {
		f, err := os.Open(planPath)
		if err == nil {
			defer f.Close()
			remotePath := fmt.Sprintf("organization/%s/workspace/%s/job/%s/step/%s/terraformLibrary.tfplan", job.OrganizationId, job.WorkspaceId, job.JobId, job.StepId)
			if err := p.Storage.UploadFile(remotePath, f); err != nil {
				log.Printf("Failed to upload plan: %v", err)
			}
		}
	}

	// For Apply/Destroy: save state JSON, raw state, output, and create history
	if job.Type == "terraformApply" || job.Type == "terraformDestroy" {
		execPath, err := p.VersionManager.Install(job.TerraformVersion, job.Tofu)
		if err != nil {
			log.Printf("Failed to install terraform for state operations: %v", err)
			return
		}

		tfExecutor := terraform.NewExecutor(job, workingDir, nil, execPath)

		// Save state JSON (terraform show)
		stateJson, err := tfExecutor.ShowState()
		if err != nil {
			log.Printf("Failed to get state JSON: %v", err)
		} else {
			stateJsonPath := fmt.Sprintf("tfstate/%s/%s/state/state.json", job.OrganizationId, job.WorkspaceId)
			if err := p.Storage.UploadFile(stateJsonPath, strings.NewReader(stateJson)); err != nil {
				log.Printf("Failed to upload state JSON: %v", err)
			}
		}

		// Save raw state (terraform state pull)
		rawState, err := tfExecutor.StatePull()
		if err != nil {
			log.Printf("Failed to pull raw state: %v", err)
		} else {
			rawStatePath := fmt.Sprintf("tfstate/%s/%s/state/state.raw.json", job.OrganizationId, job.WorkspaceId)
			if err := p.Storage.UploadFile(rawStatePath, strings.NewReader(rawState)); err != nil {
				log.Printf("Failed to upload raw state: %v", err)
			}
		}

		// Get and save terraform output
		outputJson, err := tfExecutor.Output()
		if err != nil {
			log.Printf("Failed to get terraform output: %v", err)
		} else {
			job.TerraformOutput = outputJson
		}

		// Upload step output
		outputPath := fmt.Sprintf("tfoutput/%s/%s/%s.tfoutput", job.OrganizationId, job.JobId, job.StepId)
		if job.TerraformOutput != "" {
			if err := p.Storage.UploadFile(outputPath, strings.NewReader(job.TerraformOutput)); err != nil {
				log.Printf("Failed to upload terraform output: %v", err)
			}
		}

		// Create history record
		stateURL := fmt.Sprintf("tfstate/%s/%s/state/state.json", job.OrganizationId, job.WorkspaceId)
		if err := p.Status.CreateHistory(job, stateURL); err != nil {
			log.Printf("Failed to create history record: %v", err)
		}
	}
}
