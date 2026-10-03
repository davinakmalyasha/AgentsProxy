// Package facilities implements the sports-vertical "facilities"
// feature: bookable courts/fields/pools. Uses a custom table
// (sports_facilities) because a facility is configuration with real
// column types (TIME, NUMERIC) referenced by bookings.
//
// DONE: Phase 4 (sports vertical) / STANDARDS A, D, F
package facilities

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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/features"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
)

const featureName = "facilities"

// errFacilityInUse is returned when a court still has bookings against it.
// Mapped to 409 with the count so the owner knows what is blocking the
// delete instead of seeing a 500.
var errFacilityInUse = errors.New("facility still has bookings")

func init() { Register() }

// Register registers the facilities feature. Safe to call from tests
// after features.ResetForTest().
func Register() {
	features.Register(features.Feature{
		Name:          featureName,
		Kind:          "", // custom table, not tenant_records
		VerticalSlugs: []string{"sports"},
		Routes:        routes,
	})
}

// routes mounts the facilities CRUD under /{vertical}/facilities.
// The builder wraps this with the feature gate + client auth.
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

type facilityReq struct {
	Name         string  `json:"name"`
	Type         string  `json:"type"`
	Capacity     *int    `json:"capacity"`
	PricePerHour float64 `json:"price_per_hour"`
	OpenTime     string  `json:"open_time"`
	CloseTime    string  `json:"close_time"`
	SortOrder    int     `json:"sort_order"`
	Status       string  `json:"status"`
}

