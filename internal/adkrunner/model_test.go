package adkrunner

import (
	"context"
	"iter"
	"testing"

	"google.golang.org/adk/v2/model"

	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
)

type fakeLLM struct{ name string }

func (f fakeLLM) Name() string { return f.name }

func (f fakeLLM) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(func(*model.LLMResponse, error) bool) {}
}

func TestModelForUsesProjectOverrideAndCaches(t *testing.T) {
	def := fakeLLM{name: "default"}
	projects := map[string]*agentconfig.Project{
		"a": {Slug: "a", Agent: agentconfig.AgentConfig{Model: "model-a"}},
		"b": {Slug: "b", Agent: agentconfig.AgentConfig{Model: "model-b"}},
		"c": {Slug: "c"},
	}
	built := map[string]int{}
	r := &Runner{
		cfg:      Config{Model: "default"},
		model:    def,
		projects: projects,
		models:   map[string]model.LLM{"default": def},
		newModel: func(name string) (model.LLM, error) {
			built[name]++
			return fakeLLM{name: name}, nil
		},
	}

	if got := r.modelFor("a").Name(); got != "model-a" {
		t.Fatalf("a model=%q", got)
	}
	if got := r.modelFor("b").Name(); got != "model-b" {
		t.Fatalf("b model=%q", got)
	}
	if got := r.modelFor("c").Name(); got != "default" {
		t.Fatalf("c model=%q, want default", got)
	}
	if got := r.modelFor("unknown").Name(); got != "default" {
		t.Fatalf("unknown model=%q, want default", got)
	}
	// Second lookup must not rebuild the provider client.
	_ = r.modelFor("a")
	if built["model-a"] != 1 || built["model-b"] != 1 {
		t.Fatalf("built=%v, want one build per model", built)
	}
}
