package handler

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

// ReleasesHandler proxies (with caching) the upstream Terraform/OpenTofu
// releases index so the UI's "Terraform Version" dropdown can populate.
// Mirrors Java's TerraformJsonController ("/terraform/index.json") and
// TofuJsonController ("/tofu/index.json") — this endpoint didn't exist at
// all in the Go rewrite, so the dropdown spun forever on a 404.
//
// The UI expects each upstream's native response shape passed straight
// through (no transformation): HashiCorp's index.json object for terraform,
// GitHub's releases array for tofu — see ui/.../Settings/General.tsx.
type ReleasesHandler struct {
	terraformURL string
	tofuURL      string
	githubToken  string
	cacheTTL     time.Duration
	httpClient   *http.Client

	mu    sync.Mutex
	cache map[string]cachedReleases
}

type cachedReleases struct {
	body      []byte
	fetchedAt time.Time
}

// NewReleasesHandler creates a handler serving GET /terraform/index.json and
// GET /tofu/index.json.
func NewReleasesHandler(terraformURL, tofuURL, githubToken string, cacheExpirationMinutes int64) *ReleasesHandler {
	return &ReleasesHandler{
		terraformURL: terraformURL,
		tofuURL:      tofuURL,
		githubToken:  githubToken,
		cacheTTL:     time.Duration(cacheExpirationMinutes) * time.Minute,
		httpClient:   &http.Client{Timeout: 15 * time.Second},
		cache:        make(map[string]cachedReleases),
	}
}

func (h *ReleasesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var iacType, upstreamURL string
	switch r.URL.Path {
	case "/terraform/index.json":
		iacType, upstreamURL = "terraform", h.terraformURL
	case "/tofu/index.json":
		iacType, upstreamURL = "tofu", h.tofuURL
	default:
		http.NotFound(w, r)
		return
	}

	body, err := h.getReleases(iacType, upstreamURL)
	if err != nil {
		log.Printf("ReleasesHandler: failed to fetch %s releases: %v", iacType, err)
		http.Error(w, "failed to fetch releases", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

func (h *ReleasesHandler) getReleases(iacType, upstreamURL string) ([]byte, error) {
	h.mu.Lock()
	if cached, ok := h.cache[iacType]; ok && time.Since(cached.fetchedAt) < h.cacheTTL {
		h.mu.Unlock()
		return cached.body, nil
	}
	h.mu.Unlock()

	req, err := http.NewRequest(http.MethodGet, upstreamURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "terrakubed")
	if iacType == "tofu" && h.githubToken != "" {
		req.Header.Set("Authorization", "Bearer "+h.githubToken)
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream %s returned %d: %s", upstreamURL, resp.StatusCode, string(body))
	}

	h.mu.Lock()
	h.cache[iacType] = cachedReleases{body: body, fetchedAt: time.Now()}
	h.mu.Unlock()

	return body, nil
}
