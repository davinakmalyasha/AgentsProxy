// The tool registry: every tool the agent can call. Each tool runs
// against the tenant's data through WithTenantTx (RLS) — the SAME
// path the REST API uses, so double-booking, stock guards, and tenant
// isolation are inherited.
//
// DONE: Phase 5 (telegram agentic) / STANDARDS A, D, F
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/llm"

	"github.com/davinakmalyasha/agents-proxy/internal/features/clients/sports/pricing"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
)

// Tool is one agent capability.
type Tool struct {
	Name        string
	Description string
	// Parameters is a JSON Schema for the tool's arguments. It travels with
	// the declaration to the provider, so the model has a real contract
	// instead of an English sentence in Description, and the agent can
	// reject a bad call before it reaches SQL. nil is treated as "no
	// arguments".
	Parameters json.RawMessage
	Run        func(ctx context.Context, tenantID string, args json.RawMessage) string
	// Roles that may call this tool. Empty = all (legacy). Used for isolation.
	Roles []string
	// Keywords the router matches against, lowercased. Kept separate from
	// Description so routing does not have to parse prose, and so the words
	// a user actually types can be curated independently of the wording
	// shown to the model.
	Keywords []string
	// StrongKeywords are decisive: a hit outweighs every ordinary keyword
	// a competing tool can accumulate. Use for terms with one meaning in
	// this domain ("laba", "kosong", "excel").
	StrongKeywords []string
}

