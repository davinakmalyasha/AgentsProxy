package records

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/usage"
)

// FeatureAdapter is the bridge between a shared feature (which only
// declares validation + a kind) and the underlying records handlers.
// It forces `kind` server-side so clients cannot fake it.
//
// The adapter depends on the feature's Name/Kind/ValidatePayload
// fields rather than the full features.Feature struct, so this
// package has no upstream dependency on the features registry.
//
// DONE: PRD 6.5 (feature adapter) / STANDARDS A, F

type FeatureSpec struct {
	Name            string
	Kind            string
	ValidatePayload func(json.RawMessage) error
}

type FeatureAdapter struct {
	Pool     *pgxpool.Pool
	Spec     FeatureSpec
	Vertical string // closed over at builder time; never read from URL
}

func NewFeatureAdapter(deps Deps, spec FeatureSpec, vertical string) *FeatureAdapter {
	return &FeatureAdapter{Pool: deps.Pool, Spec: spec, Vertical: vertical}
}

func (a *FeatureAdapter) Routes() http.Handler {
	r := chi.NewRouter()
	r.Post("/", a.create)
	r.Get("/", a.list)
	r.Get("/{id}", WithValidID(a.getOne))
	r.Patch("/{id}", WithValidID(a.patch))
	r.Delete("/{id}", WithValidID(a.del))
	return r
}

// writeJSON / writeJSONError are in json.go (same package).

// ---- payload types (subset of records.recordWriteReq etc.) ----

type featureWriteReq struct {
	Title   string          `json:"title"`
	Slug    string          `json:"slug"`
	Payload json.RawMessage `json:"payload"`
}

type featurePatchReq struct {
	Title   *string         `json:"title"`
	Slug    *string         `json:"slug"`
	Payload json.RawMessage `json:"payload"`
}

