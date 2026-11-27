// Package courtblocks implements the "courtblocks" feature: blocking
// a facility (or all) for maintenance / private events. Port of
// CentrumBadminton's court_blocks.
//
// DONE: Phase 6 (Centrum migration) / STANDARDS A, D, F
package courtblocks

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/features"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
)

const featureName = "courtblocks"

func init() { Register() }

func Register() {
	features.RegisterProject("centrum", "sports", features.Feature{
		Name:          featureName,
		Kind:          "",
		VerticalSlugs: []string{"sports"},
		Routes:        routes,
	})
}

func routes(r chi.Router, deps features.BuilderDeps) {
	h := &handler{pool: deps.Pool}
	r.Post("/", h.create)
	r.Get("/", h.list)
	r.Get("/{id}", records.WithValidID(h.getOne))
	r.Patch("/{id}", records.WithValidID(h.patch))
	r.Delete("/{id}", records.WithValidID(h.del))
}

type handler struct {
	pool *pgxpool.Pool
}

type blockResp struct {
	ID         string `json:"id"`
	TenantID   string `json:"tenant_id"`
	FacilityID string `json:"facility_id,omitempty"` // empty = all
	BlockDate  string `json:"block_date"`
	StartTime  string `json:"start_time"`
	EndTime    string `json:"end_time"`
	Reason     string `json:"reason"`
	CreatedAt  string `json:"created_at"`
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireOwner(w, r)
	if idn == nil {
		return
	}
	var req struct {
		FacilityID string `json:"facility_id"`
		BlockDate  string `json:"block_date"`
		StartTime  string `json:"start_time"`
		EndTime    string `json:"end_time"`
		Reason     string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.BlockDate == "" {
		records.WriteJSONError(w, http.StatusBadRequest, "block_date required (YYYY-MM-DD)")
		return
	}
	if _, err := time.Parse("2006-01-02", req.BlockDate); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "block_date must be YYYY-MM-DD")
		return
	}
	if req.StartTime == "" {
		req.StartTime = "07:00"
	}
	if req.EndTime == "" {
		req.EndTime = "23:00"
	}
	// Validate the facility if given, then insert, in one checkout:
	// two separate WithTenantTx calls cost two pool checkouts
	// (BEGIN/set_config.../COMMIT each); one Tx holds the connection
	// once for both statements.
	var errFacilityNotFound = errors.New("facility not found")
	var id string
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		if req.FacilityID != "" {
			var one int
			if err := tx.QueryRow(r.Context(),
				`SELECT 1 FROM sports_facilities WHERE id=$1::uuid AND tenant_id=$2 AND deleted_at IS NULL`,
				req.FacilityID, idn.TenantID()).Scan(&one); err != nil {
				return errFacilityNotFound
			}
		}
		return tx.QueryRow(r.Context(),
			`INSERT INTO sports_court_blocks (tenant_id, facility_id, block_date, start_time, end_time, reason)
			 VALUES ($1, NULLIF($2,'')::uuid, $3::date, $4::time, $5::time, $6) RETURNING id::text`,
			idn.TenantID(), req.FacilityID, req.BlockDate, req.StartTime, req.EndTime, req.Reason).Scan(&id)
	})
	if err != nil {
		if errors.Is(err, errFacilityNotFound) {
			records.WriteJSONError(w, http.StatusNotFound, "facility not found")
			return
		}
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	records.WriteJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	date := r.URL.Query().Get("date")
	limit, lerr := records.ParseLimit(r)
	if lerr != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid limit")
		return
	}
	out := make([]blockResp, 0, limit+1)
	sql := `SELECT id::text, tenant_id, COALESCE(facility_id::text,''), block_date::text, start_time::text, end_time::text, reason, created_at
	          FROM sports_court_blocks WHERE tenant_id=$1`
	args := []any{idn.TenantID()}
	if date != "" {
		sql += " AND block_date=$2::date"
		args = append(args, date)
	}
	sql += fmt.Sprintf(" ORDER BY block_date DESC LIMIT %d", limit)
	err := records.WithTenantRead(r.Context(), h.pool, idn.TenantID(), sql, args, func(rows pgx.Rows) error {
		for rows.Next() {
			var b blockResp
			var createdAt time.Time
			if err := rows.Scan(&b.ID, &b.TenantID, &b.FacilityID, &b.BlockDate, &b.StartTime, &b.EndTime, &b.Reason, &createdAt); err != nil {
				return err
			}
			b.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
			out = append(out, b)
		}
		return rows.Err()
	})
	if err != nil {
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	records.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *handler) getOne(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var b blockResp
	var createdAt time.Time
	err := records.WithTenantRead(r.Context(), h.pool, idn.TenantID(),
		`SELECT id::text, tenant_id, COALESCE(facility_id::text,''), block_date::text, start_time::text, end_time::text, reason, created_at
		   FROM sports_court_blocks WHERE id=$1::uuid AND tenant_id=$2`,
		[]any{id, idn.TenantID()},
		func(rows pgx.Rows) error {
			if !rows.Next() {
				if err := rows.Err(); err != nil {
					return err
				}
				return pgx.ErrNoRows
			}
			return rows.Scan(&b.ID, &b.TenantID, &b.FacilityID, &b.BlockDate, &b.StartTime, &b.EndTime, &b.Reason, &createdAt)
		})
	if err != nil {
		records.WriteJSONError(w, http.StatusNotFound, "not found")
		return
	}
	b.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
	records.WriteJSON(w, http.StatusOK, b)
}

