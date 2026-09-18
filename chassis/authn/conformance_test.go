package authn_test

import (
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/authn/authntest"
)

// The store's behaviour suite on the bundled engine. The cloud overlay runs
// the same suite against Postgres (overlay/pgauth).
func TestStoreConformanceSQLite(t *testing.T) {
	authntest.Conformance(t, func(t *testing.T) *authn.Store { return authntest.NewSQLiteStore(t) })
}
