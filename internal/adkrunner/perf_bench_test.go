package adkrunner

import (
	"testing"
	"time"

	"google.golang.org/adk/v2/session"

	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
)

func BenchmarkAssemble(b *testing.B) {
	layers := Layers{
		Role:     "owner",
		Display:  "Centrum Badminton Arena",
		ToolDefs: []string{"book_slot", "list_bookings", "get_pricing"},
		Vertical: "sports",
		Now:      agentstools.WibNow(),
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Assemble(layers)
	}
}

func BenchmarkEventToRow(b *testing.B) {
	// eventToRow runs per event (up to ~10 per agent turn): 13 JSON
	// marshals plus arg assembly. This pins its cost. No DB touched.
	sess := &pgxLocalSession{}
	ev := &session.Event{ID: "e1", Timestamp: time.Now()}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := eventToRow(sess, ev); err != nil {
			b.Fatal(err)
		}
	}
}
