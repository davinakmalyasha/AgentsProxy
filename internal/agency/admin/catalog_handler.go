package admin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	platformauth "github.com/davinakmalyasha/agents-proxy/internal/agency/auth"
	"github.com/davinakmalyasha/agents-proxy/internal/agency/response"
	"github.com/go-chi/chi/v5"
)

// RegisterCatalogRoutes mounts hosting, catalog, dashboard, audit, and
// settings chi routes (mounted under /api/v1). The dashboard uses
// dashboardGuard (any authenticated role); everything else uses guard.
func (h *Handler) RegisterCatalogRoutes(r chi.Router, guard func(http.Handler) http.Handler, dashboardGuard func(http.Handler) http.Handler) {
	// Hosting
	r.With(guard).Method("GET", "/hosting", http.HandlerFunc(h.listHosting))
	r.With(guard).Method("POST", "/hosting", http.HandlerFunc(h.createHosting))
	r.With(guard).Method("GET", "/hosting/stats", http.HandlerFunc(h.hostingStats))
	r.With(guard).Method("GET", "/hosting/{id}", http.HandlerFunc(h.getHosting))
	r.With(guard).Method("PUT", "/hosting/{id}", http.HandlerFunc(h.updateHosting))
	r.With(guard).Method("DELETE", "/hosting/{id}", http.HandlerFunc(h.deleteHosting))

	// Packages / projects / articles / promos (admin CRUD). Reads reuse the
	// public catalog endpoints, exactly like the original frontend.
	r.With(guard).Method("POST", "/packages", http.HandlerFunc(h.createPackage))
	r.With(guard).Method("PATCH", "/packages/{id}", http.HandlerFunc(h.updatePackage))
	r.With(guard).Method("DELETE", "/packages/{id}", http.HandlerFunc(h.deletePackage))
	r.With(guard).Method("POST", "/projects", http.HandlerFunc(h.createProject))
	r.With(guard).Method("PUT", "/projects/{id}", http.HandlerFunc(h.updateProject))
	r.With(guard).Method("DELETE", "/projects/{id}", http.HandlerFunc(h.deleteProject))
	r.With(guard).Method("POST", "/projects/{projectId}/images", http.HandlerFunc(h.addProjectImage))
	r.With(guard).Method("DELETE", "/projects/{projectId}/images/{imageId}", http.HandlerFunc(h.deleteProjectImage))
	r.With(guard).Method("POST", "/articles", http.HandlerFunc(h.createArticle))
	r.With(guard).Method("PUT", "/articles/{id}", http.HandlerFunc(h.updateArticle))
	r.With(guard).Method("DELETE", "/articles/{id}", http.HandlerFunc(h.deleteArticle))
	r.With(guard).Method("GET", "/promos", http.HandlerFunc(h.listPromosAdmin))
	r.With(guard).Method("POST", "/promos", http.HandlerFunc(h.createPromo))
	r.With(guard).Method("PUT", "/promos/{id}", http.HandlerFunc(h.updatePromo))
	r.With(guard).Method("DELETE", "/promos/{id}", http.HandlerFunc(h.deletePromo))

	// Settings
	r.With(guard).Method("PUT", "/settings", http.HandlerFunc(h.updateSettings))

	// Dashboard + audit (dashboard: any authenticated role).
	r.With(dashboardGuard).Method("GET", "/dashboard", http.HandlerFunc(h.dashboard))
	r.With(guard).Method("GET", "/audit", http.HandlerFunc(h.auditList))
	r.With(guard).Method("GET", "/admin/audit", http.HandlerFunc(h.auditList))

	// Webhook subscriptions (agent push delivery).
	r.With(guard).Method("GET", "/webhooks", http.HandlerFunc(h.listWebhooks))
	r.With(guard).Method("POST", "/webhooks", http.HandlerFunc(h.createWebhook))
	r.With(guard).Method("DELETE", "/webhooks/{id}", http.HandlerFunc(h.deleteWebhook))
}

// ---------------------------------------------------------------------------
// Hosting
// ---------------------------------------------------------------------------

func (h *Handler) listHosting(w http.ResponseWriter, r *http.Request) {
	limit, offset := adminPagination(r)
	entries, total, err := h.store.ListHosting(r.Context(), limit, offset)
	if err != nil {
		writeErr(w, h.logger, err)
		return
	}
	page, limitOut := pageOf(r, limit)
	writeEnvelopeList(w, entries, total, page, limitOut, "Hosting entries retrieved")
}

