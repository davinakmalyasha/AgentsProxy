package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// All paths below return before touching the pool, so nil is safe.

func runReminder(t *testing.T, ctx context.Context, text, at string) string {
	t.Helper()
	tool := setReminderTool(nil)
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
	ctx := WithChatID(context.Background(), 123)
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
	if ChatIDFromCtx(context.Background()) != "" {
		t.Fatal("empty ctx must give empty chat")
	}
	if ChatIDFromCtx(WithChatID(context.Background(), 456)) != "456" {
		t.Fatal("round-trip failed")
	}
}
