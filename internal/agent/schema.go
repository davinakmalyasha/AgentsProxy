package agent

import "encoding/json"

// noArgs is the schema for a tool that takes no arguments.
//
// A tool with no Parameters still needs a valid schema, because the
// provider expects `parameters` on every declaration and a missing field
// is rejected by some gateways. Kept here rather than inline in the meta
// table so the "no arguments" case is stated once.
func noArgs() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
