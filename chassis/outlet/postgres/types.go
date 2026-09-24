package postgres

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/netip"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// maxExactInt is the largest integer JSON readers with float64 numbers
// (every JavaScript runtime, and gjson's Float) keep exact.
const maxExactInt = 1 << 53

// elementOID maps the array types whose elements need the OID to encode
// (a date and a timestamp are both time.Time in Go) to their element type.
var elementOID = map[uint32]uint32{
	pgtype.DateArrayOID:        pgtype.DateOID,
	pgtype.TimestampArrayOID:   pgtype.TimestampOID,
	pgtype.TimestamptzArrayOID: pgtype.TimestamptzOID,
}

// encodeValue writes one column value as JSON, deterministically and
// without silent precision loss:
//
//	NULL → null; boolean; text/varchar/enum → string
//	int2, int4 → number; int8 → number within ±2^53, else string
//	float4, float8 → number; NaN and ±Inf → string
//	numeric → string; json, jsonb → the JSON value
//	arrays → array of mapped elements
//	date → "2006-01-02"; time → "15:04:05.999999"
//	timestamp → "2006-01-02T15:04:05.999999" (no zone: it has none)
//	timestamptz → RFC 3339 in UTC; interval → Postgres text
//	uuid, inet, cidr → string; bytea → base64
func encodeValue(buf *bytes.Buffer, oid uint32, v any) {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		writeString(buf, x)
	case int16:
		buf.WriteString(strconv.FormatInt(int64(x), 10))
	case int32:
		buf.WriteString(strconv.FormatInt(int64(x), 10))
	case int64:
		if x > maxExactInt || x < -maxExactInt {
			writeString(buf, strconv.FormatInt(x, 10))
		} else {
			buf.WriteString(strconv.FormatInt(x, 10))
		}
	case uint32: // oid, xid
		buf.WriteString(strconv.FormatUint(uint64(x), 10))
	case float32:
		encodeFloat(buf, float64(x))
	case float64:
		encodeFloat(buf, x)
	case json.Number:
		buf.WriteString(string(x))
	case pgtype.Numeric:
		switch {
		case !x.Valid:
			buf.WriteString("null")
		case x.NaN:
			writeString(buf, "NaN")
		case x.InfinityModifier == pgtype.Infinity:
			writeString(buf, "Infinity")
		case x.InfinityModifier == pgtype.NegativeInfinity:
			writeString(buf, "-Infinity")
		default:
			dv, err := x.Value()
			if s, ok := dv.(string); ok && err == nil {
				writeString(buf, s)
			} else {
				writeString(buf, fmt.Sprint(dv))
			}
		}
	case time.Time:
		switch oid {
		case pgtype.DateOID:
			writeString(buf, x.Format("2006-01-02"))
		case pgtype.TimestampOID:
			writeString(buf, x.UTC().Format("2006-01-02T15:04:05.999999"))
		default:
			writeString(buf, x.UTC().Format(time.RFC3339Nano))
		}
	case pgtype.InfinityModifier:
		writeString(buf, x.String())
	case pgtype.Time:
		if !x.Valid {
			buf.WriteString("null")
			return
		}
		t := time.Unix(0, 0).UTC().Add(time.Duration(x.Microseconds) * time.Microsecond)
		writeString(buf, t.Format("15:04:05.999999"))
	case pgtype.Interval:
		if !x.Valid {
			buf.WriteString("null")
			return
		}
		dv, err := x.Value()
		if s, ok := dv.(string); ok && err == nil {
			writeString(buf, s)
		} else {
			writeString(buf, fmt.Sprint(dv))
		}
	case [16]byte:
		writeString(buf, formatUUID(x))
	case netip.Prefix:
		writeString(buf, x.String())
	case netip.Addr:
		writeString(buf, x.String())
	case []byte:
		writeString(buf, base64.StdEncoding.EncodeToString(x))
	case []any:
		el := elementOID[oid]
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			encodeValue(buf, el, e)
		}
		buf.WriteByte(']')
	case map[string]any:
		b, err := json.Marshal(x)
		if err != nil {
			writeString(buf, fmt.Sprint(x))
			return
		}
		buf.Write(b)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			writeString(buf, fmt.Sprint(x))
			return
		}
		buf.Write(b)
	}
}

func encodeFloat(buf *bytes.Buffer, f float64) {
	switch {
	case math.IsNaN(f):
		writeString(buf, "NaN")
	case math.IsInf(f, 1):
		writeString(buf, "Infinity")
	case math.IsInf(f, -1):
		writeString(buf, "-Infinity")
	default:
		b, _ := json.Marshal(f)
		buf.Write(b)
	}
}

func writeString(buf *bytes.Buffer, s string) {
	b, _ := json.Marshal(s)
	buf.Write(b)
}

func formatUUID(u [16]byte) string {
	h := hex.EncodeToString(u[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
