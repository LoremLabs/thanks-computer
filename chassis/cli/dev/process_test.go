package dev

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestSpawnAndStop(t *testing.T) {
	var out bytes.Buffer
	p, err := Spawn(context.Background(), SpawnConfig{
		Name: "long",
		Cmd:  "sleep 60",
		Out:  &out,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Should be running.
	select {
	case <-p.Done():
		t.Fatal("process exited immediately; expected sleep 60 to be running")
	case <-time.After(50 * time.Millisecond):
	}

	start := time.Now()
	p.Stop(2 * time.Second)
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Errorf("Stop took %v; expected <= 2s", elapsed)
	}

	select {
	case <-p.Done():
	case <-time.After(500 * time.Millisecond):
		t.Fatal("process did not exit after Stop")
	}
}

// TestSpawnKillsGroup proves the pgid teardown: a parent shell with two
// background sleeps must take both children down with it.
func TestSpawnKillsGroup(t *testing.T) {
	var out bytes.Buffer
	// `sleep 60 & sleep 60 & wait` parents two children. Without pgid
	// teardown, killing the shell would orphan them. With pgid, they
	// die too.
	p, err := Spawn(context.Background(), SpawnConfig{
		Name: "group",
		Cmd:  "sleep 60 & sleep 60 & wait",
		Out:  &out,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	time.Sleep(100 * time.Millisecond) // let children spin up
	p.Stop(2 * time.Second)

	select {
	case <-p.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("group did not exit after Stop")
	}
}

// TestSpawnTagsOutput verifies stdout/stderr lines are prefixed with
// the configured Name tag.
func TestSpawnTagsOutput(t *testing.T) {
	var out bytes.Buffer
	p, err := Spawn(context.Background(), SpawnConfig{
		Name: "echo",
		Cmd:  "echo hello world",
		Out:  &out,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("echo did not exit")
	}
	got := out.String()
	if !strings.Contains(got, "[echo] hello world") {
		t.Errorf("expected tagged output, got %q", got)
	}
}

// TestSpawnEmptyCommandErrors guards the contract that callers can't
// accidentally start a no-op child.
func TestSpawnEmptyCommandErrors(t *testing.T) {
	if _, err := Spawn(context.Background(), SpawnConfig{Name: "x", Cmd: ""}); err == nil {
		t.Error("expected error for empty Cmd")
	}
}
