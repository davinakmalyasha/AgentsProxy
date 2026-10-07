package records

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/cache"
	"github.com/davinakmalyasha/agents-proxy/internal/usage"
)

// DONE: PRD 6.5 (records CRUD) / STANDARDS A, D, F

// Handler is the records-feature HTTP handler. It's stateless; the
// dependencies travel in via Deps at construction.
type Handler struct {
	Pool *pgxpool.Pool
}

// New returns a Handler. Kept as a constructor so future fields
// (e.g. a metrics sink) can be added without changing call sites.
func New(deps Deps) *Handler {
	return &Handler{Pool: deps.Pool}
}

// ---- payload types ----

type recordWriteReq struct {
	Kind    string          `json:"kind"`
	Title   string          `json:"title"`
	Slug    string          `json:"slug"`
	Payload json.RawMessage `json:"payload"`
	Status  string          `json:"status"`
}

type recordPatchReq struct {
	Title   *string         `json:"title"`
	Slug    *string         `json:"slug"`
	Payload json.RawMessage `json:"payload"`
	Status  *string         `json:"status"`
}

type recordResp struct {
	ID        string          `json:"id"`
	TenantID  string          `json:"tenant_id"`
	Vertical  string          `json:"vertical"`
	Kind      string          `json:"kind"`
	Title     string          `json:"title"`
	Slug      string          `json:"slug"`
	Payload   json.RawMessage `json:"payload"`
	Status    string          `json:"status"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
}

type listResp struct {
	Items      []recordResp `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty"`
	HasMore    bool         `json:"has_more"`
	// Deprecated: kept for the legacy ?page= caller (DONE: PRD 9 cursor pagination).
	// Removed from new clients; see RUNBOOK.md for the migration note.
	Page  int `json:"page,omitempty"`
	Limit int `json:"limit,omitempty"`
}

// validStatuses is the allow-list for the status field on POST/PATCH.
// `deleted` is reachable only via DELETE; clients cannot set it
// directly. `archived` is an opt-in extension for clients that want
// to hide a row without losing it.
var validStatuses = map[string]bool{
	"active":   true,
	"archived": true,
}

// Routes returns a chi router that handles the generic CRUD for one
// vertical. Mount under /<vertical>/records by the per-vertical
// builder. Caller is responsible for putting withClientAuth on the
// parent router.
func (h *Handler) Routes(vertical string) http.Handler {
	r := chi.NewRouter()
	r.Post("/", h.create(vertical))
	r.Get("/", h.list(vertical))
	r.Get("/{id}", WithValidID(h.getOne(vertical)))
	r.Patch("/{id}", WithValidID(h.patch(vertical)))
	r.Delete("/{id}", WithValidID(h.del(vertical)))
	return r
}

// ---- handlers ----

func (h *Handler) create(vertical string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cm := requireClient(w, r)
		if cm == nil {
			return
		}
		var req recordWriteReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid json")
			return
		}
		kind := strings.TrimSpace(req.Kind)
		if kind == "" {
			writeJSONError(w, http.StatusBadRequest, "kind required")
			return
		}
		if len(req.Payload) == 0 {
			req.Payload = json.RawMessage(`{}`)
		}
		status := req.Status
		if status == "" {
			status = "active"
		}
		if !validStatuses[status] {
			writeJSONError(w, http.StatusBadRequest, "invalid status")
			return
		}
		var id string
		err := withTenantTx(r.Context(), h.Pool, cm.TenantID, func(tx pgx.Tx) error {
			return tx.QueryRow(r.Context(),
				`INSERT INTO tenant_records (tenant_id, vertical, kind, title, slug, payload, status)
				 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id::text`,
				cm.TenantID, vertical, kind, req.Title, req.Slug, []byte(req.Payload), status,
			).Scan(&id)
		})
		if err != nil {
			slog.Error("create record", "err", err, "tenant", cm.TenantID, "vertical", vertical)
			writeJSONError(w, http.StatusInternalServerError, "internal")
			return
		}
		if err := usage.Record(r.Context(), h.Pool, nil, cm.ID, "mimo", "business", len(req.Payload)/4, 0); err != nil {
			slog.Error("usage record failed", "client_id", cm.ID, "err", err)
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": id})
	}
}

