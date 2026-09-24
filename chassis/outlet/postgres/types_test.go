package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/loremlabs/thanks-computer/chassis/outlet"
)

func enc(oid uint32, v any) string {
	var buf bytes.Buffer
	encodeValue(&buf, oid, v)
	return buf.String()
}

func TestEncodeValue(t *testing.T) {
	ts := time.Date(2026, 9, 24, 10, 30, 0, 123456000, time.FixedZone("x", 3600))
	cases := []struct {
		name string
		oid  uint32
		v    any
		want string
	}{
		{"null", pgtype.TextOID, nil, `null`},
		{"bool", pgtype.BoolOID, true, `true`},
		{"text", pgtype.TextOID, `a"b`, `"a\"b"`},
		{"int2", pgtype.Int2OID, int16(7), `7`},
		{"int4", pgtype.Int4OID, int32(-7), `-7`},
		{"int8 exact", pgtype.Int8OID, int64(1 << 53), `9007199254740992`},
		{"int8 beyond", pgtype.Int8OID, int64(1<<53 + 1), `"9007199254740993"`},
		{"int8 negative beyond", pgtype.Int8OID, int64(-(1<<53 + 1)), `"-9007199254740993"`},
		{"float", pgtype.Float8OID, 1.5, `1.5`},
		{"nan", pgtype.Float8OID, math.NaN(), `"NaN"`},
		{"inf", pgtype.Float8OID, math.Inf(-1), `"-Infinity"`},
		{"numeric", pgtype.NumericOID, pgtype.Numeric{Int: big.NewInt(12345), Exp: -2, Valid: true}, `"123.45"`},
		{"numeric nan", pgtype.NumericOID, pgtype.Numeric{NaN: true, Valid: true}, `"NaN"`},
		{"numeric null", pgtype.NumericOID, pgtype.Numeric{}, `null`},
		{"date", pgtype.DateOID, time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC), `"2026-09-24"`},
		{"timestamp", pgtype.TimestampOID, time.Date(2026, 9, 24, 10, 30, 0, 123456000, time.UTC), `"2026-09-24T10:30:00.123456"`},
		{"timestamptz", pgtype.TimestamptzOID, ts, `"2026-09-24T09:30:00.123456Z"`},
		{"infinity", pgtype.TimestamptzOID, pgtype.Infinity, `"infinity"`},
		{"time", pgtype.TimeOID, pgtype.Time{Microseconds: 3600_000_001, Valid: true}, `"01:00:00.000001"`},
		{"uuid", pgtype.UUIDOID, [16]byte{0xde, 0xad, 0xbe, 0xef, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, `"deadbeef-0000-0000-0000-000000000001"`},
		{"inet", pgtype.InetOID, netip.MustParsePrefix("10.0.0.0/8"), `"10.0.0.0/8"`},
		{"bytea", pgtype.ByteaOID, []byte("hi"), `"aGk="`},
		{"json number", pgtype.JSONBOID, json.Number("9007199254740993"), `9007199254740993`},
		{"json object", pgtype.JSONBOID, map[string]any{"a": []any{json.Number("1"), "b"}}, `{"a":[1,"b"]}`},
		{"int array", pgtype.Int8ArrayOID, []any{int64(1), nil, int64(3)}, `[1,null,3]`},
		{"date array", pgtype.DateArrayOID, []any{time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)}, `["2026-01-02"]`},
	}
	for _, c := range cases {
		if got := enc(c.oid, c.v); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
	if got := enc(pgtype.IntervalOID, pgtype.Interval{Days: 1, Microseconds: 7200_000_000, Valid: true}); got != `"1 day 02:00:00"` {
		t.Errorf("interval: %s", got)
	}
}

func TestClassify(t *testing.T) {
	pg := func(code string) error {
		return &pgconn.PgError{Code: code, Message: "user app on host db.internal failed"}
	}
	cases := []struct {
		name     string
		err      error
		ph       phase
		mutating bool
		code     string
		state    string
	}{
		{"unique", pg("23505"), phaseQuery, true, outlet.CodeConstraint, "23505"},
		{"auth", pg("28P01"), phaseConnect, false, outlet.CodeAuthFailed, "28P01"},
		{"cancelled", pg("57014"), phaseQuery, false, outlet.CodeTimeout, "57014"},
		{"shutdown", pg("57P01"), phaseQuery, false, outlet.CodeUnavailable, "57P01"},
		{"too many", pg("53300"), phaseConnect, false, outlet.CodeUnavailable, "53300"},
		{"read only", pg("25006"), phaseQuery, false, outlet.CodeQueryFailed, "25006"},
		{"syntax", pg("42601"), phaseQuery, false, outlet.CodeQueryFailed, "42601"},
		{"commit rollback", pgx.ErrTxCommitRollback, phaseCommit, true, outlet.CodeQueryFailed, ""},
		{"deadline", context.DeadlineExceeded, phaseQuery, false, outlet.CodeTimeout, ""},
		{"lost commit", errors.New("unexpected EOF"), phaseCommit, true, outlet.CodeOutcomeUnknown, ""},
		{"lost commit deadline", context.DeadlineExceeded, phaseCommit, true, outlet.CodeOutcomeUnknown, ""},
		{"lost read-only commit", errors.New("unexpected EOF"), phaseCommit, false, outlet.CodeUnavailable, ""},
		{"dial", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, phaseConnect, false, outlet.CodeConnectFailed, ""},
		{"guard", errors.New("egress: blocked 10.0.0.5 (private/internal address space)"), phaseConnect, false, outlet.CodeConnectFailed, ""},
		{"unknown mid-query", errors.New("conn closed"), phaseQuery, false, outlet.CodeUnavailable, ""},
	}
	for _, c := range cases {
		got := classify(c.err, c.ph, c.mutating)
		if got.Code != c.code || got.SQLState != c.state {
			t.Errorf("%s: got %s/%q want %s/%q", c.name, got.Code, got.SQLState, c.code, c.state)
		}
		for _, leak := range []string{"db.internal", "user app", "10.0.0.5"} {
			if bytes.Contains([]byte(got.Message), []byte(leak)) {
				t.Errorf("%s: message leaks %q: %s", c.name, leak, got.Message)
			}
		}
	}
}

func TestOpenRefusesNonURLDSN(t *testing.T) {
	_, err := Driver{}.Open(context.Background(), outlet.OpenParams{DSN: []byte("host=db.internal user=app password=hunter2")})
	var oe *outlet.Error
	if !errors.As(err, &oe) || oe.Code != outlet.CodeConnectFailed || bytes.Contains([]byte(oe.Message), []byte("hunter2")) {
		t.Fatalf("key/value DSN: %v", err)
	}
	_, err = Driver{}.Open(context.Background(), outlet.OpenParams{DSN: []byte("postgres://app:hunter2@db.internal:5432/crm?sslmode=bogus")})
	if !errors.As(err, &oe) || oe.Code != outlet.CodeConnectFailed || bytes.Contains([]byte(oe.Message), []byte("hunter2")) {
		t.Fatalf("bad option: %v", err)
	}
}
