// Runtime role verification for production boots.
//
// DATABASE_URL is the credential every tenant request uses. If it points at
// a superuser or a BYPASSRLS role, every RLS policy in the database is
// decorative: the same client token that should see one tenant can read all
// of them. Supabase's `postgres` role is not a superuser but does carry
// BYPASSRLS, so checking rolsuper alone would miss the production-shaped
// mistake.
package db

import (
	"context"
	"fmt"
)

// VerifyRuntimeRole rejects a runtime connection made by a role that can
// bypass row-level security, and returns the role name for the boot log.
// The admin/migration pool is deliberately exempt: DDL and the agency plane
// use it (until their own roles land).
func VerifyRuntimeRole(ctx context.Context, pool *Pool) (string, error) {
	var (
		user   string
		super  bool
		bypass bool
	)
	err := pool.QueryRow(ctx, `
		SELECT r.rolname, r.rolsuper, r.rolbypassrls
		  FROM pg_roles r
		 WHERE r.rolname = current_user`).Scan(&user, &super, &bypass)
	if err != nil {
		return "", fmt.Errorf("read runtime role: %w", err)
	}
	if super || bypass {
		return user, fmt.Errorf("DATABASE_URL must use a NOBYPASSRLS role; connected as %q (superuser=%t bypassrls=%t)", user, super, bypass)
	}
	return user, nil
}
