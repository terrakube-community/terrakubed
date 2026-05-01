package scheduler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// JobScheduler polls for pending jobs and dispatches them to an executor.
type JobScheduler struct {
	pool     *pgxpool.Pool
	executor Executor
	interval time.Duration
}

// Executor is the interface for job execution backends.
type Executor interface {
	Execute(ctx context.Context, execCtx *ExecutionContext) error
}

// ExecutionContext contains everything needed to execute a job.
type ExecutionContext struct {
	OrganizationID   string            `json:"organizationId"`
	WorkspaceID      string            `json:"workspaceId"`
	JobID            int               `json:"jobId"`
	StepID           string            `json:"stepId"`
	Source           string            `json:"source"`
	Branch           string            `json:"branch"`
	Folder           string            `json:"folder"`
	TerraformVersion string            `json:"terraformVersion"`
	VcsType          string            `json:"vcsType"`
	ConnectionType   string            `json:"connectionType"`
	AccessToken      string            `json:"accessToken"`
	ModuleSshKey     string            `json:"moduleSshKey"`
	CommitID         string            `json:"commitId"`
	Refresh          bool              `json:"refresh"`
	RefreshOnly      bool              `json:"refreshOnly"`
	IacType          string            `json:"iacType"`
	TCL              string            `json:"tcl"`
	EnvVars          map[string]string `json:"environmentVariables"`
	TFVars           map[string]string `json:"variables"`
}

// NewJobScheduler creates a new scheduler.
func NewJobScheduler(pool *pgxpool.Pool, executor Executor, interval time.Duration) *JobScheduler {
	return &JobScheduler{
		pool:     pool,
		executor: executor,
		interval: interval,
	}
}

// Start begins the polling loop.
func (s *JobScheduler) Start(ctx context.Context) {
	log.Printf("Job scheduler starting (interval: %s)", s.interval)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Job scheduler stopped")
			return
		case <-ticker.C:
			s.pollJobs(ctx)
		}
	}
}