// Defs returns the role-filtered tool set as provider-ready declarations,
// sorted for a stable request body.
func (ts *ToolSet) Defs() []llm.ToolDef {
	names := make([]string, 0, len(ts.tools))
	for n := range ts.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]llm.ToolDef, 0, len(names))
	for _, n := range names {
		t := ts.tools[n]
		params := t.Parameters
		if len(params) == 0 {
			params = noArgs()
		}
		out = append(out, llm.ToolDef{
			Type: "function",
			Function: llm.FunctionSpec{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}
	return out
}

// Names returns the available tool names, sorted.
func (ts *ToolSet) Names() []string {
	names := make([]string, 0, len(ts.tools))
	for n := range ts.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ToolSet holds the tools for one agent instance.
type ToolSet struct {
	tools map[string]Tool
	Pool  *pgxpool.Pool
}

// NewToolSet builds the standard tool set for the sports vertical (owner = all tools).
func NewToolSet(pool *pgxpool.Pool) *ToolSet {
	ts := &ToolSet{tools: map[string]Tool{}, Pool: pool}
	ts.add(bookSlotTool(pool))
	ts.add(cancelBookingTool(pool))
	ts.add(listBookingsTool(pool))
	ts.add(queryIncomeTool(pool))
	ts.add(queryAttendanceTool(pool))
	ts.add(checkStockTool(pool))
	ts.add(adjustStockTool(pool))
	ts.add(topupMemberTool(pool))
	ts.add(recordStaffPaymentTool(pool))
	ts.add(listStaffPaymentsTool(pool))
	ts.add(setReminderTool(pool))
	// discovery tools for client landing (pricing + availability)
	ts.add(getPricingTool(pool))
	ts.add(getAvailabilityTool(pool))
	// Phase 7 money tools (ported from Centrum's Gemini assistant).
	ts.add(queryNetProfitTool(pool))
	ts.add(queryRangeSummaryTool(pool))
	ts.add(topCustomersTool(pool))
	ts.add(closingReportTool(pool))
	ts.add(queryKantinSummaryTool(pool))
	// WS2: owner-only Excel download (NOT in pegawai/client allowlists below)
	ts.add(exportReportExcelTool(pool))
	return ts
}

// NewToolSetFor returns a filtered view for the given role: anon, client,
// pegawai, owner. Single shared LLM key (AgentsProxy/.env LLM_API_KEY) is
// reused, isolation is by allowlist — no role sees a tool it was not
// granted, even if the model guesses the name.
//
// "anon" exists because the public landing chat has no credential but
// still runs tools against a caller-supplied tenant; "client" is the
// authenticated API/agent tier and keeps its data tools.
func NewToolSetFor(pool *pgxpool.Pool, role string) *ToolSet {
	full := NewToolSet(pool)
	if role == "owner" {
		return full // all
	}
	allowed := allowedToolsFor(role)
	filtered := &ToolSet{tools: map[string]Tool{}, Pool: pool}
	for name, t := range full.tools {
		if allowed[name] {
			filtered.tools[name] = t
		}
	}
	return filtered
}

func allowedToolsFor(role string) map[string]bool {
	switch role {
	case "anon":
		// PUBLIC LANDING WIDGET — no credential of any kind.
		//
		// /v1/agent/client-chat accepts a caller-supplied tenant_id, so
		// this tier must not be able to read tenant data at all: the
		// caller chooses which tenant the tools run against. Anything
		// that returns rows is excluded, which is why list_bookings
		// (customer names, times, statuses) and query_attendance (visit
		// counts) are NOT here even though the "client" tier has them.
		//
		// What a public FAQ widget actually needs: can I book, what does
		// it cost, and when is it free.
		return map[string]bool{
			"get_pricing":      true,
			"get_availability": true,
		}
	case "client":
		// Authenticated API/agent integration (X-Client-Token). RLS
		// scopes these to the token's own tenant, so listing that
		// tenant's bookings is legitimate here. This tier is NOT usable
		// for a public endpoint that lets the caller pick the tenant.
		return map[string]bool{
			"check_stock":      true,
			"query_attendance": true,
			"list_bookings":    true,
			"get_pricing":      true,
			"get_availability": true,
		}
	case "pegawai":
		return map[string]bool{
			"book_slot":           true,
			"cancel_booking":      true,
			"list_bookings":       true,
			"query_income":        true,
			"query_attendance":    true,
			"check_stock":         true,
			"adjust_stock":        true,
			"set_reminder":        true,
			"closing_report":      true, // daily only
			"list_staff_payments": true,
			"get_pricing":         true,
			"get_availability":    true,
		}
	case "owner":
		return nil // handled above
	default:
		return map[string]bool{}
	}
}

func (ts *ToolSet) add(t Tool) { ts.tools[t.Name] = applyToolMeta(t) }

// Get returns a tool by name.
func (ts *ToolSet) Get(name string) (Tool, bool) {
	t, ok := ts.tools[name]
	return t, ok
}

// Definitions returns the tool list for the system prompt.
func (ts *ToolSet) Definitions() []string {
	// Sort for a stable prompt (map order is random per run).
	names := make([]string, 0, len(ts.tools))
	for n := range ts.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	defs := make([]string, 0, len(ts.tools))
	for _, n := range names {
		defs = append(defs, fmt.Sprintf("- %s: %s", ts.tools[n].Name, ts.tools[n].Description))
	}
	return defs
}

// ---- tools ----

func errMsg(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

func bookSlotTool(pool *pgxpool.Pool) Tool {
	return Tool{
		Name: "book_slot",
		Description: "Book a facility slot. Args: {facility_id, date (YYYY-MM-DD), start_time (HH:MM), " +
			"end_time (HH:MM), customer_name, customer_phone?}. The price is computed from the tenant's " +
			"pricing rule and cannot be set here. Returns booking id or an error like 'slot already booked'.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				FacilityID    string `json:"facility_id"`
				Date          string `json:"date"`
				StartTime     string `json:"start_time"`
				EndTime       string `json:"end_time"`
				CustomerName  string `json:"customer_name"`
				CustomerPhone string `json:"customer_phone"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return "error: invalid args: " + err.Error()
			}
			if strings.TrimSpace(p.CustomerName) == "" {
				return "error: customer_name required"
			}
			// Resolve facility name (or report which facilities exist).
			var facName, openStr, closeStr string
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				err := tx.QueryRow(ctx,
					`SELECT name, open_time::text, close_time::text FROM sports_facilities
					  WHERE id = $1::uuid AND tenant_id = $2 AND status='active'`,
					p.FacilityID, tenantID).Scan(&facName, &openStr, &closeStr)
				if err != nil {
					return errors.New("unknown facility")
				}
				// Validate time range quickly. Times may be HH:MM or
				// HH:MM:SS (Postgres and LLMs both); normalize to HH:MM.
				norm := func(s string) string {
					parts := strings.Split(strings.TrimSpace(s), ":")
					if len(parts) >= 2 {
						return parts[0] + ":" + parts[1]
					}
					return s
				}
				start, e1 := time.Parse("15:04", norm(p.StartTime))
				end, e2 := time.Parse("15:04", norm(p.EndTime))
				if e1 != nil || e2 != nil {
					return errors.New("start_time/end_time must be HH:MM")
				}
				if !end.After(start) {
					return errors.New("end_time must be after start_time")
				}
				open, e3 := time.Parse("15:04", norm(openStr))
				closeT, e4 := time.Parse("15:04", norm(closeStr))
				if e3 != nil || e4 != nil {
					return errors.New("facility hours misconfigured")
				}
				if start.Before(open) || end.After(closeT) {
					return fmt.Errorf("outside facility hours %s-%s", openStr, closeStr)
				}
				date, err := time.Parse("2006-01-02", p.Date)
				if err != nil {
					return errors.New("date must be YYYY-MM-DD")
				}
				status := "confirmed"
				// Price comes from the tenant's pricing rule, never from
				// the model.
				//
				// This used to INSERT the model's `price` argument
				// verbatim. When the model omitted it -- which it often
				// did, since the argument is optional in the schema -- the
				// booking committed at Rp0. And an Rp0 booking can never
				// be paid: the recalc trigger treats a 0 price as unpaid
				// forever, and the payments handler then computes
				// outstanding = 0 - 0 = 0 and refuses every payment with
				// "already paid". One bad tool call produced a booking that
				// was permanently uncollectable.
				//
				// So compute it exactly as the HTTP create path does, and
				// enforce the same opening window.
				price, err := pricing.PriceFor(ctx, tx, tenantID, date, start, end)
				if err != nil {
					if errors.Is(err, pricing.ErrNoRule) {
						return errors.New("no pricing rule covers that date — ask the owner to set prices first")
					}
					return err
				}
				rule, err := pricing.RuleFor(ctx, tx, tenantID, date)
				if err != nil {
					return err
				}
				ruleOpen, e1 := time.Parse("15:04", norm(rule.OpenTime))
				ruleClose, e2 := time.Parse("15:04", norm(rule.CloseTime))
				if e1 != nil || e2 != nil {
					return errors.New("pricing rule hours misconfigured")
				}
				if start.Before(ruleOpen) || end.After(ruleClose) {
					return fmt.Errorf("outside opening hours %s-%s", rule.OpenTime, rule.CloseTime)
				}
				_, err = tx.Exec(ctx,
					`INSERT INTO sports_bookings (tenant_id, facility_id, date, start_time, end_time, customer_name, customer_phone, status, price)
					 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
					tenantID, p.FacilityID, date, start, end, p.CustomerName, p.CustomerPhone, status, price)
				if err != nil {
					return err
				}
				return nil
			})
			if err != nil {
				// Overlap with another active booking → the friendly
				// message the tool Description promises (not raw PG text).
				if strings.Contains(err.Error(), "exclusion") {
					return "error: slot already booked"
				}
				return "error: " + err.Error()
			}
			return fmt.Sprintf("booked %s on %s %s-%s for %s", facName, p.Date, p.StartTime, p.EndTime, p.CustomerName)
		},
	}
}

