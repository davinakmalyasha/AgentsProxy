package centrumbadminton

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
)

// All paths below return before touching the pool, so nil is safe.

// reminderTool fetches the registered tool rather than calling its
// constructor directly. That is the point: the registry is the only way
// production reaches it, so the test proves registration happened.
func reminderTool(t *testing.T) agentstools.Tool {
	t.Helper()
	ts := agentstools.NewToolSet(nil)
	tool, ok := ts.Get("set_reminder")
	if !ok {
		t.Fatal("set_reminder is not registered")
	}
	return tool
}

func runReminder(t *testing.T, ctx context.Context, text, at string) string {
	t.Helper()
	tool := reminderTool(t)
	args, _ := json.Marshal(map[string]string{"text": text, "remind_at": at})
	return tool.Run(ctx, "centrum", args)
}

func TestSetReminderNeedsChat(t *testing.T) {
	got := runReminder(t, context.Background(), "order kok", "2099-01-01 08:00")
	if !strings.Contains(got, "Telegram") {
		t.Fatalf("expected Telegram-only error, got %q", got)
	}
}

func TestSetReminderBadDate(t *testing.T) {
	ctx := agentstools.WithChatID(context.Background(), 123)
	got := runReminder(t, ctx, "order kok", "not-a-date")
	if !strings.Contains(got, "YYYY-MM-DD HH:MM") {
		t.Fatalf("got %q", got)
	}
	got = runReminder(t, ctx, "order kok", "2020-01-01 08:00")
	if !strings.Contains(got, "past") {
		t.Fatalf("expected past error, got %q", got)
	}
	got = runReminder(t, ctx, "   ", "2099-01-01 08:00")
	if !strings.Contains(got, "required") {
		t.Fatalf("got %q", got)
	}
}

func TestChatIDFromCtx(t *testing.T) {
	if agentstools.ChatIDFromCtx(context.Background()) != "" {
		t.Fatal("empty ctx must give empty chat")
	}
	if agentstools.ChatIDFromCtx(agentstools.WithChatID(context.Background(), 456)) != "456" {
		t.Fatal("round-trip failed")
	}
}
