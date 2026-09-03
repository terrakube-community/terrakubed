package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/terrakube-community/terrakubed/internal/api/middleware"
)

// PatHandler handles /pat/v1 endpoints for Personal Access Token management.
type PatHandler struct {
	pool      *pgxpool.Pool
	patSecret string // base64url-encoded HMAC key
}

// NewPatHandler creates a new handler.
func NewPatHandler(pool *pgxpool.Pool, patSecret string) *PatHandler {
	return &PatHandler{pool: pool, patSecret: patSecret}
}

type patCreateRequest struct {
	Days        int    `json:"days"`
	Description string `json:"description"`
}

type patCreateResponse struct {
	Token string `json:"token"`
}

type patListItem struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Days        int    `json:"days"`
	CreatedDate string `json:"createdDate,omitempty"`
	CreatedBy   string `json:"createdBy,omitempty"`
}

func (h *PatHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		h.listTokens(w, r)
	case http.MethodPost:
		h.createToken(w, r)
	case http.MethodDelete:
		h.deleteToken(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *PatHandler) createToken(w http.ResponseWriter, r *http.Request) {
	var req patCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Get user info from context (set by AuthMiddleware)
	user := middleware.GetUser(r.Context())
	email := "unknown"
	name := "unknown"
	groups := ""
	if user != nil {
		if user.Email != "" {
			email = user.Email
		}
		if user.Name != "" {
			name = user.Name
		}
		if len(user.Groups) > 0 {
			groups = strings.Join(user.Groups, ",")
		}
	}
	_ = groups

	// Insert PAT record
	patID := uuid.New()
	_, err := h.pool.Exec(r.Context(),
		`INSERT INTO pat (id, days, deleted, description, created_by, created_date)
		 VALUES ($1, $2, false, $3, $4, $5)`,
		patID, req.Days, req.Description, email, time.Now(),
	)
	if err != nil {
		log.Printf("Failed to create PAT: %v", err)
		http.Error(w, "Failed to create token", http.StatusInternalServerError)
		return
	}

	// Generate JWT
	claims := jwt.MapClaims{
		"iss":            "Terrakube",
		"sub":            fmt.Sprintf("%s (Token)", name),
		"aud":            "Terrakube",
		"jti":            patID.String(),
		"email":          email,
		"email_verified": true,
		"name":           fmt.Sprintf("%s (Token)", name),
		"iat":            time.Now().Unix(),
	}

	// Parse groups
	if groups != "" {
		claims["groups"] = strings.Split(groups, ",")
	}

	// Set expiration if days > 0
	if req.Days > 0 {
		claims["exp"] = time.Now().Add(time.Duration(req.Days) * 24 * time.Hour).Unix()
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signingKey := []byte(h.patSecret)
	tokenString, err := token.SignedString(signingKey)
	if err != nil {
		log.Printf("Failed to sign PAT JWT: %v", err)
		http.Error(w, "Failed to generate token", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(patCreateResponse{Token: tokenString})
}

func (h *PatHandler) listTokens(w http.ResponseWriter, r *http.Request) {
	email := "unknown"
	if user := middleware.GetUser(r.Context()); user != nil && user.Email != "" {
		email = user.Email
	}

	rows, err := h.pool.Query(r.Context(),
		`SELECT id, description, days, created_date, created_by
		 FROM pat WHERE deleted = false AND created_by = $1
		 ORDER BY created_date DESC`, email,
	)
	if err != nil {
		log.Printf("Failed to list PATs: %v", err)
		http.Error(w, "Failed to list tokens", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	tokens := make([]patListItem, 0)
	for rows.Next() {
		var p patListItem
		var createdDate time.Time
		if err := rows.Scan(&p.ID, &p.Description, &p.Days, &createdDate, &p.CreatedBy); err != nil {
			continue
		}
		p.CreatedDate = createdDate.Format(time.RFC3339)
		tokens = append(tokens, p)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(tokens)
}

func (h *PatHandler) deleteToken(w http.ResponseWriter, r *http.Request) {
	// Extract token ID from path: /pat/v1/{tokenId}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/pat/v1/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "Token ID required", http.StatusBadRequest)
		return
	}
	tokenID := parts[0]

	result, err := h.pool.Exec(r.Context(),
		`UPDATE pat SET deleted = true WHERE id = $1`, tokenID,
	)
	if err != nil {
		log.Printf("Failed to delete PAT: %v", err)
		http.Error(w, "Failed to delete token", http.StatusInternalServerError)
		return
	}

	if result.RowsAffected() == 0 {
		http.Error(w, "Token not found", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

// ──────────────────────────────────────────────────
// TeamTokenHandler handles /access-token/v1/teams endpoints.
// ──────────────────────────────────────────────────

type TeamTokenHandler struct {
	pool       *pgxpool.Pool
	patSecret  string
	ownerGroup string
}

func NewTeamTokenHandler(pool *pgxpool.Pool, patSecret, ownerGroup string) *TeamTokenHandler {
	return &TeamTokenHandler{pool: pool, patSecret: patSecret, ownerGroup: ownerGroup}
}

func (h *TeamTokenHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/access-token/v1/teams")

	switch {
	case r.Method == http.MethodOptions:
		w.WriteHeader(http.StatusOK)

	case r.Method == http.MethodGet && path == "/current-teams":
		h.currentTeams(w, r)

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/permissions/organization/"):
		h.getPermissions(w, r, path)

	case r.Method == http.MethodGet && (path == "" || path == "/"):
		h.listTokens(w, r)

	case r.Method == http.MethodPost && (path == "" || path == "/"):
		h.createToken(w, r)

	case r.Method == http.MethodDelete:
		h.deleteToken(w, r, path)

	default:
		http.Error(w, "Not found", http.StatusNotFound)
	}
}

func (h *TeamTokenHandler) currentTeams(w http.ResponseWriter, r *http.Request) {
	// Get user's groups from context (set by AuthMiddleware)
	groups := make([]string, 0)
	if user := middleware.GetUser(r.Context()); user != nil {
		for _, g := range user.Groups {
			g = strings.TrimSpace(g)
			if g != "" {
				groups = append(groups, g)
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string][]string{"groups": groups})
}

func (h *TeamTokenHandler) getPermissions(w http.ResponseWriter, r *http.Request, path string) {
	// Parse: /permissions/organization/{orgId} or /permissions/organization/{orgId}/workspace/{wsId}
	parts := strings.Split(strings.TrimPrefix(path, "/permissions/organization/"), "/")
	orgID := parts[0]

	var wsID string
	if len(parts) >= 3 && parts[1] == "workspace" {
		wsID = parts[2]
	}

	// Get user's groups from context (set by AuthMiddleware)
	groups := make([]string, 0)
	isOwner := false
	userEmail := "unknown"
	if user := middleware.GetUser(r.Context()); user != nil {
		userEmail = user.Email
		// Service accounts get full permissions
		if user.IsServiceAccount() {
			isOwner = true
			log.Printf("[permissions] service account %q → isOwner=true (orgID=%s wsID=%s)", userEmail, orgID, wsID)
		} else {
			for _, g := range user.Groups {
				groups = append(groups, strings.TrimSpace(g))
				if strings.EqualFold(strings.TrimSpace(g), h.ownerGroup) {
					isOwner = true
				}
			}
			log.Printf("[permissions] user=%q groups=%v ownerGroup=%q isOwner=%v orgID=%s wsID=%s",
				userEmail, groups, h.ownerGroup, isOwner, orgID, wsID)
		}
	} else {
		log.Printf("[permissions] no user in context (unauthenticated?) orgID=%s wsID=%s", orgID, wsID)
	}

	// Start with the same 8 flags as the Java API.
	// For owners (and service accounts) all flags are true immediately.
	// For regular users they are populated from the team DB records below.
	permissions := map[string]bool{
		"manageState":      isOwner,
		"manageWorkspace":  isOwner,
		"manageModule":     isOwner,
		"manageProvider":   isOwner,
		"manageVcs":        isOwner,
		"manageTemplate":   isOwner,
		"manageCollection": isOwner,
		"manageJob":        isOwner,
	}

	if !isOwner {
		// Query team permissions from DB (same logic as Java API)
		h.loadTeamPermissions(r.Context(), orgID, groups, permissions)
		log.Printf("[permissions] after team lookup: %v", permissions)
	}

	// If workspace ID is provided, also check workspace-level access
	if wsID != "" {
		h.loadWorkspacePermissions(r.Context(), wsID, groups, permissions)
		log.Printf("[permissions] after workspace lookup (wsID=%s): %v", wsID, permissions)
	}

	// The current Terrakube UI (RBAC v2) reads three additional fields that the
	// legacy 8-flag PermissionSet never had: planJob, approveJob, managePermission.
	// managePermission gates entire admin-only Settings pages (Teams, General,
	// Global Variables, Agents, Federated Credentials, Actions) — omitting it
	// makes the UI default every such button to disabled, even for a fully
	// permissioned user, since `response.data.managePermission ?? false` always
	// resolves to false when the key is absent.
	//
	// Real Java semantics: managePermission = OR of canManageWorkspace(team) across
	// the user's teams, forced true if the user is in the instance-owner group.
	// planJob/approveJob are new team-level grants (plan_job/approve_job columns)
	// that supersede the legacy manage_job boolean; until those columns are read
	// here, mirror manage_job for both — this exactly matches how Java itself
	// backfilled plan_job/approve_job from manage_job when RBAC v2 was introduced,
	// so it's a safe, always-correct default regardless of migration state.
	permissions["planJob"] = permissions["manageJob"]
	permissions["approveJob"] = permissions["manageJob"]
	permissions["managePermission"] = permissions["manageWorkspace"] || isOwner

	log.Printf("[permissions] final response for user=%q orgID=%s wsID=%s: %v", userEmail, orgID, wsID, permissions)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if err := json.NewEncoder(w).Encode(permissions); err != nil {
		log.Printf("[permissions] ERROR encoding response: %v", err)
	}
}

func (h *TeamTokenHandler) loadTeamPermissions(ctx context.Context, orgID string, groups []string, permissions map[string]bool) {
	if len(groups) == 0 {
		log.Printf("[permissions] loadTeamPermissions: no groups, skipping DB query")
		return
	}

	// Build IN clause for groups
	args := []interface{}{orgID}
	placeholders := make([]string, len(groups))
	for i, g := range groups {
		args = append(args, strings.TrimSpace(g))
		placeholders[i] = fmt.Sprintf("$%d", i+2)
	}

	query := fmt.Sprintf(`SELECT manage_state, manage_workspace, manage_module,
		manage_provider, manage_vcs, manage_template, manage_collection, manage_job
		FROM team WHERE organization_id = $1 AND name IN (%s)`,
		strings.Join(placeholders, ","))

	log.Printf("[permissions] loadTeamPermissions: querying DB orgID=%s groups=%v", orgID, groups)

	rows, err := h.pool.Query(ctx, query, args...)
	if err != nil {
		log.Printf("[permissions] loadTeamPermissions: DB query error: %v", err)
		return
	}
	defer rows.Close()

	rowCount := 0
	for rows.Next() {
		rowCount++
		var ms, mw, mm, mp, mv, mt, mc, mj bool
		if err := rows.Scan(&ms, &mw, &mm, &mp, &mv, &mt, &mc, &mj); err != nil {
			log.Printf("[permissions] loadTeamPermissions: scan error: %v", err)
			continue
		}
		log.Printf("[permissions] loadTeamPermissions: row %d → state=%v ws=%v mod=%v prov=%v vcs=%v tpl=%v coll=%v job=%v",
			rowCount, ms, mw, mm, mp, mv, mt, mc, mj)
		permissions["manageState"] = permissions["manageState"] || ms
		permissions["manageWorkspace"] = permissions["manageWorkspace"] || mw
		permissions["manageModule"] = permissions["manageModule"] || mm
		permissions["manageProvider"] = permissions["manageProvider"] || mp
		permissions["manageVcs"] = permissions["manageVcs"] || mv
		permissions["manageTemplate"] = permissions["manageTemplate"] || mt
		permissions["manageCollection"] = permissions["manageCollection"] || mc
		permissions["manageJob"] = permissions["manageJob"] || mj
	}
	log.Printf("[permissions] loadTeamPermissions: matched %d team row(s) for orgID=%s", rowCount, orgID)
}

func (h *TeamTokenHandler) loadWorkspacePermissions(ctx context.Context, wsID string, groups []string, permissions map[string]bool) {
	if len(groups) == 0 {
		return
	}
	// Filter by user's groups: access.name contains the team/group name
	rows, err := h.pool.Query(ctx,
		`SELECT manage_state, manage_workspace, manage_job
		 FROM access
		 WHERE workspace_id = $1 AND name = ANY($2::text[])`,
		wsID, groups)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var ms, mw, mj bool
		if err := rows.Scan(&ms, &mw, &mj); err != nil {
			continue
		}
		permissions["manageState"] = permissions["manageState"] || ms
		permissions["manageWorkspace"] = permissions["manageWorkspace"] || mw
		permissions["manageJob"] = permissions["manageJob"] || mj
	}
}

type teamTokenCreateRequest struct {
	Group       string `json:"group"`
	Description string `json:"description"`
	Days        int    `json:"days"`
	Hours       int    `json:"hours"`
	Minutes     int    `json:"minutes"`
}

func (h *TeamTokenHandler) createToken(w http.ResponseWriter, r *http.Request) {
	var req teamTokenCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	email := "unknown"
	name := "unknown"
	if user := middleware.GetUser(r.Context()); user != nil {
		if user.Email != "" {
			email = user.Email
		}
		if user.Name != "" {
			name = user.Name
		}
	}

	duration := time.Duration(req.Days)*24*time.Hour +
		time.Duration(req.Hours)*time.Hour +
		time.Duration(req.Minutes)*time.Minute

	claims := jwt.MapClaims{
		"iss":            "Terrakube",
		"sub":            fmt.Sprintf("%s (Team Token)", name),
		"aud":            "Terrakube",
		"jti":            uuid.New().String(),
		"email":          email,
		"email_verified": true,
		"name":           fmt.Sprintf("%s (Team Token)", name),
		"groups":         []string{req.Group},
		"iat":            time.Now().Unix(),
	}

	if duration > 0 {
		claims["exp"] = time.Now().Add(duration).Unix()
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString([]byte(h.patSecret))
	if err != nil {
		log.Printf("Failed to sign team token: %v", err)
		http.Error(w, "Failed to generate token", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"token": tokenString})
}

func (h *TeamTokenHandler) listTokens(w http.ResponseWriter, r *http.Request) {
	// Group table stores team tokens in Java API
	email := "unknown"
	if user := middleware.GetUser(r.Context()); user != nil && user.Email != "" {
		email = user.Email
	}

	rows, err := h.pool.Query(r.Context(),
		`SELECT id, description, days, created_date, created_by
		 FROM "group" WHERE deleted = false AND created_by = $1
		 ORDER BY created_date DESC`, email,
	)
	if err != nil {
		// Table might not exist or be named differently — return empty
		log.Printf("Failed to list team tokens: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode([]interface{}{})
		return
	}
	defer rows.Close()

	tokens := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, desc, createdBy string
		var days int
		var createdDate time.Time
		if err := rows.Scan(&id, &desc, &days, &createdDate, &createdBy); err != nil {
			continue
		}
		tokens = append(tokens, map[string]interface{}{
			"id":          id,
			"description": desc,
			"days":        days,
			"createdDate": createdDate.Format(time.RFC3339),
			"createdBy":   createdBy,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(tokens)
}

func (h *TeamTokenHandler) deleteToken(w http.ResponseWriter, r *http.Request, path string) {
	tokenID := strings.TrimPrefix(path, "/")
	if tokenID == "" {
		http.Error(w, "Token ID required", http.StatusBadRequest)
		return
	}

	result, err := h.pool.Exec(r.Context(),
		`UPDATE "group" SET deleted = true WHERE id = $1`, tokenID,
	)
	if err != nil {
		http.Error(w, "Failed to delete token", http.StatusInternalServerError)
		return
	}

	if result.RowsAffected() == 0 {
		http.Error(w, "Token not found", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusAccepted)
}
