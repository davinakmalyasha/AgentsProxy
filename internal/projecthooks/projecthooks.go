// Package projecthooks is the platform-owned seam for optional project
// side effects. A project package calls a hook; the composition root
// (cmd/server) installs the implementation once before serving.
//
// This exists so cmd/server never imports a project package directly. Adding
// a project remains a folder + generated imports, not a platform edit.
package projecthooks

import "context"

// BookingNotify is called after a booking is committed and confirmed. The
// tenant is explicit because the call happens outside the booking
// transaction. Implementations must detach from the request context
// (context.WithoutCancel + timeout) so a client disconnect cannot drop the
// notification.
var BookingNotify func(ctx context.Context, tenantID, to, text string)
