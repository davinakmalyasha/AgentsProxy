// Package auth is Centrum's staff-identity provider.
//
// The platform owns the session directory and token primitives
// (internal/authapi). This package owns Centrum's user table and its
// login/profile/register rules. All queries run inside tenantdb.WithActiveTx,
// so RLS enforces the tenant boundary and a cloned test schema works exactly
// like project_centrum.
package auth

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/authapi"
	"github.com/davinakmalyasha/agents-proxy/internal/features/clientctx"
	"github.com/davinakmalyasha/agents-proxy/internal/tenantdb"
)

func init() { authapi.RegisterProvider("centrum", New) }

// Provider implements authapi.Provider for Centrum.
type Provider struct {
	Pool *pgxpool.Pool
}

// New builds the Centrum provider bound to the shared runtime pool.
func New(pool *pgxpool.Pool) authapi.Provider {
	return &Provider{Pool: pool}
}

// Authenticate reads the credential row for one tenant/email. The platform
// compares the password; this method only owns the lookup.
func (p *Provider) Authenticate(ctx context.Context, tenantID, email string) (authapi.Credentials, error) {
	var c authapi.Credentials
	email = strings.ToLower(strings.TrimSpace(email))
	err := tenantdb.WithActiveTx(ctx, p.Pool, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			SELECT id::text, email, password_hash, role, full_name,
			       COALESCE(phone, ''), is_active
			  FROM sports_users
			 WHERE tenant_id = $1 AND email = $2`,
			tenantID, email,
		).Scan(&c.User.ID, &c.User.Email, &c.PasswordHash,
			&c.User.Role, &c.User.FullName, &c.User.Phone, &c.Active); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`SELECT id::text, db_schema, vertical, features
			   FROM public.clients WHERE tenant_id = $1`, tenantID).
			Scan(&c.User.ClientID, &c.User.DBSchema, &c.User.Vertical,
				&c.User.Features)
	})
	if errors.Is(err, pgx.ErrNoRows) ||
		errors.Is(err, tenantdb.ErrUnknownTenant) ||
		errors.Is(err, tenantdb.ErrInactiveTenant) {
		return c, authapi.ErrInvalidCredentials
	}
	if err != nil {
		return c, err
	}
	c.User.TenantID = tenantID
	return c, nil
}

// Profile returns the safe public projection for a live user. A deleted or
// deactivated user invalidates the session immediately.
func (p *Provider) Profile(ctx context.Context, tenantID, userID string) (authapi.User, error) {
	var u authapi.User
	err := tenantdb.WithActiveTx(ctx, p.Pool, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			SELECT id::text, email, role, full_name, COALESCE(phone, '')
			  FROM sports_users
			 WHERE id = $1::uuid AND tenant_id = $2 AND is_active`,
			userID, tenantID,
		).Scan(&u.ID, &u.Email, &u.Role, &u.FullName, &u.Phone); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`SELECT id::text, db_schema, vertical, features
			   FROM public.clients WHERE tenant_id = $1`, tenantID).
			Scan(&u.ClientID, &u.DBSchema, &u.Vertical, &u.Features)
	})
	if errors.Is(err, pgx.ErrNoRows) ||
		errors.Is(err, tenantdb.ErrUnknownTenant) ||
		errors.Is(err, tenantdb.ErrInactiveTenant) {
		return u, authapi.ErrInvalidCredentials
	}
	if err != nil {
		return u, err
	}
	u.TenantID = tenantID
	return u, nil
}

// Register creates a staff row for the tenant. The platform has already
// checked the caller's owner session, normalized the email, and hashed the
// password.
func (p *Provider) Register(ctx context.Context, tenantID string, in authapi.RegisterInput) (authapi.User, error) {
	role := strings.ToLower(strings.TrimSpace(in.Role))
	if role != clientctx.RoleOwner && role != clientctx.RoleStaff {
		return authapi.User{}, authapi.ErrInvalidRole
	}
	var u authapi.User
	email := strings.ToLower(strings.TrimSpace(in.Email))
	err := tenantdb.WithActiveTx(ctx, p.Pool, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO sports_users (tenant_id, email, password_hash, role, full_name, phone)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING id::text, email, role, full_name, COALESCE(phone, '')`,
			tenantID, email, in.PasswordHash, role,
			strings.TrimSpace(in.FullName), strings.TrimSpace(in.Phone),
		).Scan(&u.ID, &u.Email, &u.Role, &u.FullName, &u.Phone); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`SELECT id::text, db_schema, vertical, features
			   FROM public.clients WHERE tenant_id = $1`, tenantID).
			Scan(&u.ClientID, &u.DBSchema, &u.Vertical, &u.Features)
	})
	if isUniqueViolation(err) {
		return authapi.User{}, authapi.ErrEmailTaken
	}
	if errors.Is(err, tenantdb.ErrUnknownTenant) ||
		errors.Is(err, tenantdb.ErrInactiveTenant) {
		return authapi.User{}, authapi.ErrInvalidCredentials
	}
	if err != nil {
		return authapi.User{}, err
	}
	u.TenantID = tenantID
	return u, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
