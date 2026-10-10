// Chat-generated Excel revenue reports (owner-only).
//
// Flow: owner asks in the app → export_report_excel Tool builds a .xlsx
// under durable report storage → mints a 32-byte download token in Redis
// (30 min TTL, tenant-scoped) → returns an
// "XLSX:/v1/reports/{token}.xlsx" marker the LLM presents as a link.
//
// DONE: WS2 (xlsx via chat)
package agentstools

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/xuri/excelize/v2"

	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
)

// ReportRDB stores download tokens. Set once in main.go wiring;
// nil in tests (Tool then reports unavailable).
var ReportRDB *redis.Client

const (
	reportTTL         = 30 * time.Minute
	reportTokenPrefix = "rpt:"
	MaxReportDays     = 370
)

// ReportsDir is where chat-generated .xlsx files live, tenant
// subdirectories beneath it. Exported so the download sweeper cleans
// the same directory the tools write: one path, two users.
const ReportsDir = "data/reports"

var (
	hexTokenRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
	tenantRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	DateRe     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// reportTicket binds a download token to one tenant + one object key.
type reportTicket struct {
	Tenant string `json:"tenant"`
	File   string `json:"file"` // opaque object key in the active ReportStorage
}

// ---- marker (agent reply â†” channels) ----

// XLSXMarkerPrefix starts the download-marker line the Tool emits.
const XLSXMarkerPrefix = "XLSX:"

// ParseXLSXMarker extracts the download path from an agent reply.
// Returns ("", reply) when the reply carries no marker.
func ParseXLSXMarker(reply string) (path, rest string) {
	rest = reply
	for _, line := range strings.Split(reply, "\n") {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), XLSXMarkerPrefix); ok && strings.HasPrefix(p, "/v1/reports/") {
			return strings.TrimSpace(p), strings.TrimSpace(strings.Replace(reply, line, "", 1))
		}
	}
	return "", reply
}

// lookupTicket validates a token and returns its ticket.
func lookupTicket(ctx context.Context, token string) (reportTicket, error) {
	var t reportTicket
	token = strings.TrimSuffix(token, ".xlsx")
	if !hexTokenRe.MatchString(token) {
		return t, fmt.Errorf("bad token")
	}
	if ReportRDB == nil {
		return t, fmt.Errorf("report store unavailable")
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	raw, err := ReportRDB.Get(rctx, reportTokenPrefix+token).Result()
	if err != nil {
		return t, fmt.Errorf("expired or unknown link")
	}
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return t, fmt.Errorf("bad ticket")
	}
	if !tenantRe.MatchString(t.Tenant) {
		return t, fmt.Errorf("bad ticket")
	}
	return t, nil
}

// LoadReport validates a token and returns the download filename + bytes
// from the active ReportStorage backend.
func LoadReport(ctx context.Context, token string) (filename string, data []byte, err error) {
	t, err := lookupTicket(ctx, token)
	if err != nil {
		return "", nil, err
	}
	// Never trust the ticket blindly: the store confines keys to its own
	// tenant/object layout.
	lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return ReportStorage.Load(lctx, t.Tenant, t.File)
}

// ReportTenant validates a token and returns its tenant, without loading
// the object. The download handler uses it for the quarantine fence.
func ReportTenant(ctx context.Context, token string) (string, error) {
	t, err := lookupTicket(ctx, token)
	if err != nil {
		return "", err
	}
	return t.Tenant, nil
}

// ResolveTicket validates a token once and returns the tenant plus the
// download payload. The handler uses this instead of ReportTenant +
// LoadReport so one download costs one ticket lookup, not two.
func ResolveTicket(ctx context.Context, token string) (tenant, filename string, data []byte, err error) {
	t, err := lookupTicket(ctx, token)
	if err != nil {
		return "", "", nil, err
	}
	lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	filename, data, err = ReportStorage.Load(lctx, t.Tenant, t.File)
	if err != nil {
		return "", "", nil, err
	}
	return t.Tenant, filename, data, nil
}

// ---- Tool ----

type dayRow struct {
	Date     string
	Cash     float64
	Qris     float64
	Bookings int
}

type expenseRow struct {
	Date     string
	Category string
	Note     string
	Amount   float64
}

type kantinDay struct {
	Date  string
	Omzet float64
	Modal float64
	Qty   int64
}

type topRow struct {
	Name string
	Qty  int64
}

