package agent

import "encoding/json"

// The argument contract and routing vocabulary for every tool, in one
// place.
//
// Kept as a table rather than inline on each Tool so the two things a tool
// needs to be usable — a machine-readable argument schema and the words a
// user actually types — are reviewable together, and cannot drift apart
// from the allowlists in tools.go. `NewToolSet` applies it at registration.
//
// Schemas are deliberately flat and mostly optional: every tool resolves its
// own defaults server-side (dates default to this month, limits default to a
// sane number), so a model that omits an argument still gets a useful
// answer instead of an error.
//
// Two tiers of keyword, because one is not enough:
//   - Keywords are the ordinary vocabulary. "berapa" or "jadwal" is a weak
//     signal that several tools share.
//   - Strong are decisive: one meaning in this domain, and a hit outweighs
//     everything a competing tool can accumulate. Without this, "Berapa laba
//     bulan ini?" tied with query_income on "berapa" and lost on alphabet.

type toolMeta struct {
	// Parameters is the JSON Schema sent to the provider.
	Parameters string
	// Keywords are matched against the normalised user message.
	Keywords []string
	// Strong are decisive keywords.
	Strong []string
}

var toolMetaTable = map[string]toolMeta{
	"query_income": {
		Parameters: `{"type":"object","properties":{"from":{"type":"string","format":"date","description":"start date, inclusive, defaults to this month"},"to":{"type":"string","format":"date","description":"end date, EXCLUSIVE, defaults to next month"}}}`,
		Keywords:   []string{"pendapatan", "income", "revenue", "pemasukan", "uang masuk", "berapa", "ringkas", "ringkasan"},
	},
	"query_range_summary": {
		Parameters: `{"type":"object","properties":{"from":{"type":"string","format":"date","description":"start date, inclusive"},"to":{"type":"string","format":"date","description":"end date, EXCLUSIVE"}}}`,
		Keywords:   []string{"range", "rentang", "summary", "bandingkan"},
		Strong:     []string{"vs", "bandingkan", "dibanding", "compare", "perbandingan", "jam tersibuk", "tersibuk", "peak", "paling ramai"},
	},
	"query_net_profit": {
		Parameters: `{"type":"object","properties":{"from":{"type":"string","format":"date","description":"start date, inclusive"},"to":{"type":"string","format":"date","description":"end date, EXCLUSIVE"}}}`,
		Keywords:   []string{"profit", "modal"},
		// "labanya"/"keuntungan" used to be caught as substrings of
		// "laba". Word-boundary matching does not do that for free, and
		// "berapa labanya?" is an ordinary thing to ask.
		Strong: []string{"laba", "labanya", "profit", "untung", "keuntungan", "net profit", "laba bersih"},
	},
	"closing_report": {
		Parameters: `{"type":"object","properties":{"date":{"type":"string","format":"date","description":"the day to close, defaults to today"}}}`,
		Keywords:   []string{"daily", "closing", "rekap"},
		Strong:     []string{"closing", "tutup buku", "tutupbuku", "laporan harian", "rekap harian", "tutup hari"},
	},
	"top_customers": {
		Parameters: `{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"description":"how many customers, default 5"}}}`,
		Keywords:   []string{"top", "loyal", "favorit", "sering datang"},
		Strong:     []string{"pelanggan paling", "paling sering", "pelanggan", "customer"},
	},
	"export_report_excel": {
		Parameters: `{"type":"object","properties":{"from":{"type":"string","format":"date","description":"start date, inclusive"},"to":{"type":"string","format":"date","description":"end date, EXCLUSIVE"},"type":{"type":"string","enum":["lapangan","kantin","semua"],"description":"which ledger to export, default semua"}}}`,
		Keywords:   []string{"file", "export", "laporan file"},
		Strong:     []string{"excel", "xlsx", "spreadsheet", "unduh", "unduhan", "download", "ekspor"},
	},
	"query_kantin_summary": {
		Parameters: `{"type":"object","properties":{"from":{"type":"string","format":"date","description":"start date, inclusive"},"to":{"type":"string","format":"date","description":"end date, EXCLUSIVE"}}}`,
		Keywords:   []string{"omzet", "snack", "minuman"},
		Strong:     []string{"kantin"},
	},
	"get_availability": {
		Parameters: `{"type":"object","properties":{"date":{"type":"string","format":"date","description":"the day to check, defaults to today"}}}`,
		Keywords:   []string{"slot", "jam", "jadwal", "bisa"},
		Strong:     []string{"kosong", "tersedia", "bisa booking", "availability", "cek jadwal"},
	},
	"get_pricing": {
		Parameters: `{"type":"object","properties":{}}`,
		Keywords:   []string{"harga", "price", "tarif", "biaya", "berapa rupiah"},
		// "harganya"/"harga berapa" are the natural phrasings and were
		// caught as substrings before. Stated explicitly now.
		Strong: []string{"harga", "harganya", "price", "tarif", "biaya", "berapa rupiah"},
	},
	"check_stock": {
		Parameters: `{"type":"object","properties":{"name":{"type":"string","description":"product or item name, may be partial"}}}`,
		Keywords:   []string{"sisa", "barang", "cek", "habis"},
		// "stoknya" is the possessive form people actually type; it was a
		// substring hit on "stok" and is now stated.
		Strong: []string{"stok", "stoknya", "stock", "sisa stok"},
	},
	"list_bookings": {
		Parameters: `{"type":"object","properties":{"date":{"type":"string","format":"date","description":"the day to list, defaults to today"}}}`,
		// "pesanan" belongs here and NOT to book_slot. It is the noun
		// ("an order/booking already placed") and used to reach book_slot
		// only because that tool's keyword "pesan" is a substring of it --
		// so "hari ini ada pesanan?" pinned the write tool. Matching is on
		// word boundaries now, which is exactly what separates the noun
		// from the verb.
		Keywords: []string{"booking", "daftar", "list", "pemesanan", "pesanan", "ada booking", "jadwal hari ini", "booking hari ini"},
		Strong:   []string{"daftar booking", "list booking"},
	},
	"book_slot": {
		// The parameter was named `court`, but the tool body reads
		// `facility_id` from the same JSON. So every call the model made
		// from this schema sent `court: "Lapangan 1"` and no
		// facility_id at all, which reached the INSERT as an empty
		// string and came back "unknown facility". The two names have to
		// agree; `facility_id` is what the tool and the proxy path use.
		Parameters: `{"type":"object","properties":{"facility_id":{"type":"string","description":"the facility (court) id, from get_availability"},"date":{"type":"string","format":"date"},"start_time":{"type":"string","description":"HH:MM, 24h"},"end_time":{"type":"string","description":"HH:MM, 24h"},"customer_name":{"type":"string"},"customer_phone":{"type":"string"}},"required":["facility_id","date","start_time","end_time","customer_name"]}`,
		// Verbs only. The noun forms live in list_bookings, and a
		// request to CREATE is always explicit -- "pesan lapangan jam 9"
		// -- so these do not need to catch inflections.
		Keywords: []string{"book", "pesan", "reservasi", "reservasikan", "buat booking", "catat booking", "pesan lapangan"},
		Strong:   []string{"buat booking", "catat booking", "reservasi baru"},
	},
	"cancel_booking": {
		Parameters: `{"type":"object","properties":{"booking_id":{"type":"string","description":"the booking id"}}}`,
		Keywords:   []string{"cancel", "hapus booking"},
		Strong:     []string{"batal", "batalkan", "pembatalan", "batalkan booking"},
	},
	"adjust_stock": {
		Parameters: `{"type":"object","properties":{"product_id":{"type":"string"},"name":{"type":"string","description":"product name, may be partial"},"delta":{"type":"integer","description":"signed change to stock"},"count":{"type":"integer","minimum":0,"description":"absolute counted level for a stock-take; replaces delta"}}}`,
		Keywords:   []string{"tambah", "kurangi", "adjust", "stocktake"},
		Strong:     []string{"tambah stok", "kurangi stok", "restock", "opname", "stok masuk", "stok berkurang"},
	},
	"query_attendance": {
		Parameters: `{"type":"object","properties":{"date":{"type":"string","format":"date"}}}`,
		Keywords:   []string{"kehadiran", "attendance", "hadir", "sedang main", "main"},
		Strong:     []string{"sedang main", "check in", "checkin"},
	},
	"topup_member": {
		Parameters: `{"type":"object","properties":{"name":{"type":"string"},"phone":{"type":"string"},"amount":{"type":"number","minimum":1}}}`,
		Keywords:   []string{"topup", "isi", "kredit"},
		Strong:     []string{"topup", "isi saldo"},
	},
	"record_staff_payment": {
		Parameters: `{"type":"object","properties":{"email":{"type":"string","description":"staff email"},"amount":{"type":"number","minimum":1},"method":{"type":"string","enum":["cash","qris"]}}}`,
		Keywords:   []string{"staff", "pegawai", "payment"},
		Strong:     []string{"bayar staff", "kas staff", "bayar pegawai"},
	},
	"list_staff_payments": {
		Parameters: `{"type":"object","properties":{}}`,
		Keywords:   []string{"riwayat", "daftar"},
		Strong:     []string{"riwayat bayar staff", "daftar bayar staff"},
	},
	"set_reminder": {
		Parameters: `{"type":"object","properties":{"text":{"type":"string","description":"what to be reminded about"},"when":{"type":"string","description":"when, e.g. tomorrow 09:00"}}}`,
		Keywords:   []string{"reminder", "pengingat", "order", "ingatkan"},
		Strong:     []string{"ingatkan", "pengingat", "reminder"},
	},
}

// applyToolMeta fills in Parameters, Keywords and StrongKeywords from the
// table. An inline value on the Tool always wins, so a one-off tool can
// declare its own without touching the table.
func applyToolMeta(t Tool) Tool {
	m, ok := toolMetaTable[t.Name]
	if !ok {
		return t
	}
	if len(t.Parameters) == 0 && m.Parameters != "" {
		t.Parameters = json.RawMessage(m.Parameters)
	}
	if len(t.Keywords) == 0 {
		t.Keywords = m.Keywords
	}
	if len(t.StrongKeywords) == 0 {
		t.StrongKeywords = m.Strong
	}
	return t
}
