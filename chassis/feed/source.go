// Package feed is the control-event feed seam: where a chassis reads the
// ordered stream of control events it must apply to stay in sync.
//
// An event is a notification + a content-addressed artifact reference (see
// chassis/controlevent and internal docs/todo-architecture-saas-fleet.md §3.1); the
// feed only carries the small events, never the payload. The backend is
// selected by name through a registry, so an additional backend can be added
// without changing this package or its callers. The built-in "nop" backend
// disables the feed (single-node default); "file" reads events from a local
// directory.
package feed

import (
	"context"

	"github.com/loremlabs/thanks-computer/chassis/controlevent"
)

// Source yields control events strictly after a cursor position.
type Source interface {
	// Poll returns events with ControlVersion > sinceControlVersion,
	// sorted ascending by ControlVersion. An empty slice means "nothing
	// new" (not an error). Implementations must be cheap to call on a
	// timer and must not block beyond ctx.
	Poll(ctx context.Context, sinceControlVersion uint64) ([]controlevent.Event, error)

	// Name is the backend identity (for logs).
	Name() string
}
