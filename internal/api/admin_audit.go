package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/auth"
	"github.com/davinakmalyasha/agents-proxy/internal/middleware"
)

// AdminAudit writes one append-only row for every request that passes the
// admin gate. The row is inserted BEFORE the handler runs, so an action
// cannot execute without a trace; status and duration are filled in after.
// If the insert fails the request fails closed (500): an unaudited admin
// mutation is worse than a failed one.
//
// Request bodies are deliberately not stored: POST /clients/{id}/owner
// carries a password. Query parameters, route pattern, and the actor
// fingerprint are enough to reconstruct who did what.
type AdminAudit struct {
	Pool *pgxpool.Pool
}

type auditRecorder struct {
	http.ResponseWriter
	status int
}

func (a *auditRecorder) WriteHeader(code int) {
	if a.status == 0 {
		a.status = code
	}
	a.ResponseWriter.WriteHeader(code)
}

func (a *auditRecorder) Write(b []byte) (int, error) {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	return a.ResponseWriter.Write(b)
}

func (a *auditRecorder) Unwrap() http.ResponseWriter { return a.ResponseWriter }

func (a *auditRecorder) Flush() {
	if a.status == 0 {
		a.WriteHeader(http.StatusOK)
	}
	if f, ok := a.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (a *AdminAudit) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Pool == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "audit unavailable")
			return
		}
		actor := auth.AdminActor(r.Context())
		if actor == "" {
			// RequireAdmin always sets this; default rather than write an
			// empty actor if the middleware is ever mounted out of order.
			actor = "admin-key:unknown"
		}
		action := adminAuditAction(r)
		var detail []byte
		if d, err := json.Marshal(adminAuditDetail(r)); err == nil {
			detail = d
		} else {
			detail = []byte(`{}`)
		}

		var id int64
		err := a.Pool.QueryRow(r.Context(),
			`INSERT INTO public.admin_audit_log
			   (actor, action, target, method, path, status, request_id, remote_addr, detail)
			 VALUES ($1, $2, $3, $4, $5, 0, $6, $7, $8::jsonb)
			 RETURNING id`,
			actor, action, adminAuditTarget(r), r.Method, r.URL.Path,
			auditRequestID(r), clientIP(r), string(detail)).Scan(&id)
		if err != nil {
			slog.Error("admin audit insert", "action", action, "err", err)
			writeJSONError(w, http.StatusInternalServerError, "audit unavailable")
			return
		}

		start := time.Now()
		rec := &auditRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		// The handler is finished and the client may already be gone;
		// use a short detached context so the audit update survives it.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := a.Pool.Exec(ctx,
			`UPDATE public.admin_audit_log
			    SET status = $2, duration_ms = $3
			  WHERE id = $1`,
			id, status, time.Since(start).Milliseconds()); err != nil {
			slog.Error("admin audit update", "action", action, "err", err)
		}
	})
}

func adminAuditAction(r *http.Request) string {
	pattern := chi.RouteContext(r.Context()).RoutePattern()
	// Use a parameterized pattern for stable grouping; fall back to the
	// concrete path for static routes and for mounts that do not expose a
	// chi pattern.
	if pattern == "" || !strings.Contains(pattern, "{") {
		pattern = r.URL.Path
	}
	return r.Method + " " + pattern
}

func adminAuditTarget(r *http.Request) string {
	values := make(map[string]string, 5)
	if id := chi.URLParam(r, "id"); id != "" {
		values["id"] = truncateAudit(id, 128)
	}
	for key, list := range r.URL.Query() {
		if len(list) == 0 || !safeAdminTargetField(key) {
			continue
		}
		values[key] = truncateAudit(list[0], 256)
	}
	// Safe body fields make create/update actions attributable without
	// storing the body: tenant_id, origin, suffix, feature. Passwords and
	// user emails are never in this whitelist.
	for key, value := range adminAuditBodyFields(r) {
		if value != "" {
			values[key] = truncateAudit(value, 256)
		}
	}
	order := []string{"id", "tenant_id", "origin", "suffix", "feature"}
	parts := make([]string, 0, len(order))
	for _, key := range order {
		if value := values[key]; value != "" {
			parts = append(parts, key+"="+value)
		}
	}
	return truncateAudit(strings.Join(parts, " "), 512)
}

func safeAdminTargetField(key string) bool {
	switch key {
	case "tenant_id", "origin", "suffix", "feature":
		return true
	default:
		return false
	}
}

const maxAdminAuditBody = 64 << 10

// adminAuditBodyFields extracts only the safe target fields above. The body
// is restored so the handler sees the original request. Bodies larger than
// the cap are left unread (and unlogged) rather than truncated for the
// handler.
func adminAuditBodyFields(r *http.Request) map[string]string {
	if r.Body == nil ||
		(r.Method != http.MethodPost && r.Method != http.MethodPatch && r.Method != http.MethodPut) {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAdminAuditBody+1))
	if err != nil {
		return nil
	}
	if len(body) > maxAdminAuditBody {
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
		return nil
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	out := make(map[string]string, 4)
	for key := range raw {
		if !safeAdminTargetField(key) {
			continue
		}
		var value string
		if err := json.Unmarshal(raw[key], &value); err == nil {
			out[key] = value
		}
	}
	return out
}

func adminAuditDetail(r *http.Request) map[string]any {
	safe := map[string]string{}
	for key, values := range r.URL.Query() {
		if len(values) == 0 {
			continue
		}
		safe[key] = truncateAudit(values[0], 256)
	}
	return map[string]any{
		"query":      safe,
		"user_agent": truncateAudit(r.UserAgent(), 256),
	}
}

func auditRequestID(r *http.Request) string {
	id, _ := r.Context().Value(middleware.CtxRequestID).(string)
	return id
}

func truncateAudit(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
