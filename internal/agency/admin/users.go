package admin

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	platformauth "github.com/davinakmalyasha/agents-proxy/internal/agency/auth"
)

// ManagedUser is the admin view of a user.
type ManagedUser struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	LeadID    *int64    `json:"leadId"`
	LeadName  *string   `json:"leadName"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ListUsers returns users with optional role/search filters.
func (s *Store) ListUsers(ctx context.Context, role, search string, limit, offset int) ([]ManagedUser, int64, error) {
	where := `WHERE u.deleted_at IS NULL`
	args := []any{}
	if role != "" {
		args = append(args, role)
		where += ` AND u.role = $` + itoa(len(args))
	}
	if strings.TrimSpace(search) != "" {
		args = append(args, "%"+strings.TrimSpace(search)+"%")
		where += ` AND (u.username ILIKE $` + itoa(len(args)) + ` OR u.email ILIKE $` + itoa(len(args)) + `)`
	}

	var total int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users u `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	args = append(args, limit, offset)
	rows, err := s.pool.Query(ctx, `SELECT u.id, u.username, u.email, u.role, u.lead_id, l.name, u.created_at, u.updated_at
		FROM users u LEFT JOIN leads l ON l.id = u.lead_id `+where+
		` ORDER BY u.created_at DESC LIMIT $`+itoa(len(args)-1)+` OFFSET $`+itoa(len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []ManagedUser{}
	for rows.Next() {
		var (
			u    ManagedUser
			lead pgtype.Int8
			name pgtype.Text
		)
		if err := rows.Scan(&u.ID, &u.Username, &u.Email, &u.Role, &lead, &name, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, 0, err
		}
		u.LeadID = int8Ptr(lead)
		u.LeadName = textPtr(name)
		out = append(out, u)
	}
	return out, total, rows.Err()
}

// CreateUser inserts a user with a real password credential.
func (s *Store) CreateUser(ctx context.Context, username, email, password, role string, leadID *int64) (*ManagedUser, error) {
	hash, err := platformauth.HashPassword(password)
	if err != nil {
		return nil, err
	}
	username = strings.TrimSpace(username)
	email = normalizeEmail(email)
	var u ManagedUser
	err = s.pool.QueryRow(ctx, `INSERT INTO users (username, email, password_hash, role, lead_id)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id, username, email, role, lead_id, created_at, updated_at`,
		username, email, hash, role, leadID,
	).Scan(&u.ID, &u.Username, &u.Email, &u.Role, &u.LeadID, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// UpdateUser applies username/email/role/lead changes.
func (s *Store) UpdateUser(ctx context.Context, id int64, username, email, role *string, leadID *int64, clearLead bool) (*ManagedUser, error) {
	sets := []string{}
	args := []any{}
	add := func(column string, value any) {
		args = append(args, value)
		sets = append(sets, column+` = $`+itoa(len(args)))
	}
	if username != nil {
		add("username", strings.TrimSpace(*username))
	}
	if email != nil {
		add("email", normalizeEmail(*email))
	}
	if role != nil {
		add("role", *role)
		// A role change must invalidate existing tokens; the middleware also
		// reads the live role, but a re-login keeps the JWT payload honest.
		sets = append(sets, `token_version = token_version + 1`)
	}
	if clearLead {
		add("lead_id", nil)
	} else if leadID != nil {
		add("lead_id", *leadID)
	}
	if len(sets) == 0 {
		return s.GetUser(ctx, id)
	}
	args = append(args, id)
	var (
		u    ManagedUser
		lead pgtype.Int8
	)
	err := s.pool.QueryRow(ctx, `UPDATE users SET `+joinComma(sets)+
		` WHERE id = $`+itoa(len(args))+` AND deleted_at IS NULL
		RETURNING id, username, email, role, lead_id, created_at, updated_at`, args...,
	).Scan(&u.ID, &u.Username, &u.Email, &u.Role, &lead, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	u.LeadID = int8Ptr(lead)
	return &u, nil
}

// GetUser returns one user.
func (s *Store) GetUser(ctx context.Context, id int64) (*ManagedUser, error) {
	var (
		u    ManagedUser
		lead pgtype.Int8
		name pgtype.Text
	)
	err := s.pool.QueryRow(ctx, `SELECT u.id, u.username, u.email, u.role, u.lead_id, l.name, u.created_at, u.updated_at
		FROM users u LEFT JOIN leads l ON l.id = u.lead_id
		WHERE u.id = $1 AND u.deleted_at IS NULL`, id,
	).Scan(&u.ID, &u.Username, &u.Email, &u.Role, &lead, &name, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	u.LeadID = int8Ptr(lead)
	u.LeadName = textPtr(name)
	return &u, nil
}

// SoftDeleteUser soft-deletes and revokes sessions.
func (s *Store) SoftDeleteUser(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET deleted_at = now(), token_version = token_version + 1
		WHERE id = $1 AND deleted_at IS NULL`, id)
	return err
}

// RevokeSessions bumps the token version.
func (s *Store) RevokeSessions(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET token_version = token_version + 1
		WHERE id = $1 AND deleted_at IS NULL`, id)
	return err
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
