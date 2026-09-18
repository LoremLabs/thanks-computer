package tcp

import (
	"context"
	"io"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
)

// TestConnectRunCarriesTheConnectFee — accepting a connection is charged
// on its connect run, as that run's starting fuel; a small line after it
// has accrued nothing worth a unit and starts from zero like any request.
func TestConnectRunCarriesTheConnectFee(t *testing.T) {
	for name, tc := range map[string]struct {
		fee  int
		want string // _txc.fuel_used on the connect run; "" = absent
	}{
		"charged":  {fee: 100, want: "100"},
		"disabled": {fee: 0, want: ""},
	} {
		t.Run(name, func(t *testing.T) {
			connect, line := make(chan string, 1), make(chan string, 1)
			h := newHarness(t, func(c *config.Config) { c.TCPConnFuel = tc.fee }, nil, func(env *event.Envelope) event.DispatchResult {
				if isConnect(env) {
					connect <- env.Payload.Raw
				} else {
					line <- env.Payload.Raw
				}
				return greetThenUpper(env)
			})
			c, r := dial(t, h.addr)
			readLineT(t, r)
			if _, err := c.Write([]byte("x\n")); err != nil {
				t.Fatal(err)
			}
			readLineT(t, r)
			if got := gjson.Get(<-connect, "_txc.fuel_used").String(); got != tc.want {
				t.Errorf("connect run fuel_used = %q, want %q", got, tc.want)
			}
			if raw := <-line; gjson.Get(raw, "_txc.fuel_used").Exists() {
				t.Errorf("a few bytes are not a unit of fuel; line run must start from zero: %s", raw)
			}
		})
	}
}

// TestWorkOutsideARunRidesTheNextEvent — bytes a handler moves without an
// event per read, and work it charges itself, land on the connection's
// next run; once billed they are not billed again.
func TestWorkOutsideARunRidesTheNextEvent(t *testing.T) {
	const moved = 2 << 20 // 2 MiB in = 200 fuel
	helloServe.Store(func(ctx context.Context, rc RoutedConn) error {
		if _, err := io.CopyN(io.Discard, rc.Conn, moved); err != nil {
			return err
		}
		rc.AddFuel(7)
		for i := 0; i < 2; i++ {
			if _, err := rc.Emit(ctx, Event{}); err != nil {
				return err
			}
		}
		return nil
	})
	events := make(chan string, 2)
	h := newHarness(t, withHandler("hello"), nil, func(env *event.Envelope) event.DispatchResult {
		if env.Src == "hello" {
			events <- env.Payload.Raw
		}
		return inHello(verdict("", ""))
	})
	c, r := dial(t, h.addr)
	if _, err := c.Write(make([]byte, moved)); err != nil {
		t.Fatal(err)
	}
	expectEOF(t, r)
	if got := gjson.Get(<-events, "_txc.fuel_used").Int(); got != 207 {
		t.Errorf("first event fuel_used = %d, want 207 (2 MiB at %d/MiB + 7 added)", got, fuelPerMiB)
	}
	if raw := <-events; gjson.Get(raw, "_txc.fuel_used").Exists() {
		t.Errorf("already billed; second event must start from zero: %s", raw)
	}
}

// TestPrechargeNeverExhaustsARun — what accrued between events is handed
// over at most half a request's budget at a time, so the next run is
// never dead on arrival; the rest follows on later runs.
func TestPrechargeNeverExhaustsARun(t *testing.T) {
	helloServe.Store(func(ctx context.Context, rc RoutedConn) error {
		rc.AddFuel(130)
		for i := 0; i < 4; i++ {
			if _, err := rc.Emit(ctx, Event{}); err != nil {
				return err
			}
		}
		return nil
	})
	events := make(chan int64, 4)
	tune := func(c *config.Config) { withHandler("hello")(c); c.MaxFuelPerRequest = 100 }
	h := newHarness(t, tune, nil, func(env *event.Envelope) event.DispatchResult {
		if env.Src == "hello" {
			events <- gjson.Get(env.Payload.Raw, "_txc.fuel_used").Int()
		}
		return inHello(verdict("", ""))
	})
	_, r := dial(t, h.addr)
	expectEOF(t, r)
	for i, want := range []int64{50, 50, 30, 0} {
		if got := <-events; got != want {
			t.Errorf("event %d fuel_used = %d, want %d", i, got, want)
		}
	}
}

// TestMeterKeepsTheRemainder — small frames add up: bytes short of a
// unit are carried, not dropped, and the lifetime totals say what never
// reached a run.
func TestMeterKeepsTheRemainder(t *testing.T) {
	var m meter
	for i := 0; i < 105; i++ { // 1,050,000 bytes: just over 1 MiB
		m.moved(&m.in, 10_000)
		if i == 50 {
			if got := m.take(0); got != 48 { // 510,000 B = 48.6 units
				t.Fatalf("mid-way take = %d, want 48", got)
			}
		}
	}
	m.add(3)
	in, out, fuel, unbilled := m.totals()
	if in != 1_050_000 || out != 0 || fuel != 103 || unbilled != 55 {
		t.Errorf("totals = in %d out %d fuel %d unbilled %d, want 1050000 0 103 55", in, out, fuel, unbilled)
	}
}
