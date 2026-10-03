// Idempotency keys for money writes (Phase B1: pure upgrade, optional phase).
//
// A retried POST (double-click, timeout retry, 502 retry) must never create
// a second charge. The client sends Idempotency-Key; the handler stores it
// on the created row under a partial unique index
// (tenant_id, idempotency_key) WHERE key IS NOT NULL, and a repeated key
// returns the ORIGINAL row instead of inserting.
//
// Optional-first: a missing header behaves exactly as today (no key stored,
// no dedup). Enforcement (400 when absent) comes later, after every client
// sends keys. Nothing about the success shape changes otherwise.
package records

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

const maxIdempotencyKeyLen = 64

// IdempotencyKey reads and validates the Idempotency-Key request header.
// It returns ("", false, nil) when the header is absent -- the caller then
// behaves exactly as before (optional phase). A present-but-malformed key
// is a 400 written here, returning ok=false with a non-nil error so the
// caller stops.
func IdempotencyKey(w http.ResponseWriter, r *http.Request) (key string, present bool, err error) {
	raw := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if raw == "" {
		return "", false, nil
	}
	if len(raw) > maxIdempotencyKeyLen {
		WriteJSONError(w, http.StatusBadRequest, "idempotency-key too long (max 64)")
		return "", false, errors.New("idempotency-key too long")
	}
	for _, c := range raw {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			WriteJSONError(w, http.StatusBadRequest, "idempotency-key must be [A-Za-z0-9_-]")
			return "", false, errors.New("idempotency-key invalid charset")
		}
	}
	return raw, true, nil
}

// IsUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505) -- the signal that a concurrent duplicate won
// the race, so the caller must fetch and return the existing row.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
