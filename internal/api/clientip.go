// Caller-IP resolution now lives in internal/requestip so the platform
// limiter, public chat history, agency limiter, and audit rows all share one
// trust boundary. This wrapper keeps the existing api call sites small.
package api

import (
	"net/http"

	"github.com/davinakmalyasha/agents-proxy/internal/requestip"
)

func clientIP(r *http.Request) string { return requestip.ClientIP(r) }
