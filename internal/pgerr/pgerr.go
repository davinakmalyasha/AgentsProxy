// Package pgerr is the single place that classifies Postgres errors.
//
// Every handler used to re-implement this with its own spelling: nine
// copies of the "23505" literal (two via a bare type assertion that
// does not unwrap), plus ad-hoc strings.Contains on error text (which
// breaks on any server message change and allocates the string).
// A new error class is added here once, not in nine handlers.
package pgerr

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// Code returns the SQLSTATE of a Postgres error, unwrapping first.
// Empty when err is not a Postgres error.
func Code(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// Constraint returns the constraint or trigger name, if any.
func Constraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

// IsUniqueViolation reports SQLSTATE 23505: a concurrent duplicate won
// the race, so the caller must fetch and return the existing row.
func IsUniqueViolation(err error) bool {
	return Code(err) == "23505"
}

// IsDeadlock reports SQLSTATE 40P01: the transaction aborted on a
// deadlock and the closure may be retried (see withTenantTx).
func IsDeadlock(err error) bool {
	return Code(err) == "40P01"
}

// IsExclusion reports SQLSTATE 23P01: an exclusion-constraint or
// exclusion_violation trigger refusal — double-booking or blocked
// slot. Both map to 409.
func IsExclusion(err error) bool {
	return Code(err) == "23P01"
}

// IsRLSDenied reports SQLSTATE 42501: row-level security (or a missing
// grant) refused the statement. Never a client error: it means the
// role, policy, or tenant GUC is wrong.
func IsRLSDenied(err error) bool {
	return Code(err) == "42501"
}
