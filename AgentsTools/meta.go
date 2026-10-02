package agentstools

import "encoding/json"

// The argument contract for every tool, in one place.
//
// Kept as a table rather than inline on each Tool so the machine-readable
// argument schemas are reviewable together. `NewToolSet` applies it at
// registration; an inline Parameters on the Tool itself always wins, so a
// one-off tool can declare its own without touching the table.
//
// (This table used to carry a second column -- routing keywords the old
// Go keyword router matched against. The router is deleted: the model
// chooses tools from Description + schema now. The keywords went with
// it.)
//
// Schemas are deliberately flat and mostly optional: every tool resolves its
// own defaults server-side (dates default to this month, limits default to a
// sane number), so a model that omits an argument still gets a useful
// answer instead of an error.
type toolMeta struct {
	// Parameters is the JSON Schema sent to the provider.
	Parameters string
}

var toolMetaTable = map[string]toolMeta{
	"query_income": {
		Parameters: `{"type":"object","properties":{"from":{"type":"string","format":"date","description":"start date, inclusive, defaults to this month"},"to":{"type":"string","format":"date","description":"end date, EXCLUSIVE, defaults to next month"}}}`,
	},
	"query_range_summary": {
		Parameters: `{"type":"object","properties":{"from":{"type":"string","format":"date","description":"start date, inclusive"},"to":{"type":"string","format":"date","description":"end date, EXCLUSIVE"}}}`,
	},
	"query_net_profit": {
		Parameters: `{"type":"object","properties":{"from":{"type":"string","format":"date","description":"start date, inclusive"},"to":{"type":"string","format":"date","description":"end date, EXCLUSIVE"}}}`,
	},
	"closing_report": {
		Parameters: `{"type":"object","properties":{"date":{"type":"string","format":"date","description":"the day to close, defaults to today"}}}`,
	},
	"top_customers": {
		Parameters: `{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"description":"how many customers, default 5"}}}`,
	},
	"export_report_excel": {
		Parameters: `{"type":"object","properties":{"from":{"type":"string","format":"date","description":"start date, inclusive"},"to":{"type":"string","format":"date","description":"end date, EXCLUSIVE"},"type":{"type":"string","enum":["lapangan","kantin","semua"],"description":"which ledger to export, default semua"}}}`,
	},
	"query_kantin_summary": {
		Parameters: `{"type":"object","properties":{"from":{"type":"string","format":"date","description":"start date, inclusive"},"to":{"type":"string","format":"date","description":"end date, EXCLUSIVE"}}}`,
	},
	"get_availability": {
		Parameters: `{"type":"object","properties":{"date":{"type":"string","format":"date","description":"the day to check, defaults to today"}}}`,
	},
	"get_pricing": {
		Parameters: `{"type":"object","properties":{}}`,
	},
	"check_stock": {
		Parameters: `{"type":"object","properties":{"name":{"type":"string","description":"product or item name, may be partial"}}}`,
	},
	"list_bookings": {
		Parameters: `{"type":"object","properties":{"date":{"type":"string","format":"date","description":"the day to list, defaults to today"}}}`,
	},
	"book_slot": {
		// The parameter was named `court`, but the tool body reads
		// `facility_id` from the same JSON. So every call the model made
		// from this schema sent `court: "Lapangan 1"` and no
		// facility_id at all, which reached the INSERT as an empty
		// string and came back "unknown facility". The two names have to
		// agree; `facility_id` is what the tool and the proxy path use.
		Parameters: `{"type":"object","properties":{"facility_id":{"type":"string","description":"the facility (court) id, from get_availability"},"date":{"type":"string","format":"date"},"start_time":{"type":"string","description":"HH:MM, 24h"},"end_time":{"type":"string","description":"HH:MM, 24h"},"customer_name":{"type":"string"},"customer_phone":{"type":"string"}},"required":["facility_id","date","start_time","end_time","customer_name"]}`,
	},
	"cancel_booking": {
		Parameters: `{"type":"object","properties":{"booking_id":{"type":"string","description":"the booking id"}}}`,
	},
	"adjust_stock": {
		Parameters: `{"type":"object","properties":{"product_id":{"type":"string"},"name":{"type":"string","description":"product name, may be partial"},"delta":{"type":"integer","description":"signed change to stock"},"count":{"type":"integer","minimum":0,"description":"absolute counted level for a stock-take; replaces delta"}}}`,
	},
	"query_attendance": {
		Parameters: `{"type":"object","properties":{"date":{"type":"string","format":"date"}}}`,
	},
	"topup_member": {
		Parameters: `{"type":"object","properties":{"name":{"type":"string"},"phone":{"type":"string"},"amount":{"type":"number","minimum":1}}}`,
	},
	"record_staff_payment": {
		Parameters: `{"type":"object","properties":{"email":{"type":"string","description":"staff email"},"amount":{"type":"number","minimum":1},"method":{"type":"string","enum":["cash","qris"]}}}`,
	},
	"list_staff_payments": {
		Parameters: `{"type":"object","properties":{}}`,
	},
	"set_reminder": {
		Parameters: `{"type":"object","properties":{"text":{"type":"string","description":"what to be reminded about"},"when":{"type":"string","description":"when, e.g. tomorrow 09:00"}}}`,
	},
}

// applyToolMeta fills in Parameters from the table. An inline value on
// the Tool always wins, so a one-off tool can declare its own without
// touching the table.
func applyToolMeta(t Tool) Tool {
	m, ok := toolMetaTable[t.Name]
	if !ok {
		return t
	}
	if len(t.Parameters) == 0 && m.Parameters != "" {
		t.Parameters = json.RawMessage(m.Parameters)
	}
	return t
}
