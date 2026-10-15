package handler

import (
	"net/http"
	"strconv"
)

// logSlice returns the part of data the TFE log protocol asks for.
//
// Terraform CLI (go-tfe's LogReader) polls the log-read-url with
// ?limit=<buffer size>&offset=<bytes already read> and advances its offset by
// however many bytes come back. It only checks whether the run has finished
// when a poll returns *no* new bytes. Serving the whole log on every request
// (as the plan/apply log endpoints did) meant every poll returned data, so the
// CLI never reached that check: it re-read the log head forever (one run was
// observed re-fetching the same 120,957-byte log ~10 times a second) and never
// noticed the plan was already finished.
//
// A missing/invalid offset means 0; a missing or non-positive limit means "to
// the end"; an offset at or past the end yields an empty slice.
func logSlice(data []byte, offsetParam, limitParam string) []byte {
	offset, err := strconv.Atoi(offsetParam)
	if err != nil || offset < 0 {
		offset = 0
	}
	if offset >= len(data) {
		return nil
	}
	end := len(data)
	if limit, err := strconv.Atoi(limitParam); err == nil && limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return data[offset:end]
}

// serveLogSlice writes the requested slice of a log as text/plain.
func serveLogSlice(w http.ResponseWriter, r *http.Request, data []byte) {
	q := r.URL.Query()
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write(logSlice(data, q.Get("offset"), q.Get("limit")))
}
