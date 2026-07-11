package app

import (
	"strings"
	"testing"
)

func TestRedisConfigFromAddr(t *testing.T) {
	const fallback = "envpass"

	tests := []struct {
		name         string
		addr         string
		wantAddr     string
		wantTLS      bool
		wantSNI      string
		wantUser     string
		wantPassword string
		wantDB       int
	}{
		{
			name:         "bare host:port passthrough",
			addr:         "moon-01:6379",
			wantAddr:     "moon-01:6379",
			wantTLS:      false,
			wantPassword: fallback, // password from the fallback arg
		},
		{
			name:         "redis:// no tls, db from path",
			addr:         "redis://h:6379/2",
			wantAddr:     "h:6379",
			wantTLS:      false,
			wantPassword: fallback, // no userinfo → fallback kept
			wantDB:       2,
		},
		{
			name:         "rediss:// upstash-style with user+token",
			addr:         "rediss://default:tok@h.upstash.io:6379",
			wantAddr:     "h.upstash.io:6379",
			wantTLS:      true,
			wantSNI:      "h.upstash.io",
			wantUser:     "default",
			wantPassword: "tok", // URL userinfo wins over fallback
		},
		{
			name:         "rediss:// password-only (no username)",
			addr:         "rediss://:tok@h:6379",
			wantAddr:     "h:6379",
			wantTLS:      true,
			wantSNI:      "h",
			wantUser:     "",
			wantPassword: "tok",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rc, addr, err := redisConfigFromAddr(tc.addr, fallback)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if addr != tc.wantAddr {
				t.Errorf("addr = %q, want %q", addr, tc.wantAddr)
			}
			if (rc.TLS != nil) != tc.wantTLS {
				t.Errorf("TLS present = %v, want %v", rc.TLS != nil, tc.wantTLS)
			}
			if tc.wantTLS && rc.TLS != nil && rc.TLS.ServerName != tc.wantSNI {
				t.Errorf("TLS.ServerName = %q, want %q", rc.TLS.ServerName, tc.wantSNI)
			}
			if rc.Username != tc.wantUser {
				t.Errorf("Username = %q, want %q", rc.Username, tc.wantUser)
			}
			if rc.Password != tc.wantPassword {
				t.Errorf("Password = %q, want %q", rc.Password, tc.wantPassword)
			}
			if rc.DB != tc.wantDB {
				t.Errorf("DB = %d, want %d", rc.DB, tc.wantDB)
			}
		})
	}
}

// A malformed URL must error, and — critically — the error must NOT leak the
// embedded credential (it's redacted via config.RedactDSN before it reaches a
// fatal log line).
func TestRedisConfigFromAddr_MalformedURLRedactsToken(t *testing.T) {
	const token = "s3cr3t-token"
	// A rediss:// URL with a non-numeric port fails goredis.ParseURL.
	rc, addr, err := redisConfigFromAddr("rediss://default:"+token+"@h.upstash.io:notaport", "")
	if err == nil {
		t.Fatalf("expected error for malformed url, got rc=%v addr=%q", rc, addr)
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("error leaked the token: %q", err.Error())
	}
}
