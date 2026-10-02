package server

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// A node with no scheduled store answers an error the rule can read; it
// does not panic and it does not pretend to have armed anything.
func TestScheduleOpWithoutStoreIsDisabled(t *testing.T) {
	out, err := scheduleOp(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("want an error from txco://schedule with no store")
	}
	got := gjson.Get(out.Raw, "_schedule.error").String()
	if !strings.HasPrefix(got, "txco_schedule_disabled") {
		t.Fatalf("_schedule.error = %q, want it to start with txco_schedule_disabled", got)
	}
}
