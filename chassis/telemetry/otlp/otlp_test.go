package otlp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/egress"
	_ "github.com/loremlabs/thanks-computer/chassis/egress/private"
	"github.com/loremlabs/thanks-computer/chassis/telemetry"
)

var t0 = time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

// otlpServer captures every OTLP/HTTP export request it receives.
type otlpServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []*http.Request
}

func newOTLPServer(t *testing.T) *otlpServer {
	t.Helper()
	s := &otlpServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.reqs = append(s.reqs, r.Clone(context.Background()))
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *otlpServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

func (s *otlpServer) lastHeader(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reqs) == 0 {
		return ""
	}
	return s.reqs[len(s.reqs)-1].Header.Get(name)
}

// fakeSecrets is a mutable SecretSource that counts endpoint lookups.
type fakeSecrets struct {
	mu       sync.Mutex
	endpoint string
	headers  string // "" = not set (ErrSecretNotFound)
	err      error  // returned for the endpoint lookup when non-nil
	lookups  int
}

func (f *fakeSecrets) set(endpoint, headers string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endpoint, f.headers, f.err = endpoint, headers, err
}

func (f *fakeSecrets) lookupCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups
}

func (f *fakeSecrets) src() telemetry.SecretSource {
	return func(_ context.Context, _, name string) ([]byte, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch name {
		case telemetry.SecretEndpointName:
			f.lookups++
			if f.err != nil {
				return nil, f.err
			}
			if f.endpoint == "" {
				return nil, telemetry.ErrSecretNotFound
			}
			return []byte(f.endpoint), nil
		case telemetry.SecretHeadersName:
			if f.headers == "" {
				return nil, telemetry.ErrSecretNotFound
			}
			return []byte(f.headers), nil
		}
		return nil, telemetry.ErrSecretNotFound
	}
}

