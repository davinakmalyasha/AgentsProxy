package agent

import (
	"testing"

	"github.com/davinakmalyasha/agents-proxy/AgentsTools"
	_ "github.com/davinakmalyasha/agents-proxy/AgentsTools/CentrumBadminton"
)

// TestRouteAgainstRealRegistry routes the prompts the owner UI actually
// offers, through the real ToolSet — real descriptions, real keyword table.
// The fake-tool test cannot catch a metadata mistake here.
func TestOwnerPromptsRouteToTheirTool(t *testing.T) {
	ts := agentstools.NewToolSetFor(nil, "owner")
	cases := []struct {
		msg  string
		want string
	}{
		{"Ringkas pendapatan hari ini", "query_income"},
		{"Buatkan laporan Excel bulan ini", "export_report_excel"},
		{"Jam tersibuk bulan ini?", "query_range_summary"},
		{"Siapa pelanggan paling sering?", "top_customers"},
		{"Bandingkan pendapatan bulan ini vs bulan lalu", "query_range_summary"},
		{"buat closing report 2026-09-30", "closing_report"},
		{"Berapa laba bulan ini?", "query_net_profit"},
		{"Stok shuttlecock sisa berapa?", "check_stock"},
		{"Booking jam 19 kosong?", "get_availability"},
		{"Berapa harga lapangan?", "get_pricing"},
		{"Omzet kantin bulan ini?", "query_kantin_summary"},
	}
	for _, c := range cases {
		if got := ts.Route(c.msg); got.Tool != c.want {
			t.Errorf("route(%q) = %q (score %d), want %q", c.msg, got.Tool, got.Score, c.want)
		}
	}
}
