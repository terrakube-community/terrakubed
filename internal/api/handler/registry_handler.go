package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/terrakube-community/terrakubed/internal/storage"
)

// RegistryHandler serves the Terraform Module and Provider Registry APIs.
//
// Module Registry Protocol:
//   GET /registry/v1/modules/{namespace}/{name}/{provider}/versions
//   GET /registry/v1/modules/{namespace}/{name}/{provider}/{version}/download
//
// Provider Registry Protocol:
//   GET /registry/v1/providers/{namespace}/{type}/versions
//   GET /registry/v1/providers/{namespace}/{type}/{version}/download/{os}/{arch}
type RegistryHandler struct {
	pool     *pgxpool.Pool
	hostname string
	storage  storage.StorageService
}

// NewRegistryHandler creates a new handler.
func NewRegistryHandler(pool *pgxpool.Pool, hostname string, storageSvc storage.StorageService) *RegistryHandler {
	return &RegistryHandler{pool: pool, hostname: hostname, storage: storageSvc}
}

func (h *RegistryHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	path := strings.TrimPrefix(r.URL.Path, "/registry/v1/")

	switch {
	case strings.HasPrefix(path, "modules/"):
		h.handleModules(w, r, strings.TrimPrefix(path, "modules/"))
	case strings.HasPrefix(path, "providers/"):
		h.handleProviders(w, r, strings.TrimPrefix(path, "providers/"))
	default:
		http.Error(w, `{"errors":[{"detail":"Not found"}]}`, http.StatusNotFound)
	}
}

// ──────────────────────────────────────────────────
// Module Registry
// ──────────────────────────────────────────────────

func (h *RegistryHandler) handleModules(w http.ResponseWriter, r *http.Request, path string) {
	// Expected patterns:
	//   {namespace}/{name}/{provider}/versions
	//   {namespace}/{name}/{provider}/{version}/download
	parts := strings.Split(strings.TrimSuffix(path, "/"), "/")

	if len(parts) < 3 {
		http.Error(w, `{"errors":[{"detail":"Invalid module path"}]}`, http.StatusBadRequest)
		return
	}

	namespace := parts[0]
	name := parts[1]
	provider := parts[2]

	// List versions: GET /registry/v1/modules/{ns}/{name}/{provider}/versions
	if len(parts) == 4 && parts[3] == "versions" {
		h.listModuleVersions(w, r, namespace, name, provider)
		return
	}

	// Download: GET /registry/v1/modules/{ns}/{name}/{provider}/{version}/download
	if len(parts) == 5 && parts[4] == "download" {
		h.downloadModule(w, r, namespace, name, provider, parts[3])
		return
	}

	// Get specific version info: GET /registry/v1/modules/{ns}/{name}/{provider}/{version}
	if len(parts) == 4 {
		h.getModuleVersion(w, r, namespace, name, provider, parts[3])
		return
	}

	http.Error(w, `{"errors":[{"detail":"Not found"}]}`, http.StatusNotFound)
}

