package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSnapshotMapsActivityToUsage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_BRIDGE_HOME", home)
	t.Setenv("AGENT_BRIDGE_STATE_DIR", filepath.Join(home, "bridge"))
	activity := filepath.Join(home, "activity")
	t.Setenv("AGENT_BRIDGE_ACTIVITY_DIR", activity)
	if err := os.MkdirAll(activity, 0700); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Format(time.RFC3339)
	claudeID := "11111111-1111-1111-1111-111111111111"
	codexID := "22222222-2222-2222-2222-222222222222"
	fixtures := []struct{ id, name, phase string }{
		{"claude-" + shortHash("claude:"+claudeID), "Claude", "waiting"},
		{"codex-" + shortHash(codexID), "Codex", "tool"},
	}
	for _, f := range fixtures {
		b, _ := json.Marshal(map[string]string{"id": f.id, "name": f.name, "phase": f.phase, "sampledAt": stamp})
		if err := os.WriteFile(filepath.Join(activity, f.id+".json"), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	claudeDir := filepath.Join(home, ".claude", "projects", "project")
	if err := os.MkdirAll(claudeDir, 0700); err != nil {
		t.Fatal(err)
	}
	claudeRows := "{\"type\":\"assistant\",\"sessionId\":\"" + claudeID + "\",\"cwd\":\"/project\",\"message\":{\"id\":\"m1\",\"model\":\"claude-test\",\"usage\":{\"input_tokens\":2,\"cache_read_input_tokens\":10,\"cache_creation_input_tokens\":3,\"output_tokens\":4}}}\n"
	claudeRows += claudeRows // repeated streaming record must count once
	claudeRows += "{\"type\":\"assistant\",\"sessionId\":\"" + claudeID + "\",\"cwd\":\"/project\",\"message\":{\"id\":\"m2\",\"model\":\"claude-test\",\"usage\":{\"input_tokens\":1,\"output_tokens\":5}}}\n"
	if err := os.WriteFile(filepath.Join(claudeDir, claudeID+".jsonl"), []byte(claudeRows), 0600); err != nil {
		t.Fatal(err)
	}
	codexDir := filepath.Join(home, ".codex", "sessions", "2026", "10", "10")
	if err := os.MkdirAll(codexDir, 0700); err != nil {
		t.Fatal(err)
	}
	codexRows := "{\"type\":\"session_meta\",\"payload\":{\"id\":\"" + codexID + "\",\"cwd\":\"/project\"}}\n"
	codexRows += "{\"type\":\"turn_context\",\"payload\":{\"model\":\"gpt-test\"}}\n"
	codexRows += "{\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"info\":{\"total_token_usage\":{\"input_tokens\":20,\"cached_input_tokens\":7,\"output_tokens\":6}}}}\n"
	if err := os.WriteFile(filepath.Join(codexDir, "rollout-2026-10-10T00-00-00-"+codexID+".jsonl"), []byte(codexRows), 0600); err != nil {
		t.Fatal(err)
	}
	jobID := "test-job"
	jobPath := filepath.Join(home, "bridge", "jobs", jobID)
	if err := os.MkdirAll(jobPath, 0700); err != nil {
		t.Fatal(err)
	}
	job := Job{ID: jobID, Status: "running", CreatedAt: time.Now().UTC(), SpawnedBy: &Spawner{AgentID: fixtures[0].id, Provider: "claude", SessionID: claudeID}, EventsPath: filepath.Join(jobPath, "events.jsonl")}
	if err := writeJob(job); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(job.EventsPath, []byte("{\"type\":\"thread.started\",\"thread_id\":\""+codexID+"\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := collectSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Agents) != 2 {
		t.Fatalf("agents = %d, want 2", len(s.Agents))
	}
	if s.Agents[0].Provider != "claude" || s.Agents[0].Usage.Input != 16 || s.Agents[0].Usage.Output != 9 || s.Agents[0].Model != "claude-test" {
		t.Fatalf("Claude usage: %+v", s.Agents[0])
	}
	if s.Agents[1].Provider != "codex" || s.Agents[1].Usage.Input != 20 || s.Agents[1].Usage.CacheRead != 7 || s.Agents[1].Model != "gpt-test" {
		t.Fatalf("Codex usage: %+v", s.Agents[1])
	}
	if len(s.Jobs) != 1 || s.Jobs[0].SessionID != codexID || s.Agents[1].SpawnedBy == nil || s.Agents[1].SpawnedBy.AgentID != fixtures[0].id {
		t.Fatalf("spawn relationship missing: jobs=%+v agent=%+v", s.Jobs, s.Agents[1])
	}
	if s.Agents[0].ActivityState != "has_running_children" || len(s.Agents[0].ActiveJobIDs) != 1 || s.Agents[1].ActivityState != "running_job" {
		t.Fatalf("incorrect job-aware states: Claude=%+v Codex=%+v", s.Agents[0], s.Agents[1])
	}
}

func TestQuietHookDoesNotClaimReadiness(t *testing.T) {
	a := AgentUsage{Phase: "waiting", Detail: "ready for direction", ActiveJobIDs: []string{}}
	classifyAgent(&a, nil)
	if a.ActivityState != "unverified" || a.StateDetail == "ready for direction" {
		t.Fatalf("quiet hook claimed readiness: %+v", a)
	}
	a = AgentUsage{ID: "codex-worker", Provider: "codex", SessionID: "worker-session", Phase: "waiting", Detail: "approval requested for Bash"}
	classifyAgent(&a, []Job{{ID: "job", SessionID: "worker-session", Status: "running"}})
	if a.ActivityState != "approval_required" || len(a.ActiveJobIDs) != 1 {
		t.Fatalf("approval wait hidden by running job: %+v", a)
	}
	a = AgentUsage{Phase: "waiting", Detail: "ready for direction", Halt: &Halt{Kind: "context_limit", Code: "context_length_exceeded"}}
	classifyAgent(&a, nil)
	if a.ActivityState != "halted_usage" {
		t.Fatalf("usage halt hidden by quiet hook: %+v", a)
	}
}

func TestClaudeUsageErrorClearsAfterSuccessfulResponse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.jsonl")
	errorRow := `{"type":"system","subtype":"api_error","timestamp":"2026-10-10T20:00:00Z","error":{"code":"usage_limit_exceeded","message":"usage limit reached"}}` + "\n"
	if err := os.WriteFile(path, []byte(errorRow), 0600); err != nil {
		t.Fatal(err)
	}
	a := AgentUsage{}
	if err := readClaudeUsage(path, &a); err != nil || a.Halt == nil || a.Halt.Kind != "usage_limit" {
		t.Fatalf("Claude usage halt missing: %+v %v", a, err)
	}
	success := `{"type":"assistant","sessionId":"session","cwd":"/project","message":{"id":"m1","model":"claude-test","usage":{"output_tokens":1}}}` + "\n"
	if err := os.WriteFile(path, []byte(errorRow+success), 0600); err != nil {
		t.Fatal(err)
	}
	a = AgentUsage{}
	if err := readClaudeUsage(path, &a); err != nil || a.Halt != nil {
		t.Fatalf("resolved Claude error remained active: %+v %v", a, err)
	}
}
