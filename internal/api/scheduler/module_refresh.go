package scheduler

import (
	"context"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/terrakube-community/terrakubed/internal/api/vcs"
	"github.com/terrakube-community/terrakubed/internal/git"
)

// ModuleRefreshScheduler periodically checks each module's source repository
// for new tags and records them as module_version rows, mirroring Java's
// ModuleRefreshJob/ModuleRefreshService (a per-module Quartz job firing every
// 300s). There is no per-module job bookkeeping here — every tick just walks
// every module and diffs its tags against what's already stored, which is
// simpler and gives every module (not just newly-created ones) an equal
// chance to pick up new releases.
type ModuleRefreshScheduler struct {
	pool     *pgxpool.Pool
	git      git.GitService
	interval time.Duration
}

// NewModuleRefreshScheduler creates a new scheduler. Call Start() to begin.
func NewModuleRefreshScheduler(pool *pgxpool.Pool, interval time.Duration) *ModuleRefreshScheduler {
	return &ModuleRefreshScheduler{
		pool:     pool,
		git:      git.NewService(),
		interval: interval,
	}
}

// Start runs an immediate refresh, then repeats on the configured interval
// until ctx is cancelled.
func (s *ModuleRefreshScheduler) Start(ctx context.Context) {
	log.Printf("Module refresh scheduler starting (interval=%s)...", s.interval)
	s.refreshAll(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Module refresh scheduler stopped")
			return
		case <-ticker.C:
			s.refreshAll(ctx)
		}
	}
}

type moduleRow struct {
	id        string
	org       string
	name      string
	provider  string
	source    string
	tagPrefix string
	vcsID     *string
	sshID     *string
}

func (s *ModuleRefreshScheduler) refreshAll(ctx context.Context) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.id::text, o.name, m.name, m.provider, m.source,
		       COALESCE(m.tag_prefix,''), m.vcs_id::text, m.ssh_id::text
		FROM module m
		JOIN organization o ON m.organization_id = o.id
	`)
	if err != nil {
		log.Printf("ModuleRefresh: failed to list modules: %v", err)
		return
	}
	defer rows.Close()

	var modules []moduleRow
	for rows.Next() {
		var m moduleRow
		var vcsID, sshID *string
		if err := rows.Scan(&m.id, &m.org, &m.name, &m.provider, &m.source, &m.tagPrefix, &vcsID, &sshID); err != nil {
			log.Printf("ModuleRefresh: scan error: %v", err)
			continue
		}
		m.vcsID, m.sshID = vcsID, sshID
		modules = append(modules, m)
	}
	rows.Close()

	for _, m := range modules {
		s.refreshModule(ctx, m)
	}
}

func (s *ModuleRefreshScheduler) refreshModule(ctx context.Context, m moduleRow) {
	vcsType, connectionType, accessToken := s.resolveCredentials(ctx, m)

	tags, err := s.git.ListRemoteTags(m.source, vcsType, connectionType, accessToken)
	if err != nil {
		log.Printf("ModuleRefresh: %s/%s/%s: failed to list remote tags: %v", m.org, m.name, m.provider, err)
		return
	}
	if len(tags) == 0 {
		return
	}

	// Apply tag_prefix filtering the same way Java's ModuleRefreshJob does:
	// a tag only counts if it starts with the prefix, and the prefix is
	// stripped from the stored version. An empty prefix keeps every tag as-is.
	versions := make(map[string]string, len(tags))
	for tag, sha := range tags {
		if m.tagPrefix == "" {
			versions[tag] = sha
		} else if strings.HasPrefix(tag, m.tagPrefix) {
			versions[strings.TrimPrefix(tag, m.tagPrefix)] = sha
		}
	}
	if len(versions) == 0 {
		return
	}

	existingRows, err := s.pool.Query(ctx, `SELECT version FROM module_version WHERE module_id = $1`, m.id)
	if err != nil {
		log.Printf("ModuleRefresh: %s/%s/%s: failed to load existing versions: %v", m.org, m.name, m.provider, err)
		return
	}
	existing := make(map[string]bool)
	for existingRows.Next() {
		var v string
		if err := existingRows.Scan(&v); err == nil {
			existing[v] = true
		}
	}
	existingRows.Close()

	added := 0
	for version, sha := range versions {
		if existing[version] {
			continue
		}
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO module_version (id, version, commit_info, module_id) VALUES ($1, $2, $3, $4)`,
			uuid.New(), version, sha, m.id); err != nil {
			log.Printf("ModuleRefresh: %s/%s/%s: failed to insert version %s: %v", m.org, m.name, m.provider, version, err)
			continue
		}
		existing[version] = true
		added++
		log.Printf("ModuleRefresh: %s/%s/%s: added new version %s", m.org, m.name, m.provider, version)
	}

	if added > 0 {
		s.updateLatestVersion(ctx, m, existing)
	}
}