// newTestExporter builds the exporter with a controllable clock and a
// recording DropFunc. Returns the concrete type for seam access.
func newTestExporter(t *testing.T, secrets *fakeSecrets, client *http.Client) (*exporter, *map[string]int64, *time.Time) {
	t.Helper()
	drops := map[string]int64{}
	var dropMu sync.Mutex
	current := t0
	exp, err := New(telemetry.ExporterConfig{
		NodeID:      "test-node",
		Environment: "test",
		Logger:      zap.NewNop(),
		HTTPClient:  client,
		Secrets:     secrets.src(),
		Dropped: func(_, reason string, n int64) {
			dropMu.Lock()
			drops[reason] += n
			dropMu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e := exp.(*exporter)
	e.now = func() time.Time { return current }
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = e.Close(ctx)
	})
	return e, &drops, &current
}

func counterEvent(name string) telemetry.MetricEvent {
	return telemetry.MetricEvent{
		Tenant: "driplit", Stack: "driplit/www", Src: "http",
		Name: name, Kind: "counter", Value: 1, Time: t0,
		Attrs: map[string]any{"source": "search"},
	}
}

func TestColdStartExportAndHeaders(t *testing.T) {
	srv := newOTLPServer(t)
	secrets := &fakeSecrets{endpoint: srv.URL, headers: "x-api-key=sekrit, x-team = blue"}
	e, drops, _ := newTestExporter(t, secrets, &http.Client{})

	e.Record(context.Background(), "driplit", []telemetry.MetricEvent{
		counterEvent("book.queued"),
		{Tenant: "driplit", Stack: "driplit/www", Src: "http",
			Name: "checkout.duration", Kind: "histogram", Value: 812.5, Unit: "ms", Time: t0},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if srv.count() == 0 {
		t.Fatal("no export reached the tenant endpoint")
	}
	if got := srv.lastHeader("x-api-key"); got != "sekrit" {
		t.Errorf("tenant header x-api-key = %q, want sekrit", got)
	}
	if got := srv.lastHeader("x-team"); got != "blue" {
		t.Errorf("tenant header x-team = %q, want blue (spaces should trim)", got)
	}
	if len(*drops) != 0 {
		t.Errorf("unexpected drops: %v", *drops)
	}
}

// TestNoEnvBleedThrough pins the credential-leak defense: the chassis's
// own OTEL_EXPORTER_OTLP_* env vars must never influence a tenant
// exporter — neither the destination nor the headers.
func TestNoEnvBleedThrough(t *testing.T) {
	tenantSrv := newOTLPServer(t)
	chassisSrv := newOTLPServer(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", chassisSrv.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", chassisSrv.URL+"/v1/metrics")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-chassis-cred=leaked-secret")

	secrets := &fakeSecrets{endpoint: tenantSrv.URL} // no tenant headers
	e, _, _ := newTestExporter(t, secrets, &http.Client{})

	e.Record(context.Background(), "driplit", []telemetry.MetricEvent{counterEvent("book.queued")})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if tenantSrv.count() == 0 {
		t.Fatal("no export reached the tenant endpoint")
	}
	if chassisSrv.count() != 0 {
		t.Errorf("env-derived endpoint received %d exports; explicit endpoint must win", chassisSrv.count())
	}
	if got := tenantSrv.lastHeader("x-chassis-cred"); got != "" {
		t.Errorf("chassis env header leaked to tenant endpoint: %q", got)
	}
}

func TestRotationFlushesOldThenUsesNew(t *testing.T) {
	oldSrv := newOTLPServer(t)
	newSrv := newOTLPServer(t)
	secrets := &fakeSecrets{endpoint: oldSrv.URL}
	e, drops, clock := newTestExporter(t, secrets, &http.Client{})

	e.Record(context.Background(), "driplit", []telemetry.MetricEvent{counterEvent("book.queued")})

	// Rotate the secret and step past the TTL: the next Record must
	// build a fresh provider (flushing the old one to the OLD endpoint)
	// and aggregate into the new one.
	secrets.set(newSrv.URL, "", nil)
	*clock = t0.Add(resolveTTL + time.Second)
	e.Record(context.Background(), "driplit", []telemetry.MetricEvent{counterEvent("book.started")})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if oldSrv.count() == 0 {
		t.Errorf("old endpoint never received the pre-rotation flush")
	}
	if newSrv.count() == 0 {
		t.Errorf("new endpoint never received the post-rotation export")
	}
	if len(*drops) != 0 {
		t.Errorf("rotation should not drop: %v", *drops)
	}
}

func TestNegativeCacheAndTTL(t *testing.T) {
	secrets := &fakeSecrets{} // no endpoint set → ErrSecretNotFound
	e, drops, clock := newTestExporter(t, secrets, &http.Client{})

	e.Record(context.Background(), "driplit", []telemetry.MetricEvent{counterEvent("a")})
	e.Record(context.Background(), "driplit", []telemetry.MetricEvent{counterEvent("b"), counterEvent("c")})

	if (*drops)[dropDisabled] != 3 {
		t.Errorf("disabled drops = %d, want 3", (*drops)[dropDisabled])
	}
	if got := secrets.lookupCount(); got != 1 {
		t.Errorf("endpoint lookups = %d, want 1 (negative cache)", got)
	}

	*clock = t0.Add(resolveTTL + time.Second)
	e.Record(context.Background(), "driplit", []telemetry.MetricEvent{counterEvent("d")})
	if got := secrets.lookupCount(); got != 2 {
		t.Errorf("endpoint lookups after TTL = %d, want 2", got)
	}
}

func TestStaleWhileError(t *testing.T) {
	srv := newOTLPServer(t)
	secrets := &fakeSecrets{endpoint: srv.URL}
	e, drops, clock := newTestExporter(t, secrets, &http.Client{})

	e.Record(context.Background(), "driplit", []telemetry.MetricEvent{counterEvent("a")})

	// Secret store hiccup past the TTL: the cached provider keeps
	// exporting rather than dropping.
	secrets.set(srv.URL, "", errors.New("db locked"))
	*clock = t0.Add(resolveTTL + time.Second)
	e.Record(context.Background(), "driplit", []telemetry.MetricEvent{counterEvent("b")})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if srv.count() == 0 {
		t.Fatal("no export despite healthy cached provider")
	}
	if len(*drops) != 0 {
		t.Errorf("stale-while-error must not drop: %v", *drops)
	}
}

func TestEndpointPolicy(t *testing.T) {
	cases := []struct {
		endpoint string
		ok       bool
	}{
		{"https://ingest.example.com:4318", true},
		{"http://localhost:4318", true},
		{"http://127.0.0.1:4318", true},
		{"http://example.com:4318", false}, // plaintext off-box
		{"https://user:pass@example.com", false},
		{"ftp://example.com", false},
		{"", false},
		{"https://", false}, // no host
	}
	for _, tc := range cases {
		err := validateEndpoint(tc.endpoint)
		if (err == nil) != tc.ok {
			t.Errorf("validateEndpoint(%q) err=%v, want ok=%v", tc.endpoint, err, tc.ok)
		}
	}
}

func TestBadEndpointDropsWithReason(t *testing.T) {
	secrets := &fakeSecrets{endpoint: "http://internal.corp:4318"}
	e, drops, _ := newTestExporter(t, secrets, &http.Client{})

	e.Record(context.Background(), "driplit", []telemetry.MetricEvent{counterEvent("a")})
	if (*drops)[dropBadEndpoint] != 1 {
		t.Errorf("drops = %v, want bad_endpoint=1", *drops)
	}
}

// TestEgressGuardBlocksForbiddenDial proves the SSRF posture end to
// end: under the "private" egress policy a tenant endpoint resolving
// to a loopback/internal address is refused at dial time, the export
// fails, and the failure is visible as an export_error drop.
func TestEgressGuardBlocksForbiddenDial(t *testing.T) {
	srv := newOTLPServer(t) // loopback — exactly what "private" forbids
	guard, err := egress.Open("private", egress.Config{})
	if err != nil {
		t.Fatalf("egress.Open: %v", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 5 * time.Second,
		Control: egress.DialControl(guard)}).DialContext
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	secrets := &fakeSecrets{endpoint: srv.URL}
	e, drops, _ := newTestExporter(t, secrets, client)

	e.Record(context.Background(), "driplit", []telemetry.MetricEvent{counterEvent("a")})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = e.Close(ctx) // flush attempt → guarded dial refuses

	if srv.count() != 0 {
		t.Errorf("guarded client reached a forbidden address (%d requests)", srv.count())
	}
	if (*drops)[dropExportError] == 0 {
		t.Errorf("blocked export not visible in drops: %v", *drops)
	}
}

func TestParseHeaders(t *testing.T) {
	got := parseHeaders("a=1, b = two ,malformed, =nokey, c=x=y")
	want := map[string]string{"a": "1", "b": "two", "c": "x=y"}
	if len(got) != len(want) {
		t.Fatalf("parseHeaders = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("header %q = %q, want %q", k, got[k], v)
		}
	}
}
