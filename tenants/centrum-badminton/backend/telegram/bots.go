// Owner-only Telegram bot registry API (T5).
//
// Pasted BotFather tokens are validated via getMe, encrypted with the
// vault key, and never returned (list shows masked tokens). Creating or
// re-enabling a bot registers the webhook when PublicURL is set.
//
// DONE: T5 (bot admin)
package telegram_feature

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
	"github.com/davinakmalyasha/agents-proxy/internal/telegram"
	"github.com/davinakmalyasha/agents-proxy/internal/tgvault"
)

// VaultKey encrypts bot tokens. Set once in main.go wiring.
var VaultKey []byte

// PublicURL is the proxy's public base (https://...) for setWebhook.
// Empty = webhook skipped (dev polling).
var PublicURL string

type handler struct {
	pool *pgxpool.Pool
}

func botRoutes(r chi.Router, pool *pgxpool.Pool) {
	h := &handler{pool: pool}
	r.Get("/", h.list)
	r.Post("/", h.create)
	r.Patch("/{id}", records.WithValidID(h.patch))
	r.Delete("/{id}", records.WithValidID(h.del))
}

type botResp struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Role         string  `json:"role"`
	TokenMasked  string  `json:"token_masked"`
	AllowChatIDs []int64 `json:"allow_chat_ids"`
	Status       string  `json:"status"`
	CreatedAt    string  `json:"created_at"`
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireStaffOwner(w, r)
	if idn == nil {
		return
	}
	var out []botResp
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(),
			`SELECT id::text, name, role, token_enc, allow_chat_ids, status, created_at
			   FROM telegram_bots WHERE tenant_id=$1 ORDER BY created_at`, idn.TenantID())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var enc []byte
			var b botResp
			var createdAt time.Time
			var allow []int64
			if err := rows.Scan(&b.ID, &b.Name, &b.Role, &enc, &allow, &b.Status, &createdAt); err != nil {
				return err
			}
			_ = enc
			b.TokenMasked = "stored-encrypted"
			b.AllowChatIDs = allow
			if b.AllowChatIDs == nil {
				b.AllowChatIDs = []int64{}
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
	if out == nil {
		out = []botResp{}
	}
	records.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireStaffOwner(w, r)
	if idn == nil {
		return
	}
	if len(VaultKey) == 0 {
		records.WriteJSONError(w, http.StatusServiceUnavailable, "vault not configured")
		return
	}
	var req struct {
		Name         string  `json:"name"`
		Role         string  `json:"role"`
		Token        string  `json:"token"`
		AllowChatIDs []int64 `json:"allow_chat_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Role = strings.ToLower(strings.TrimSpace(req.Role))
	req.Token = strings.TrimSpace(req.Token)
	if req.Name == "" {
		records.WriteJSONError(w, http.StatusBadRequest, "name required")
		return
	}
	if req.Role != "owner" && req.Role != "pegawai" && req.Role != "client" {
		records.WriteJSONError(w, http.StatusBadRequest, "role must be owner, pegawai, or client")
		return
	}
	if req.Token == "" || !strings.Contains(req.Token, ":") {
		records.WriteJSONError(w, http.StatusBadRequest, "token invalid")
		return
	}
	if (req.Role == "owner" || req.Role == "pegawai") && len(req.AllowChatIDs) == 0 {
		records.WriteJSONError(w, http.StatusBadRequest, "owner/pegawai bots need allow_chat_ids")
		return
	}
	// Prove the pasted token works (and learn the username).
	bot := telegram.New(req.Token)
	info, err := bot.GetMe(r.Context())
	if err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "token rejected by Telegram: "+shortErr(err))
		return
	}
	enc, err := tgvault.Encrypt(VaultKey, []byte(req.Token))
	if err != nil {
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	secret := randHex(32)
	var id string
	err = records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(),
			`INSERT INTO telegram_bots (tenant_id, name, role, token_enc, webhook_secret, allow_chat_ids)
			 VALUES ($1,$2,$3,$4,$5,$6) RETURNING id::text`,
			idn.TenantID(), req.Name, req.Role, enc, secret, req.AllowChatIDs).Scan(&id)
	})
	if err != nil {
		if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique") {
			records.WriteJSONError(w, http.StatusConflict, "bot name already exists")
			return
		}
		records.WriteJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	// Register webhook now (manager Sync also ensures it every minute).
	if PublicURL != "" {
		if err := bot.SetWebhook(r.Context(), PublicURL+"/telegram/hook/"+id, secret); err != nil {
			slog.Warn("bot setWebhook", "bot", id, "err", err)
		}
	}
	records.WriteJSON(w, http.StatusCreated, map[string]any{
		"id": id, "username": info.Username, "role": req.Role,
	})
}

func (h *handler) patch(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireStaffOwner(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	var req struct {
		Name         *string `json:"name"`
		Role         *string `json:"role"`
		AllowChatIDs []int64 `json:"allow_chat_ids"`
		HasAllow     bool
		Status       *string `json:"status"`
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		records.WriteJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	// Detect explicit allow_chat_ids (empty array = clear the list).
	req.HasAllow = req.AllowChatIDs != nil
	sets := []string{"updated_at = now()"}
	args := []any{}
	idx := 1
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			records.WriteJSONError(w, http.StatusBadRequest, "name required")
			return
		}
		sets = append(sets, "name = $"+strconv.Itoa(idx))
		args = append(args, name)
		idx++
	}
	if req.Role != nil {
		role := strings.ToLower(strings.TrimSpace(*req.Role))
		if role != "owner" && role != "pegawai" && role != "client" {
			records.WriteJSONError(w, http.StatusBadRequest, "role must be owner, pegawai, or client")
			return
		}
		sets = append(sets, "role = $"+strconv.Itoa(idx))
		args = append(args, role)
		idx++
	}
	if req.HasAllow {
		sets = append(sets, "allow_chat_ids = $"+strconv.Itoa(idx))
		args = append(args, req.AllowChatIDs)
		idx++
	}
	if req.Status != nil {
		if *req.Status != "active" && *req.Status != "disabled" {
			records.WriteJSONError(w, http.StatusBadRequest, "status must be active or disabled")
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
	// Owner/pegawai bots must never end up with an empty allowlist.
	if req.HasAllow && len(req.AllowChatIDs) == 0 {
		var curRole string
		_ = records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
			return tx.QueryRow(r.Context(),
				`SELECT role FROM telegram_bots WHERE id=$1::uuid AND tenant_id=$2`,
				id, idn.TenantID()).Scan(&curRole)
		})
		wantRole := curRole
		if req.Role != nil {
			wantRole = strings.ToLower(strings.TrimSpace(*req.Role))
		}
		if wantRole == "owner" || wantRole == "pegawai" {
			records.WriteJSONError(w, http.StatusBadRequest, "owner/pegawai bots need allow_chat_ids")
			return
		}
	}
	args = append(args, id, idn.TenantID())
	var affected int64
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(),
			`UPDATE telegram_bots SET `+strings.Join(sets, ", ")+
				` WHERE id = $`+strconv.Itoa(idx)+`::uuid AND tenant_id = $`+strconv.Itoa(idx+1),
			args...)
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
	// Disabling takes effect on next manager Sync (≤1 min); also drop
	// the webhook now so Telegram stops pushing immediately.
	if req.Status != nil && *req.Status == "disabled" {
		h.dropWebhook(r, idn.TenantID(), id)
	}
	records.WriteJSON(w, http.StatusOK, map[string]string{"id": id, "status": "updated"})
}

func (h *handler) del(w http.ResponseWriter, r *http.Request) {
	idn := records.RequireStaffOwner(w, r)
	if idn == nil {
		return
	}
	id := chi.URLParam(r, "id")
	h.dropWebhook(r, idn.TenantID(), id)
	var affected int64
	err := records.WithTenantTx(r.Context(), h.pool, idn.TenantID(), func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(),
			`DELETE FROM telegram_bots WHERE id=$1::uuid AND tenant_id=$2`, id, idn.TenantID())
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

// dropWebhook best-effort removes the Telegram-side hook (row delete
// alone would leave Telegram pushing to a dead endpoint).
func (h *handler) dropWebhook(r *http.Request, tenantID, id string) {
	if len(VaultKey) == 0 {
		return
	}
	var enc []byte
	_ = records.WithTenantTx(r.Context(), h.pool, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(),
			`SELECT token_enc FROM telegram_bots WHERE id=$1::uuid AND tenant_id=$2`,
			id, tenantID).Scan(&enc)
	})
	if len(enc) == 0 {
		return
	}
	tok, err := tgvault.Decrypt(VaultKey, enc)
	if err != nil {
		return
	}
	_ = telegram.New(string(tok)).DeleteWebhook(r.Context())
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}
