package reminder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/SherClockHolmes/webpush-go"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/pushsafe"
)

// PushSender delivers reminders via Web Push (VAPID) to the PWA
// subscriptions registered for a recipient (migration 057).
//
// Contract with the scheduler (see sendOne/deliver):
//   - no subscriptions for (tenant, recipient) → transient error: the
//     row is preserved and attempts bump, never dead-lettered for
//     missing addressability. An empty recipient_ref (pre-push rows)
//     lands here too.
//   - 410/404 per endpoint → subscription deleted (endpoint rotated),
//     counts as a transient failure for that endpoint.
//   - at least one 2xx → nil: the scheduler marks the row sent.
//   - every endpoint failing transiently → error: attempts bump.
//
// Payload shape matches public/sw.js (title/body/url/tag); the tag is
// omitted (SendFunc carries no id) — the service worker defaults it.
type PushSender struct {
	Pool         *pgxpool.Pool
	VAPIDPublic  string
	VAPIDPrivate string
	// Subject identifies the sender in the VAPID JWT (a contact URL
	// or mailto). Push services may refuse without one.
	Subject string
	// HTTP is swappable in tests (httptest server as push service).
	HTTP *http.Client
	// allowEndpoint overrides the outbound URL policy in tests. Nil means
	// pushsafe.ValidateEndpoint, the production SSRF boundary.
	allowEndpoint func(string) error
}

type pushPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
}

// errNoSubscriptions is transient: addressability may arrive later
// (user subscribes a new device), so the row must wait, not die.
var errNoSubscriptions = errors.New("no push subscriptions for recipient")

// validateEndpoint applies the outbound push-service allowlist. Tests that
// point at an httptest server override allowEndpoint; production always uses
// pushsafe.ValidateEndpoint.
func (s *PushSender) validateEndpoint(raw string) error {
	if s.allowEndpoint != nil {
		return s.allowEndpoint(raw)
	}
	return pushsafe.ValidateEndpoint(raw)
}

// Send delivers text to every live subscription for the recipient.
func (s *PushSender) Send(ctx context.Context, tenantID, recipientRef, text string) error {
	if recipientRef == "" {
		return errNoSubscriptions
	}
	rows, err := s.Pool.Query(ctx,
		`SELECT endpoint, p256dh, auth FROM push_subscriptions
		  WHERE tenant_id = $1 AND user_ref = $2`,
		tenantID, recipientRef)
	if err != nil {
		return err
	}
	type sub struct {
		endpoint, p256dh, auth string
	}
	var subs []sub
	for rows.Next() {
		var e sub
		if err := rows.Scan(&e.endpoint, &e.p256dh, &e.auth); err != nil {
			rows.Close()
			return err
		}
		subs = append(subs, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(subs) == 0 {
		return errNoSubscriptions
	}

	body, err := json.Marshal(pushPayload{
		Title: "Centrum Badminton",
		Body:  "⏰ Reminder: " + text,
		URL:   "/app",
	})
	if err != nil {
		return err
	}
	client := s.HTTP
	if client == nil {
		client = pushsafe.NewHTTPClient(8 * time.Second)
	}
	sent, failed := 0, 0
	for _, e := range subs {
		if err := s.validateEndpoint(e.endpoint); err != nil {
			// The row predates the allowlist or was written by a bad
			// actor. Never dial it; prune it so the worker does not
			// retry an SSRF target on every tick.
			slog.Warn("push endpoint rejected",
				"tenant", tenantID, "endpoint_host", pushsafe.Host(e.endpoint), "err", err)
			if _, derr := s.Pool.Exec(ctx,
				`DELETE FROM push_subscriptions WHERE tenant_id = $1 AND endpoint = $2`,
				tenantID, e.endpoint); derr != nil {
				slog.Warn("push invalid subscription prune failed", "err", derr)
			}
			failed++
			continue
		}
		resp, err := webpush.SendNotificationWithContext(ctx, body,
			&webpush.Subscription{
				Endpoint: e.endpoint,
				Keys:     webpush.Keys{Auth: e.auth, P256dh: e.p256dh},
			},
			&webpush.Options{
				HTTPClient:      client,
				Subscriber:      s.Subject,
				TTL:             86400,
				VAPIDPublicKey:  s.VAPIDPublic,
				VAPIDPrivateKey: s.VAPIDPrivate,
			})
		if err != nil {
			slog.Warn("push send error", "tenant", tenantID, "endpoint_host", pushsafe.Host(e.endpoint), "err", err)
			failed++
			continue
		}
		_ = resp.Body.Close()
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			sent++
			if _, uerr := s.Pool.Exec(ctx,
				`UPDATE push_subscriptions SET last_used_at = now()
				  WHERE tenant_id = $1 AND endpoint = $2`,
				tenantID, e.endpoint); uerr != nil {
				slog.Warn("push last_used_at touch failed", "err", uerr)
			}
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
			// Endpoint rotated (app data cleared, resubscribed elsewhere):
			// prune so future ticks stop paying for a dead address.
			slog.Info("push subscription gone, pruning", "tenant", tenantID, "endpoint_host", pushsafe.Host(e.endpoint))
			if _, derr := s.Pool.Exec(ctx,
				`DELETE FROM push_subscriptions WHERE tenant_id = $1 AND endpoint = $2`,
				tenantID, e.endpoint); derr != nil {
				slog.Warn("push prune failed", "err", derr)
			}
			failed++
		default:
			slog.Warn("push send status", "tenant", tenantID, "endpoint_host", pushsafe.Host(e.endpoint), "status", resp.StatusCode)
			failed++
		}
	}
	if sent > 0 {
		return nil
	}
	return fmt.Errorf("push failed on %d subscription(s)", failed)
}
