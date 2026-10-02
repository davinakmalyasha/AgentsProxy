package adkrunner

import (
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
)

// adaptTool wraps one registry tool as an ADK function tool.
//
// The declaration comes from the tool's own Parameters schema -- the same
// bytes that travel to the provider today -- so the contract the model
// sees is unchanged. tenantID is captured in the closure, which is what
// makes tenant isolation structural: the model never supplies it, so it
// cannot be talked into another tenant's data.
//
// The handler runs the two checks the old loop ran before every execution:
// normalise (defaults, aliases) then hold the call to the advertised
// schema. A rejection comes back as a tool result, not a Go error, so the
// model corrects itself on the next turn instead of aborting the run.
func adaptTool(t agentstools.Tool, tenantID string) (tool.Tool, error) {
	params := t.Parameters
	if len(params) == 0 {
		params = json.RawMessage(`{"type":"object"}`)
	}
	schema := &jsonschema.Schema{}
	if err := json.Unmarshal(params, schema); err != nil {
		return nil, fmt.Errorf("adkrunner: tool %s has an unparsable schema: %w", t.Name, err)
	}
	return functiontool.New[map[string]any, map[string]any](
		functiontool.Config{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		},
		func(toolCtx agent.Context, args map[string]any) (map[string]any, error) {
			// toolCtx IS a context.Context (agent.Context embeds it) and
			// carries the caller's values -- chat ID, bot ID -- when the
			// runner propagates them. TestCtxReachesTool proves it; if a
			// future ADK version stops, that test fails loudly.
			raw, err := json.Marshal(args)
			if err != nil {
				return map[string]any{"result": "Error: could not encode arguments: " + err.Error()}, nil
			}
			norm := agentstools.NormaliseArguments(string(raw))
			if verr := agentstools.ValidateArgs(t.Parameters, norm); verr != nil {
				return map[string]any{"result": "Error: invalid arguments — " + verr.Error() + ". Call the tool again with corrected arguments."}, nil
			}
			return map[string]any{"result": t.Run(toolCtx, tenantID, norm)}, nil
		},
	)
}
