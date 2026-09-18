package tcp

import (
	"sync/atomic"

	"github.com/loremlabs/thanks-computer/chassis/jsonx"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// fuelPerMiB prices the application bytes a connection moves: the rate
// blobs, KV values and drive files already pay per MiB. Those round each
// operation up to a whole MiB; a stream has no operations to round, so it
// pays for exactly what it moved, fractions carried (settle).
const (
	fuelPerMiB = processor.FuelCostBlobPerMiB
	mib        = int64(1) << 20
)

// meter accounts for what a connection costs OUTSIDE its pipeline runs.
// A run meters itself (scopes, EXECs, compute) and its fuel lands on the
// run's usage event; but between runs a connection is still work the
// chassis does for a tenant — accepting it, the bytes a handler moves
// without an event per read, whatever protocol chores the handler does in
// Go — and none of that has a run to be counted in.
//
// So it accrues here and rides the connection's NEXT run: the head
// pre-stamps the accrued amount as that run's starting `_txc.fuel_used`
// (chassis-owned; a run seeds its meter from it), which puts it on the
// same usage event, attributed to the same pinned tenant, as everything
// else the run costs. No second billing path.
//
// What is charged: the connect (`--tcp-conn-fuel`, on the connect run),
// bytes in and out at fuelPerMiB, and anything a handler adds
// (RoutedConn.AddFuel). What is not: time. An idle socket does no work;
// what it holds is capacity, and that is what --tcp-max-conns and
// --tcp-max-conns-per-tenant bound.
type meter struct {
	pending atomic.Int64 // fuel accrued and not yet on a run
	total   atomic.Int64 // fuel accrued over the connection's life
	in, out atomic.Int64 // application bytes
	unpaid  atomic.Int64 // bytes moved and not yet turned into fuel
}

// add accrues fuel for work done outside a run.
func (m *meter) add(n int64) {
	if n <= 0 {
		return
	}
	m.pending.Add(n)
	m.total.Add(n)
}

func (m *meter) moved(dir *atomic.Int64, n int) {
	if n <= 0 {
		return
	}
	dir.Add(int64(n))
	m.unpaid.Add(int64(n))
}

// settle turns whole fuel units' worth of moved bytes into fuel and keeps
// the remainder for next time, so a chatty connection of small frames
// still pays for its megabytes eventually.
func (m *meter) settle() {
	b := m.unpaid.Swap(0)
	fuel := b * fuelPerMiB / mib
	m.unpaid.Add(b - fuel*mib/fuelPerMiB)
	m.add(fuel)
}

// take hands over what has accrued, for one run to start from. At most
// half of --max-fuel-per-request at a time: a connection that moved a lot
// between events must not make its next run arrive already exhausted. The
// rest waits for the run after.
func (m *meter) take(maxRunFuel int64) int64 {
	m.settle()
	f := m.pending.Swap(0)
	if lim := maxRunFuel / 2; maxRunFuel > 0 && f > lim {
		m.pending.Add(f - lim)
		f = lim
	}
	return f
}

// totals reports the connection's lifetime figures for the close log.
// unbilled is fuel that accrued after the last run and so reached no
// usage event: a connection's final bytes, or a handler that never emits.
func (m *meter) totals() (in, out, fuel, unbilled int64) {
	m.settle()
	return m.in.Load(), m.out.Load(), m.total.Load(), m.pending.Load()
}

// Read and Write count the application bytes a handler (or the head's own
// greeting) moves. The TLS handshake and the edge's PROXY header are read
// beneath this, on c.Conn, and are not the tenant's bytes.
func (c *connection) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.meter.moved(&c.meter.in, n)
	return n, err
}

func (c *connection) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.meter.moved(&c.meter.out, n)
	return n, err
}

// precharge starts a run from what the connection accrued since its last
// one. Nothing accrued leaves the field off, and the run starts from zero
// as any other would.
func (tcp *TCPController) precharge(b *jsonx.Builder, c *connection) {
	if f := c.meter.take(tcp.lim.maxRunFuel); f > 0 {
		b.Set("_txc.fuel_used", f)
	}
}
