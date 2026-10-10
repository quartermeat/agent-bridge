package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPublishedSnapshotIsPrivateAndRejectsStaleData(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	t.Setenv("AGENT_BRIDGE_STATE_DIR", dir)
	s := Snapshot{Version: 1, SampledAt: time.Now().UTC(), Agents: []AgentUsage{}, Jobs: []Job{}, Sources: []SourceStatus{}}
	if err := publishSnapshot(s); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "latest.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("snapshot mode = %o, want 600", info.Mode().Perm())
	}
	got, err := readLatest(10 * time.Second)
	if err != nil || got.Version != 1 {
		t.Fatalf("fresh snapshot: %+v, %v", got, err)
	}
	s.SampledAt = time.Now().Add(-time.Minute)
	if err := publishSnapshot(s); err != nil {
		t.Fatal(err)
	}
	if _, err := readLatest(10 * time.Second); err == nil {
		t.Fatal("stale snapshot accepted")
	}
}
