// DONE: PRD 6 (json helpers) / STANDARDS B
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/davinakmalyasha/agents-proxy/internal/quota"
)

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func qeResetAt(err error) time.Time {
	var qe *quota.QuotaExceededError
	if errors.As(err, &qe) {
		return qe.ResetAt
	}
	return time.Now().Add(time.Hour)
}
