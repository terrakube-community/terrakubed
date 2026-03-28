package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/terrakube-community/terrakubed/internal/auth"
	"github.com/terrakube-community/terrakubed/internal/executor/terraform"
	"github.com/terrakube-community/terrakubed/internal/model"
)

const (
	structuredPlanMarker = `<div data-terrakube-structured-plan="true"></div>`
	contextPlanKey       = "planStructuredOutput"
	contextUIKey         = "terrakubeUI"
)

func normalizeAction(actions []interface{}) string {
	has := func(s string) bool {
		for _, a := range actions {
			if v, ok := a.(string); ok && v == s {
				return true
			}
		}
		return false
	}
	switch {
	case has("delete") && has("create"):
		return "replace"
	case has("create"):
		return "create"
	case has("delete"):
		return "delete"
	case has("update"):
		return "update"
	case has("read"):
		return "read"
	case has("no-op"):
		return "no-op"
	default:
		return "unknown"
	}
}

func buildChangesFromPlanJSON(planJSON string) ([]map[string]interface{}, error) {
	var plan map[string]interface{}
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		return nil, fmt.Errorf("failed to parse plan JSON: %w", err)
	}

	resourceChanges, _ := plan["resource_changes"].([]interface{})
	var result []map[string]interface{}

	for _, rc := range resourceChanges {
		change, ok := rc.(map[string]interface{})
		if !ok {
			continue
		}
		changeBlock, ok := change["change"].(map[string]interface{})
		if !ok {
			continue
		}
		actions, _ := changeBlock["actions"].([]interface{})
		action := normalizeAction(actions)
		if action == "no-op" {
			continue
		}
		result = append(result, map[string]interface{}{
			"address":         change["address"],
			"moduleAddress":   change["module_address"],
			"resourceType":    change["type"],
			"resourceName":    change["name"],
			"actions":         actions,
			"action":          action,
			"before":          changeBlock["before"],
			"beforeSensitive": changeBlock["before_sensitive"],
			"after":           changeBlock["after"],
			"afterSensitive":  changeBlock["after_sensitive"],
			"afterUnknown":    changeBlock["after_unknown"],
		})
	}
	return result, nil
}

func (p *JobProcessor) getCurrentContext(jobId string) map[string]interface{} {
	token, err := auth.GenerateTerrakubeToken(p.Config.InternalSecret)
	if err != nil {
		log.Printf("Failed to generate token for context GET: %v", err)
		return map[string]interface{}{}
	}

	url := fmt.Sprintf("%s/context/v1/%s", p.Config.AzBuilderApiUrl, jobId)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return map[string]interface{}{}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Failed to GET context for job %s: %v", jobId, err)
		return map[string]interface{}{}
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var ctx map[string]interface{}
	if err := json.Unmarshal(body, &ctx); err != nil || ctx == nil {
		return map[string]interface{}{}
	}
	return ctx
}

func (p *JobProcessor) saveContext(jobId string, ctx map[string]interface{}) error {
	token, err := auth.GenerateTerrakubeToken(p.Config.InternalSecret)
	if err != nil {
		return fmt.Errorf("failed to generate token for context POST: %w", err)
	}

	data, err := json.Marshal(ctx)
	if err != nil {
		return fmt.Errorf("failed to marshal context: %w", err)
	}

	url := fmt.Sprintf("%s/context/v1/%s", p.Config.AzBuilderApiUrl, jobId)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to build context POST request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to POST context for job %s: %w", jobId, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("context POST returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (p *JobProcessor) uploadPlanJSON(job *model.TerraformJob, workingDir string, execPath string) {
	tfExecutor := terraform.NewExecutor(job, workingDir, nil, execPath)
	planJSON, err := tfExecutor.ShowPlanRawJSON()
	if err != nil {
		log.Printf("Failed to get plan JSON (skipping context upload): %v", err)
		return
	}

	changes, err := buildChangesFromPlanJSON(planJSON)
	if err != nil {
		log.Printf("Failed to build changes from plan JSON: %v", err)
		return
	}

	ctx := p.getCurrentContext(job.JobId)

	planOutput, _ := ctx[contextPlanKey].(map[string]interface{})
	if planOutput == nil {
		planOutput = map[string]interface{}{}
	}
	planOutput[job.StepId] = changes
	ctx[contextPlanKey] = planOutput

	uiOutput, _ := ctx[contextUIKey].(map[string]interface{})
	if uiOutput == nil {
		uiOutput = map[string]interface{}{}
	}
	uiOutput[job.StepId] = structuredPlanMarker
	ctx[contextUIKey] = uiOutput

	if err := p.saveContext(job.JobId, ctx); err != nil {
		log.Printf("Failed to save plan context for job %s: %v", job.JobId, err)
		return
	}
	log.Printf("Saved structured plan context for job %s step %s (%d changes)", job.JobId, job.StepId, len(changes))
}

func (p *JobProcessor) uploadStateAndOutput(job *model.TerraformJob, workingDir string) {
	// Upload Plan if exists (terraformPlan / terraformPlanDestroy).
	// Stored at a job-level path (no step ID) so the apply step can always
	// find it regardless of its own step ID.
	planPath := filepath.Join(workingDir, "terraform.tfplan")
	if _, err := os.Stat(planPath); err == nil {
		f, err := os.Open(planPath)
		if err == nil {
			defer f.Close()
			remotePath := fmt.Sprintf("organization/%s/workspace/%s/job/%s/plan/terraformLibrary.tfplan", job.OrganizationId, job.WorkspaceId, job.JobId)
			if err := p.Storage.UploadFile(remotePath, f); err != nil {
				log.Printf("Failed to upload plan: %v", err)
			}
		}
	}

	// For Apply/Destroy: save state JSON, raw state, output, and create history.
	// NOTE: terraform.tfstate is NOT uploaded here — the S3/Azure/GCS backend
	// configured via terrakube_override.tf writes state directly to cloud storage.
	if job.Type == "terraformApply" || job.Type == "terraformDestroy" {
		execPath, err := p.VersionManager.Install(job.TerraformVersion, job.Tofu)
		if err != nil {
			log.Printf("Failed to install terraform for state operations: %v", err)
			return
		}

		tfExecutor := terraform.NewExecutor(job, workingDir, nil, execPath)

		// Save state JSON (terraform show) — UUID filename matches Java API history protocol.
		// Each apply creates a new immutable snapshot; the history record links to it.
		stateFilename := uuid.New().String()
		stateJson, err := tfExecutor.ShowState()
		if err != nil {
			log.Printf("Failed to get state JSON: %v", err)
		} else {
			stateJsonPath := fmt.Sprintf("tfstate/%s/%s/state/%s.json", job.OrganizationId, job.WorkspaceId, stateFilename)
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

		// Create history record — URL matches Java API TerraformStatePathService format:
		// {api_url}/tfstate/v1/organization/{orgId}/workspace/{wsId}/state/{UUID}.json
		stateURL := fmt.Sprintf("%s/tfstate/v1/organization/%s/workspace/%s/state/%s.json",
			p.Config.AzBuilderApiUrl, job.OrganizationId, job.WorkspaceId, stateFilename)
		if err := p.Status.CreateHistory(job, stateURL); err != nil {
			log.Printf("Failed to create history record: %v", err)
		}
	}
}
