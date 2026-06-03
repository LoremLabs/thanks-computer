package usage

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// fakeSink is a throwaway Sink to exercise Register/Open without touching
// the real backends.
type fakeSink struct{ cfg SinkConfig }

func (f *fakeSink) WriteEvent(UsageEvent)       {}
func (f *fakeSink) Name() string                { return "fake" }
func (f *fakeSink) Close(context.Context) error { return nil }

func TestOpenDefaultZap(t *testing.T) {
	// The bundled "zap" sink self-registers from init(), so it is
	// available with no blank import.
	s, err := Open("zap", SinkConfig{Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("Open(zap) error: %v", err)
	}
	if _, ok := s.(*ZapSink); !ok {
		t.Fatalf("Open(zap) = %T, want *ZapSink", s)
	}
	if s.Name() != "zap" {
		t.Fatalf("Name() = %q, want zap", s.Name())
	}
}

func TestOpenUnknownLists(t *testing.T) {
	_, err := Open("nope", SinkConfig{})
	if err == nil {
		t.Fatal("Open(nope) = nil error, want unknown-sink error")
	}
	// The message lists available backends; "zap" is always present.
	if !strings.Contains(err.Error(), "zap") {
		t.Fatalf("error %q does not list available sinks", err.Error())
	}
}

func TestRegisterAndConfigPassthrough(t *testing.T) {
	var got SinkConfig
	Register("fake", func(cfg SinkConfig) (Sink, error) {
		got = cfg
		return &fakeSink{cfg: cfg}, nil
	})
	t.Cleanup(func() { delete(registry, "fake") })

	want := SinkConfig{Epoch: "ep1", NodeID: "node-a", DataDir: "/tmp/x", Logger: zap.NewNop()}
	s, err := Open("fake", want)
	if err != nil {
		t.Fatalf("Open(fake) error: %v", err)
	}
	if s.Name() != "fake" {
		t.Fatalf("Name() = %q, want fake", s.Name())
	}
	if got.Epoch != want.Epoch || got.NodeID != want.NodeID || got.DataDir != want.DataDir {
		t.Fatalf("SinkConfig not passed through: got %+v want %+v", got, want)
	}
}
