// Package series implements the "series" feature: recurring weekly
// booking series (community groups). Port of CentrumBadminton's
// recurring_series.
//
// DONE: Phase 6 (Centrum migration) / STANDARDS A, D, F
package series

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/features"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
)

const featureName = "series"

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

type seriesResp struct {
	ID            string `json:"id"`
	TenantID      string `json:"tenant_id"`
	FacilityID    string `json:"facility_id"`
	CustomerName  string `json:"customer_name"`
	CustomerPhone string `json:"customer_phone"`
	Weekday       int    `json:"weekday"`
	StartTime     string `json:"start_time"`
	EndTime       string `json:"end_time"`
	StartDate     string `json:"start_date"`
	EndDate       string `json:"end_date,omitempty"`
	Status        string `json:"status"`
	CreatedAt     string `json:"created_at"`
}

var validSeriesStatuses = map[string]bool{"active": true, "paused": true, "ended": true}

// terminalSeriesStatuses are the statuses that mean "this subscription is
// no longer holding courts". Moving into one of them releases the future
// bookings it generated.
var terminalSeriesStatuses = map[string]bool{"paused": true, "ended": true}

// todayJakarta is the current date in WIB. Cancelling "the future" needs a
// single definition of where the future begins, and it must be the venue's
// date, not the server's: a server on UTC is a day ahead of WIB from 07:00
// to 14:00 WIB, so using now() there would cancel today's evening slot at
// lunchtime.
func todayJakarta() string {
	return agentstools.WibNow().Format("2006-01-02")
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireOwner(w, r)
	if idn == nil {
		return
	}
	var req struct {
		FacilityID    string `json:"facility_id"`
		CustomerName  string `json:"customer_name"`
		CustomerPhone string `json:"customer_phone"`
		Weekday       *int   `json:"weekday"`
		StartTime     string `json:"start_time"`
		EndTime       string `json:"end_time"`
		StartDate     string `json:"start_date"`
		EndDate       string `json:"end_date"`
		Status        string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if strings.TrimSpace(req.FacilityID) == "" || strings.TrimSpace(req.CustomerName) == "" {
		records.WriteJSONError(w, http.StatusBadRequest, "facility_id and customer_name required")
		return
	}
	if req.Weekday == nil || *req.Weekday < 0 || *req.Weekday > 6 {
		records.WriteJSONError(w, http.StatusBadRequest, "weekday must be 0-6 (0=Sunday)")
		return
	}
	if req.StartTime == "" || req.EndTime == "" || req.StartTime >= req.EndTime {
		records.WriteJSONError(w, http.StatusBadRequest, "start_time must be before end_time")
		return
	}
	if req.StartDate == "" {
		records.WriteJSONError(w, http.StatusBadRequest, "start_date required (YYYY-MM-DD)")
		return
	}
	if _, err := time.Parse("2006-01-02", req.StartDate); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "start_date must be YYYY-MM-DD")
		return
	}
	status := req.Status
	if status == "" {
		status = "active"
	}
	if !validSeriesStatuses[status] {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid status")
		return
	}
	var id string
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(),
			`INSERT INTO sports_recurring_series (tenant_id, facility_id, customer_name, customer_phone, weekday, start_time, end_time, start_date, end_date, status)
			 VALUES ($1,$2::uuid,$3,$4,$5,$6::time,$7::time,$8::date,NULLIF($9,'')::date,$10) RETURNING id::text`,
			idn.TenantID(), req.FacilityID, req.CustomerName, req.CustomerPhone, *req.Weekday,
			req.StartTime, req.EndTime, req.StartDate, req.EndDate, status).Scan(&id)
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
	limit, lerr := records.ParseLimit(r)
	if lerr != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid limit")
		return
	}
	out := make([]seriesResp, 0, limit+1)
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(),
			`SELECT id::text, tenant_id, facility_id::text, customer_name, customer_phone, weekday, start_time::text, end_time::text, start_date::text, COALESCE(end_date::text,''), status, created_at
			   FROM sports_recurring_series WHERE tenant_id=$1 AND deleted_at IS NULL ORDER BY weekday, start_time LIMIT $2`,
			idn.TenantID(), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s seriesResp
			var createdAt time.Time
			if err := rows.Scan(&s.ID, &s.TenantID, &s.FacilityID, &s.CustomerName, &s.CustomerPhone, &s.Weekday,
				&s.StartTime, &s.EndTime, &s.StartDate, &s.EndDate, &s.Status, &createdAt); err != nil {
				return err
			}
			s.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
			out = append(out, s)
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
	var s seriesResp
	var createdAt time.Time
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(),
			`SELECT id::text, tenant_id, facility_id::text, customer_name, customer_phone, weekday, start_time::text, end_time::text, start_date::text, COALESCE(end_date::text,''), status, created_at
			   FROM sports_recurring_series WHERE id=$1::uuid AND tenant_id=$2 AND deleted_at IS NULL`,
			id, idn.TenantID(),
		).Scan(&s.ID, &s.TenantID, &s.FacilityID, &s.CustomerName, &s.CustomerPhone, &s.Weekday,
			&s.StartTime, &s.EndTime, &s.StartDate, &s.EndDate, &s.Status, &createdAt)
	})
	if err != nil {
		records.WriteJSONError(w, http.StatusNotFound, "not found")
		return
	}
	s.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
	records.WriteJSON(w, http.StatusOK, s)
}