func cancelBookingTool(pool *pgxpool.Pool) Tool {
	return Tool{
		Name:        "cancel_booking",
		Description: "Cancel a booking by id. Args: {booking_id}. Returns cancelled.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				BookingID string `json:"booking_id"`
			}
			if err := json.Unmarshal(args, &p); err != nil || p.BookingID == "" {
				return "error: booking_id required"
			}
			// Cancellation is only meaningful for upcoming/active
			// bookings — completed history must stay immutable.
			var status string
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx,
					`SELECT status FROM sports_bookings
					  WHERE id=$1::uuid AND tenant_id=$2`,
					p.BookingID, tenantID).Scan(&status)
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return "error: no booking found with that id"
				}
				return "error: " + err.Error()
			}
			if status == "cancelled" {
				return "error: booking already cancelled"
			}
			if status == "completed" || status == "no_show" {
				return "error: booking already " + status + " — cannot cancel"
			}
			err = records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx,
					`UPDATE sports_bookings SET status='cancelled', updated_at=now()
					  WHERE id=$1::uuid AND tenant_id=$2 AND status != 'cancelled'`,
					p.BookingID, tenantID)
				return err
			})
			if err != nil {
				return "error: " + err.Error()
			}
			return "booking cancelled"
		},
	}
}

func listBookingsTool(pool *pgxpool.Pool) Tool {
	return Tool{
		Name:        "list_bookings",
		Description: "List bookings. Args: {date? (YYYY-MM-DD), facility_id?}. Returns bookings with time, customer, facility.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				Date       string `json:"date"`
				FacilityID string `json:"facility_id"`
			}
			_ = json.Unmarshal(args, &p) // all optional
			var rows []string
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				sql := `SELECT f.name, b.date::text, b.start_time::text, b.end_time::text, b.customer_name, b.status
				          FROM sports_bookings b JOIN sports_facilities f ON f.id = b.facility_id
				         WHERE b.tenant_id = $1`
				qargs := []any{tenantID}
				idx := 2
				if p.Date != "" {
					sql += fmt.Sprintf(" AND b.date = $%d::date", idx)
					qargs = append(qargs, p.Date)
					idx++
				}
				if p.FacilityID != "" {
					sql += fmt.Sprintf(" AND b.facility_id = $%d::uuid", idx)
					qargs = append(qargs, p.FacilityID)
					idx++
				}
				sql += " ORDER BY b.date, b.start_time LIMIT 20"
				r, err := tx.Query(ctx, sql, qargs...)
				if err != nil {
					return err
				}
				defer r.Close()
				for r.Next() {
					var name, date, st, et, cust, status string
					if err := r.Scan(&name, &date, &st, &et, &cust, &status); err != nil {
						return err
					}
					rows = append(rows, fmt.Sprintf("%s %s %s-%s %s (%s)", date, name, st, et, cust, status))
				}
				return r.Err()
			})
			if err != nil {
				return "error: " + err.Error()
			}
			if len(rows) == 0 {
				return "no bookings found"
			}
			return strings.Join(rows, "\n")
		},
	}
}

