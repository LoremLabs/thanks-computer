package admission

import (
	"testing"

	"go.uber.org/zap"
)

func TestAllowRate(t *testing.T) {
	db := newDB(t)
	seedTenant(t, db, "tnt_a", "acme", false)
	seedTenant(t, db, "tnt_b", "free", false) // no row → unlimited
	mustExec(t, db, `INSERT INTO tenant_runtime_state (tenant_id, rate_limit_rps, rate_burst) VALUES ('tnt_a', 2, 2)`)

	p := NewSQLiteProvider(zap.NewNop())
	if err := p.Rebuild(db); err != nil {
		t.Fatal(err)
	}

	// burst=2 → first two allowed, third denied with a positive retry.
	for i := 0; i < 2; i++ {
		if ok, _ := p.AllowRate("acme"); !ok {
			t.Fatalf("AllowRate call %d: want allow", i+1)
		}
	}
	ok, retry := p.AllowRate("acme")
	if ok {
		t.Fatal("third call should be rate-limited")
	}
	if retry <= 0 {
		t.Errorf("denied retry = %v, want > 0", retry)
	}
	// A denied call must NOT consume a token (Reserve+Cancel): a repeat is
	// still denied with a retry that hasn't ballooned.
	if ok2, retry2 := p.AllowRate("acme"); ok2 || retry2 <= 0 || retry2 > 2*retry {
		t.Errorf("repeat denied = (%v, %v); want still denied, retry not ballooned", ok2, retry2)
	}
	// Unconfigured / _sys / unknown → always allow.
	for _, ten := range []string{"free", "_sys", "nobody"} {
		if ok, _ := p.AllowRate(ten); !ok {
			t.Errorf("AllowRate(%q) want allow (no limit)", ten)
		}
	}
}

func TestAcquireConcurrency(t *testing.T) {
	db := newDB(t)
	seedTenant(t, db, "tnt_a", "acme", false)
	mustExec(t, db, `INSERT INTO tenant_runtime_state (tenant_id, concurrency_limit) VALUES ('tnt_a', 2)`)

	p := NewSQLiteProvider(zap.NewNop())
	if err := p.Rebuild(db); err != nil {
		t.Fatal(err)
	}

	l := NewLease()
	if !p.AcquireConcurrency("acme", l) || !p.AcquireConcurrency("acme", l) {
		t.Fatal("first two acquires should succeed")
	}
	if p.AcquireConcurrency("acme", l) {
		t.Fatal("third acquire should fail (at capacity)")
	}
	l.Release() // frees both slots
	if !p.AcquireConcurrency("acme", NewLease()) {
		t.Fatal("after release a slot should be free")
	}

	// cap=0 / unconfigured / _sys → admit without taking a slot.
	for _, ten := range []string{"unknown", "_sys"} {
		if !p.AcquireConcurrency(ten, NewLease()) {
			t.Errorf("AcquireConcurrency(%q) want admit", ten)
		}
	}
}

// TestReloadPreservesInFlight: a config-apply (Rebuild) must update caps in
// place without resetting the live in-flight counter.
func TestReloadPreservesInFlight(t *testing.T) {
	db := newDB(t)
	seedTenant(t, db, "tnt_a", "acme", false)
	mustExec(t, db, `INSERT INTO tenant_runtime_state (tenant_id, concurrency_limit) VALUES ('tnt_a', 2)`)

	p := NewSQLiteProvider(zap.NewNop())
	if err := p.Rebuild(db); err != nil {
		t.Fatal(err)
	}
	l := NewLease()
	p.AcquireConcurrency("acme", l)
	p.AcquireConcurrency("acme", l) // inFlight = 2 (at cap)

	if err := p.Rebuild(db); err != nil { // reload must not zero inFlight
		t.Fatal(err)
	}
	if p.AcquireConcurrency("acme", NewLease()) {
		t.Fatal("reload reset inFlight — acquire should still fail at cap 2")
	}
	l.Release()
	if !p.AcquireConcurrency("acme", NewLease()) {
		t.Fatal("after release a slot should be free")
	}
}

// TestConcurrencyEviction: a deconfigured tenant's entry survives while
// in-flight > 0 (so a late Release can't underflow), and is evicted once it
// drains to 0.
func TestConcurrencyEviction(t *testing.T) {
	db := newDB(t)
	seedTenant(t, db, "tnt_a", "acme", false)
	mustExec(t, db, `INSERT INTO tenant_runtime_state (tenant_id, concurrency_limit) VALUES ('tnt_a', 2)`)
	p := NewSQLiteProvider(zap.NewNop())
	if err := p.Rebuild(db); err != nil {
		t.Fatal(err)
	}
	l := NewLease()
	p.AcquireConcurrency("acme", l) // inFlight = 1

	mustExec(t, db, `UPDATE tenant_runtime_state SET concurrency_limit = 0 WHERE tenant_id = 'tnt_a'`)
	if err := p.Rebuild(db); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	_, present := p.cc["acme"]
	p.mu.Unlock()
	if !present {
		t.Fatal("entry evicted while inFlight > 0 — a late Release would underflow")
	}

	l.Release() // inFlight → 0
	if err := p.Rebuild(db); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	_, present = p.cc["acme"]
	p.mu.Unlock()
	if present {
		t.Error("entry should be evicted once inFlight drained and deconfigured")
	}
}

func TestRebuildReadsLimitColumns(t *testing.T) {
	db := newDB(t)
	seedTenant(t, db, "tnt_a", "acme", false)
	mustExec(t, db, `INSERT INTO tenant_runtime_state (tenant_id, rate_limit_rps, rate_burst, concurrency_limit) VALUES ('tnt_a', 7, 9, 3)`)
	p := NewSQLiteProvider(zap.NewNop())
	if err := p.Rebuild(db); err != nil {
		t.Fatal(err)
	}
	m := p.snap.Load()
	st := (*m)["acme"]
	if st.RateLimitRPS != 7 || st.RateBurst != 9 || st.ConcurrencyLimit != 3 {
		t.Errorf("snapshot limits = %+v, want rps=7 burst=9 conc=3", st)
	}
}