// resolveCredentials mirrors the registry service's own credential resolution
// (internal/registry/server.go), but reads straight from Postgres instead of
// going through GraphQL, since this scheduler runs inside the API process
// with direct pool access.
func (s *ModuleRefreshScheduler) resolveCredentials(ctx context.Context, m moduleRow) (vcsType, connectionType, accessToken string) {
	if m.vcsID != nil {
		var vType, cType string
		if err := s.pool.QueryRow(ctx,
			`SELECT vcs_type, COALESCE(connection_type,'') FROM vcs WHERE id = $1`, *m.vcsID,
		).Scan(&vType, &cType); err != nil {
			log.Printf("ModuleRefresh: %s/%s/%s: VCS %s not found: %v", m.org, m.name, m.provider, *m.vcsID, err)
			return "PUBLIC", "", ""
		}
		token, err := vcs.GetFreshToken(ctx, s.pool, *m.vcsID)
		if err != nil {
			log.Printf("ModuleRefresh: %s/%s/%s: failed to fetch VCS token: %v", m.org, m.name, m.provider, err)
		}
		return vType, cType, token
	}

	if m.sshID != nil {
		var sshType, privateKey string
		if err := s.pool.QueryRow(ctx,
			`SELECT ssh_type, private_key FROM ssh WHERE id = $1`, *m.sshID,
		).Scan(&sshType, &privateKey); err != nil {
			log.Printf("ModuleRefresh: %s/%s/%s: SSH %s not found: %v", m.org, m.name, m.provider, *m.sshID, err)
			return "PUBLIC", "", ""
		}
		return "SSH~" + sshType, "", privateKey
	}

	return "PUBLIC", "", ""
}

// updateLatestVersion recalculates module.latest_version as the highest
// semver-parseable version, matching Java's calculateLatestModuleVersion
// (ModuleDescriptor.Version.parse over every stored version, ignoring ones
// that don't parse, "Version pending" if none do).
func (s *ModuleRefreshScheduler) updateLatestVersion(ctx context.Context, m moduleRow, versions map[string]bool) {
	latest := "Version pending"
	var latestParsed [3]int
	found := false

	for v := range versions {
		parsed, ok := parseSemver(v)
		if !ok {
			continue
		}
		if !found || compareSemver(parsed, latestParsed) > 0 {
			latest = v
			latestParsed = parsed
			found = true
		}
	}

	if _, err := s.pool.Exec(ctx, `UPDATE module SET latest_version = $1 WHERE id = $2`, latest, m.id); err != nil {
		log.Printf("ModuleRefresh: %s/%s/%s: failed to update latest_version: %v", m.org, m.name, m.provider, err)
		return
	}
	log.Printf("ModuleRefresh: %s/%s/%s: latest version is now %s", m.org, m.name, m.provider, latest)
}

// parseSemver parses a "vX.Y.Z" or "X.Y.Z" version (ignoring any
// pre-release/build suffix after '-' or '+') into [major, minor, patch].
// Returns ok=false for anything that doesn't start with a numeric major.minor.patch.
func parseSemver(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// compareSemver returns >0 if a>b, <0 if a<b, 0 if equal.
func compareSemver(a, b [3]int) int {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] - b[i]
		}
	}
	return 0
}