func queryIncomeTool(pool *pgxpool.Pool) Tool {
	return Tool{
		Name: "query_income",
		Description: "Income for a period: money actually received from payments plus canteen, " +
			"with the committed booking value reported alongside. " +
			"Args: {from? (YYYY-MM-DD), to? (YYYY-MM-DD), month? (YYYY-MM)}. Defaults to this month.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				From  string `json:"from"`
				To    string `json:"to"`
				Month string `json:"month"`
			}
			_ = json.Unmarshal(args, &p)
			from, to := p.From, p.To
			if from == "" && p.Month != "" {
				from = p.Month + "-01"
				t, _ := time.Parse("2006-01-02", from)
				to = t.AddDate(0, 1, 0).Format("2006-01-02")
			}
			if from == "" {
				now := wibNow()
				from = now.Format("2006-01") + "-01"
				to = now.AddDate(0, 1, 0).Format("2006-01-02")
			}
			var b basis
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				var err error
				b, err = loadBasis(ctx, tx, tenantID, from, to)
				return err
			})
			if err != nil {
				return "error: " + err.Error()
			}
			// "Income" is money in the drawer, not the value of slots that
			// were reserved. This used to sum booking prices, so a fully
			// unpaid month reported income while the Laporan screen the
			// owner reads showed zero. Both figures are returned now, with
			// the received one first.
			return fmt.Sprintf("income %s (%s to %s), %d payments",
				b.describe(), from, to, b.Count)
		},
	}
}

func queryAttendanceTool(pool *pgxpool.Pool) Tool {
	return Tool{
		Name:        "query_attendance",
		Description: "Count people who booked. Args: {date? (YYYY-MM-DD)}. Defaults to today. Returns count.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				Date string `json:"date"`
			}
			_ = json.Unmarshal(args, &p)
			if p.Date == "" {
				p.Date = wibNow().Format("2006-01-02")
			}
			var n int
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx,
					`SELECT COUNT(*) FROM sports_bookings
					  WHERE tenant_id=$1 AND date=$2::date AND status IN ('confirmed','checked_in')`,
					tenantID, p.Date).Scan(&n)
			})
			if err != nil {
				return "error: " + err.Error()
			}
			return fmt.Sprintf("%d people booked on %s", n, p.Date)
		},
	}
}

