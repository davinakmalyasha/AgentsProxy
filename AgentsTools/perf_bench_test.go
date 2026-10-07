package agentstools

import (
	"strings"
	"testing"
)

func BenchmarkIdr(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = Idr(1234567.89)
		_ = Idr(40000)
		_ = Rp(-4405000)
	}
}

func BenchmarkSanitiseReply_Clean(b *testing.B) {
	// Plain prose with no markup: must hit the ContainsAny fast path.
	msg := "Berapa harga sewa lapangan badminton per jam untuk hari Sabtu pagi?"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = SanitiseReply(msg)
	}
}

func BenchmarkSanitiseReply_Markup(b *testing.B) {
	msg := "Harganya Rp40.000 per jam. <tool_call>{\"function\": \"x\"}</tool_call> Silakan booking."
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = SanitiseReply(msg)
	}
}

func BenchmarkIsOffTopicFor(b *testing.B) {
	msg := strings.Repeat("berapa harga sewa lapangan per jam? ", 20)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = IsOffTopicFor(msg, "sports")
	}
}
