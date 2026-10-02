// Package centrumbadminton implements Centrum Badminton's agent tools.
//
// One file per tool. Each file is self-contained: the Args shape, the
// description the model sees, and the Run function that executes it.
package centrumbadminton

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
)

func init() { agentstools.Register("export_report_excel", exportReportExcelTool) }

func exportReportExcelTool(pool *pgxpool.Pool) agentstools.Tool {
	return agentstools.Tool{
		Name:        "export_report_excel",
		Description: `Build a downloadable Excel (.xlsx) revenue report. Args: {from? (YYYY-MM-DD), to? (YYYY-MM-DD), type? ("lapangan"|"kantin"|"semua", default "semua")}. Defaults to this month, max range 1 year. Returns an XLSX:/v1/reports/... download marker plus a one-line summary â€” ALWAYS present the marker to the user as the download link, never invent another URL. OWNER ONLY.`,
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				From string `json:"from"`
				To   string `json:"to"`
				Type string `json:"type"`
			}
			_ = json.Unmarshal(args, &p)
			typ := strings.ToLower(strings.TrimSpace(p.Type))
			if typ == "" {
				typ = "semua"
			}
			if typ != "lapangan" && typ != "kantin" && typ != "semua" {
				return `error: type must be "lapangan", "kantin", or "semua"`
			}
			from, to := agentstools.DefaultMonthRange(p.From, p.To)
			if !agentstools.DateRe.MatchString(from) || !agentstools.DateRe.MatchString(to) {
				return "error: from/to must be YYYY-MM-DD"
			}
			d0, err0 := time.Parse("2006-01-02", from)
			d1, err1 := time.Parse("2006-01-02", to)
			if err0 != nil || err1 != nil || !d1.After(d0) || d1.Sub(d0).Hours()/24 > agentstools.MaxReportDays {
				return "error: invalid range (need from < to, max 1 year)"
			}
			data, err := agentstools.FetchReportData(ctx, pool, tenantID, from, to)
			if err != nil {
				return "error: " + err.Error()
			}
			f, err := agentstools.BuildReportWorkbook(data, tenantID, from, to)
			if err != nil {
				return "error: " + err.Error()
			}
			rel, err := agentstools.SaveReportFile(tenantID, from, to, typ, f)
			if err != nil {
				return "error: " + err.Error()
			}
			token, err := agentstools.MintReportToken(tenantID, rel)
			if err != nil {
				return "error: " + err.Error()
			}
			net := data.Cash + data.Qris + data.KantinOmzet - data.ExpensesTot
			summary := fmt.Sprintf("Laporan %sâ€“%s: pemasukan %s, pengeluaran %s, bersih %s, %d booking.",
				from, to, agentstools.Rp(data.Cash+data.Qris+data.KantinOmzet), agentstools.Rp(data.ExpensesTot), agentstools.Rp(net), data.Bookings)
			return fmt.Sprintf("%s/v1/reports/%s.xlsx\n%s", agentstools.XLSXMarkerPrefix, token, summary)
		},
	}
}

// ---- data ----
