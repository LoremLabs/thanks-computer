package continuation_test

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/continuation"
	"github.com/loremlabs/thanks-computer/chassis/continuation/filestore"
)

func newStore(t *testing.T) continuation.Store {
	t.Helper()
	s, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatalf("filestore.New: %v", err)
	}
	return s
}

func TestCreateIfAbsent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if _, err := s.Create(ctx, "a/b/c.json", bytes.NewReader([]byte(`{"x":1}`)), continuation.Meta{}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	// Second create at the same key must be ErrExists and must NOT
	// overwrite (immutability).
	if _, err := s.Create(ctx, "a/b/c.json", bytes.NewReader([]byte(`{"x":2}`)), continuation.Meta{}); err != continuation.ErrExists {
		t.Fatalf("second Create err = %v, want ErrExists", err)
	}
	b, _, err := s.Get(ctx, "a/b/c.json")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(b) != `{"x":1}` {
		t.Fatalf("content = %s, want original (immutable)", b)
	}
	if ok, _ := s.Exists(ctx, "a/b/c.json"); !ok {
		t.Fatal("Exists = false, want true")
	}
	if _, _, err := s.Get(ctx, "missing"); err != continuation.ErrNotFound {
		t.Fatalf("Get missing err = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "a/b/c.json"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete(ctx, "a/b/c.json"); err != nil {
		t.Fatalf("Delete absent should be nil, got %v", err)
	}
}

// Exactly one of N concurrent Creates on the same key wins; this is the
// entire coordination model (resume-claim, op-terminal).
func TestCreateRaceExactlyOneWinner(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	const n = 32
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, exists := 0, 0
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, err := s.Create(ctx, "claim.json", bytes.NewReader([]byte(`{}`)), continuation.Meta{})
			mu.Lock()
			switch err {
			case nil:
				wins++
			case continuation.ErrExists:
				exists++
			default:
				t.Errorf("unexpected err: %v", err)
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if wins != 1 || exists != n-1 {
		t.Fatalf("wins=%d exists=%d, want 1 and %d", wins, exists, n-1)
	}
}

func TestRunsDerivedStateLifecycle(t *testing.T) {
	ctx := context.Background()
	r := continuation.NewRuns(newStore(t))

	runID, rcid, err := r.CreateRun(ctx, "tenantA", "website", "v1", "website/100", "", time.Time{})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	manifest := []continuation.OpManifestEntry{
		{Ordinal: 0, Op: "research", Async: true},
		{Ordinal: 1, Op: "sidecar", Async: false},
	}
	if err := r.SuspendStage(ctx, runID, "website/100", `{"_txc":{}}`, "v1", manifest); err != nil {
		t.Fatalf("SuspendStage: %v", err)
	}
	// Re-suspend is idempotent (ErrExists swallowed).
	if err := r.SuspendStage(ctx, runID, "website/100", `{"_txc":{}}`, "v1", manifest); err != nil {
		t.Fatalf("SuspendStage idempotent: %v", err)
	}

	st, err := r.StageState(ctx, runID, "website/100", manifest)
	if err != nil || st != continuation.StateWaiting {
		t.Fatalf("state=%q err=%v, want waiting", st, err)
	}

	// One op terminal → still waiting.
	if rec, err := r.RecordTerminal(ctx, runID, "website/100", 1, "sidecar", "completed", []byte(`{"ok":true}`)); err != nil || !rec {
		t.Fatalf("RecordTerminal sidecar rec=%v err=%v", rec, err)
	}
	if st, _ := r.StageState(ctx, runID, "website/100", manifest); st != continuation.StateWaiting {
		t.Fatalf("state=%q, want waiting (one op outstanding)", st)
	}
	// Duplicate terminal → recorded=false, no error (idempotent).
	if rec, err := r.RecordTerminal(ctx, runID, "website/100", 1, "sidecar", "completed", []byte(`{"ok":false}`)); err != nil || rec {
		t.Fatalf("duplicate RecordTerminal rec=%v err=%v, want false,nil", rec, err)
	}

	// Last op terminal → resumable.
	if _, err := r.RecordTerminal(ctx, runID, "website/100", 0, "research", "completed", []byte(`{"sum":"x"}`)); err != nil {
		t.Fatalf("RecordTerminal research: %v", err)
	}
	if st, _ := r.StageState(ctx, runID, "website/100", manifest); st != continuation.StateResumable {
		t.Fatalf("state=%q, want resumable", st)
	}

	// Exactly one resumer.
	won1, _ := r.ClaimResume(ctx, runID, "website/100")
	won2, _ := r.ClaimResume(ctx, runID, "website/100")
	if !won1 || won2 {
		t.Fatalf("ClaimResume won1=%v won2=%v, want true,false", won1, won2)
	}

	if err := r.WriteResult(ctx, runID, []byte(`{"done":true}`)); err != nil {
		t.Fatalf("WriteResult: %v", err)
	}
	if st, _ := r.StageState(ctx, runID, "website/100", manifest); st != continuation.StateCompleted {
		t.Fatalf("state=%q, want completed (result precedence)", st)
	}

	// Client GET path: derived run state via current stage.
	rs, err := r.RunState(ctx, runID)
	if err != nil || rs != continuation.StateCompleted {
		t.Fatalf("RunState=%q err=%v, want completed", rs, err)
	}

	// O(1) lookup resolution.
	gotRun, err := r.ResolveRunContinuation(ctx, rcid)
	if err != nil || gotRun != runID {
		t.Fatalf("ResolveRunContinuation=%q err=%v, want %q", gotRun, err, runID)
	}
}

func TestStageFailedDerivation(t *testing.T) {
	ctx := context.Background()
	r := continuation.NewRuns(newStore(t))
	runID, _, err := r.CreateRun(ctx, "", "s", "v1", "s/0", "", time.Time{})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	manifest := []continuation.OpManifestEntry{{Ordinal: 0, Op: "x", Async: true}}
	if err := r.SuspendStage(ctx, runID, "s/0", `{}`, "v1", manifest); err != nil {
		t.Fatalf("SuspendStage: %v", err)
	}
	if err := r.FailStage(ctx, runID, "s/0", "STACK_VERSION_CHANGED"); err != nil {
		t.Fatalf("FailStage: %v", err)
	}
	if st, _ := r.StageState(ctx, runID, "s/0", manifest); st != continuation.StateFailed {
		t.Fatalf("state=%q, want failed", st)
	}
}

func TestCreateOpRecordsAndOpContinuationLookup(t *testing.T) {
	ctx := context.Background()
	r := continuation.NewRuns(newStore(t))
	runID, _, err := r.CreateRun(ctx, "", "s", "v1", "s/0", "", time.Time{})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	token, hash, err := continuation.MintToken()
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	if continuation.HashToken(token) != hash {
		t.Fatal("HashToken mismatch")
	}
	specs := []continuation.OpRecordSpec{
		{Ordinal: 0, Op: "research", Async: true, OpContinuationID: "opc_abc", TokenHash: hash, Input: []byte(`{"topic":"x"}`)},
		{Ordinal: 1, Op: "plain", Async: false, Input: []byte(`{}`)},
	}
	if err := r.CreateOpRecords(ctx, runID, "s/0", specs); err != nil {
		t.Fatalf("CreateOpRecords: %v", err)
	}
	// Re-create is idempotent.
	if err := r.CreateOpRecords(ctx, runID, "s/0", specs); err != nil {
		t.Fatalf("CreateOpRecords idempotent: %v", err)
	}
	lk, err := r.ResolveOpContinuation(ctx, "opc_abc")
	if err != nil {
		t.Fatalf("ResolveOpContinuation: %v", err)
	}
	if lk.RunID != runID || lk.Stage != "s/0" || lk.Op != "research" || lk.Ordinal != 0 || lk.TokenHash != hash {
		t.Fatalf("lookup mismatch: %+v", lk)
	}
}

func TestTraceLinks(t *testing.T) {
	ctx := context.Background()
	r := continuation.NewRuns(newStore(t))

	runID, rcid, err := r.CreateRun(ctx, "tnt", "site", "v1", "site/100", "Cb4origREQ", time.Time{})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	for _, st := range []string{"site/100", "site/300"} {
		if err := r.SuspendStage(ctx, runID, st, `{}`, "v1",
			[]continuation.OpManifestEntry{{Ordinal: 0, Op: "a", Async: true}}); err != nil {
			t.Fatalf("SuspendStage %s: %v", st, err)
		}
	}

	// rid → run reverse lookup.
	gotRun, err := r.ResolveRequestContinuation(ctx, "Cb4origREQ")
	if err != nil || gotRun != runID {
		t.Fatalf("ResolveRequestContinuation=%q err=%v want %q", gotRun, err, runID)
	}

	tl, err := r.ReadTraceLinks(ctx, runID)
	if err != nil {
		t.Fatalf("ReadTraceLinks: %v", err)
	}
	if tl.OriginRID != "Cb4origREQ" || tl.RunContinuationID != rcid {
		t.Fatalf("links origin/rcid mismatch: %+v (rcid=%s)", tl, rcid)
	}
	if len(tl.Resumes) != 2 {
		t.Fatalf("resumes=%d want 2: %+v", len(tl.Resumes), tl.Resumes)
	}
	for _, rr := range tl.Resumes {
		if rr.RID != continuation.ResumeTraceRID(runID, rr.Stage) {
			t.Fatalf("resume RID %q != ResumeTraceRID(%q,%q)", rr.RID, runID, rr.Stage)
		}
		gotID, ok := continuation.ParseResumeTraceRID(rr.RID)
		if !ok || gotID != runID {
			t.Fatalf("ParseResumeTraceRID(%q)=%q,%v want %q,true", rr.RID, gotID, ok, runID)
		}
		if strings.ContainsAny(rr.RID, "/. ") {
			t.Fatalf("resume RID not path/validRID-safe: %q", rr.RID)
		}
	}
	// A normal rid (not a resume, no suspend) → no link.
	if _, ok := continuation.ParseResumeTraceRID("Cb4normalRID"); ok {
		t.Fatal("ParseResumeTraceRID matched a normal rid")
	}
	if _, e := r.ResolveRequestContinuation(ctx, "Cb4normalRID"); e != continuation.ErrNotFound {
		t.Fatalf("ResolveRequestContinuation(unknown) err=%v want ErrNotFound", e)
	}
}
