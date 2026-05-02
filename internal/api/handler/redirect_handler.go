package handler

import (
	"fmt"
	"net/http"
	"strings"
)

// AppRedirectHandler handles the /app/{orgId}/{wsId}/runs/{jobId} URL pattern.
// This is used by Slack notifications (and other integrations) when they don't
// have an explicit UI URL configured — the API itself acts as a redirect pivot.
//
// The Java API had a RedirectController that did the same: it resolves the
// org/workspace/job from the URL params and redirects to the UI's run page.
//
//	GET /app/{orgId}/{wsId}/runs/{jobId}
//	→ 302 {uiURL}/organizations/{orgId}/workspaces/{wsId}/runs/{jobId}
type AppRedirectHandler struct {
	uiURL string
}

// NewAppRedirectHandler creates a redirect handler. If uiURL is empty,
// the handler returns a plain-text link instead of a 302.
func NewAppRedirectHandler(uiURL string) *AppRedirectHandler {
	return &AppRedirectHandler{uiURL: uiURL}
}

func (h *AppRedirectHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Path: /app/{orgId}/{wsId}/runs/{jobId}
	path := strings.TrimPrefix(r.URL.Path, "/app/")
	parts := strings.Split(path, "/")
	if len(parts) < 4 || parts[2] != "runs" {
		http.Error(w, "invalid path — expected /app/{orgId}/{wsId}/runs/{jobId}", http.StatusBadRequest)
		return
	}

	orgID := parts[0]
	wsID := parts[1]
	jobID := parts[3]

	uiBase := strings.TrimRight(h.uiURL, "/")
	if uiBase == "" {
		// No UI URL — return a minimal HTML page with the details
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body>
<h2>Terrakube Run</h2>
<p>Organization: %s</p>
<p>Workspace: %s</p>
<p>Job: %s</p>
<p>Configure <code>TERRAKUBE_UI_URL</code> to enable direct UI links.</p>
</body></html>`, orgID, wsID, jobID)
		return
	}

	// Redirect to UI
	target := fmt.Sprintf("%s/organizations/%s/workspaces/%s/runs/%s", uiBase, orgID, wsID, jobID)
	http.Redirect(w, r, target, http.StatusFound)
}