// pollJobs checks for pending jobs and processes them.
func (s *JobScheduler) pollJobs(ctx context.Context) {
	// Find jobs in "pending" or "queue" status
	rows, err := s.pool.Query(ctx, `
		SELECT j.id, j.status, j.tcl, j.template_reference, j.commit_id,
		       j.organization_id, j.workspace_id, j.refresh, j.refresh_only,
		       w.source, w.branch, w.folder, w.terraform_version, w.iac_type,
		       w.module_ssh_key,
		       v.vcs_type, v.connection_type, v.access_token
		FROM job j
		JOIN workspace w ON j.workspace_id = w.id
		LEFT JOIN vcs v ON w.vcs_id = v.id
		WHERE j.status IN ('pending', 'queue')
		ORDER BY j.id ASC
		LIMIT 10
	`)
	if err != nil {
		log.Printf("Error polling jobs: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var (
			jobID            int
			status           string
			tcl              *string
			templateRef      *string
			commitID         *string
			orgID            string
			workspaceID      string
			refresh          bool
			refreshOnly      bool
			source           *string
			branch           *string
			folder           *string
			terraformVersion *string
			iacType          *string
			moduleSshKey     *string
			vcsType          *string
			connectionType   *string
			accessToken      *string
		)

		if err := rows.Scan(
			&jobID, &status, &tcl, &templateRef, &commitID,
			&orgID, &workspaceID, &refresh, &refreshOnly,
			&source, &branch, &folder, &terraformVersion, &iacType,
			&moduleSshKey,
			&vcsType, &connectionType, &accessToken,
		); err != nil {
			log.Printf("Error scanning job row: %v", err)
			continue
		}

		if status == "pending" {
			// Transition to "queue" status
			_, err := s.pool.Exec(ctx, "UPDATE job SET status = 'queue' WHERE id = $1", jobID)
			if err != nil {
				log.Printf("Error updating job %d status: %v", jobID, err)
				continue
			}
			log.Printf("Job %d queued", jobID)
			continue
		}

		// Status is "queue" — find the first pending step
		var stepID string
		err := s.pool.QueryRow(ctx,
			`SELECT id FROM step WHERE job_id = $1 AND status = 'pending' ORDER BY step_number ASC LIMIT 1`,
			jobID,
		).Scan(&stepID)
		if err != nil {
			log.Printf("No pending step for job %d: %v", jobID, err)
			continue
		}

		// Build execution context
		execCtx := &ExecutionContext{
			OrganizationID:   orgID,
			WorkspaceID:      workspaceID,
			JobID:            jobID,
			StepID:           stepID,
			Source:           deref(source),
			Branch:           deref(branch),
			Folder:           deref(folder),
			TerraformVersion: deref(terraformVersion),
			VcsType:          deref(vcsType),
			ConnectionType:   deref(connectionType),
			AccessToken:      deref(accessToken),
			ModuleSshKey:     deref(moduleSshKey),
			CommitID:         deref(commitID),
			Refresh:          refresh,
			RefreshOnly:      refreshOnly,
			IacType:          deref(iacType),
			TCL:              deref(tcl),
		}

		// Load environment and terraform variables
		execCtx.EnvVars = s.loadVariables(ctx, orgID, workspaceID, "ENV")
		execCtx.TFVars = s.loadVariables(ctx, orgID, workspaceID, "TERRAFORM")

		// Mark step as running
		_, err = s.pool.Exec(ctx, "UPDATE step SET status = 'running' WHERE id = $1", stepID)
		if err != nil {
			log.Printf("Error marking step %s as running: %v", stepID, err)
			continue
		}

		// Mark job as running
		_, err = s.pool.Exec(ctx, "UPDATE job SET status = 'running' WHERE id = $1", jobID)
		if err != nil {
			log.Printf("Error marking job %d as running: %v", jobID, err)
			continue
		}

		log.Printf("Dispatching job %d step %s", jobID, stepID)

		// Execute asynchronously
		go func(jID int, sID string, ec *ExecutionContext) {
			if err := s.executor.Execute(ctx, ec); err != nil {
				log.Printf("Job %d step %s execution failed: %v", jID, sID, err)
				s.pool.Exec(ctx, "UPDATE step SET status = 'failed' WHERE id = $1", sID)
				s.pool.Exec(ctx, "UPDATE job SET status = 'failed' WHERE id = $1", jID)
			}
		}(jobID, stepID, execCtx)
	}
}

// loadVariables loads workspace variables and global variables for a given category.
func (s *JobScheduler) loadVariables(ctx context.Context, orgID, workspaceID, category string) map[string]string {
	vars := make(map[string]string)

	// Load global variables
	rows, err := s.pool.Query(ctx,
		`SELECT variable_key, variable_value FROM globalvar
		 WHERE organization_id = $1 AND variable_category = $2`,
		orgID, category,
	)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var key, value string
			if rows.Scan(&key, &value) == nil {
				vars[key] = value
			}
		}
	}

	// Load workspace variables (override globals)
	rows2, err := s.pool.Query(ctx,
		`SELECT variable_key, variable_value FROM variable
		 WHERE workspace_id = $1 AND variable_category = $2`,
		workspaceID, category,
	)
	if err == nil {
		defer rows2.Close()
		for rows2.Next() {
			var key, value string
			if rows2.Scan(&key, &value) == nil {
				vars[key] = value
			}
		}
	}

	return vars
}