func (h *handler) patch(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireOwner(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var req struct {
		CustomerName  *string `json:"customer_name"`
		CustomerPhone *string `json:"customer_phone"`
		StartTime     *string `json:"start_time"`
		EndTime       *string `json:"end_time"`
		EndDate       *string `json:"end_date"`
		Status        *string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	sets := []string{}
	args := []any{}
	idx := 1
	if req.CustomerName != nil {
		sets = append(sets, "customer_name = $"+strconv.Itoa(idx))
		args = append(args, *req.CustomerName)
		idx++
	}
	if req.CustomerPhone != nil {
		sets = append(sets, "customer_phone = $"+strconv.Itoa(idx))
		args = append(args, *req.CustomerPhone)
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
	if req.EndDate != nil {
		sets = append(sets, "end_date = $"+strconv.Itoa(idx)+"::date")
		args = append(args, *req.EndDate)
		idx++
	}
	if req.Status != nil {
		if !validSeriesStatuses[*req.Status] {
			records.WriteJSONError(w, http.StatusBadRequest, "invalid status")
			return
		}
		sets = append(sets, "status = $"+strconv.Itoa(idx))
		args = append(args, *req.Status)
		idx++
	}
	if len(sets) == 0 {
		records.WriteJSONError(w, http.StatusBadRequest, "no fields to update")
		return
	}
	args = append(args, id, idn.TenantID())
	sql := fmt.Sprintf("UPDATE sports_recurring_series SET %s WHERE id=$%d::uuid AND tenant_id=$%d AND deleted_at IS NULL",
		strings.Join(sets, ", "), idx, idx+1)
	var affected int64
	var cancelledBookings int64
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), sql, args...)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		if affected == 0 {
			return nil
		}
		// Pausing or ending a subscription must actually release the courts.
		//
		// The status change used to be the whole action: the series row said
		// "paused" while every future booking it had generated stayed
		// confirmed. The customer cancelled their subscription — or the owner
		// ended it because the customer moved away — and the courts were
		// still held, unbookable by anyone else and still showing up on the
		// schedule as if nothing had happened. The subscription row was the
		// only thing that changed.
		//
		// Same transaction as the status update, so there is no window where
		// the series reads "ended" but the courts are still held.
		//
		// Only pending and confirmed. A booking that has been checked in,
		// completed, or no-show'd is history: someone turned up, or didn't,
		// and that is settled. Cancelling those would rewrite the past and,
		// for completed ones, break the revenue the courts earned.
		if req.Status != nil && terminalSeriesStatuses[*req.Status] {
			tag, err := tx.Exec(r.Context(),
				`UPDATE sports_bookings
				    SET status='cancelled', updated_at=now()
				  WHERE tenant_id=$1
				    AND recurring_series_id=$2::uuid
				    AND date >= $3::date
				    AND status IN ('pending','confirmed')`,
				idn.TenantID(), id, todayJakarta())
			if err != nil {
				return err
			}
			cancelledBookings = tag.RowsAffected()
		}
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
	// Report what was released, so the owner sees the consequence of the
	// click rather than a bare "updated" and having to check the calendar.
	records.WriteJSON(w, http.StatusOK, map[string]any{
		"id":                 id,
		"status":             "updated",
		"cancelled_bookings": cancelledBookings,
	})
}

func (h *handler) del(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireOwner(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var affected int64
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		// Tombstone, not DELETE: the series row is preserved with who/when,
		// hidden from lists/reads. (del carries no booking side effect;
		// the future-booking release lives on patch → paused/ended and is
		// unchanged.)
		deletedBy := idn.UserID
		tag, err := tx.Exec(r.Context(),
			`UPDATE sports_recurring_series SET deleted_at = now(), deleted_by = $3
			  WHERE id=$1::uuid AND tenant_id=$2 AND deleted_at IS NULL`,
			id, idn.TenantID(), deletedBy)
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
