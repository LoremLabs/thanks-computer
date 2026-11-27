package home

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

const testID = "atc-0123456789abcdef0123456789abcdef"

func testCred(t *testing.T) Credential {
	t.Helper()
	return Credential{
		ID: testID, Address: "https://attach.example.com", Root: t.TempDir(),
		Seed: make([]byte, ed25519.SeedSize), CreatedAt: time.Now().UTC(), WhileRunning: true,
	}
}

// otherProcess holds id's lock as another process would: on a file of its
// own opening.
func otherProcess(t *testing.T, h Home, id string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(h.CredentialsDir(), id+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
}

func TestWhileRunningSaved(t *testing.T) {
	h := Home{Dir: t.TempDir()}
	if err := h.Save(testCred(t)); err != nil {
		t.Fatal(err)
	}
	if c, ok := h.Get(testID); !ok || !c.WhileRunning {
		t.Errorf("read back %+v, %v", c, ok)
	}
}

func TestHold(t *testing.T) {
	h := Home{Dir: t.TempDir()}
	if err := h.Hold(testID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Release(testID) })
	if !h.Holds(testID) {
		t.Error("held, but Holds says no")
	}
	// A second holder, in this process or another, is refused.
	if h.Hold(testID) == nil {
		t.Error("held twice")
	}
	if h.Left(testID) {
		t.Error("this process holds it, yet it reads as left")
	}
	h.Release(testID)
	if h.Holds(testID) {
		t.Error("released, but Holds says yes")
	}
	if !h.Left(testID) {
		t.Error("nobody holds it, yet it does not read as left")
	}
	if h.Holds(testID) {
		t.Error("Left kept the lock")
	}
	if h.Hold("../x") == nil {
		t.Error("held a path")
	}
}

func TestHeldByAnother(t *testing.T) {
	h := Home{Dir: t.TempDir()}
	if err := h.Save(testCred(t)); err != nil {
		t.Fatal(err)
	}
	otherProcess(t, h, testID)
	if h.Left(testID) || h.Hold(testID) == nil || h.Holds(testID) {
		t.Error("another process's lock was not respected")
	}
}

func TestRemove(t *testing.T) {
	h := Home{Dir: t.TempDir()}
	if err := h.Hold(testID); err != nil {
		t.Fatal(err)
	}
	if err := h.Save(testCred(t)); err != nil {
		t.Fatal(err)
	}
	if err := h.Remove(testID); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.Get(testID); ok {
		t.Error("still there")
	}
	if _, err := os.Stat(filepath.Join(h.CredentialsDir(), testID+".lock")); !os.IsNotExist(err) {
		t.Errorf("lock file: %v", err)
	}
	if h.Holds(testID) {
		t.Error("still held")
	}
	if creds, err := h.Load(); err != nil || len(creds) != 0 {
		t.Errorf("load: %+v, %v", creds, err)
	}
	if err := h.Remove(testID); err != nil {
		t.Errorf("removing what is gone: %v", err)
	}
}
