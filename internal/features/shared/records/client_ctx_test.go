package records

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/davinakmalyasha/agents-proxy/internal/cache"
	"github.com/davinakmalyasha/agents-proxy/internal/features/clientctx"
)

func TestCallerFromCtx(t *testing.T) {
	t.Run("client token", func(t *testing.T) {
		ctx := clientctx.WithClient(context.Background(), &cache.ClientModel{
			ID: "client-a", TenantID: "tenant-a", Status: "active",
		})
		c := callerFromCtx(ctx)
		if c == nil {
			t.Fatal("client token did not resolve a caller")
		}
		if c.TenantID != "tenant-a" || c.ClientID != "client-a" {
			t.Fatalf("caller = %+v, want tenant-a/client-a", c)
		}
	})

	t.Run("staff session", func(t *testing.T) {
		ctx := clientctx.With(context.Background(), &clientctx.Identity{
			UserID: "user-1", Role: clientctx.RoleOwner,
			Tenant: "tenant-a", ClientID: "client-a",
		})
		c := callerFromCtx(ctx)
		if c == nil {
			t.Fatal("staff session did not resolve a caller")
		}
		if c.TenantID != "tenant-a" || c.ClientID != "client-a" {
			t.Fatalf("caller = %+v, want tenant-a/client-a", c)
		}
	})

	t.Run("legacy client model fallback", func(t *testing.T) {
		ctx := clientctx.With(context.Background(), &clientctx.Identity{
			Role: clientctx.RoleOwner, Tenant: "tenant-a",
			Client: &cache.ClientModel{ID: "client-a"},
		})
		c := callerFromCtx(ctx)
		if c == nil || c.ClientID != "client-a" {
			t.Fatalf("caller = %+v, want legacy client id", c)
		}
	})

	t.Run("public read is a read-only caller", func(t *testing.T) {
		ctx := clientctx.With(context.Background(), &clientctx.Identity{
			Role: clientctx.RolePublic, Tenant: "tenant-a",
		})
		c := callerFromCtx(ctx)
		if c == nil || !c.Public || c.TenantID != "tenant-a" {
			t.Fatalf("public caller = %+v, want read-only tenant-a", c)
		}
		req := httptest.NewRequest(http.MethodPost, "/records", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		if got := requireActor(rec, req); got != nil {
			t.Fatalf("public POST resolved caller %+v", got)
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("public POST status = %d, want 403", rec.Code)
		}
		req = httptest.NewRequest(http.MethodGet, "/records", nil).WithContext(ctx)
		rec = httptest.NewRecorder()
		if got := requireActor(rec, req); got == nil {
			t.Fatal("public GET was refused")
		}
	})

	t.Run("unresolved identity", func(t *testing.T) {
		if c := callerFromCtx(context.Background()); c != nil {
			t.Fatalf("unresolved identity resolved as caller %+v", c)
		}
	})
}
