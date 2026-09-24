// Package postgres is the PostgreSQL outlet driver: pgx pools opened from a
// DSN the chassis holds, every dial through the egress guard, `query` in a
// READ ONLY transaction and `exec` in an explicit one that rolls back when
// a ceiling is crossed. It registers as driver "postgres" on import.
//
// The bundled chassis includes PostgreSQL as an outlet protocol client, not
// as a chassis storage backend: this package never registers the
// database/sql "pgx" driver, so internal persistence stays SQLite unless a
// downstream overlay supplies Postgres storage independently.
package postgres

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/egress"
	"github.com/loremlabs/thanks-computer/chassis/outlet"
)

func init() { outlet.Register(Driver{}) }

// dialTimeout bounds one TCP connect; the op's own deadline still applies
// on top through the context.
const dialTimeout = 10 * time.Second

// Driver opens pgx pools.
type Driver struct{}

// Name implements outlet.Driver.
func (Driver) Name() string { return "postgres" }

// Open parses the DSN into a pool config and creates the pool. Nothing is
// dialed here (MinConns is 0), so a database that is down surfaces on the
// first statement, never at open. The DSN must be URL form: the key/value
// form (`host=... password=...`) has no `://` and would slip past DSN
// redaction in logs.
//
// The pool config keeps the password for reconnects; a Go string cannot be
// zeroed. It is never logged and never encoded — pgxpool.Config has no
// JSON form — and the runtime zeroes the []byte it was handed.
func (Driver) Open(ctx context.Context, p outlet.OpenParams) (outlet.Conn, error) {
	dsn := string(p.DSN)
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		return nil, outlet.NewError(outlet.CodeConnectFailed, "outlet secret must be a postgres:// URL")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// pgx's message echoes parts of the DSN; report a fixed one.
		return nil, outlet.NewError(outlet.CodeConnectFailed, "outlet secret is not a valid postgres:// URL")
	}
	d := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	if p.Guard != nil {
		// Control runs per resolved IP before connect and before TLS, so
		// a DSN pointing the chassis at private address space is refused
		// however the name resolves.
		d.Control = egress.DialControl(p.Guard)
	}
	cfg.ConnConfig.DialFunc = d.DialContext
	// DescribeExec: the extended protocol (one statement per call, values
	// bound out of band) with a describe round trip on every execution —
	// works behind transaction-mode poolers and never serves a stale
	// statement description. CacheDescribe is the later optimization;
	// the simple protocol is never an option (it accepts multiple
	// statements in one string).
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeDescribeExec
	cfg.MinConns = 0
	cfg.MaxConns = int32(p.Pool.MaxConns)
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = 4
	}
	if p.Pool.IdleTimeout > 0 {
		cfg.MaxConnIdleTime = p.Pool.IdleTimeout
	}
	cfg.MaxConnLifetime = time.Hour
	cfg.AfterConnect = registerJSONCodecs
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, outlet.NewError(outlet.CodeConnectFailed, "outlet pool could not be configured")
	}
	log := p.Logger
	if log == nil {
		log = zap.NewNop()
	}
	return &conn{pool: pool, log: log}, nil
}

// registerJSONCodecs makes json and jsonb values decode with UseNumber, so
// an integer beyond float64's exact range survives the trip into the
// envelope as written.
func registerJSONCodecs(_ context.Context, c *pgx.Conn) error {
	m := c.TypeMap()
	m.RegisterType(&pgtype.Type{Name: "json", OID: pgtype.JSONOID, Codec: &pgtype.JSONCodec{Marshal: json.Marshal, Unmarshal: unmarshalUseNumber}})
	m.RegisterType(&pgtype.Type{Name: "jsonb", OID: pgtype.JSONBOID, Codec: &pgtype.JSONBCodec{Marshal: json.Marshal, Unmarshal: unmarshalUseNumber}})
	return nil
}

func unmarshalUseNumber(data []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	return dec.Decode(v)
}
