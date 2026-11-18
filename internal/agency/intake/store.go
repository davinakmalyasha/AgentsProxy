package intake

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Store owns intake writes.
type Store struct {
	pool interface {
		Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
		QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
		Begin(ctx context.Context) (pgx.Tx, error)
	}
}

// NewStore builds an intake Store. The pool is the pgx pool; the narrow
// interface keeps the store testable.
func NewStore(pool interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
},
) *Store {
	return &Store{pool: pool}
}

// Lead is a created inquiry.
type Lead struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Whatsapp     string    `json:"whatsapp"`
	BusinessName *string   `json:"businessName"`
	Message      *string   `json:"message"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"createdAt"`
}

// CreatedOrder is the order intake result.
type CreatedOrder struct {
	ID          string    `json:"id"`
	TotalAmount float64   `json:"totalAmount"`
	Status      string    `json:"status"`
	LeadID      int64     `json:"leadId"`
	PackageID   int64     `json:"packageId"`
	CreatedAt   time.Time `json:"createdAt"`
	IsDuplicate bool      `json:"isDuplicate,omitempty"`
}

// CreateLead inserts a public inquiry.
func (s *Store) CreateLead(ctx context.Context, req CreateLeadRequest) (*Lead, error) {
	var (
		l   Lead
		biz pgtype.Text
		msg pgtype.Text
	)
	err := s.pool.QueryRow(ctx, `INSERT INTO leads (name, whatsapp, business_name, message)
		VALUES ($1,$2,$3,$4) RETURNING id, name, whatsapp, business_name, message, status, created_at`,
		strings.TrimSpace(req.Name), strings.TrimSpace(req.Whatsapp), req.BusinessName, req.Message,
	).Scan(&l.ID, &l.Name, &l.Whatsapp, &biz, &msg, &l.Status, &l.CreatedAt)
	if err != nil {
		return nil, err
	}
	l.BusinessName = textPtr(biz)
	l.Message = textPtr(msg)
	return &l, nil
}

// CreateOrder runs the full intake transaction: idempotency check, lead
// find-or-create, package pricing, order + UNPAID payment rows, and an
// ORDER_CREATED activity. It mirrors the original Express semantics exactly.
func (s *Store) CreateOrder(ctx context.Context, req CreateOrderRequest, idempotencyKey string, userID *int64) (*CreatedOrder, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// 1. Idempotency: return the existing order untouched.
	if idempotencyKey != "" {
		var existing CreatedOrder
		var amount pgtype.Numeric
		err := tx.QueryRow(ctx, `SELECT id, total_amount, status, lead_id, package_id, created_at
			FROM orders WHERE idempotency_key = $1`, idempotencyKey,
		).Scan(&existing.ID, &amount, &existing.Status, &existing.LeadID, &existing.PackageID, &existing.CreatedAt)
		if err == nil {
			if userID != nil {
				if _, err := tx.Exec(ctx, `UPDATE orders SET user_id = COALESCE(user_id, $1) WHERE id = $2`,
					*userID, existing.ID); err != nil {
					return nil, err
				}
				if err := linkUserLead(ctx, tx, *userID, existing.LeadID); err != nil {
					return nil, err
				}
				if err := tx.Commit(ctx); err != nil {
					return nil, err
				}
			}
			existing.TotalAmount = numericFloat(amount)
			existing.IsDuplicate = true
			return &existing, nil
		}
		if err != pgx.ErrNoRows {
			return nil, err
		}
	}

	// 2. Find-or-create lead by WhatsApp.
	whatsapp := strings.TrimSpace(req.Whatsapp)
	var leadID int64
	err = tx.QueryRow(ctx, `SELECT id FROM leads WHERE whatsapp = $1 AND deleted_at IS NULL`, whatsapp).Scan(&leadID)
	if err == pgx.ErrNoRows {
		err = tx.QueryRow(ctx, `INSERT INTO leads (name, whatsapp, business_name)
			VALUES ($1,$2,$3) RETURNING id`,
			strings.TrimSpace(req.Name), whatsapp, req.BusinessName).Scan(&leadID)
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else {
		_, err = tx.Exec(ctx, `UPDATE leads SET name = $1,
			business_name = COALESCE($2, business_name) WHERE id = $3`,
			strings.TrimSpace(req.Name), req.BusinessName, leadID)
		if err != nil {
			return nil, err
		}
	}

	// 3. Package pricing: discount wins when present.
	var (
		price    pgtype.Numeric
		discount pgtype.Numeric
	)
	if err := tx.QueryRow(ctx, `SELECT price, discount_price FROM packages
		WHERE id = $1 AND deleted_at IS NULL`, req.PackageID).Scan(&price, &discount); err != nil {
		return nil, err
	}
	total := numericFloat(price)
	if d := numericFloatPtr(discount); d != nil {
		total = *d
	}

	// 4. Order + UNPAID payment.
	brief := []byte(`{}`)
	if len(req.BriefData) > 0 {
		brief = req.BriefData
	}
	var created CreatedOrder
	var amount pgtype.Numeric
	idem := nullIfEmpty(idempotencyKey)
	err = tx.QueryRow(ctx, `INSERT INTO orders
		(lead_id, package_id, total_amount, brief_data, agreed_to_terms_at, status, idempotency_key, user_id)
		VALUES ($1,$2,$3,$4,now(),'PENDING_PAYMENT',$5,$6)
		RETURNING id, total_amount, status, lead_id, package_id, created_at`,
		leadID, req.PackageID, total, brief, idem, userID,
	).Scan(&created.ID, &amount, &created.Status, &created.LeadID, &created.PackageID, &created.CreatedAt)
	if err != nil {
		return nil, err
	}
	created.TotalAmount = numericFloat(amount)

	if _, err := tx.Exec(ctx, `INSERT INTO payments (order_id, payment_method, payment_status)
		VALUES ($1,'MANUAL','UNPAID')`, created.ID); err != nil {
		return nil, err
	}

	meta, _ := json.Marshal(map[string]any{"packageId": req.PackageID, "totalAmount": total})
	if _, err := tx.Exec(ctx, `INSERT INTO order_activities (order_id, type, message, meta)
		VALUES ($1,'ORDER_CREATED',$2,$3)`, created.ID,
		"New order placed", meta); err != nil {
		return nil, err
	}

	if userID != nil {
		if err := linkUserLead(ctx, tx, *userID, leadID); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &created, nil
}

// linkUserLead attaches the user to the order's lead when no other account
// owns it. orders.user_id is the primary ownership link; this keeps CRM
// filtering (users.lead_id) useful without ever stealing another account.
func linkUserLead(ctx context.Context, tx pgx.Tx, userID, leadID int64) error {
	_, err := tx.Exec(ctx, `UPDATE users u SET lead_id = $1
		WHERE u.id = $2 AND u.lead_id IS NULL AND u.deleted_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM users x WHERE x.lead_id = $1 AND x.id <> $2)`,
		leadID, userID)
	return err
}
