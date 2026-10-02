package agentstools

import (
	"path/filepath"
	"testing"

	"github.com/xuri/excelize/v2"
)

func sampleReportData() reportData {
	return reportData{
		Days: []dayRow{
			{Date: "2026-09-01", Cash: 400000, Qris: 50000, Bookings: 5},
			{Date: "2026-09-02", Cash: 200000, Qris: 100000, Bookings: 3},
		},
		Expenses: []expenseRow{
			{Date: "2026-09-01", Category: "listrik", Note: "PLN", Amount: 150000},
		},
		KantinDays: []kantinDay{
			{Date: "2026-09-01", Omzet: 120000, Modal: 70000, Qty: 12},
		},
		KantinTop:   []topRow{{Name: "Aqua", Qty: 10}},
		Cash:        600000,
		Qris:        150000,
		Bookings:    8,
		ExpensesTot: 150000,
		KantinOmzet: 120000,
		KantinModal: 70000,
	}
}

func TestIdr(t *testing.T) {
	if got := Idr(1500000); got != "1.500.000" {
		t.Fatalf("Idr(1500000) = %q", got)
	}
	if got := Idr(50000); got != "50.000" {
		t.Fatalf("Idr(50000) = %q", got)
	}
	if got := Idr(-4404999); got != "-4.404.999" {
		t.Fatalf("Idr(-4404999) = %q", got)
	}
	if got := Rp(-4404999); got != "-Rp4.404.999" {
		t.Fatalf("Rp(-4404999) = %q", got)
	}
	if got := Rp(95000); got != "Rp95.000" {
		t.Fatalf("Rp(95000) = %q", got)
	}
}

func TestParseXLSXMarker(t *testing.T) {
	reply := "Berikut laporannya ya!\nXLSX:/v1/reports/abc123.xlsx\nRingkasan: ok"
	path, rest := ParseXLSXMarker(reply)
	if path != "/v1/reports/abc123.xlsx" {
		t.Fatalf("path = %q", path)
	}
	if rest == "" || len(rest) >= len(reply) {
		t.Fatalf("rest should be reply minus marker, got %q", rest)
	}
	if p, _ := ParseXLSXMarker("no marker here"); p != "" {
		t.Fatalf("expected empty path, got %q", p)
	}
}

func TestBuildReportWorkbook(t *testing.T) {
	f, err := BuildReportWorkbook(sampleReportData(), "centrum", "2026-09-01", "2026-10-01")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Ringkasan", "Lapangan", "Kantin", "Pengeluaran"} {
		if _, err := f.GetSheetIndex(want); err != nil {
			t.Fatalf("sheet %s missing: %v", want, err)
		}
	}
	// Ringkasan totals: B8 total pemasukan = 600k+150k+120k
	if v, _ := f.GetCellValue("Ringkasan", "B8"); v != "870,000" {
		t.Fatalf("B8 total pemasukan = %q, want 870000", v)
	}
	// Laba bersih B10 = 870000-150000
	if v, _ := f.GetCellValue("Ringkasan", "B10"); v != "720,000" {
		t.Fatalf("B10 laba bersih = %q, want 720000", v)
	}
	// Lapangan first row + total
	if v, _ := f.GetCellValue("Lapangan", "A2"); v != "2026-09-01" {
		t.Fatalf("Lapangan A2 = %q", v)
	}
	if v, _ := f.GetCellValue("Lapangan", "D4"); v != "750,000" {
		t.Fatalf("Lapangan D4 total = %q, want 750000", v)
	}
	// Kantin top product (1 daily row → header A4, first product A5)
	if v, _ := f.GetCellValue("Kantin", "A5"); v != "Aqua" {
		t.Fatalf("Kantin A5 top = %q", v)
	}
	// Round-trip through disk
	p := filepath.Join(t.TempDir(), "r.xlsx")
	if err := f.SaveAs(p); err != nil {
		t.Fatal(err)
	}
	f2, err := excelize.OpenFile(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()
	if v, _ := f2.GetCellValue("Pengeluaran", "D3"); v != "150,000" {
		t.Fatalf("Pengeluaran D3 total = %q, want 150000", v)
	}
}
