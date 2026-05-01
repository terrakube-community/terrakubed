// Package tcl implements Terrakube Configuration Language processing.
// TCL defines the workflow for a job as a base64-encoded YAML document.
//
// Example decoded TCL:
//
//	flow:
//	  - type: terraformPlan
//	    step: 100
//	    name: "Plan"
//	  - type: approval
//	    step: 200
//	    name: "Approval"
//	  - type: terraformApply
//	    step: 300
//	    name: "Apply"
package tcl

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"
)

// FlowConfig is the top-level TCL document.
type FlowConfig struct {
	Flow []Flow `yaml:"flow"`
}

// Flow is a single step in the workflow.
type Flow struct {
	Type        string `yaml:"type"`
	Step        int    `yaml:"step"`
	Name        string `yaml:"name"`
	Team        string `yaml:"team"`
	IgnoreError bool   `yaml:"ignoreError"`
}

// Processor creates step records from a job's TCL definition.
type Processor struct {
	pool *pgxpool.Pool
}

// NewProcessor creates a new TCL processor.
func NewProcessor(pool *pgxpool.Pool) *Processor {
	return &Processor{pool: pool}
}

// InitJobSteps reads the TCL for a job (from template or inline), parses it,
// and inserts one step row per flow entry. Mirrors Java TclService.initJobConfiguration().
func (p *Processor) InitJobSteps(ctx context.Context, jobID int) error {
	// Load job TCL (inline or from template reference)
	var tcl, templateRef string
	err := p.pool.QueryRow(ctx,
		`SELECT COALESCE(tcl,''), COALESCE(template_reference,'') FROM job WHERE id = $1`,
		jobID,
	).Scan(&tcl, &templateRef)
	if err != nil {
		return fmt.Errorf("job %d not found: %w", jobID, err)
	}

	// If template reference is set, fetch TCL from template table
	if templateRef != "" && tcl == "" {
		err = p.pool.QueryRow(ctx,
			`SELECT COALESCE(tcl,'') FROM template WHERE id = $1`,
			templateRef,
		).Scan(&tcl)
		if err != nil {
			log.Printf("Template %s not found, using empty TCL: %v", templateRef, err)
		} else {
			// Persist resolved TCL back to job (mirrors Java behaviour)
			_, _ = p.pool.Exec(ctx, `UPDATE job SET tcl = $1 WHERE id = $2`, tcl, jobID)
		}
	}

	if tcl == "" {
		return fmt.Errorf("job %d has no TCL and no template reference", jobID)
	}

	flowConfig, err := parseTCL(tcl)
	if err != nil {
		return fmt.Errorf("failed to parse TCL for job %d: %w", jobID, err)
	}

	if len(flowConfig.Flow) == 0 {
		return fmt.Errorf("TCL for job %d has no flow entries", jobID)
	}

	// Check if steps already exist — idempotent
	var count int
	_ = p.pool.QueryRow(ctx, `SELECT COUNT(*) FROM step WHERE job_id = $1`, jobID).Scan(&count)
	if count > 0 {
		log.Printf("Job %d already has %d steps, skipping TCL init", jobID, count)
		return nil
	}

	// Insert one step per flow entry
	for _, flow := range flowConfig.Flow {
		stepID := uuid.New()
		name := flow.Name
		if name == "" {
			name = fmt.Sprintf("Running Step %d", flow.Step)
		}
		_, err := p.pool.Exec(ctx,
			`INSERT INTO step (id, step_number, name, status, output, log_status, job_id)
			 VALUES ($1, $2, $3, 'pending', '', 'pending', $4)`,
			stepID, flow.Step, name, jobID,
		)
		if err != nil {
			return fmt.Errorf("failed to insert step %d for job %d: %w", flow.Step, jobID, err)
		}
		log.Printf("Job %d: created step %d (%s) type=%s", jobID, flow.Step, name, flow.Type)
	}

	return nil
}

// parseTCL decodes base64 and unmarshals the YAML flow config.
func parseTCL(encoded string) (*FlowConfig, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}

	var cfg FlowConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("yaml unmarshal: %w", err)
	}

	return &cfg, nil
}