type reportData struct {
	Days        []dayRow
	Expenses    []expenseRow
	KantinDays  []kantinDay
	KantinTop   []topRow
	Cash        float64
	Qris        float64
	Bookings    int
	ExpensesTot float64
	KantinOmzet float64
	KantinModal float64
}

func FetchReportData(ctx context.Context, pool *pgxpool.Pool, tenantID, from, to string) (reportData, error) {
	var d reportData
	// Build into a local and assign once at the end: WithTenantTx
	// retries the closure on deadlock, and accumulating (+=) into the
	// outer struct would double-count every total on a retry.
	err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
		var local reportData
		// A 370-day export legitimately outlives the pool-wide
		// statement_timeout: raise it for this transaction only.
		if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '120s'`); err != nil {
			return err
		}
		// Lapangan income per day (WIB calendar day, same convention as laporan page).
		r, err := tx.Query(ctx,
			`SELECT (paid_at AT TIME ZONE 'Asia/Jakarta')::date AS d,
			        COALESCE(SUM(amount) FILTER (WHERE method='cash'),0),
			        COALESCE(SUM(amount) FILTER (WHERE method='qris'),0),
			        COUNT(*)
			   FROM sports_payments
			  WHERE tenant_id=$1 AND deleted_at IS NULL AND (paid_at AT TIME ZONE 'Asia/Jakarta')::date >= $2::date
			    AND (paid_at AT TIME ZONE 'Asia/Jakarta')::date < $3::date
			  GROUP BY 1 ORDER BY 1`,
			tenantID, from, to)
		if err != nil {
			return err
		}
		for r.Next() {
			var row dayRow
			var dt time.Time
			if err := r.Scan(&dt, &row.Cash, &row.Qris, &row.Bookings); err != nil {
				r.Close()
				return err
			}
			row.Date = dt.Format("2006-01-02")
			local.Days = append(local.Days, row)
			local.Cash += row.Cash
			local.Qris += row.Qris
			local.Bookings += row.Bookings
		}
		r.Close()
		if err := r.Err(); err != nil {
			return err
		}
		// Expenses detail.
		r, err = tx.Query(ctx,
			`SELECT spent_at, category, note, amount FROM sports_expenses
			  WHERE tenant_id=$1 AND deleted_at IS NULL AND spent_at >= $2::date AND spent_at < $3::date
			  ORDER BY spent_at`,
			tenantID, from, to)
		if err != nil {
			return err
		}
		for r.Next() {
			var row expenseRow
			var dt time.Time
			if err := r.Scan(&dt, &row.Category, &row.Note, &row.Amount); err != nil {
				r.Close()
				return err
			}
			row.Date = dt.Format("2006-01-02")
			local.Expenses = append(local.Expenses, row)
			local.ExpensesTot += row.Amount
		}
		r.Close()
		if err := r.Err(); err != nil {
			return err
		}
		// Kantin per day (correct COGS from sale_items, unlike the UI's 0).
		r, err = tx.Query(ctx,
			`SELECT (s.created_at AT TIME ZONE 'Asia/Jakarta')::date AS d,
			        COALESCE(SUM(i.unit_price*i.qty),0),
			        COALESCE(SUM(i.unit_cost*i.qty),0),
			        COALESCE(SUM(i.qty),0)
			   FROM sports_kantin_sale_items i
			   JOIN sports_kantin_sales s ON s.id = i.sale_id
			  WHERE i.tenant_id=$1 AND s.voided_at IS NULL
			    AND (s.created_at AT TIME ZONE 'Asia/Jakarta')::date >= $2::date
			    AND (s.created_at AT TIME ZONE 'Asia/Jakarta')::date < $3::date
			  GROUP BY 1 ORDER BY 1`,
			tenantID, from, to)
		if err != nil {
			return err
		}
		for r.Next() {
			var row kantinDay
			var dt time.Time
			if err := r.Scan(&dt, &row.Omzet, &row.Modal, &row.Qty); err != nil {
				r.Close()
				return err
			}
			row.Date = dt.Format("2006-01-02")
			local.KantinDays = append(local.KantinDays, row)
			local.KantinOmzet += row.Omzet
			local.KantinModal += row.Modal
		}
		r.Close()
		if err := r.Err(); err != nil {
			return err
		}
		// Kantin top products.
		r, err = tx.Query(ctx,
			`SELECT i.product_name, SUM(i.qty) FROM sports_kantin_sale_items i
			   JOIN sports_kantin_sales s ON s.id = i.sale_id
			  WHERE i.tenant_id=$1 AND s.voided_at IS NULL
			    AND (s.created_at AT TIME ZONE 'Asia/Jakarta')::date >= $2::date
			    AND (s.created_at AT TIME ZONE 'Asia/Jakarta')::date < $3::date
			  GROUP BY 1 ORDER BY 2 DESC LIMIT 10`,
			tenantID, from, to)
		if err != nil {
			return err
		}
		for r.Next() {
			var row topRow
			if err := r.Scan(&row.Name, &row.Qty); err != nil {
				r.Close()
				return err
			}
			local.KantinTop = append(local.KantinTop, row)
		}
		r.Close()
		if err := r.Err(); err != nil {
			return err
		}
		d = local
		return nil
	})
	return d, err
}

// ---- workbook ----

func Idr(v float64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	// Single pass, no Sprintf, no intermediate string: append the
	// digits reversed with separators, then reverse in place.
	var buf []byte
	n := int64(v + 0.5)
	if n == 0 {
		buf = append(buf, '0')
	}
	digits := 0
	for n > 0 {
		if digits > 0 && digits%3 == 0 {
			buf = append(buf, '.')
		}
		buf = append(buf, byte('0'+n%10))
		n /= 10
		digits++
	}
	if neg {
		buf = append(buf, '-')
	}
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}

// rp renders an IDR amount with the sign in front: -Rp4.405.000, Rp95.000.
func Rp(v float64) string {
	if v < 0 {
		return "-Rp" + Idr(-v)
	}
	return "Rp" + Idr(v)
}

// safeCell neutralizes spreadsheet-formula injection: user-controlled
// text starting with =,+,-,@ (after whitespace) is prefixed with a
// literal quote so Excel/Sheets render it as text, never execute it.
func safeCell(s string) string {
	t := strings.TrimLeft(s, " \t\r\n")
	if t == "" {
		return s
	}
	switch t[0] {
	case '=', '+', '-', '@', '|', '%':
		return "'" + s
	}
	return s
}

func headerStyle(f *excelize.File) int {
	id, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill: excelize.Fill{Type: "pattern", Color: []string{"1E3A5F"}, Pattern: 1},
	})
	return id
}

func moneyStyle(f *excelize.File) int {
	id, _ := f.NewStyle(&excelize.Style{NumFmt: 3}) // #,##0
	return id
}

func BuildReportWorkbook(d reportData, tenant, from, to string) (*excelize.File, error) {
	f := excelize.NewFile()
	hs, ms := headerStyle(f), moneyStyle(f)

	// --- Ringkasan ---
	sum := "Ringkasan"
	if err := f.SetSheetName("Sheet1", sum); err != nil {
		return nil, err
	}
	net := d.Cash + d.Qris + d.KantinOmzet - d.ExpensesTot
	rows := [][]any{
		{"Centrum Badminton â€” Laporan " + from + " s/d " + to},
		{"Tenant", tenant},
		{},
		{"Keterangan", "Jumlah (Rp)"},
		{"Pemasukan lapangan (tunai)", d.Cash},
		{"Pemasukan lapangan (QRIS)", d.Qris},
		{"Omzet kantin", d.KantinOmzet},
		{"Total pemasukan", d.Cash + d.Qris + d.KantinOmzet},
		{"Total pengeluaran", d.ExpensesTot},
		{"LABA BERSIH", net},
		{},
		{"Booking (terbayar/aktif)", d.Bookings},
		{"Modal kantin (COGS)", d.KantinModal},
		{"Laba kotor kantin", d.KantinOmzet - d.KantinModal},
	}
	for i, r := range rows {
		for j, v := range r {
			cell, _ := excelize.CoordinatesToCellName(j+1, i+1)
			if err := f.SetCellValue(sum, cell, v); err != nil {
				return nil, err
			}
		}
	}
	f.SetCellStyle(sum, "B5", "B10", ms)
	f.SetCellStyle(sum, "A4", "B4", hs)
	f.SetColWidth(sum, "A", "A", 30)
	f.SetColWidth(sum, "B", "B", 22)

	// --- Lapangan ---
	lap := "Lapangan"
	if _, err := f.NewSheet(lap); err != nil {
		return nil, err
	}
	lhead := []string{"Tanggal", "Tunai (Rp)", "QRIS (Rp)", "Total (Rp)", "Booking"}
	for j, h := range lhead {
		cell, _ := excelize.CoordinatesToCellName(j+1, 1)
		f.SetCellValue(lap, cell, h)
	}
	for i, r := range d.Days {
		n := i + 2
		vals := []any{r.Date, r.Cash, r.Qris, r.Cash + r.Qris, r.Bookings}
		for j, v := range vals {
			cell, _ := excelize.CoordinatesToCellName(j+1, n)
			f.SetCellValue(lap, cell, v)
		}
	}
	totalRow := len(d.Days) + 2
	f.SetCellValue(lap, fmt.Sprintf("A%d", totalRow), "TOTAL")
	f.SetCellValue(lap, fmt.Sprintf("B%d", totalRow), d.Cash)
	f.SetCellValue(lap, fmt.Sprintf("C%d", totalRow), d.Qris)
	f.SetCellValue(lap, fmt.Sprintf("D%d", totalRow), d.Cash+d.Qris)
	f.SetCellValue(lap, fmt.Sprintf("E%d", totalRow), d.Bookings)
	f.SetCellStyle(lap, "A1", "E1", hs)
	if totalRow >= 2 {
		f.SetCellStyle(lap, "B2", fmt.Sprintf("D%d", totalRow), ms)
	}
	f.SetColWidth(lap, "A", "A", 14)
	f.SetColWidth(lap, "B", "D", 18)
	f.SetColWidth(lap, "E", "E", 10)

	// --- Kantin ---
	kan := "Kantin"
	if _, err := f.NewSheet(kan); err != nil {
		return nil, err
	}
	khead := []string{"Tanggal", "Omzet (Rp)", "Modal (Rp)", "Laba (Rp)", "Item"}
	for j, h := range khead {
		cell, _ := excelize.CoordinatesToCellName(j+1, 1)
		f.SetCellValue(kan, cell, h)
	}
	for i, r := range d.KantinDays {
		n := i + 2
		vals := []any{r.Date, r.Omzet, r.Modal, r.Omzet - r.Modal, r.Qty}
		for j, v := range vals {
			cell, _ := excelize.CoordinatesToCellName(j+1, n)
			f.SetCellValue(kan, cell, v)
		}
	}
	f.SetCellStyle(kan, "A1", "E1", hs)
	lastK := len(d.KantinDays) + 1
	if lastK >= 2 {
		f.SetCellStyle(kan, "B2", fmt.Sprintf("D%d", lastK), ms)
	}
	topStart := lastK + 2
	f.SetCellValue(kan, fmt.Sprintf("A%d", topStart), "Top produk")
	f.SetCellValue(kan, fmt.Sprintf("B%d", topStart), "Qty")
	f.SetCellStyle(kan, fmt.Sprintf("A%d", topStart), fmt.Sprintf("B%d", topStart), hs)
	for i, t := range d.KantinTop {
		_ = f.SetCellStr(kan, fmt.Sprintf("A%d", topStart+1+i), safeCell(t.Name))
		_ = f.SetCellValue(kan, fmt.Sprintf("B%d", topStart+1+i), t.Qty)
	}
	f.SetColWidth(kan, "A", "A", 16)
	f.SetColWidth(kan, "B", "D", 16)
	f.SetColWidth(kan, "E", "E", 10)

	// --- Pengeluaran ---
	exp := "Pengeluaran"
	if _, err := f.NewSheet(exp); err != nil {
		return nil, err
	}
	ehead := []string{"Tanggal", "Kategori", "Keterangan", "Jumlah (Rp)"}
	for j, h := range ehead {
		cell, _ := excelize.CoordinatesToCellName(j+1, 1)
		f.SetCellValue(exp, cell, h)
	}
	for i, r := range d.Expenses {
		n := i + 2
		vals := []any{r.Date, safeCell(r.Category), safeCell(r.Note), r.Amount}
		for j, v := range vals {
			cell, _ := excelize.CoordinatesToCellName(j+1, n)
			if s, ok := v.(string); ok {
				_ = f.SetCellStr(exp, cell, s)
			} else {
				_ = f.SetCellValue(exp, cell, v)
			}
		}
	}
	etot := len(d.Expenses) + 2
	f.SetCellValue(exp, fmt.Sprintf("A%d", etot), "TOTAL")
	f.SetCellValue(exp, fmt.Sprintf("D%d", etot), d.ExpensesTot)
	f.SetCellStyle(exp, "A1", "D1", hs)
	if etot >= 2 {
		f.SetCellStyle(exp, "D2", fmt.Sprintf("D%d", etot), ms)
	}
	f.SetColWidth(exp, "A", "A", 14)
	f.SetColWidth(exp, "B", "B", 16)
	f.SetColWidth(exp, "C", "C", 32)
	f.SetColWidth(exp, "D", "D", 16)

	if idx, err := f.GetSheetIndex(sum); err == nil {
		f.SetActiveSheet(idx)
	}
	return f, nil
}

// ---- storage + tokens ----

func SafeTenant(tenant string) string {
	t := strings.ToLower(strings.TrimSpace(tenant))
	if !tenantRe.MatchString(t) {
		return "unknown"
	}
	return t
}

// ReportFilename builds the server-side download filename. Random suffix
// avoids collisions; it is not auth (the ticket is).
func ReportFilename(from, to, typ string) (string, error) {
	if !DateRe.MatchString(from) || !DateRe.MatchString(to) {
		return "", fmt.Errorf("from/to must be YYYY-MM-DD")
	}
	var rb [8]byte
	if _, err := rand.Read(rb[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("laporan-%s_%s-%s-%s.xlsx", from, to, typ, hex.EncodeToString(rb[:])[:8]), nil
}

// WriteWorkbook serializes a workbook to bytes for the storage backend.
func WriteWorkbook(f *excelize.File) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := f.WriteTo(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// SaveReport persists report bytes through the active ReportStorage
// backend and returns the opaque object key for the ticket.
func SaveReport(ctx context.Context, tenant, name string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("empty report")
	}
	if len(data) > ReportMaxBytes {
		return "", fmt.Errorf("report too large (%d bytes, max %d)", len(data), ReportMaxBytes)
	}
	return ReportStorage.Save(ctx, tenant, name, data)
}

func reportTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// MintReportToken binds an object key to a download token: Redis holds the
// hot ticket (30 min TTL), Postgres holds the manifest row the sweeper
// uses to reclaim the object. On manifest failure the ticket is revoked
// and the object removed so no orphan survives.
//
// The manifest write goes through private.mint_report (migration 053):
// report_objects is FORCE RLS on app.tenant_id, which the runtime pool
// never sets, so a direct INSERT fails 42501 on every export.
func MintReportToken(ctx context.Context, pool *pgxpool.Pool, tenant, objectKey string, size int) (string, error) {
	if ReportRDB == nil {
		return "", fmt.Errorf("report store unavailable (Redis)")
	}
	if pool == nil {
		return "", fmt.Errorf("report store unavailable (database)")
	}
	var rb [32]byte
	if _, err := rand.Read(rb[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(rb[:])
	t := reportTicket{Tenant: SafeTenant(tenant), File: objectKey}
	b, _ := json.Marshal(t)
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := ReportRDB.Set(rctx, reportTokenPrefix+token, string(b), reportTTL).Err(); err != nil {
		return "", err
	}
	var minted bool
	if err := pool.QueryRow(ctx,
		`SELECT private.mint_report($1,$2,$3,$4,$5)`,
		reportTokenHash(token), SafeTenant(tenant), objectKey, size, int(reportTTL.Seconds())).Scan(&minted); err != nil {
		_ = ReportRDB.Del(rctx, reportTokenPrefix+token).Err()
		_ = ReportStorage.Delete(ctx, SafeTenant(tenant), objectKey)
		return "", err
	}
	return token, nil
}

// PurgeExpiredReports deletes expired manifest rows and their objects.
// Idempotent: safe for concurrent sweepers and for either backend.
//
// Both statements go through definer functions (migration 053): the
// manifest is FORCE RLS, so direct SELECT/DELETE on the runtime pool
// match zero rows — a silent storage leak. Deletes are one batched
// call, not one round trip per row.
func PurgeExpiredReports(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	if pool == nil {
		return 0, nil
	}
	rows, err := pool.Query(ctx,
		`SELECT token_hash, tenant_id, object_key FROM private.expired_reports($1)`, 100)
	if err != nil {
		return 0, err
	}
	type expired struct{ hash, tenant, key string }
	batch := make([]expired, 0, 100)
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.hash, &e.tenant, &e.key); err != nil {
			rows.Close()
			return 0, err
		}
		batch = append(batch, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	hashes := make([]string, 0, len(batch))
	for _, e := range batch {
		// Storage deletes stay serial: they are HTTP calls, and one
		// slow object must not fail the whole batch.
		_ = ReportStorage.Delete(ctx, e.tenant, e.key)
		hashes = append(hashes, e.hash)
		n++
	}
	if len(hashes) > 0 {
		if err := pool.QueryRow(ctx,
			`SELECT private.delete_reports($1::text[])`, hashes).Scan(&n); err != nil {
			return 0, err
		}
	}
	return n, nil
}