func (h *Handler) list(vertical string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cm := requireClient(w, r)
		if cm == nil {
			return
		}
		kind := strings.TrimSpace(r.URL.Query().Get("kind"))
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		status := strings.TrimSpace(r.URL.Query().Get("status"))
		if status == "" {
			status = "active" // hardening #1: soft-delete leak fix
		}
		if status != "active" && status != "deleted" {
			writeJSONError(w, http.StatusBadRequest, "invalid status")
			return
		}
		limit := parseLimit(r.URL.Query().Get("limit"))
		cur, err := decodeCursor(r.URL.Query().Get("before"))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid cursor")
			return
		}

		var (
			items  []recordResp
			lastTs time.Time
			lastID string
		)
		err = withTenantTx(r.Context(), h.Pool, cm.TenantID, func(tx pgx.Tx) error {
			listSQL := `SELECT id::text, tenant_id, vertical, kind, title, slug, payload, status, created_at, updated_at
			            FROM tenant_records
			            WHERE vertical = $1 AND status = $2`
			args := []any{vertical, status}
			idx := 3
			if kind != "" {
				listSQL += fmt.Sprintf(" AND kind = $%d", idx)
				args = append(args, kind)
				idx++
			}
			if q != "" {
				listSQL += fmt.Sprintf(" AND (title ILIKE $%d OR payload::text ILIKE $%d)", idx, idx)
				args = append(args, "%"+q+"%")
				idx++
			}
			// Cursor: skip rows (created_at, id) >= (cur.ts, cur.id) under
			// DESC ordering. The composite condition keeps the sort
			// stable when two rows share a created_at.
			if !cur.ts.IsZero() {
				listSQL += fmt.Sprintf(" AND (created_at, id) < ($%d, $%d::uuid)", idx, idx+1)
				args = append(args, cur.ts, cur.id.String())
				idx += 2
			}
			listSQL += fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d", idx)
			args = append(args, limit+1) // +1 over-fetch to detect has_more

			rows, err := tx.Query(r.Context(), listSQL, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var rec recordResp
				var payload []byte
				var createdAt, updatedAt time.Time
				if err := rows.Scan(&rec.ID, &rec.TenantID, &rec.Vertical, &rec.Kind, &rec.Title, &rec.Slug, &payload, &rec.Status, &createdAt, &updatedAt); err != nil {
					return err
				}
				if len(payload) == 0 {
					payload = []byte("{}")
				}
				rec.Payload = json.RawMessage(payload)
				rec.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
				rec.UpdatedAt = updatedAt.UTC().Format(time.RFC3339Nano)
				items = append(items, rec)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			slog.Error("list records", "err", err)
			writeJSONError(w, http.StatusInternalServerError, "internal")
			return
		}
		hasMore := len(items) > limit
		if hasMore {
			items = items[:limit]
		}
		if hasMore && len(items) > 0 {
			last := items[len(items)-1]
			lastTs, _ = time.Parse(time.RFC3339Nano, last.CreatedAt)
			lastID = last.ID
		}
		out := listResp{Items: items, HasMore: hasMore, Limit: limit}
		if hasMore {
			uid, _ := uuidFromString(lastID)
			out.NextCursor = encodeCursor(lastTs, uid)
		}
		// Back-compat: include page/limit if the caller asked.
		if r.URL.Query().Get("page") != "" {
			out.Page, _ = strconv.Atoi(r.URL.Query().Get("page"))
			if out.Page < 1 {
				out.Page = 1
			}
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func (h *Handler) getOne(vertical string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cm := requireClient(w, r)
		if cm == nil {
			return
		}
		id := chi.URLParam(r, "id")
		includeDeleted := r.URL.Query().Get("status") == "deleted"
		var rec recordResp
		var payload []byte
		var createdAt, updatedAt time.Time
		err := withTenantTx(r.Context(), h.Pool, cm.TenantID, func(tx pgx.Tx) error {
			sql := `SELECT id::text, tenant_id, vertical, kind, title, slug, payload, status, created_at, updated_at
			        FROM tenant_records WHERE id = $1::uuid AND vertical = $2`
			if !includeDeleted {
				sql += " AND status != 'deleted'"
			}
			row := tx.QueryRow(r.Context(), sql, id, vertical)
			return row.Scan(&rec.ID, &rec.TenantID, &rec.Vertical, &rec.Kind, &rec.Title, &rec.Slug, &payload, &rec.Status, &createdAt, &updatedAt)
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSONError(w, http.StatusNotFound, "not found")
				return
			}
			slog.Error("get record", "err", err)
			writeJSONError(w, http.StatusInternalServerError, "internal")
			return
		}
		if len(payload) == 0 {
			payload = []byte("{}")
		}
		rec.Payload = json.RawMessage(payload)
		rec.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
		rec.UpdatedAt = updatedAt.UTC().Format(time.RFC3339Nano)
		writeJSON(w, http.StatusOK, rec)
	}
}

func (h *Handler) patch(vertical string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cm := requireClient(w, r)
		if cm == nil {
			return
		}
		id := chi.URLParam(r, "id")
		var req recordPatchReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid json")
			return
		}
		if req.Title == nil && req.Slug == nil && req.Payload == nil && req.Status == nil {
			writeJSONError(w, http.StatusBadRequest, "no fields to update")
			return
		}
		if req.Status != nil && !validStatuses[*req.Status] {
			writeJSONError(w, http.StatusBadRequest, "invalid status")
			return
		}
		set := []string{"updated_at = now()"}
		args := []any{}
		idx := 1
		if req.Title != nil {
			set = append(set, fmt.Sprintf("title = $%d", idx))
			args = append(args, *req.Title)
			idx++
		}
		if req.Slug != nil {
			set = append(set, fmt.Sprintf("slug = $%d", idx))
			args = append(args, *req.Slug)
			idx++
		}
		if req.Payload != nil {
			set = append(set, fmt.Sprintf("payload = $%d", idx))
			args = append(args, []byte(req.Payload))
			idx++
		}
		if req.Status != nil {
			set = append(set, fmt.Sprintf("status = $%d", idx))
			args = append(args, *req.Status)
			idx++
		}
		args = append(args, id, vertical)
		sql := fmt.Sprintf("UPDATE tenant_records SET %s WHERE id = $%d::uuid AND vertical = $%d",
			strings.Join(set, ", "), idx, idx+1)

		var affected int64
		err := withTenantTx(r.Context(), h.Pool, cm.TenantID, func(tx pgx.Tx) error {
			tag, err := tx.Exec(r.Context(), sql, args...)
			if err != nil {
				return err
			}
			affected = tag.RowsAffected()
			return nil
		})
		if err != nil {
			slog.Error("patch record", "err", err)
			writeJSONError(w, http.StatusInternalServerError, "internal")
			return
		}
		if affected == 0 {
			writeJSONError(w, http.StatusNotFound, "not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "updated"})
	}
}

func (h *Handler) del(vertical string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cm := requireClient(w, r)
		if cm == nil {
			return
		}
		id := chi.URLParam(r, "id")
		var affected int64
		err := withTenantTx(r.Context(), h.Pool, cm.TenantID, func(tx pgx.Tx) error {
			tag, err := tx.Exec(r.Context(),
				`UPDATE tenant_records SET status = 'deleted', updated_at = now() WHERE id = $1::uuid AND vertical = $2`,
				id, vertical,
			)
			if err != nil {
				return err
			}
			affected = tag.RowsAffected()
			return nil
		})
		if err != nil {
			slog.Error("delete record", "err", err)
			writeJSONError(w, http.StatusInternalServerError, "internal")
			return
		}
		if affected == 0 {
			writeJSONError(w, http.StatusNotFound, "not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "deleted"})
	}
}

// requireClient reads the cached client from context. It writes a
// 401 and returns nil if the auth middleware was bypassed (which
// must not happen, but the check is cheap).
func requireClient(w http.ResponseWriter, r *http.Request) *cache.ClientModel {
	cm := clientFromCtx(r.Context())
	if cm == nil {
		writeJSONError(w, http.StatusUnauthorized, "client not resolved")
	}
	return cm
}

func uuidFromString(s string) (uuid.UUID, error) {
	return uuid.Parse(s)
}
