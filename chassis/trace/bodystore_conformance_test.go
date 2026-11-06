package trace_test

import (
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/trace/tracetest"
)

func TestBodyStoreConformanceInMemory(t *testing.T) {
	tracetest.BodyStoreConformance(t, &tracetest.MemBodyStore{}, "traces/bodies/acme/1-rid.bodies")
}
