package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/davinakmalyasha/agents-proxy/internal/authapi"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
	"github.com/davinakmalyasha/agents-proxy/internal/pushsafe"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Push subscription registry endpoint (migration 057 → PWA Phase 2).
//
// The PWA calls this after PushManager.subscribe() to register the
// browser endpoint. Staff-session authed (X-Session-Token): subscribers
// are interactive users, never API clients. user_ref is the staff
// email — the same value set_reminder takes as recipient, so the two
// sides match by construction.
//
// Writes run inside withTenantTx (RLS GUC scoping): the app pool role
// holds table grants but the FORCE policy needs the tenant context.
type Push struct {
	Pool *pgxpool.Pool
	Auth *authapi.Auth
}

type pushSubscribeReq struct {
	Endpoint string `json:"endpoint"`
	P256dh   string `json:"p256dh"`
	Auth     string `json:"auth"`
}

func (p *Push) userFromSession(w http.ResponseWriter, r *http.Request) *authapi.User {
	sess := strings.TrimSpace(r.Header.Get("X-Session-Token"))
	if sess == "" {
		writeJSONError(w, http.StatusUnauthorized, "session required")
		return nil
	}
	u, err := p.Auth.UserByToken(r.Context(), sess)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "invalid or expired session")
		return nil
	}
	return u
}

func validPushKeys(endpoint, p256dh, auth string) bool {
	if len(endpoint) < 12 || len(endpoint) > 2048 {
		return false
	}
	// The endpoint is a server-side request target, not just a string:
	// restrict it to the browser push services. See internal/pushsafe.
	if err := pushsafe.ValidateEndpoint(endpoint); err != nil {
		return false
	}
	// Key material is validated at send time (encryption fails
	// transiently on garbage); the write path checks shape only so a
	// browser upgrade changing key formats never bricks subscribing.
	return len(p256dh) >= 16 && len(p256dh) <= 512 && len(auth) >= 8 && len(auth) <= 256
}

// Subscribe upserts one browser endpoint (re-subscription rotates keys).
func (p *Push) Subscribe(w http.ResponseWriter, r *http.Request) {
	u := p.userFromSession(w, r)
	if u == nil {
		return
	}
	var req pushSubscribeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	endpoint := strings.TrimSpace(req.Endpoint)
	if !validPushKeys(endpoint, strings.TrimSpace(req.P256dh), strings.TrimSpace(req.Auth)) {
		writeJSONError(w, http.StatusBadRequest, "invalid subscription")
		return
	}
	err := records.WithTenantTx(r.Context(), p.Pool, u.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(r.Context(),
			`INSERT INTO push_subscriptions (tenant_id, user_ref, endpoint, p256dh, auth, last_used_at)
			 VALUES ($1, $2, $3, $4, $5, now())
			 ON CONFLICT (tenant_id, endpoint)
			 DO UPDATE SET user_ref = EXCLUDED.user_ref, p256dh = EXCLUDED.p256dh,
			               auth = EXCLUDED.auth, last_used_at = now()`,
			u.TenantID, u.Email, endpoint, strings.TrimSpace(req.P256dh), strings.TrimSpace(req.Auth))
		return err
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"subscribed": endpoint})
}

// Unsubscribe removes one endpoint (?endpoint=). Missing rows are 404,
// not silent success — the caller must know the device is still armed.
func (p *Push) Unsubscribe(w http.ResponseWriter, r *http.Request) {
	u := p.userFromSession(w, r)
	if u == nil {
		return
	}
	endpoint := strings.TrimSpace(r.URL.Query().Get("endpoint"))
	if endpoint == "" {
		writeJSONError(w, http.StatusBadRequest, "endpoint required")
		return
	}
	var deleted bool
	err := records.WithTenantTx(r.Context(), p.Pool, u.TenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(),
			`DELETE FROM push_subscriptions WHERE tenant_id = $1 AND endpoint = $2`,
			u.TenantID, endpoint)
		if err != nil {
			return err
		}
		deleted = tag.RowsAffected() > 0
		return nil
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	if !deleted {
		writeJSONError(w, http.StatusNotFound, "no such subscription")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": endpoint})
}