// MarshalJSON serializes ExecutionContext to JSON for passing to ephemeral pods.
func (e *ExecutionContext) MarshalJSON2() ([]byte, error) {
	return json.Marshal(*e)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ──────────────────────────────────────────────────
// Ephemeral Executor (Kubernetes Jobs)
// ──────────────────────────────────────────────────

// EphemeralConfig holds config for the K8s ephemeral executor.
type EphemeralConfig struct {
	Namespace      string
	Image          string
	SecretName     string
	ServiceAccount string
	NodeSelector   map[string]string
	Annotations    map[string]string
	Tolerations    []map[string]string
}

// EphemeralExecutor creates Kubernetes Jobs for each execution.
// Mirrors Java's EphemeralExecutorService behaviour.
type EphemeralExecutor struct {
	config    EphemeralConfig
	clientset *kubernetes.Clientset
}

// NewEphemeralExecutor creates a new ephemeral executor.
// It auto-detects in-cluster config and falls back to kubeconfig.
func NewEphemeralExecutor(config EphemeralConfig) (*EphemeralExecutor, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		// Fall back to kubeconfig (local dev / out-of-cluster)
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(),
			&clientcmd.ConfigOverrides{},
		).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to build k8s config: %w", err)
		}
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create k8s client: %w", err)
	}

	return &EphemeralExecutor{config: config, clientset: clientset}, nil
}

// Execute creates a Kubernetes Job for the given execution context.
// The job runs the terrakubed executor image with the serialised
// ExecutionContext passed as EphemeralJobData (base64-encoded JSON).
func (e *EphemeralExecutor) Execute(ctx context.Context, execCtx *ExecutionContext) error {
	jobName := fmt.Sprintf("terrakube-job-%d-%s", execCtx.JobID, execCtx.StepID[:8])

	execData, err := json.Marshal(execCtx)
	if err != nil {
		return fmt.Errorf("failed to serialize execution context: %w", err)
	}
	execB64 := base64.StdEncoding.EncodeToString(execData)

	ttl := int32(30)
	backoff := int32(0)
	privileged := false

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: e.config.Namespace,
			Labels: map[string]string{
				"terrakube.io/organization": execCtx.OrganizationID,
				"terrakube.io/workspace":    execCtx.WorkspaceID,
				"terrakube.io/job":          fmt.Sprintf("%d", execCtx.JobID),
				"app":                       "terrakubed-executor",
			},
			Annotations: e.config.Annotations,
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			BackoffLimit:            &backoff,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":                       "terrakubed-executor",
						"terrakube.io/organization": execCtx.OrganizationID,
						"terrakube.io/workspace":    execCtx.WorkspaceID,
					},
					Annotations: e.config.Annotations,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: e.config.ServiceAccount,
					RestartPolicy:      corev1.RestartPolicyNever,
					NodeSelector:       e.config.NodeSelector,
					Tolerations:        e.buildTolerations(),
					Containers: []corev1.Container{
						{
							Name:            "executor",
							Image:           e.config.Image,
							ImagePullPolicy: corev1.PullAlways,
							SecurityContext: &corev1.SecurityContext{
								Privileged: &privileged,
							},
							Env: []corev1.EnvVar{
								{
									Name:  "EphemeralJobData",
									Value: execB64,
								},
							},
							EnvFrom: e.buildEnvFrom(),
						},
					},
				},
			},
		},
	}

	_, err = e.clientset.BatchV1().Jobs(e.config.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create k8s job %s: %w", jobName, err)
	}

	log.Printf("K8s Job created: %s (namespace: %s)", jobName, e.config.Namespace)
	return nil
}

func (e *EphemeralExecutor) buildEnvFrom() []corev1.EnvFromSource {
	if e.config.SecretName == "" {
		return nil
	}
	return []corev1.EnvFromSource{
		{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: e.config.SecretName,
				},
			},
		},
	}
}

func (e *EphemeralExecutor) buildTolerations() []corev1.Toleration {
	var tolerations []corev1.Toleration
	for _, t := range e.config.Tolerations {
		toleration := corev1.Toleration{}
		if v, ok := t["key"]; ok {
			toleration.Key = v
		}
		if v, ok := t["operator"]; ok {
			toleration.Operator = corev1.TolerationOperator(v)
		}
		if v, ok := t["value"]; ok {
			toleration.Value = v
		}
		if v, ok := t["effect"]; ok {
			toleration.Effect = corev1.TaintEffect(v)
		}
		tolerations = append(tolerations, toleration)
	}
	return tolerations
}
