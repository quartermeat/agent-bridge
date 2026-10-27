package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProfilesAndCeilings(t *testing.T) {
	profiles, err := loadProfiles()
	if err != nil || len(profiles) != 7 || profiles["claude-epm"].TimeLimit != 3*time.Hour {
		t.Fatalf("profiles: %v %+v", err, profiles)
	}
	for _, bad := range []string{"profiles:\n  test:\n    agent: codex\n    model: x\n    effort: huge\n    time_limit: 1m\n", "profiles:\n  test:\n    agent: codex\n    model: x\n    effort: low\n    time_limit: 0s\n", "profiles:\n  test:\n    agent: codex\n    model: x\n    effort: low\n    mystery: x\n    time_limit: 1m\n"} {
		if _, err := parseProfiles([]byte(bad)); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	r := launchRequest{Role: "coder", Agent: "auto", Profile: "codex-std"}
	for _, body := range []string{`{"verdict":"approve","agent":"codex","role":"coder","profile":"codex-deep","rationale":"x"}`, `{"verdict":"approve","agent":"claude","role":"coder","profile":"claude-deep","rationale":"x"}`} {
		if _, err := parseModelVerdict(body, r, true); err == nil {
			t.Fatalf("accepted upgrade %s", body)
		}
	}
	if _, err := parseModelVerdict(`{"verdict":"approve","agent":"codex","role":"coder","profile":"codex-quick","rationale":"x"}`, r, true); err != nil {
		t.Fatal(err)
	}
	for i, name := range profileOrder {
		if profileRank(name) != i {
			t.Fatalf("routing order %q", name)
		}
	}
	s := Snapshot{Agents: []AgentUsage{{Provider: "claude", ActivityState: "halted_usage"}}}
	if got := substituteProfile("claude-std", s); got != "codex-std" {
		t.Fatalf("substitute = %q", got)
	}
	if got := substituteProfile("claude-epm", s); got != "" {
		t.Fatalf("EPM substitute = %q", got)
	}
}

func TestWorkerTimeLimit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_BRIDGE_STATE_DIR", filepath.Join(home, "state"))
	t.Setenv("AGENT_BRIDGE_CODEX_BIN", filepath.Join(home, "fake-codex"))
	if err := os.WriteFile(os.Getenv("AGENT_BRIDGE_CODEX_BIN"), []byte("#!/bin/sh\nsleep 2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	id := "time-limit-test"
	dir, _ := jobDir(id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("work"), 0600); err != nil {
		t.Fatal(err)
	}
	j := Job{ID: id, Agent: "codex", Role: "coder", Cwd: home, Sandbox: "read-only", TimeLimit: "20ms", EventsPath: filepath.Join(dir, "events.jsonl"), StderrPath: filepath.Join(dir, "stderr.log"), ResultPath: filepath.Join(dir, "result.txt")}
	if err := writeJob(j); err != nil {
		t.Fatal(err)
	}
	if err := worker(id); err != nil {
		t.Fatal(err)
	}
	j, _ = readJob(id)
	if j.Status != "halted" || j.Halt == nil || j.Halt.Code != "time_limit" {
		t.Fatalf("job %+v", j)
	}
}

func TestLocalActivityAndPassiveModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_BRIDGE_HOME", home)
	t.Setenv("AGENT_BRIDGE_ACTIVITY_DIR", filepath.Join(home, "activity"))
	t.Setenv("AGENT_BRIDGE_STATE_DIR", filepath.Join(home, "state"))
	oldRunner, oldClient := runNvidiaSMI, ollamaClient
	t.Cleanup(func() { runNvidiaSMI, ollamaClient = oldRunner, oldClient })
	runNvidiaSMI = func(context.Context, ...string) ([]byte, error) { return nil, os.ErrNotExist }
	ollamaClient = fakeHTTP(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"models":[{"name":"other:latest"}]}`))}, nil
	})
	fakeOllama(t, func(*http.Request) (*http.Response, error) {
		return grokResponse(200, `{"message":{"content":"done"},"prompt_eval_count":3,"eval_count":2}`), nil
	})
	if _, _, _, _, err := ollamaCompletion(context.Background(), "http://localhost:11434", "qwen3:30b", "", "hello"); err != nil {
		t.Fatal(err)
	}
	s, err := collectSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	active, passive := false, false
	for _, a := range s.Agents {
		if a.Provider == "local" && a.Model == "qwen3:30b" && a.UsageKnown && a.Usage.Input == 3 && a.Usage.Output == 2 && a.ActivityState == "idle" {
			active = true
		}
		if a.ID == "local-ollama-other:latest" && a.ActivityState == "loaded" && !a.UsageKnown {
			passive = true
		}
	}
	if !active || !passive {
		b, _ := json.Marshal(s.Agents)
		t.Fatalf("local rows: %s", b)
	}
	f := tokenFeed{Sessions: map[string]int64{}}
	f.addSnapshot(s)
	if f.Local != 5 {
		t.Fatalf("local tokens: %+v", f)
	}
	if err := f.publish(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	read, err := loadTokenFeed()
	if err != nil || read.Local != 5 {
		t.Fatalf("feed: %+v %v", read, err)
	}
}