func checkStockTool(pool *pgxpool.Pool) Tool {
	return Tool{
		Name:        "check_stock",
		Description: "List inventory items with quantities. Args: {} (optional {name} to filter).",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(args, &p)
			var rows []string
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				sql := `SELECT name, quantity, unit_price, low_stock_threshold FROM sports_inventory WHERE tenant_id=$1`
				qargs := []any{tenantID}
				if p.Name != "" {
					sql += " AND name ILIKE $2"
					qargs = append(qargs, "%"+p.Name+"%")
				}
				sql += " ORDER BY name LIMIT 50"
				r, err := tx.Query(ctx, sql, qargs...)
				if err != nil {
					return err
				}
				defer r.Close()
				for r.Next() {
					var name string
					var qty int
					var price, thr float64
					if err := r.Scan(&name, &qty, &price, &thr); err != nil {
						return err
					}
					flag := ""
					if float64(qty) <= thr {
						flag = " (LOW)"
					}
					rows = append(rows, fmt.Sprintf("%s: %d%s @ %.2f", name, qty, flag, price))
				}
				return r.Err()
			})
			if err != nil {
				return "error: " + err.Error()
			}
			if len(rows) == 0 {
				return "no inventory"
			}
			return strings.Join(rows, "\n")
		},
	}
}

func adjustStockTool(pool *pgxpool.Pool) Tool {
	return Tool{
		Name:        "adjust_stock",
		Description: "Adjust stock by delta (positive=restock, negative=sale). Args: {item_id, delta}. Returns new quantity.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				ItemID string `json:"item_id"`
				Delta  int    `json:"delta"`
			}
			if err := json.Unmarshal(args, &p); err != nil || p.ItemID == "" {
				return "error: item_id and delta required"
			}
			var newQty int
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx,
					`UPDATE sports_inventory SET quantity = quantity + $1, updated_at=now()
					  WHERE id=$2::uuid AND tenant_id=$3 AND quantity + $1 >= 0
					  RETURNING quantity`,
					p.Delta, p.ItemID, tenantID).Scan(&newQty)
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return "error: insufficient stock or item not found"
				}
				return "error: " + err.Error()
			}
			return fmt.Sprintf("new quantity: %d", newQty)
		},
	}
}

func topupMemberTool(pool *pgxpool.Pool) Tool {
	return Tool{
		Name:        "topup_member",
		Description: "Add balance to a member. Args: {member_id, amount}. Returns new balance.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				MemberID string  `json:"member_id"`
				Amount   float64 `json:"amount"`
			}
			if err := json.Unmarshal(args, &p); err != nil || p.MemberID == "" {
				return "error: member_id and amount required"
			}
			if p.Amount <= 0 {
				return "error: amount must be > 0"
			}
			var bal float64
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx,
					`UPDATE sports_members SET balance = balance + $1, updated_at=now()
					  WHERE id=$2::uuid AND tenant_id=$3 RETURNING balance`,
					p.Amount, p.MemberID, tenantID).Scan(&bal)
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return "error: member not found"
				}
				return "error: " + err.Error()
			}
			return fmt.Sprintf("new balance: %.2f", bal)
		},
	}
}

func recordStaffPaymentTool(pool *pgxpool.Pool) Tool {
	return Tool{
		Name:        "record_staff_payment",
		Description: "Record a staff payment in the ledger (no real money moved). Args: {staff_name, amount, paid_at? (YYYY-MM-DD, default today), note?}.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				StaffName string  `json:"staff_name"`
				Amount    float64 `json:"amount"`
				PaidAt    string  `json:"paid_at"`
				Note      string  `json:"note"`
			}
			if err := json.Unmarshal(args, &p); err != nil || strings.TrimSpace(p.StaffName) == "" {
				return "error: staff_name and amount required"
			}
			if p.Amount <= 0 {
				return "error: amount must be > 0"
			}
			if p.PaidAt == "" {
				p.PaidAt = wibNow().Format("2006-01-02")
			}
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx,
					`INSERT INTO agent_staff_payments (tenant_id, staff_name, amount, paid_at, note) VALUES ($1,$2,$3,$4::date,$5)`,
					tenantID, p.StaffName, p.Amount, p.PaidAt, p.Note)
				return err
			})
			if err != nil {
				return "error: " + err.Error()
			}
			return fmt.Sprintf("recorded payment %s %.2f on %s", p.StaffName, p.Amount, p.PaidAt)
		},
	}
}

