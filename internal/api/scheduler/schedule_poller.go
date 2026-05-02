package scheduler

import (
	"context"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/robfig/cron/v3"
)

// SchedulePoller watches workspace schedules and triggers jobs on time.
// Mirrors Java ScheduleJobTrigger / ScheduleVcsService behaviour.
type SchedulePoller struct {
	pool         *pgxpool.Pool
	tclProcessor TclInitializer
	cron         *cron.Cron
	entries      map[string]cron.EntryID // scheduleID → cron entry
}

// TclInitializer is satisfied by *tcl.Processor — avoids import cycle.
type TclInitializer interface {
	InitJobSteps(ctx context.Context, jobID int) error
}

// NewSchedulePoller creates a new poller. Call Start() to begin watching.
func NewSchedulePoller(pool *pgxpool.Pool, tclProc TclInitializer) *SchedulePoller {
	return &SchedulePoller{
		pool:         pool,
		tclProcessor: tclProc,
		cron:         cron.New(cron.WithSeconds()),
		entries:      make(map[string]cron.EntryID),
	}
}

// Start loads all active schedules and begins the cron engine.
// It also starts a refresh loop that re-syncs schedules every minute.
func (p *SchedulePoller) Start(ctx context.Context) {
	log.Printf("Schedule poller starting...")
	p.syncSchedules(ctx)
	p.cron.Start()

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			p.cron.Stop()
			log.Println("Schedule poller stopped")
			return
		case <-ticker.C:
			p.syncSchedules(ctx)
		}
	}
}

// syncSchedules loads active workspace schedules and registers/deregisters cron entries.
func (p *SchedulePoller) syncSchedules(ctx context.Context) {
	rows, err := p.pool.Query(ctx, `
		SELECT s.id, s.cron, s.workspace_id
		FROM schedule s
		JOIN workspace w ON s.workspace_id = w.id
		WHERE s.enabled = true AND w.deleted = false
	`)
	if err != nil {
		log.Printf("SchedulePoller: failed to load schedules: %v", err)
		return
	}
	defer rows.Close()

	active := make(map[string]bool)

	for rows.Next() {
		var schedID, cronExpr, wsID string
		if err := rows.Scan(&schedID, &cronExpr, &wsID); err != nil {
			continue
		}
		active[schedID] = true

		// Already registered — skip
		if _, ok := p.entries[schedID]; ok {
			continue
		}

		// Register new cron entry
		entryID, err := p.cron.AddFunc(cronExpr, func() {
			p.triggerScheduledJob(context.Background(), wsID, schedID)
		})
		if err != nil {
			log.Printf("SchedulePoller: invalid cron %q for schedule %s: %v", cronExpr, schedID, err)
			continue
		}
		p.entries[schedID] = entryID
		log.Printf("SchedulePoller: registered schedule %s (%s) for workspace %s", schedID, cronExpr, wsID)
	}

	// Remove entries for deleted/disabled schedules
	for schedID, entryID := range p.entries {
		if !active[schedID] {
			p.cron.Remove(entryID)
			delete(p.entries, schedID)
			log.Printf("SchedulePoller: removed schedule %s", schedID)
		}
	}
}

// triggerScheduledJob creates a job for the workspace when the cron fires.
func (p *SchedulePoller) triggerScheduledJob(ctx context.Context, workspaceID, scheduleID string) {
	log.Printf("SchedulePoller: firing schedule %s for workspace %s", scheduleID, workspaceID)

	var orgID, templateRef, defaultTemplate string
	var locked bool
	err := p.pool.QueryRow(ctx, `
		SELECT w.organization_id::text, COALESCE(w.default_template,''),
		       COALESCE(o.default_template,''), w.locked
		FROM workspace w
		JOIN organization o ON w.organization_id = o.id
		WHERE w.id = $1 AND w.deleted = false
	`, workspaceID).Scan(&orgID, &templateRef, &defaultTemplate, &locked)
	if err != nil {
		log.Printf("SchedulePoller: workspace %s not found: %v", workspaceID, err)
		return
	}

	if locked {
		log.Printf("SchedulePoller: workspace %s is locked, skipping scheduled run", workspaceID)
		return
	}

	resolvedTemplate := templateRef
	if resolvedTemplate == "" {
		resolvedTemplate = defaultTemplate
	}

	orgUUID, err := uuid.Parse(orgID)
	if err != nil {
		log.Printf("SchedulePoller: invalid org id %s: %v", orgID, err)
		return
	}
	wsUUID, err := uuid.Parse(workspaceID)
	if err != nil {
		log.Printf("SchedulePoller: invalid workspace id %s: %v", workspaceID, err)
		return
	}

	var jobID int
	err = p.pool.QueryRow(ctx, `
		INSERT INTO job (status, output, comments, commit_id, template_reference, via,
		                 refresh, refresh_only, plan_changes, terraform_plan, approval_team,
		                 organization_id, workspace_id)
		VALUES ('pending', '', '', '', $1, 'schedule',
		        false, false, false, '', '',
		        $2, $3)
		RETURNING id
	`, resolvedTemplate, orgUUID, wsUUID).Scan(&jobID)
	if err != nil {
		log.Printf("SchedulePoller: failed to create job for workspace %s: %v", workspaceID, err)
		return
	}

	log.Printf("SchedulePoller: created job %d for workspace %s (schedule %s)", jobID, workspaceID, scheduleID)

	if err := p.tclProcessor.InitJobSteps(ctx, jobID); err != nil {
		log.Printf("SchedulePoller: failed to init steps for job %d: %v", jobID, err)
	}
}
