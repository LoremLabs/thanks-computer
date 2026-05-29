package processor

import (
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/operation"
)

func metaOp(meta string) operation.Operation { return operation.Operation{Meta: meta} }

func TestOpAsyncBudget(t *testing.T) {
	pu := &Unit{Conf: config.Config{AsyncRuntimeDefault: "10m", OpTimeoutMax: "10m"}}

	// Omitted timeout → async-runtime-default.
	if got := pu.opAsyncBudget(metaOp(`{"mode":"async"}`)); got != 10*time.Minute {
		t.Fatalf("default budget = %v, want 10m", got)
	}
	// String duration override, NOT capped by op-timeout-max (async path).
	if got := pu.opAsyncBudget(metaOp(`{"mode":"async","timeout":"2h"}`)); got != 2*time.Hour {
		t.Fatalf("budget = %v, want 2h (uncapped)", got)
	}
	// Numeric override is milliseconds (matches WITH timeout = 1500).
	if got := pu.opAsyncBudget(metaOp(`{"timeout":1500}`)); got != 1500*time.Millisecond {
		t.Fatalf("budget = %v, want 1.5s", got)
	}
}

func TestOpAckTimeout(t *testing.T) {
	pu := &Unit{Conf: config.Config{AsyncAckTimeout: "5s"}}
	if got := pu.opAckTimeout(); got != 5*time.Second {
		t.Fatalf("ack = %v, want 5s", got)
	}
}

func TestOpJoinAtScope(t *testing.T) {
	if s, ok := opJoinAtScope(metaOp(`{"join_at_scope":200}`)); !ok || s != 200 {
		t.Fatalf("join_at_scope = (%d,%v), want (200,true)", s, ok)
	}
	if _, ok := opJoinAtScope(metaOp(`{"mode":"async"}`)); ok {
		t.Fatal("join_at_scope reported present on an op that omits it")
	}
}

func TestDeadlineHorizon(t *testing.T) {
	pu := &Unit{Conf: config.Config{AsyncRuntimeDefault: "10m", DeferredJoinSlack: "60s"}}
	now := time.Unix(1_000_000, 0).UTC()

	// Default budget (10m) + slack (60s) = 11m.
	if got := pu.deadlineHorizon(now, metaOp(`{"mode":"async"}`)); !got.Equal(now.Add(11 * time.Minute)) {
		t.Fatalf("horizon = %v, want now+11m", got)
	}
	// Explicit long timeout (2h) + slack (60s).
	if got := pu.deadlineHorizon(now, metaOp(`{"mode":"async","timeout":"2h"}`)); !got.Equal(now.Add(2*time.Hour + time.Minute)) {
		t.Fatalf("horizon = %v, want now+2h1m", got)
	}
}
