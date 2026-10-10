package handler

import (
	"encoding/json"
	"net/http"

	"github.com/april/shorten-link/internal/middleware"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeIfTooLarge writes 413 and returns true if err came from an oversized body.
func writeIfTooLarge(w http.ResponseWriter, err error) bool {
	if middleware.IsBodyTooLarge(err) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return true
	}
	return false
}