func listStaffPaymentsTool(pool *pgxpool.Pool) Tool {
	return Tool{
		Name:        "list_staff_payments",
		Description: "List staff payment ledger. Args: {staff_name?, from? (YYYY-MM-DD), to? (YYYY-MM-DD)}.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				StaffName string `json:"staff_name"`
				From      string `json:"from"`
				To        string `json:"to"`
			}
			_ = json.Unmarshal(args, &p)
			var rows []string
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				sql := `SELECT staff_name, amount, paid_at::text, note FROM agent_staff_payments WHERE tenant_id=$1`
				qargs := []any{tenantID}
				idx := 2
				if p.StaffName != "" {
					sql += fmt.Sprintf(" AND staff_name ILIKE $%d", idx)
					qargs = append(qargs, "%"+p.StaffName+"%")
					idx++
				}
				if p.From != "" {
					sql += fmt.Sprintf(" AND paid_at >= $%d::date", idx)
					qargs = append(qargs, p.From)
					idx++
				}
				if p.To != "" {
					sql += fmt.Sprintf(" AND paid_at <= $%d::date", idx)
					qargs = append(qargs, p.To)
					idx++
				}
				sql += " ORDER BY paid_at DESC LIMIT 20"
				r, err := tx.Query(ctx, sql, qargs...)
				if err != nil {
					return err
				}
				defer r.Close()
				for r.Next() {
					var name, paidAt, note string
					var amt float64
					if err := r.Scan(&name, &amt, &paidAt, &note); err != nil {
						return err
					}
					rows = append(rows, fmt.Sprintf("%s %s %.2f %s", paidAt, name, amt, note))
				}
				return r.Err()
			})
			if err != nil {
				return "error: " + err.Error()
			}
			if len(rows) == 0 {
				return "no staff payments found"
			}
			return strings.Join(rows, "\n")
		},
	}
}

func setReminderTool(pool *pgxpool.Pool) Tool {
	return Tool{
		Name:        "set_reminder",
		Description: "Set a reminder delivered via Telegram. Args: {text, remind_at (YYYY-MM-DD HH:MM WIB)}. Returns the reminder id. Only works inside a Telegram chat.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				Text     string `json:"text"`
				RemindAt string `json:"remind_at"`
			}
			if err := json.Unmarshal(args, &p); err != nil || strings.TrimSpace(p.Text) == "" {
				return "error: text and remind_at required"
			}
			text := strings.TrimSpace(p.Text)
			if len(text) > 500 {
				return "error: text too long (max 500 chars)"
			}
			// Delivery is Telegram-only: without the sender's chat there
			// is nowhere to send. Never store undeliverable rows.
			chatID := ChatIDFromCtx(ctx)
			if chatID == "" {
				return "error: reminders can only be set from Telegram chat — I don't know where to send this"
			}
			when, err := time.ParseInLocation("2006-01-02 15:04", strings.TrimSpace(p.RemindAt), wibLoc())
			if err != nil {
				return "error: remind_at must be YYYY-MM-DD HH:MM"
			}
			if when.Before(wibNow().Add(-time.Minute)) {
				return "error: remind_at is in the past"
			}
			if when.After(wibNow().AddDate(1, 0, 0)) {
				return "error: remind_at too far ahead (max 1 year)"
			}
			var id string
			botID := BotIDFromCtx(ctx)
			err = records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				if botID == "" {
					return tx.QueryRow(ctx,
						`INSERT INTO agent_reminders (tenant_id, chat_id, text, remind_at) VALUES ($1,$2,$3,$4) RETURNING id::text`,
						tenantID, chatID, text, when).Scan(&id)
				}
				return tx.QueryRow(ctx,
					`INSERT INTO agent_reminders (tenant_id, bot_id, chat_id, text, remind_at) VALUES ($1,$2::uuid,$3,$4,$5) RETURNING id::text`,
					tenantID, botID, chatID, text, when).Scan(&id)
			})
			if err != nil {
				return "error: " + err.Error()
			}
			return fmt.Sprintf("reminder set for %s (id %s): %s", p.RemindAt, id, text)
		},
	}
}