func (h *Handler) getHosting(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r)
	if !ok {
		return
	}
	entry, err := h.store.GetHosting(r.Context(), id)
	if err != nil {
		writeErr(w, h.logger, err)
		return
	}
	response.OK(w, entry, "Hosting entry retrieved")
}

func (h *Handler) createHosting(w http.ResponseWriter, r *http.Request) {
	var body Hosting
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Domain) == "" || strings.TrimSpace(body.ClientName) == "" {
		response.ValidationError(w, "Invalid hosting", map[string]string{"domain": "required", "clientName": "required"})
		return
	}
	if body.Status == "" {
		body.Status = "ACTIVE"
	}
	if body.NotifyBeforeDays <= 0 {
		body.NotifyBeforeDays = 30
	}
	entry, err := h.store.CreateHosting(r.Context(), body)
	if err != nil {
		writeErr(w, h.logger, err)
		return
	}
	h.audit(r, "CREATE_HOSTING", "Hosting: "+entry.Domain, nil)
	response.Created(w, entry, "Hosting entry created")
}

func (h *Handler) updateHosting(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r)
	if !ok {
		return
	}
	var body map[string]any
	if !decodeJSON(w, r, &body) {
		return
	}
	entry, err := h.store.UpdateHosting(r.Context(), id, body)
	if err != nil {
		writeErr(w, h.logger, err)
		return
	}
	h.audit(r, "UPDATE_HOSTING", "Hosting ID: "+itoa64(id), nil)
	response.OK(w, entry, "Hosting entry updated")
}

func (h *Handler) deleteHosting(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r)
	if !ok {
		return
	}
	if err := h.store.SoftDeleteHosting(r.Context(), id); err != nil {
		writeErr(w, h.logger, err)
		return
	}
	h.audit(r, "DELETE_HOSTING", "Hosting ID: "+itoa64(id), nil)
	response.OK(w, nil, "Hosting entry deleted")
}

func (h *Handler) hostingStats(w http.ResponseWriter, r *http.Request) {
	_ = h.store.RefreshHostingStatuses(r.Context())
	stats, err := h.store.HostingStats(r.Context())
	if err != nil {
		writeErr(w, h.logger, err)
		return
	}
	response.OK(w, stats, "Hosting stats retrieved")
}

// ---------------------------------------------------------------------------
// Packages (admin)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Packages (admin)

func (h *Handler) createPackage(w http.ResponseWriter, r *http.Request) {
	var body ManagePackage
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Name) == "" || strings.TrimSpace(body.Slug) == "" {
		response.ValidationError(w, "Invalid package", map[string]string{"name": "required", "slug": "required"})
		return
	}
	pkg, err := h.store.CreatePackage(r.Context(), body)
	if err != nil {
		writeErr(w, h.logger, err)
		return
	}
	h.audit(r, "CREATE_PACKAGE", "Package: "+pkg.Slug, nil)
	response.Created(w, pkg, "Package created")
}

func (h *Handler) updatePackage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r)
	if !ok {
		return
	}
	var body map[string]any
	if !decodeJSON(w, r, &body) {
		return
	}
	pkg, err := h.store.UpdatePackage(r.Context(), id, body)
	if err != nil {
		writeErr(w, h.logger, err)
		return
	}
	h.audit(r, "UPDATE_PACKAGE", "Package ID: "+itoa64(id), nil)
	response.OK(w, pkg, "Package updated")
}

func (h *Handler) deletePackage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r)
	if !ok {
		return
	}
	if err := h.store.SoftDeletePackage(r.Context(), id); err != nil {
		writeErr(w, h.logger, err)
		return
	}
	h.audit(r, "DELETE_PACKAGE", "Package ID: "+itoa64(id), nil)
	response.OK(w, nil, "Package deleted")
}

// ---------------------------------------------------------------------------
// Projects (admin)
// ---------------------------------------------------------------------------

func (h *Handler) createProject(w http.ResponseWriter, r *http.Request) {
	var body ManageProject
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Title) == "" || strings.TrimSpace(body.Slug) == "" {
		response.ValidationError(w, "Invalid project", map[string]string{"title": "required", "slug": "required"})
		return
	}
	project, err := h.store.CreateProject(r.Context(), body)
	if err != nil {
		writeErr(w, h.logger, err)
		return
	}
	h.audit(r, "CREATE_PROJECT", "Project: "+project.Slug, nil)
	response.Created(w, project, "Project created")
}

