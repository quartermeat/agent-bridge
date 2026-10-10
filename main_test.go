package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStartAndObserve(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "agent-bridge")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	fake := filepath.Join(tmp, "fake-codex")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ncat >/dev/null\nprintf '{\"type\":\"turn.completed\"}\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(tmp, "state")
	start := exec.Command(bin, "start", "--cwd", tmp, "--name", "smoke")
	start.Env = append(os.Environ(), "AGENT_BRIDGE_STATE_DIR="+state, "AGENT_BRIDGE_CODEX_BIN="+fake, "AGENT_BRIDGE_LAUNCH_MODE=direct")
	start.Stdin = strings.NewReader("Report status.")
	out, err := start.CombinedOutput()
	if err != nil {
		t.Fatalf("start: %v: %s", err, out)
	}
	id := strings.TrimSpace(string(out))
	wait := exec.Command(bin, "wait", "--interval", "100ms", id)
	wait.Env = start.Env
	waitOut, err := wait.CombinedOutput()
	if err != nil {
		t.Fatalf("wait: %v: %s", err, waitOut)
	}
	var waited []Job
	if err := json.Unmarshal(waitOut, &waited); err != nil || len(waited) != 1 || waited[0].Status != "completed" {
		t.Fatalf("wait result: %v: %s", err, waitOut)
	}
	var job Job
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		show := exec.Command(bin, "show", id)
		show.Env = start.Env
		b, err := show.Output()
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &job); err != nil {
			t.Fatal(err)
		}
		if job.Status == "completed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if job.Status != "completed" {
		t.Fatalf("status = %q, error = %q", job.Status, job.Error)
	}
	if job.ExitCode == nil || *job.ExitCode != 0 {
		t.Fatalf("exit code = %v", job.ExitCode)
	}
	if _, err := os.Stat(filepath.Join(state, "jobs", id, "prompt.txt")); !os.IsNotExist(err) {
		t.Fatalf("prompt file retained: %v", err)
	}
	b, err := os.ReadFile(job.EventsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "turn.completed") {
		t.Fatalf("missing event: %s", b)
	}
}