func (h *handler) patch(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireOwner(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var req struct {
		BlockDate *string `json:"block_date"`
		StartTime *string `json:"start_time"`
		EndTime   *string `json:"end_time"`
		Reason    *string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	sets := []string{}
	args := []any{}
	idx := 1
	if req.BlockDate != nil {
		if _, err := time.Parse("2006-01-02", *req.BlockDate); err != nil {
			records.WriteJSONError(w, http.StatusBadRequest, "block_date must be YYYY-MM-DD")
			return
		}
		sets = append(sets, "block_date = $"+strconv.Itoa(idx)+"::date")
		args = append(args, *req.BlockDate)
		idx++
	}
	if req.StartTime != nil {
		sets = append(sets, "start_time = $"+strconv.Itoa(idx)+"::time")
		args = append(args, *req.StartTime)
		idx++
	}
	if req.EndTime != nil {
		sets = append(sets, "end_time = $"+strconv.Itoa(idx)+"::time")
		args = append(args, *req.EndTime)
		idx++
	}
	if req.Reason != nil {
		sets = append(sets, "reason = $"+strconv.Itoa(idx))
		args = append(args, *req.Reason)
		idx++
	}
	if len(sets) == 0 {
		records.WriteJSONError(w, http.StatusBadRequest, "no fields to update")
		return
	}
	args = append(args, id, idn.TenantID())
	sql := fmt.Sprintf("UPDATE sports_court_blocks SET %s WHERE id=$%d::uuid AND tenant_id=$%d",
		strings.Join(sets, ", "), idx, idx+1)
	var affected int64
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), sql, args...)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	if err != nil {
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	if affected == 0 {
		records.WriteJSONError(w, http.StatusNotFound, "not found")
		return
	}
	records.WriteJSON(w, http.StatusOK, map[string]string{"id": id, "status": "updated"})
}

func (h *handler) del(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireOwner(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var affected int64
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(),
			`DELETE FROM sports_court_blocks WHERE id=$1::uuid AND tenant_id=$2`,
			id, idn.TenantID())
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	if err != nil {
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	if affected == 0 {
		records.WriteJSONError(w, http.StatusNotFound, "not found")
		return
	}
	records.WriteJSON(w, http.StatusOK, map[string]string{"id": id, "status": "deleted"})
}