func (h *Handler) updateProject(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r)
	if !ok {
		return
	}
	var body map[string]any
	if !decodeJSON(w, r, &body) {
		return
	}
	project, err := h.store.UpdateProject(r.Context(), id, body)
	if err != nil {
		writeErr(w, h.logger, err)
		return
	}
	h.audit(r, "UPDATE_PROJECT", "Project ID: "+itoa64(id), nil)
	response.OK(w, project, "Project updated")
}

func (h *Handler) deleteProject(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r)
	if !ok {
		return
	}
	if err := h.store.SoftDeleteProject(r.Context(), id); err != nil {
		writeErr(w, h.logger, err)
		return
	}
	h.audit(r, "DELETE_PROJECT", "Project ID: "+itoa64(id), nil)
	response.OK(w, nil, "Project deleted")
}

func (h *Handler) addProjectImage(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathInt64(w, r, "projectId")
	if !ok {
		return
	}
	var body struct {
		ImageURL string  `json:"imageUrl"`
		Caption  *string `json:"caption"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.ImageURL) == "" {
		response.ValidationError(w, "Invalid image", map[string]string{"imageUrl": "required"})
		return
	}
	img, err := h.store.AddProjectImage(r.Context(), projectID, strings.TrimSpace(body.ImageURL), body.Caption)
	if err != nil {
		writeErr(w, h.logger, err)
		return
	}
	response.Created(w, img, "Image added")
}

func (h *Handler) deleteProjectImage(w http.ResponseWriter, r *http.Request) {
	imageID, ok := pathInt64(w, r, "imageId")
	if !ok {
		return
	}
	if err := h.store.DeleteProjectImage(r.Context(), imageID); err != nil {
		writeErr(w, h.logger, err)
		return
	}
	response.OK(w, nil, "Image deleted")
}

// ---------------------------------------------------------------------------
// Articles (admin)
// ---------------------------------------------------------------------------

func (h *Handler) createArticle(w http.ResponseWriter, r *http.Request) {
	var body ManageArticle
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Title) == "" || strings.TrimSpace(body.Slug) == "" {
		response.ValidationError(w, "Invalid article", map[string]string{"title": "required", "slug": "required"})
		return
	}
	actor, _ := platformauth.ActorFrom(r.Context())
	var authorID *int64
	if actor.Type == "user" {
		authorID = &actor.UserID
	}
	article, err := h.store.CreateArticle(r.Context(), body, authorID)
	if err != nil {
		writeErr(w, h.logger, err)
		return
	}
	h.audit(r, "CREATE_ARTICLE", "Article: "+article.Slug, nil)
	response.Created(w, article, "Article created")
}

func (h *Handler) updateArticle(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r)
	if !ok {
		return
	}
	var body map[string]any
	if !decodeJSON(w, r, &body) {
		return
	}
	article, err := h.store.UpdateArticle(r.Context(), id, body)
	if err != nil {
		writeErr(w, h.logger, err)
		return
	}
	h.audit(r, "UPDATE_ARTICLE", "Article ID: "+itoa64(id), nil)
	response.OK(w, article, "Article updated")
}

func (h *Handler) deleteArticle(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r)
	if !ok {
		return
	}
	if err := h.store.SoftDeleteArticle(r.Context(), id); err != nil {
		writeErr(w, h.logger, err)
		return
	}
	h.audit(r, "DELETE_ARTICLE", "Article ID: "+itoa64(id), nil)
	response.OK(w, nil, "Article deleted")
}

// ---------------------------------------------------------------------------
// Promos (admin)
// ---------------------------------------------------------------------------

func (h *Handler) listPromosAdmin(w http.ResponseWriter, r *http.Request) {
	limit, offset := adminPagination(r)
	promos, total, err := h.store.ListAllPromos(r.Context(), limit, offset)
	if err != nil {
		writeErr(w, h.logger, err)
		return
	}
	page, limitOut := pageOf(r, limit)
	writeEnvelopeList(w, promos, total, page, limitOut, "Promos retrieved")
}

func (h *Handler) createPromo(w http.ResponseWriter, r *http.Request) {
	var body ManagePromo
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		response.ValidationError(w, "Invalid promo", map[string]string{"text": "required"})
		return
	}
	promo, err := h.store.CreatePromo(r.Context(), body)
	if err != nil {
		writeErr(w, h.logger, err)
		return
	}
	response.Created(w, promo, "Promo created")
}

func (h *Handler) updatePromo(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r)
	if !ok {
		return
	}
	var body map[string]any
	if !decodeJSON(w, r, &body) {
		return
	}
	promo, err := h.store.UpdatePromo(r.Context(), id, body)
	if err != nil {
		writeErr(w, h.logger, err)
		return
	}
	response.OK(w, promo, "Promo updated")
}

func (h *Handler) deletePromo(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r)
	if !ok {
		return
	}
	if err := h.store.DeletePromo(r.Context(), id); err != nil {
		writeErr(w, h.logger, err)
		return
	}
	response.OK(w, nil, "Promo deleted")
}

// ---------------------------------------------------------------------------
// Settings / dashboard / audit
// ---------------------------------------------------------------------------

func (h *Handler) updateSettings(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if !decodeJSON(w, r, &body) {
		return
	}
	settings, err := h.store.UpdateSettings(r.Context(), body)
	if err != nil {
		writeErr(w, h.logger, err)
		return
	}
	h.audit(r, "UPDATE_SETTINGS", "global", nil)
	response.OK(w, settings, "Settings updated")
}

func (h *Handler) dashboard(w http.ResponseWriter, r *http.Request) {
	analytics, err := h.store.GetAnalytics(r.Context())
	if err != nil {
		writeErr(w, h.logger, err)
		return
	}
	response.OK(w, analytics, "Analytics retrieved")
}

func (h *Handler) auditList(w http.ResponseWriter, r *http.Request) {
	limit, offset := adminPagination(r)
	entries, total, err := h.store.ListAudit(r.Context(), strings.TrimSpace(r.URL.Query().Get("action")), limit, offset)
	if err != nil {
		writeErr(w, h.logger, err)
		return
	}
	page, limitOut := pageOf(r, limit)
	totalPages := int((total + int64(limitOut) - 1) / int64(limitOut))
	// The logs page reads res.data.data.logs + res.data.data.pagination.
	response.OK(w, map[string]any{
		"logs": entries,
		"pagination": map[string]any{
			"total": total, "page": page, "limit": limitOut, "totalPages": totalPages,
			"hasNextPage": page < totalPages, "hasPrevPage": page > 1,
		},
	}, "Audit logs retrieved")
}

func (h *Handler) listWebhooks(w http.ResponseWriter, r *http.Request) {
	subs, err := h.store.ListWebhooks(r.Context())
	if err != nil {
		writeErr(w, h.logger, err)
		return
	}
	response.OK(w, subs, "Webhooks retrieved")
}

func (h *Handler) createWebhook(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL        string   `json:"url"`
		EventTypes []string `json:"eventTypes"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.URL) == "" ||
		(!strings.HasPrefix(body.URL, "https://") && !strings.HasPrefix(body.URL, "http://localhost")) {
		response.ValidationError(w, "Invalid webhook", map[string]string{"url": "https URL required"})
		return
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		writeErr(w, h.logger, err)
		return
	}
	secret := "whsec_" + hex.EncodeToString(raw[:])
	actor, _ := platformauth.ActorFrom(r.Context())
	var createdBy *int64
	if actor.Type == "user" {
		createdBy = &actor.UserID
	}
	sub, err := h.store.CreateWebhook(r.Context(), strings.TrimSpace(body.URL), secret, body.EventTypes, createdBy)
	if err != nil {
		writeErr(w, h.logger, err)
		return
	}
	h.audit(r, "CREATE_WEBHOOK", "Webhook: "+sub.URL, nil)
	response.Created(w, map[string]any{"subscription": sub, "secret": secret}, "Webhook created — store the secret, it is shown once")
}

func (h *Handler) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		response.BadRequest(w, "Invalid id")
		return
	}
	if err := h.store.DeleteWebhook(r.Context(), id); err != nil {
		writeErr(w, h.logger, err)
		return
	}
	h.audit(r, "DELETE_WEBHOOK", "Webhook: "+id, nil)
	response.OK(w, nil, "Webhook deleted")
}

// ---------------------------------------------------------------------------
// Project images store helpers
// ---------------------------------------------------------------------------

func writeEnvelopeList(w http.ResponseWriter, data any, total int64, page, limit int, message string) {
	totalPages := int((total + int64(limit) - 1) / int64(limit))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"message": message,
		"data":    data,
		"meta": map[string]any{
			"total": total, "page": page, "limit": limit, "totalPages": totalPages,
			"hasNextPage": page < totalPages, "hasPrevPage": page > 1,
		},
		"error": nil,
	})
}
