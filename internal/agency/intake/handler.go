package intake

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	platformauth "github.com/davinakmalyasha/agents-proxy/internal/agency/auth"
	ratelimit "github.com/davinakmalyasha/agents-proxy/internal/agency/ratelimit"
	"github.com/davinakmalyasha/agents-proxy/internal/agency/response"
	"github.com/davinakmalyasha/agents-proxy/internal/agency/worker"
)

// Handler serves public intake routes.
type Handler struct {
	store  *Store
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewHandler builds an intake handler.
func NewHandler(store *Store, pool *pgxpool.Pool, logger *slog.Logger) *Handler {
	return &Handler{store: store, pool: pool, logger: logger}
}

// RegisterRoutes mounts public intake chi routes (mounted under /api/v1).
// Leads are open (WhatsApp/custom inquiries); web orders require a session.
func (h *Handler) RegisterRoutes(r chi.Router, formLimiter *ratelimit.RateLimiter, orderGuard func(http.Handler) http.Handler) {
	r.With(formLimiter.Limit).Post("/leads", h.createLead)
	r.With(formLimiter.Limit, orderGuard).Post("/orders", h.createOrder)
}

// ---------------------------------------------------------------------------
// Leads & orders
// ---------------------------------------------------------------------------

func (h *Handler) createLead(w http.ResponseWriter, r *http.Request) {
	var req CreateLeadRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if fields := req.validate(); len(fields) > 0 {
		response.ValidationError(w, "Invalid lead", fields)
		return
	}
	lead, err := h.store.CreateLead(r.Context(), req)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "create lead failed", "error", err)
		response.Internal(w)
		return
	}
	h.logger.InfoContext(r.Context(), "new lead", "id", lead.ID, "name", lead.Name)
	worker.Emit(r.Context(), h.pool, "lead.created", "lead", itoa64(lead.ID), lead)
	response.Created(w, lead, "Lead received")
}

func (h *Handler) createOrder(w http.ResponseWriter, r *http.Request) {
	actor, ok := platformauth.ActorFrom(r.Context())
	if !ok || actor.Type != "user" {
		response.Unauthorized(w, "Sign in to place an order")
		return
	}
	var req CreateOrderRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if fields := req.validate(); len(fields) > 0 {
		response.ValidationError(w, "Invalid order", fields)
		return
	}
	order, err := h.store.CreateOrder(r.Context(), req, strings.TrimSpace(r.Header.Get("X-Idempotency-Key")), &actor.UserID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.ValidationError(w, "Package not found", map[string]string{"packageId": "unknown package"})
			return
		}
		h.logger.ErrorContext(r.Context(), "create order failed", "error", err)
		response.Internal(w)
		return
	}
	if order.IsDuplicate {
		response.OK(w, order, "Order already received")
		return
	}
	h.logger.InfoContext(r.Context(), "new order", "id", order.ID, "amount", order.TotalAmount)
	worker.Emit(r.Context(), h.pool, "order.created", "order", order.ID, order)
	response.Created(w, order, "Order created")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		response.BadRequest(w, "Unable to read request body")
		return false
	}
	if len(body) == 0 {
		response.BadRequest(w, "Request body is required")
		return false
	}
	if err := json.Unmarshal(body, dst); err != nil {
		response.BadRequest(w, "Invalid JSON body")
		return false
	}
	return true
}
