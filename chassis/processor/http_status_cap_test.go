package processor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
)

// answerWith serves one fixed status and body.
func answerWith(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func execHTTP(t *testing.T, srv *httptest.Server, conf config.Config, meta string) (string, error) {
	t.Helper()
	pu := &Unit{Logger: zap.NewNop(), HTTPClient: srv.Client(), Conf: conf}
	op := operation.Operation{
		Resonator: &resonator.Resonator{Exec: srv.URL},
		Meta:      meta,
		Input:     `{}`,
	}
	payload, err := pu.ExecHTTP(context.Background(), op)
	if err != nil {
		return payload.Meta, err
	}
	return payload.Raw, nil
}

// Without `WITH status_into` the answer is exactly what the server sent,
// whatever its status: a 4xx/5xx still merges as it always has.
func TestExecHTTP_NoStatusIntoLeavesAnswerAlone(t *testing.T) {
	for _, status := range []int{200, 404, 500} {
		body := `{"error":{"message":"nope"},"n":1}`
		got, err := execHTTP(t, answerWith(t, status, body), config.Config{}, `{"method":"GET"}`)
		if err != nil {
			t.Fatalf("status %d: ExecHTTP: %v", status, err)
		}
		if got != body {
			t.Errorf("status %d: answer = %s, want it unchanged: %s", status, got, body)
		}
	}
}

// `WITH status_into` writes the status code, an integer, beside the answer.
func TestExecHTTP_StatusInto(t *testing.T) {
	got, err := execHTTP(t, answerWith(t, 500, `{"error":"boom"}`), config.Config{},
		`{"method":"GET","status_into":"_res_status"}`)
	if err != nil {
		t.Fatalf("ExecHTTP: %v", err)
	}
	if v := gjson.Get(got, "_res_status"); v.Type != gjson.Number || v.Int() != 500 {
		t.Errorf("_res_status = %s, want the number 500 (answer %s)", v.Raw, got)
	}
	if gjson.Get(got, "error").String() != "boom" {
		t.Errorf("the body was lost: %s", got)
	}
}

// With `into`, the body nests under its key and the status sits beside it.
func TestExecHTTP_StatusIntoWithInto(t *testing.T) {
	got, err := execHTTP(t, answerWith(t, 404, `{"why":"missing"}`), config.Config{},
		`{"method":"GET","into":"_res","status_into":"_res_status"}`)
	if err != nil {
		t.Fatalf("ExecHTTP: %v", err)
	}
	if gjson.Get(got, "_res.why").String() != "missing" || gjson.Get(got, "_res_status").Int() != 404 {
		t.Errorf("answer = %s, want {_res:{why:missing}, _res_status:404}", got)
	}
}

// An answer that is not a JSON object (a proxy's HTML error page, say) could
// not merge anyway; with status_into the status is what comes back.
func TestExecHTTP_StatusIntoNonObjectAnswer(t *testing.T) {
	for _, meta := range []string{
		`{"method":"GET","status_into":"_st"}`,
		`{"method":"GET","into":"_res","status_into":"_st"}`,
	} {
		got, err := execHTTP(t, answerWith(t, 502, `<html>Bad Gateway</html>`), config.Config{}, meta)
		if err != nil {
			t.Fatalf("%s: ExecHTTP: %v", meta, err)
		}
		if got != `{"_st":502}` {
			t.Errorf("%s: answer = %s, want {\"_st\":502}", meta, got)
		}
	}
}

// An answer over --op-payload-max is refused, not cut and merged.
func TestExecHTTP_AnswerOverPayloadMax(t *testing.T) {
	big := `{"pad":"` + strings.Repeat("x", 64) + `"}`
	meta, err := execHTTP(t, answerWith(t, 200, big), config.Config{OpPayloadMax: 32}, `{"method":"GET"}`)
	if !errors.Is(err, errOpAnswerTooLarge) {
		t.Fatalf("err = %v, want errOpAnswerTooLarge", err)
	}
	if gjson.Get(meta, "error.0").String() != "http-response-too-large" {
		t.Errorf("meta = %s, want error [http-response-too-large]", meta)
	}
}

func TestReadOpAnswerLimit(t *testing.T) {
	if b, err := readOpAnswer(strings.NewReader("12345678"), 8); err != nil || string(b) != "12345678" {
		t.Errorf("exactly at the limit: %q, %v; want the bytes and no error", b, err)
	}
	if b, err := readOpAnswer(strings.NewReader("123456789"), 8); !errors.Is(err, errOpAnswerTooLarge) || b != nil {
		t.Errorf("one byte over: %q, %v; want no bytes and errOpAnswerTooLarge", b, err)
	}
	// Unset means the 4 MiB default, not "no limit".
	if _, err := readOpAnswer(strings.NewReader(strings.Repeat("x", 4<<20+1)), 0); !errors.Is(err, errOpAnswerTooLarge) {
		t.Errorf("unset limit: err = %v, want errOpAnswerTooLarge past 4 MiB", err)
	}
}
