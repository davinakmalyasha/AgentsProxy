// Chat-generated Excel revenue reports (owner-only).
//
// Flow: owner asks in Telegram/staff webapp â†’ export_report_excel Tool
// builds a .xlsx under data/reports/{tenant}/ â†’ mints a 32-byte download
// token in Redis (30 min TTL, tenant-scoped) â†’ returns an
// "XLSX:/v1/reports/{token}.xlsx" marker the LLM presents as a link and
// the Telegram handler turns into a document attachment.
//
// DONE: WS2 (xlsx via chat)
package agentstools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	reportsDir        = "data/reports"
	reportTokenHexLen = 64 // 32 random bytes
	MaxReportDays     = 370
)

var (
	hexTokenRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
	tenantRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	DateRe     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// reportTicket binds a download token to one tenant + one file.
type reportTicket struct {
	Tenant string `json:"tenant"`
	File   string `json:"file"` // base name only, under reportsDir/tenant/
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
func lookupTicket(token string) (reportTicket, error) {
	var t reportTicket
	token = strings.TrimSuffix(token, ".xlsx")
	if !hexTokenRe.MatchString(token) {
		return t, fmt.Errorf("bad token")
	}
	if ReportRDB == nil {
		return t, fmt.Errorf("report store unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	raw, err := ReportRDB.Get(ctx, reportTokenPrefix+token).Result()
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

// ticketPath confines a ticket to reportsDir/tenant/, base name only.
func ticketPath(t reportTicket) string {
	return filepath.Join(reportsDir, t.Tenant, filepath.Base(t.File))
}

// OpenReportFile validates a token and opens the file for streaming
// (HTTP downloads). Caller must Close.
func OpenReportFile(token string) (*os.File, string, error) {
	t, err := lookupTicket(token)
	if err != nil {
		return nil, "", err
	}
	p := ticketPath(t)
	f, err := os.Open(p)
	if err != nil {
		return nil, "", fmt.Errorf("file gone")
	}
	return f, filepath.Base(t.File), nil
}

// ResolveReportDownload validates a token from the download path and
// returns the file name + bytes. Used by the Telegram handler (server-side,
// no HTTP round-trip) and the HTTP download route.
func ResolveReportDownload(token string) (filename string, data []byte, err error) {
	t, err := lookupTicket(token)
	if err != nil {
		return "", nil, err
	}
	// Never trust the ticket blindly: confine to reportsDir/tenant/, base name only.
	b, err := os.ReadFile(ticketPath(t))
	if err != nil {
		return "", nil, fmt.Errorf("file gone")
	}
	return filepath.Base(t.File), b, nil
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
	err := records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
		// Lapangan income per day (WIB calendar day, same convention as laporan page).
		r, err := tx.Query(ctx,
			`SELECT (paid_at AT TIME ZONE 'Asia/Jakarta')::date AS d,
			        COALESCE(SUM(amount) FILTER (WHERE method='cash'),0),
			        COALESCE(SUM(amount) FILTER (WHERE method='qris'),0),
			        COUNT(*)
			   FROM sports_payments
			  WHERE tenant_id=$1 AND (paid_at AT TIME ZONE 'Asia/Jakarta')::date >= $2::date
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
			d.Days = append(d.Days, row)
			d.Cash += row.Cash
			d.Qris += row.Qris
			d.Bookings += row.Bookings
		}
		r.Close()
		if err := r.Err(); err != nil {
			return err
		}
		// Expenses detail.
		r, err = tx.Query(ctx,
			`SELECT spent_at, category, note, amount FROM sports_expenses
			  WHERE tenant_id=$1 AND spent_at >= $2::date AND spent_at < $3::date
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
			d.Expenses = append(d.Expenses, row)
			d.ExpensesTot += row.Amount
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
			d.KantinDays = append(d.KantinDays, row)
			d.KantinOmzet += row.Omzet
			d.KantinModal += row.Modal
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
			d.KantinTop = append(d.KantinTop, row)
		}
		r.Close()
		return r.Err()
	})
	return d, err
}

// ---- workbook ----

func Idr(v float64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	n := int64(v + 0.5)
	s := fmt.Sprintf("%d", n)
	// thousands separator "."
	var out []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, '.')
		}
		out = append(out, byte(c))
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// rp renders an IDR amount with the sign in front: -Rp4.405.000, Rp95.000.
func Rp(v float64) string {
	if v < 0 {
		return "-Rp" + Idr(-v)
	}
	return "Rp" + Idr(v)
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
		f.SetCellValue(kan, fmt.Sprintf("A%d", topStart+1+i), t.Name)
		f.SetCellValue(kan, fmt.Sprintf("B%d", topStart+1+i), t.Qty)
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
		vals := []any{r.Date, r.Category, r.Note, r.Amount}
		for j, v := range vals {
			cell, _ := excelize.CoordinatesToCellName(j+1, n)
			f.SetCellValue(exp, cell, v)
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

func SaveReportFile(tenant, from, to, typ string, f *excelize.File) (string, error) {
	t := SafeTenant(tenant)
	dir := filepath.Join(reportsDir, t)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	var rb [8]byte
	if _, err := rand.Read(rb[:]); err != nil {
		return "", err
	}
	name := fmt.Sprintf("laporan-%s_%s-%s-%s.xlsx", from, to, typ, hex.EncodeToString(rb[:])[:8])
	if err := f.SaveAs(filepath.Join(dir, name)); err != nil {
		return "", err
	}
	return name, nil
}

func MintReportToken(tenant, file string) (string, error) {
	if ReportRDB == nil {
		return "", fmt.Errorf("report store unavailable (Redis)")
	}
	var rb [32]byte
	if _, err := rand.Read(rb[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(rb[:])
	t := reportTicket{Tenant: SafeTenant(tenant), File: filepath.Base(file)}
	b, _ := json.Marshal(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := ReportRDB.Set(ctx, reportTokenPrefix+token, string(b), reportTTL).Err(); err != nil {
		return "", err
	}
	return token, nil
}
