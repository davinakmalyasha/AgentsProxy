package records

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// DONE: PRD 6.5 (cursor pagination) / STANDARDS A, F
//
// Cursor format: base64url("<created_at_unix_nano>|<uuid>")
// Sort key: (created_at DESC, id DESC) — see SQL in handlers.go.
// Limit: default 20, max 100. The +1 over-fetch trick computes
// has_more without an extra round-trip.

const (
	defaultLimit = 20
	maxLimit     = 100
)

type cursor struct {
	ts time.Time
	id uuid.UUID
}

// encodeCursor produces a stable, URL-safe cursor token.
func encodeCursor(t time.Time, id uuid.UUID) string {
	raw := fmt.Sprintf("%d|%s", t.UnixNano(), id.String())
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor parses a token previously produced by encodeCursor.
// Returns an error for malformed input; the caller decides on 400.
func decodeCursor(s string) (cursor, error) {
	if s == "" {
		return cursor{}, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, fmt.Errorf("invalid cursor encoding: %w", err)
	}
	for i := 0; i < len(b); i++ {
		if b[i] == '|' {
			ns, err := strconv.ParseInt(string(b[:i]), 10, 64)
			if err != nil {
				return cursor{}, fmt.Errorf("invalid cursor ts: %w", err)
			}
			id, err := uuid.Parse(string(b[i+1:]))
			if err != nil {
				return cursor{}, fmt.Errorf("invalid cursor id: %w", err)
			}
			return cursor{ts: time.Unix(0, ns), id: id}, nil
		}
	}
	return cursor{}, errors.New("invalid cursor format")
}

// parseLimit clamps the requested limit to the allowed range.
func parseLimit(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return defaultLimit
	}
	if n > maxLimit {
		return maxLimit
	}
	return n
}
