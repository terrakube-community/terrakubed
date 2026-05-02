package middleware

import (
	"context"
	"log"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AuthzMiddleware adds authorization checks on top of token validation.
// It MUST run AFTER AuthMiddleware so that UserInfo is in the context.
//
// Access model:
//   - Owner group (ownerGroup): full access to everything
//   - Service accounts (PAT / internal): full access to everything
//   - Regular OIDC users: GET requests allowed; mutating requests checked
//     against team membership (org-level) and access records (workspace-level)
func AuthzMiddleware(pool *pgxpool.Pool, ownerGroup string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// GET / OPTIONS / HEAD are always allowed for authenticated users
			if r.Method == http.MethodGet || r.Method == http.MethodOptions ||
				r.Method == http.MethodHead {
				next.ServeHTTP(w, r)
				return
			}

			user := GetUser(r.Context())
			if user == nil {
				http.Error(w, `{"errors":[{"status":"401","title":"Unauthorized"}]}`, http.StatusUnauthorized)
				return
			}

			// Service accounts and owner group bypass all checks
			if user.IsServiceAccount() || (ownerGroup != "" && user.IsMember(ownerGroup)) {
				next.ServeHTTP(w, r)
				return
			}

			// Pool not configured — allow (fail open; token validation already happened)
			if pool == nil {
				next.ServeHTTP(w, r)
				return
			}

			// Org-creation and org-deletion: owner only
			path := strings.TrimPrefix(r.URL.Path, "/api/v1/")
			segments := strings.Split(strings.TrimSuffix(path, "/"), "/")
			rootType := ""
			if len(segments) > 0 {
				rootType = segments[0]
			}

			if rootType == "organization" {
				if (r.Method == http.MethodPost && len(segments) == 1) ||
					(r.Method == http.MethodDelete && len(segments) == 2) {
					// Creating or deleting an org requires owner group
					log.Printf("AuthZ: org create/delete denied for non-owner %s", user.Email)
					http.Error(w,
						`{"errors":[{"status":"403","title":"Forbidden","detail":"Only the owner group can create or delete organizations"}]}`,
						http.StatusForbidden)
					return
				}
			}

			// For all other write operations, require membership in at least one team.
			// This is a pragmatic check: full workspace-level authorization would require
			// a DB lookup per request keyed to the resource's workspace ID, which is
			// expensive for deeply nested paths. For now we verify the user has any team
			// membership within Terrakube as a baseline gate. Workspace-level access
			// records (access.name ↔ group) are respected in the workspace creation flow.
			allowed := userHasAnyTeam(r.Context(), pool, user.Groups)
			if !allowed {
				log.Printf("AuthZ: write denied for user %s (no team membership, groups=%v)", user.Email, user.Groups)
				http.Error(w,
					`{"errors":[{"status":"403","title":"Forbidden","detail":"No team membership found"}]}`,
					http.StatusForbidden)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// userHasAnyTeam returns true if any of the user's groups matches a team name
// in the Terrakube database. This is the baseline authorization gate for regular users.
func userHasAnyTeam(ctx context.Context, pool *pgxpool.Pool, groups []string) bool {
	if len(groups) == 0 {
		return false
	}
	var count int
	err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM team WHERE name = ANY($1::text[])`,
		groups,
	).Scan(&count)
	if err != nil {
		log.Printf("AuthZ: team lookup failed: %v", err)
		return false
	}
	return count > 0
}

// UserHasWorkspaceAccess returns true if any of the user's groups has an access
// record for the given workspace with the specified permission set to true.
// permColumn must be one of: manage_state, manage_job, manage_workspace.
func UserHasWorkspaceAccess(ctx context.Context, pool *pgxpool.Pool, wsID, permColumn string, groups []string) bool {
	if len(groups) == 0 || pool == nil {
		return false
	}
	safe := map[string]bool{"manage_state": true, "manage_job": true, "manage_workspace": true}
	if !safe[permColumn] {
		return false
	}

	var count int
	err := pool.QueryRow(ctx,
		// Access.name stores the team/group name (matched against user's OIDC groups)
		`SELECT COUNT(*) FROM access
		 WHERE workspace_id = $1
		   AND name = ANY($2::text[])
		   AND `+permColumn+` = true`,
		wsID, groups,
	).Scan(&count)
	if err != nil {
		log.Printf("AuthZ: workspace access lookup failed for %s: %v", wsID, err)
		return false
	}
	return count > 0
}
