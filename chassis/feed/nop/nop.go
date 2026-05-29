// Package nop is the disabled feed source + sink: the single-node
// default. Source yields no events (applier inert); Sink discards
// events (producer pump never starts). Together they make chassis
// behaviour identical to today when --feed-source=nop and
// --feed-sink=nop.
package nop

import (
	"context"

	"github.com/loremlabs/thanks-computer/chassis/controlevent"
	"github.com/loremlabs/thanks-computer/chassis/feed"
)

func init() {
	feed.Register("nop", func(feed.SourceConfig) (feed.Source, error) {
		return Source{}, nil
	})
	feed.RegisterSink("nop", func(feed.SourceConfig) (feed.Sink, error) {
		return Sink{}, nil
	})
}

// Source is the no-op feed source.
type Source struct{}

func (Source) Name() string { return "nop" }

func (Source) Poll(context.Context, uint64) ([]controlevent.Event, error) {
	return nil, nil
}

// Sink is the no-op feed sink. It accepts events and discards them.
// Producers using the nop sink still get a non-error return so the
// pump can mark the outbox row "published" — but no other chassis
// will see the event. Useful only as a default for single-node
// deployments where the pump is gated off entirely (controlpublish
// checks --feed-sink != nop before starting).
type Sink struct{}

func (Sink) Name() string { return "nop" }

func (Sink) Append(_ context.Context, e controlevent.Event) (controlevent.Event, error) {
	return e, nil
}

var (
	_ feed.Source = Source{}
	_ feed.Sink   = Sink{}
)