func getPricingTool(pool *pgxpool.Pool) Tool {
	return Tool{
		Name:        "get_pricing",
		Description: "Get pricing for a date. Args: {date? (YYYY-MM-DD, default today)}. Returns open/close, day/evening price.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				Date string `json:"date"`
			}
			_ = json.Unmarshal(args, &p)
			if p.Date == "" {
				p.Date = wibNow().Format("2006-01-02")
			}
			var open, close, dayStart, dayEnd string
			var priceDay, priceEvening float64
			var validFrom string
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx,
					`SELECT valid_from::text, open_time::text, close_time::text, day_start::text, day_end::text, price_day, price_evening
					   FROM sports_pricing_rules WHERE tenant_id=$1 AND is_active AND valid_from <= $2::date ORDER BY valid_from DESC LIMIT 1`,
					tenantID, p.Date).Scan(&validFrom, &open, &close, &dayStart, &dayEnd, &priceDay, &priceEvening)
			})
			if err != nil {
				// Only a genuine "no active rule" is a no-pricing
				// outcome — a DB outage must surface as an error, not
				// a silent "no pricing" that the LLM would relay as fact.
				if errors.Is(err, pgx.ErrNoRows) {
					return "no pricing found for " + p.Date
				}
				return "error: " + err.Error()
			}
			return fmt.Sprintf("pricing %s: %s-%s day %s-%s %.0f/%.0f (valid from %s)", p.Date, open, close, dayStart, dayEnd, priceDay, priceEvening, validFrom)
		},
	}
}

func getAvailabilityTool(pool *pgxpool.Pool) Tool {
	return Tool{
		Name:        "get_availability",
		Description: "Get busy slots for a date. Args: {date (YYYY-MM-DD), type? (facility type, default badminton)}. Returns facilities with busy intervals.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				Date string `json:"date"`
				Type string `json:"type"`
			}
			if err := json.Unmarshal(args, &p); err != nil || p.Date == "" {
				return "error: date required YYYY-MM-DD"
			}
			if _, err := time.Parse("2006-01-02", p.Date); err != nil {
				return "error: date must be YYYY-MM-DD"
			}
			typ := strings.ToLower(strings.TrimSpace(p.Type))
			if typ == "" {
				typ = "badminton"
			}
			var out []string
			err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				// One grouped query instead of a per-facility round-trip:
				// facilities LEFT JOIN that day's active bookings, then
				// aggregate busy windows per facility. Times are cast to
				// text and truncated to HH:MM in SQL so no Go parsing.
				rows, err := tx.Query(ctx,
					`SELECT f.name,
					        COALESCE(string_agg(to_char(b.start_time,'HH24:MI') || '-' || to_char(b.end_time,'HH24:MI'), ', ' ORDER BY b.start_time), '')
					   FROM sports_facilities f
					   LEFT JOIN sports_bookings b
					          ON b.facility_id = f.id AND b.tenant_id = f.tenant_id
					         AND b.date = $3::date AND b.status NOT IN ('cancelled','no_show')
					  WHERE f.tenant_id=$1 AND f.status='active' AND f.type=$2
					  GROUP BY f.id, f.name
					  ORDER BY f.name`,
					tenantID, typ, p.Date)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var name, busyAgg string
					if err := rows.Scan(&name, &busyAgg); err != nil {
						return err
					}
					if busyAgg == "" {
						out = append(out, name+": kosong semua")
						continue
					}
					out = append(out, name+": "+busyAgg)
				}
				return rows.Err()
			})
			if err != nil {
				return "error: " + err.Error()
			}
			if len(out) == 0 {
				return "no active facilities of type " + typ
			}
			return strings.Join(out, "\n")
		},
	}
}
