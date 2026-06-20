package clicmd

import (
	"context"
	"testing"
)

func TestCursorRoundtrip(t *testing.T) {
	if got := Cursor(context.Background()); got != "" {
		t.Fatalf("cursor on a bare context = %q, want empty", got)
	}
	ctx := WithCursor(context.Background(), "c123")
	if got := Cursor(ctx); got != "c123" {
		t.Fatalf("cursor = %q, want c123", got)
	}
}
