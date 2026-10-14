package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The real Terraform CLI creates a configuration version via
// POST /workspaces/{id}/configuration-versions. That path used to 404.
func TestWorkspaceScopedConfigVersionCreate(t *testing.T) {
	h := NewRemoteTFEHandler(nil, "api.example.com", nil)

	for _, path := range []string{
		"/remote/tfe/v2/workspaces/ws-123/configuration-versions",
		"/remote/tfe/v2/configuration-versions",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s: status = %d, want 201 (body %s)", path, rec.Code, rec.Body.String())
		}
		var doc struct {
			Data struct {
				ID         string                 `json:"id"`
				Type       string                 `json:"type"`
				Attributes map[string]interface{} `json:"attributes"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		up, _ := doc.Data.Attributes["upload-url"].(string)
		if doc.Data.Type != "configuration-versions" || !strings.Contains(up, "/configuration-versions/"+doc.Data.ID) {
			t.Fatalf("%s: unexpected doc %s", path, rec.Body.String())
		}
	}
}
