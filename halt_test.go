package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUsageHaltClassification(t *testing.T) {
	at := time.Now().UTC()
	for _, tc := range []struct{ code, message, kind string }{
		{"context_length_exceeded", "", "context_limit"},
		{"session_budget_exceeded", "", "usage_limit"},
		{"credit_balance_exhausted", "", "usage_limit"},
		{"rate_limit_exceeded", "", "rate_limit"},
		{"", "You've hit your usage limit", "usage_limit"},
	} {
		got := classifyUsageError(tc.code, tc.message, "test", at)
		if got == nil || got.Kind != tc.kind {
			t.Fatalf("classify %q %q = %+v, want %s", tc.code, tc.message, got, tc.kind)
		}
	}
	if got := classifyUsageError("server_overloaded", "temporary failure", "test", at); got != nil {
		t.Fatalf("unrelated failure classified as token halt: %+v", got)
	}
	code, message := errorFields([]byte(`{"formatted":"You've hit your usage limit"}`))
	if got := classifyUsageError(code, message, "test", at); got == nil || got.Kind != "usage_limit" {
		t.Fatalf("formatted provider error missed: %+v", got)
	}
}

func TestWorkerRecordsHaltUntilAcknowledged(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_BRIDGE_STATE_DIR", dir)
	fake := filepath.Join(dir, "fake-codex")
	content := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"failed-session\"}' '{\"type\":\"turn.failed\",\"error\":{\"code\":\"session_budget_exceeded\",\"message\":\"budget reached\"}}'\nexit 1\n"
	if err := os.WriteFile(fake, []byte(content), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_BRIDGE_CODEX_BIN", fake)
	id := "halt-test"
	jobDirPath := filepath.Join(dir, "jobs", id)
	if err := os.MkdirAll(jobDirPath, 0700); err != nil {
		t.Fatal(err)
	}
	j := Job{ID: id, Cwd: dir, Sandbox: "read-only", Status: "queued", CreatedAt: time.Now().UTC(), EventsPath: filepath.Join(jobDirPath, "events.jsonl"), StderrPath: filepath.Join(jobDirPath, "stderr.log"), ResultPath: filepath.Join(jobDirPath, "result.txt")}
	if err := writeJob(j); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDirPath, "prompt.txt"), []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := worker(id); err != nil {
		t.Fatal(err)
	}
	got, err := readJob(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "halted" || got.Halt == nil || got.Halt.Kind != "usage_limit" {
		t.Fatalf("job did not record usage halt: %+v", got)
	}
	if got.AcknowledgedAt != nil {
		t.Fatal("halt was acknowledged without a handoff")
	}
	if err := ack([]string{id}); err != nil {
		t.Fatal(err)
	}
	got, err = readJob(id)
	if err != nil || got.AcknowledgedAt == nil {
		t.Fatalf("acknowledgement missing: %+v %v", got, err)
	}
}

func TestToolOutputAndRecoveredErrorDoNotHaltJob(t *testing.T) {
	dir := t.TempDir()
	events := filepath.Join(dir, "events.jsonl")
	stderr := filepath.Join(dir, "stderr.log")
	content := strings.Join([]string{
		`{"type":"item.completed","item":{"type":"command_execution","aggregated_output":"usage limit reached"}}`,
		`{"type":"error","message":"rate limit exceeded"}`,
		`{"type":"turn.completed"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(events, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stderr, []byte("warning: usage limit reached in test fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := detectJobHalt(events, stderr, time.Now(), 0); got != nil {
		t.Fatalf("successful turn falsely halted: %+v", got)
	}
	if err := os.WriteFile(events, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stderr, []byte("You've hit your usage limit.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := detectJobHalt(events, stderr, time.Now(), 1); got == nil || got.Kind != "usage_limit" {
		t.Fatalf("fatal stderr limit missed: %+v", got)
	}
}