type facilityResp struct {
	ID           string  `json:"id"`
	TenantID     string  `json:"tenant_id"`
	Name         string  `json:"name"`
	Type         string  `json:"type"`
	Capacity     int     `json:"capacity"`
	PricePerHour float64 `json:"price_per_hour"`
	OpenTime     string  `json:"open_time"`
	CloseTime    string  `json:"close_time"`
	Status       string  `json:"status"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
}

var validFacilityTypes = map[string]bool{
	"badminton": true, "soccer": true, "swimming": true, "padel": true, "other": true,
}

// Venue config is staff-editable BY DESIGN (owner + pegawai both manage
// courts). RequireIdentity (not RequireOwner) is intentional here, on
// patch, and on del.
func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	var req facilityReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		records.WriteJSONError(w, http.StatusBadRequest, "name required")
		return
	}
	typ := req.Type
	if typ == "" {
		typ = "other"
	}
	if !validFacilityTypes[typ] {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid type")
		return
	}
	status := req.Status
	if status == "" {
		status = "active"
	}
	if status != "active" && status != "paused" {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid status")
		return
	}
	capacity := 1
	if req.Capacity != nil {
		if *req.Capacity < 1 {
			records.WriteJSONError(w, http.StatusBadRequest, "capacity must be >= 1")
			return
		}
		capacity = *req.Capacity
	}
	if req.PricePerHour < 0 {
		records.WriteJSONError(w, http.StatusBadRequest, "price_per_hour must be >= 0")
		return
	}
	if req.OpenTime == "" {
		req.OpenTime = "06:00"
	}
	if req.CloseTime == "" {
		req.CloseTime = "22:00"
	}
	if req.OpenTime >= req.CloseTime {
		records.WriteJSONError(w, http.StatusBadRequest, "open_time must be before close_time")
		return
	}
	var id string
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(),
			`INSERT INTO sports_facilities (tenant_id, name, type, capacity, price_per_hour, open_time, close_time, status, sort_order)
			 VALUES ($1,$2,$3,$4,$5,$6::time,$7::time,$8,$9) RETURNING id::text`,
			idn.TenantID(), req.Name, typ, capacity, req.PricePerHour, req.OpenTime, req.CloseTime, status, req.SortOrder,
		).Scan(&id)
	})
	if err != nil {
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
	var out []facilityResp
	typ := r.URL.Query().Get("type")
	limit, lerr := records.ParseLimit(r)
	if lerr != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid limit")
		return
	}
	var q string
	var args []any
	if typ != "" {
		q = fmt.Sprintf(`SELECT id::text, tenant_id, name, type, capacity, price_per_hour, open_time::text, close_time::text, status, created_at, updated_at
			   FROM sports_facilities WHERE tenant_id = $1 AND deleted_at IS NULL AND type=$2 ORDER BY sort_order, name LIMIT %d`, limit)
		args = []any{idn.TenantID(), typ}
	} else {
		q = fmt.Sprintf(`SELECT id::text, tenant_id, name, type, capacity, price_per_hour, open_time::text, close_time::text, status, created_at, updated_at
			   FROM sports_facilities WHERE tenant_id = $1 AND deleted_at IS NULL ORDER BY sort_order, name LIMIT %d`, limit)
		args = []any{idn.TenantID()}
	}
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f facilityResp
			var createdAt, updatedAt time.Time
			if err := rows.Scan(&f.ID, &f.TenantID, &f.Name, &f.Type, &f.Capacity, &f.PricePerHour,
				&f.OpenTime, &f.CloseTime, &f.Status, &createdAt, &updatedAt); err != nil {
				return err
			}
			f.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
			f.UpdatedAt = updatedAt.UTC().Format(time.RFC3339Nano)
			out = append(out, f)
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
	var f facilityResp
	var createdAt, updatedAt time.Time
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(),
			`SELECT id::text, tenant_id, name, type, capacity, price_per_hour, open_time::text, close_time::text, status, created_at, updated_at
			   FROM sports_facilities WHERE id = $1::uuid AND tenant_id = $2 AND deleted_at IS NULL`,
			id, idn.TenantID(),
		).Scan(&f.ID, &f.TenantID, &f.Name, &f.Type, &f.Capacity, &f.PricePerHour,
			&f.OpenTime, &f.CloseTime, &f.Status, &createdAt, &updatedAt)
	})
	if err != nil {
		records.WriteJSONError(w, http.StatusNotFound, "not found")
		return
	}
	f.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
	f.UpdatedAt = updatedAt.UTC().Format(time.RFC3339Nano)
	records.WriteJSON(w, http.StatusOK, f)
}

// Staff-editable by design (see create).
func (h *handler) patch(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var req struct {
		Name         *string  `json:"name"`
		Type         *string  `json:"type"`
		Capacity     *int     `json:"capacity"`
		PricePerHour *float64 `json:"price_per_hour"`
		OpenTime     *string  `json:"open_time"`
		CloseTime    *string  `json:"close_time"`
		Status       *string  `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	sets := []string{"updated_at = now()"}
	args := []any{}
	idx := 1
	if req.Name != nil {
		sets = append(sets, "name = $"+strconv.Itoa(idx))
		args = append(args, *req.Name)
		idx++
	}
	if req.Type != nil {
		if !validFacilityTypes[*req.Type] {
			records.WriteJSONError(w, http.StatusBadRequest, "invalid type")
			return
		}
		sets = append(sets, "type = $"+strconv.Itoa(idx))
		args = append(args, *req.Type)
		idx++
	}
	if req.Capacity != nil {
		if *req.Capacity < 1 {
			records.WriteJSONError(w, http.StatusBadRequest, "capacity must be >= 1")
			return
		}
		sets = append(sets, "capacity = $"+strconv.Itoa(idx))
		args = append(args, *req.Capacity)
		idx++
	}
	if req.PricePerHour != nil {
		if *req.PricePerHour < 0 {
			records.WriteJSONError(w, http.StatusBadRequest, "price_per_hour must be >= 0")
			return
		}
		sets = append(sets, "price_per_hour = $"+strconv.Itoa(idx))
		args = append(args, *req.PricePerHour)
		idx++
	}
	if req.OpenTime != nil {
		sets = append(sets, "open_time = $"+strconv.Itoa(idx)+"::time")
		args = append(args, *req.OpenTime)
		idx++
	}
	if req.CloseTime != nil {
		sets = append(sets, "close_time = $"+strconv.Itoa(idx)+"::time")
		args = append(args, *req.CloseTime)
		idx++
	}
	if req.Status != nil {
		if *req.Status != "active" && *req.Status != "paused" {
			records.WriteJSONError(w, http.StatusBadRequest, "invalid status")
			return
		}
		sets = append(sets, "status = $"+strconv.Itoa(idx))
		args = append(args, *req.Status)
		idx++
	}
	if len(sets) == 1 {
		records.WriteJSONError(w, http.StatusBadRequest, "no fields to update")
		return
	}
	args = append(args, id, idn.TenantID())
	sql := fmt.Sprintf("UPDATE sports_facilities SET %s WHERE id = $%d::uuid AND tenant_id = $%d AND deleted_at IS NULL",
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
	// Staff-editable by design (see create).
	idn := records.RequireIdentity(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var affected int64
	var bookings int
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		// Count the bookings that still point at this court first. The
		// FK is a plain NO ACTION, so a court with any history cannot be
		// deleted — including history that is only CANCELLED. That used
		// to surface as a bare 500 "internal", which is useless to an
		// owner trying to retire a court: nothing told them the court
		// still had bookings against it.
		//
		// Cancelled bookings count too, deliberately. If they did not,
		// a court could be deleted once every booking on it was
		// cancelled, silently taking the schedule and revenue history
		// with it.
		if err := tx.QueryRow(r.Context(),
			`SELECT COUNT(*) FROM sports_bookings WHERE facility_id = $1::uuid AND tenant_id = $2`,
			id, idn.TenantID()).Scan(&bookings); err != nil {
			return err
		}
		if bookings > 0 {
			return errFacilityInUse
		}
		// Tombstone, not DELETE: the court row is preserved with who/when,
		// hidden from lists/reads. The in-use guard above is unchanged:
		// bookings are soft-cancelled via status, never tombstoned, so any
		// history still blocks the delete.
		deletedBy := idn.UserID
		tag, err := tx.Exec(r.Context(),
			`UPDATE sports_facilities SET deleted_at = now(), deleted_by = $3
			  WHERE id = $1::uuid AND tenant_id = $2 AND deleted_at IS NULL`,
			id, idn.TenantID(), deletedBy)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	if errors.Is(err, errFacilityInUse) {
		records.WriteJSONError(w, http.StatusConflict,
			fmt.Sprintf("lapangan masih dipakai oleh %d booking — tidak bisa dihapus", bookings))
		return
	}
	if err != nil {
		slog.Error("facility delete", "err", err)
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	if affected == 0 {
		records.WriteJSONError(w, http.StatusNotFound, "not found")
		return
	}
	records.WriteJSON(w, http.StatusOK, map[string]string{"id": id, "status": "deleted"})
}