func (h *RegistryHandler) listModuleVersions(w http.ResponseWriter, r *http.Request, namespace, name, provider string) {
	log.Printf("Registry: list module versions %s/%s/%s", namespace, name, provider)

	rows, err := h.pool.Query(r.Context(), `
		SELECT mv.id::text, mv.version
		FROM module_version mv
		JOIN module m ON mv.module_id = m.id
		WHERE m.name = $1 AND m.provider = $2
		  AND LOWER(m.namespace) = LOWER($3)
		  AND mv.status = 'OK'
		ORDER BY mv.version DESC
	`, name, provider, namespace)
	if err != nil {
		log.Printf("Registry: listModuleVersions query error: %v", err)
		http.Error(w, `{"errors":[{"detail":"Internal error"}]}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type Version struct {
		Version string `json:"version"`
	}
	var versions []Version
	for rows.Next() {
		var id, ver string
		if rows.Scan(&id, &ver) == nil {
			versions = append(versions, Version{Version: ver})
		}
	}

	if len(versions) == 0 {
		http.Error(w, `{"errors":[{"detail":"Module not found"}]}`, http.StatusNotFound)
		return
	}

	doc := map[string]interface{}{
		"modules": []map[string]interface{}{
			{
				"versions": versions,
			},
		},
	}
	json.NewEncoder(w).Encode(doc)
}

func (h *RegistryHandler) downloadModule(w http.ResponseWriter, r *http.Request, namespace, name, provider, version string) {
	log.Printf("Registry: download module %s/%s/%s@%s", namespace, name, provider, version)

	var moduleID, versionID string
	err := h.pool.QueryRow(r.Context(), `
		SELECT m.id::text, mv.id::text
		FROM module_version mv
		JOIN module m ON mv.module_id = m.id
		WHERE m.name = $1 AND m.provider = $2
		  AND LOWER(m.namespace) = LOWER($3)
		  AND mv.version = $4
	`, name, provider, namespace, version).Scan(&moduleID, &versionID)
	if err != nil {
		http.Error(w, `{"errors":[{"detail":"Module version not found"}]}`, http.StatusNotFound)
		return
	}

	_ = moduleID

	// The module tar.gz is stored at: modules/{namespace}/{name}/{provider}/{version}.zip
	storageKey := fmt.Sprintf("modules/%s/%s/%s/%s.zip", namespace, name, provider, version)

	// Check if the file exists in storage by generating a download URL
	// For local/minio storage, we serve it directly
	// For cloud storage, we can redirect to a presigned URL
	downloadURL := fmt.Sprintf("https://%s/registry/v1/modules/%s/%s/%s/%s/archive",
		h.hostname, namespace, name, provider, version)

	log.Printf("Registry: module download URL for %s: %s (storageKey: %s)", versionID, downloadURL, storageKey)

	// Terraform expects X-Terraform-Get header with the download URL
	w.Header().Set("X-Terraform-Get", downloadURL)
	w.WriteHeader(http.StatusNoContent)
}

func (h *RegistryHandler) getModuleVersion(w http.ResponseWriter, r *http.Request, namespace, name, provider, version string) {
	// Return module version metadata
	var versionID, description, source string
	err := h.pool.QueryRow(r.Context(), `
		SELECT mv.id::text, COALESCE(m.description,''), COALESCE(m.source,'')
		FROM module_version mv
		JOIN module m ON mv.module_id = m.id
		WHERE m.name = $1 AND m.provider = $2
		  AND LOWER(m.namespace) = LOWER($3)
		  AND mv.version = $4
	`, name, provider, namespace, version).Scan(&versionID, &description, &source)
	if err != nil {
		http.Error(w, `{"errors":[{"detail":"Module version not found"}]}`, http.StatusNotFound)
		return
	}

	doc := map[string]interface{}{
		"id":          versionID,
		"namespace":   namespace,
		"name":        name,
		"provider":    provider,
		"version":     version,
		"description": description,
		"source":      source,
		"published_at": "",
		"downloads":   0,
		"verified":    false,
	}
	json.NewEncoder(w).Encode(doc)
}

// serveModuleArchive serves the module zip directly from storage.
// GET /registry/v1/modules/{ns}/{name}/{provider}/{version}/archive
func (h *RegistryHandler) serveModuleArchive(w http.ResponseWriter, r *http.Request, namespace, name, provider, version string) {
	storageKey := fmt.Sprintf("modules/%s/%s/%s/%s.zip", namespace, name, provider, version)
	reader, err := h.storage.DownloadFile(storageKey)
	if err != nil {
		log.Printf("Registry: module archive not found: %s: %v", storageKey, err)
		http.Error(w, `{"errors":[{"detail":"Module archive not found"}]}`, http.StatusNotFound)
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", "application/zip")
	w.WriteHeader(http.StatusOK)
	if _, err := copy(w, reader); err != nil {
		log.Printf("Registry: error serving module archive: %v", err)
	}
}

// ──────────────────────────────────────────────────
// Provider Registry
// ──────────────────────────────────────────────────

func (h *RegistryHandler) handleProviders(w http.ResponseWriter, r *http.Request, path string) {
	// Expected patterns:
	//   {namespace}/{type}/versions
	//   {namespace}/{type}/{version}/download/{os}/{arch}
	parts := strings.Split(strings.TrimSuffix(path, "/"), "/")

	if len(parts) < 2 {
		http.Error(w, `{"errors":[{"detail":"Invalid provider path"}]}`, http.StatusBadRequest)
		return
	}

	namespace := parts[0]
	provType := parts[1]

	if len(parts) == 3 && parts[2] == "versions" {
		h.listProviderVersions(w, r, namespace, provType)
		return
	}

	if len(parts) == 5 && parts[3] == "download" {
		// {version}/download/{os}/{arch}
		h.downloadProvider(w, r, namespace, provType, parts[2], parts[4], "")
		return
	}

	if len(parts) == 6 && parts[3] == "download" {
		// {version}/download/{os}/{arch}
		h.downloadProvider(w, r, namespace, provType, parts[2], parts[4], parts[5])
		return
	}

	http.Error(w, `{"errors":[{"detail":"Not found"}]}`, http.StatusNotFound)
}

func (h *RegistryHandler) listProviderVersions(w http.ResponseWriter, r *http.Request, namespace, provType string) {
	log.Printf("Registry: list provider versions %s/%s", namespace, provType)

	rows, err := h.pool.Query(r.Context(), `
		SELECT pv.version
		FROM provider_version pv
		JOIN provider p ON pv.provider_id = p.id
		WHERE LOWER(p.namespace) = LOWER($1) AND p.name = $2
		ORDER BY pv.version DESC
	`, namespace, provType)
	if err != nil {
		log.Printf("Registry: listProviderVersions query error: %v", err)
		http.Error(w, `{"errors":[{"detail":"Internal error"}]}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type VersionInfo struct {
		Version   string             `json:"version"`
		Protocols []string           `json:"protocols"`
		Platforms []providerPlatform `json:"platforms"`
	}

	var versions []VersionInfo
	for rows.Next() {
		var ver string
		if rows.Scan(&ver) == nil {
			// Load platforms for this version
			platforms := h.loadProviderPlatforms(r.Context(), namespace, provType, ver)
			versions = append(versions, VersionInfo{
				Version:   ver,
				Protocols: []string{"5.0"},
				Platforms: platforms,
			})
		}
	}

	if len(versions) == 0 {
		http.Error(w, `{"errors":[{"detail":"Provider not found"}]}`, http.StatusNotFound)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"versions": versions})
}

type providerPlatform struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

func (h *RegistryHandler) loadProviderPlatforms(ctx context.Context, namespace, provType, version string) []providerPlatform {
	rows, err := h.pool.Query(ctx, `
		SELECT os, arch
		FROM provider_version pv
		JOIN provider p ON pv.provider_id = p.id
		WHERE LOWER(p.namespace) = LOWER($1) AND p.name = $2 AND pv.version = $3
	`, namespace, provType, version)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var platforms []providerPlatform
	for rows.Next() {
		var os, arch string
		if rows.Scan(&os, &arch) == nil {
			platforms = append(platforms, providerPlatform{OS: os, Arch: arch})
		}
	}
	return platforms
}

func (h *RegistryHandler) downloadProvider(w http.ResponseWriter, r *http.Request, namespace, provType, version, os_, arch string) {
	log.Printf("Registry: download provider %s/%s@%s %s/%s", namespace, provType, version, os_, arch)

	var filename, shasumsURL, shasumsSignatureURL string
	err := h.pool.QueryRow(r.Context(), `
		SELECT COALESCE(filename,''), COALESCE(shasums_url,''), COALESCE(shasums_signature_url,'')
		FROM provider_version pv
		JOIN provider p ON pv.provider_id = p.id
		WHERE LOWER(p.namespace) = LOWER($1) AND p.name = $2
		  AND pv.version = $3 AND pv.os = $4 AND pv.arch = $5
	`, namespace, provType, version, os_, arch).Scan(&filename, &shasumsURL, &shasumsSignatureURL)
	if err != nil {
		http.Error(w, `{"errors":[{"detail":"Provider version not found for this platform"}]}`, http.StatusNotFound)
		return
	}

	if filename == "" {
		filename = fmt.Sprintf("terraform-provider-%s_%s_%s_%s.zip", provType, version, os_, arch)
	}

	downloadURL := fmt.Sprintf("https://%s/registry/v1/providers/%s/%s/%s/%s/%s/archive",
		h.hostname, namespace, provType, version, os_, arch)

	doc := map[string]interface{}{
		"protocols":              []string{"5.0"},
		"os":                     os_,
		"arch":                   arch,
		"filename":               filename,
		"download_url":           downloadURL,
		"shasums_url":            shasumsURL,
		"shasums_signature_url":  shasumsSignatureURL,
		"signing_keys":           map[string][]interface{}{"gpg_public_keys": {}},
	}
	json.NewEncoder(w).Encode(doc)
}

// copy is a helper to avoid shadowing io.Copy in this package.
var copy = func(dst http.ResponseWriter, src interface{ Read([]byte) (int, error) }) (int64, error) {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			dst.Write(buf[:n])
			total += int64(n)
		}
		if err != nil {
			break
		}
	}
	return total, nil
}
