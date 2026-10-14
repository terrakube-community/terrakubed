package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Unauthenticated writes to paths AuthMiddleware treats as public must reach
// their handler; AuthzMiddleware used to 401 them because no user was set.
func TestAuthzPassesPublicPathsWithoutUser(t *testing.T) {
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	h := AuthzMiddleware(nil, "")(next)

	cases := []struct {
		method, path string
		wantReached  bool
		wantStatus   int
	}{
		{http.MethodPut, "/remote/tfe/v2/configuration-versions/abc", true, http.StatusOK},
		{http.MethodPut, "/tfstate/v1/archive/abc/terraform.tfstate", true, http.StatusOK},
		{http.MethodPost, "/webhook/v1/some-id", true, http.StatusOK},
		// Non-public writes without a user must still be rejected.
		{http.MethodPut, "/api/v1/organization/x", false, http.StatusUnauthorized},
		{http.MethodPost, "/remote/tfe/v2/runs", false, http.StatusUnauthorized},
	}
	for _, c := range cases {
		reached = false
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if reached != c.wantReached || rec.Code != c.wantStatus {
			t.Errorf("%s %s: reached=%v status=%d, want reached=%v status=%d",
				c.method, c.path, reached, rec.Code, c.wantReached, c.wantStatus)
		}
	}
}