func (a *FeatureAdapter) create(w http.ResponseWriter, r *http.Request) {
	cm := requireClient(w, r)
	if cm == nil {
		return
	}
	vertical := a.Vertical
	var req featureWriteReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if len(req.Payload) == 0 {
		req.Payload = json.RawMessage(`{}`)
	}
	if a.Spec.ValidatePayload != nil {
		if err := a.Spec.ValidatePayload(req.Payload); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	var id string
	err := withTenantTx(r.Context(), a.Pool, cm.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(),
			`INSERT INTO tenant_records (tenant_id, vertical, kind, title, slug, payload, status)
			 VALUES ($1, $2, $3, $4, $5, $6, 'active') RETURNING id::text`,
			cm.TenantID, vertical, a.Spec.Kind, req.Title, req.Slug, []byte(req.Payload),
		).Scan(&id)
	})
	if err != nil {
		slog.Error("feature create", "feature", a.Spec.Name, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	mctx, mcancel := detachedCtx(r.Context())
	defer mcancel()
	// usage.kind is CHECKed to ('chat','business') and no reader discriminates
	// on it (month_tokens sums kind-agnostic), so record shared-feature
	// writes as 'business' like Handler.create. Passing Spec.Name here used
	// to fail the CHECK on every call and silently drop the metering row.
	// If per-feature metering is ever wanted, add a dedicated column —
	// do not overload kind.
	if err := usage.Record(mctx, a.Pool, nil, cm.ID, "mimo", "business", len(req.Payload)/4, 0); err != nil {
		slog.Warn("usage record failed", "client_id", cm.ID, "err", err)
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (a *FeatureAdapter) list(w http.ResponseWriter, r *http.Request) {
	cm := requireClient(w, r)
	if cm == nil {
		return
	}
	vertical := a.Vertical
	// One parse (see Handler.list for why).
	qv := r.URL.Query()
	kind := a.Spec.Kind
	if kind == "" {
		kind = qv.Get("kind")
	}
	status := "active"
	if s := qv.Get("status"); s != "" {
		if s != "active" && s != "deleted" {
			writeJSONError(w, http.StatusBadRequest, "invalid status")
			return
		}
		status = s
	}
	limit := parseLimit(qv.Get("limit"))
	cur, err := decodeCursor(qv.Get("before"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid cursor")
		return
	}
	var items = make([]recordResp, 0, limit+1)
	var lastTs time.Time
	var lastID string
	err = withTenantTx(r.Context(), a.Pool, cm.TenantID, func(tx pgx.Tx) error {
		sql := `SELECT id::text, tenant_id, vertical, kind, title, slug, payload, status, created_at, updated_at
		        FROM tenant_records WHERE vertical = $1 AND kind = $2 AND status = $3`
		args := []any{vertical, kind, status}
		idx := 4
		if !cur.ts.IsZero() {
			sql += " AND (created_at, id) < ($4, $5::uuid)"
			args = append(args, cur.ts, cur.id.String())
			idx = 6
		}
		sql += " ORDER BY created_at DESC, id DESC LIMIT $" + strconv.Itoa(idx)
		args = append(args, limit+1)
		rows, err := tx.Query(r.Context(), sql, args...)
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
				payload = emptyPayloadJSON
			}
			rec.Payload = json.RawMessage(payload)
			rec.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
			rec.UpdatedAt = updatedAt.UTC().Format(time.RFC3339Nano)
			items = append(items, rec)
			if len(items) <= limit {
				lastTs, lastID = createdAt, rec.ID
			}
		}
		return rows.Err()
	})
	if err != nil {
		slog.Error("feature list", "feature", a.Spec.Name, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	out := listResp{Items: items, HasMore: hasMore, Limit: limit}
	if hasMore {
		uid, _ := uuidFromString(lastID)
		out.NextCursor = encodeCursor(lastTs, uid)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *FeatureAdapter) getOne(w http.ResponseWriter, r *http.Request) {
	cm := requireClient(w, r)
	if cm == nil {
		return
	}
	vertical := a.Vertical
	kind := a.Spec.Kind
	if kind == "" {
		kind = r.URL.Query().Get("kind")
	}
	id := chi.URLParam(r, "id")
	includeDeleted := r.URL.Query().Get("status") == "deleted"
	var rec recordResp
	var payload []byte
	var createdAt, updatedAt time.Time
	err := withTenantTx(r.Context(), a.Pool, cm.TenantID, func(tx pgx.Tx) error {
		sql := `SELECT id::text, tenant_id, vertical, kind, title, slug, payload, status, created_at, updated_at
		        FROM tenant_records WHERE id = $1::uuid AND vertical = $2 AND kind = $3`
		if !includeDeleted {
			sql += " AND status != 'deleted'"
		}
		row := tx.QueryRow(r.Context(), sql, id, vertical, kind)
		return row.Scan(&rec.ID, &rec.TenantID, &rec.Vertical, &rec.Kind, &rec.Title, &rec.Slug, &payload, &rec.Status, &createdAt, &updatedAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSONError(w, http.StatusNotFound, "not found")
			return
		}
		slog.Error("feature get", "feature", a.Spec.Name, "err", err)
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

func (a *FeatureAdapter) patch(w http.ResponseWriter, r *http.Request) {
	cm := requireClient(w, r)
	if cm == nil {
		return
	}
	vertical := a.Vertical
	kind := a.Spec.Kind
	if kind == "" {
		kind = r.URL.Query().Get("kind")
	}
	id := chi.URLParam(r, "id")
	var req featurePatchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.Title == nil && req.Slug == nil && req.Payload == nil {
		writeJSONError(w, http.StatusBadRequest, "no fields to update")
		return
	}
	if a.Spec.ValidatePayload != nil && req.Payload != nil {
		if err := a.Spec.ValidatePayload(req.Payload); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	sets := []string{"updated_at = now()"}
	args := []any{}
	idx := 1
	if req.Title != nil {
		sets = append(sets, "title = $"+strconv.Itoa(idx))
		args = append(args, *req.Title)
		idx++
	}
	if req.Slug != nil {
		sets = append(sets, "slug = $"+strconv.Itoa(idx))
		args = append(args, *req.Slug)
		idx++
	}
	if req.Payload != nil {
		sets = append(sets, "payload = $"+strconv.Itoa(idx))
		args = append(args, []byte(req.Payload))
		idx++
	}
	args = append(args, id, vertical, kind)
	sql := "UPDATE tenant_records SET " + strings.Join(sets, ", ") +
		" WHERE id = $" + strconv.Itoa(idx) + "::uuid AND vertical = $" + strconv.Itoa(idx+1) +
		" AND kind = $" + strconv.Itoa(idx+2)
	var affected int64
	err := withTenantTx(r.Context(), a.Pool, cm.TenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), sql, args...)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	if err != nil {
		slog.Error("feature patch", "feature", a.Spec.Name, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	if affected == 0 {
		writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "updated"})
}

func (a *FeatureAdapter) del(w http.ResponseWriter, r *http.Request) {
	cm := requireClient(w, r)
	if cm == nil {
		return
	}
	vertical := a.Vertical
	kind := a.Spec.Kind
	if kind == "" {
		kind = r.URL.Query().Get("kind")
	}
	id := chi.URLParam(r, "id")
	var affected int64
	err := withTenantTx(r.Context(), a.Pool, cm.TenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(),
			`UPDATE tenant_records SET status = 'deleted', updated_at = now()
			   WHERE id = $1::uuid AND vertical = $2 AND kind = $3`,
			id, vertical, kind,
		)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	if err != nil {
		slog.Error("feature delete", "feature", a.Spec.Name, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	if affected == 0 {
		writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "deleted"})
}
