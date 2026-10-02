package adkrunner

import (
	"errors"
	"sync"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
)

// maxRoundsError aborts the run when the model keeps chaining tool calls
// instead of answering. It is a distinct type so a deliberate budget abort
// is distinguishable from a provider error that happens to mention limits.
type maxRoundsError struct{}

func (e *maxRoundsError) Error() string { return "adkrunner: model call budget exceeded" }

func NewMaxRoundsError() error { return &maxRoundsError{} }

// IsMaxRounds reports whether err is (or wraps) the budget abort. The
// runner may wrap callback errors, so this uses errors.As, not ==.
func IsMaxRounds(err error) bool {
	var mre *maxRoundsError
	return errors.As(err, &mre)
}

// maxModelCalls preserves the old loop's maxRounds=5 bound as a model-call
// budget. The old loop returned after one tool round plus narration, so it
// never actually spent more than ~2 model calls; 5 gives the ADK loop room
// to chain (check stock, then book) while still bounding cost.
const maxModelCalls = 5

// maxRoundsMessage is the reply when the budget trips. Same wording as the
// old loop's exhaustion path, so clients see no change.
const maxRoundsMessage = "I hit my processing limit — please rephrase or ask in smaller steps."

// budgetGuard counts model calls per run and aborts past the budget. One
// instance is built per RunWithRole call, so the counter needs no keying by
// invocation -- but the mutex stays because callbacks have no documented
// sequential-execution guarantee.
type budgetGuard struct {
	mu    sync.Mutex
	calls int
	max   int
}

func (g *budgetGuard) beforeModel(ctx agent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	if g.calls > g.max {
		return nil, NewMaxRoundsError()
	}
	return nil, nil
}

// newBudgetPlugin wires the guard into the runner. Caps and abuse replies
// stay in RunWithRole (they run before any model call); this plugin only
// bounds what the model itself spends.
func newBudgetPlugin(max int) (*plugin.Plugin, error) {
	g := &budgetGuard{max: max}
	return plugin.New(plugin.Config{
		Name:                "budget-guard",
		BeforeModelCallback: llmagent.BeforeModelCallback(g.beforeModel),
	})
}
