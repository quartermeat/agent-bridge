package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFallbackHaltReconcilesOnlyAfterLinkedCompletion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_BRIDGE_HOME", home)
	t.Setenv("AGENT_BRIDGE_STATE_DIR", filepath.Join(home, "bridge"))
	t.Setenv("AGENT_BRIDGE_ACTIVITY_DIR", filepath.Join(home, "activity"))
	created := time.Now().UTC().Add(-2 * time.Hour)
	halt := &Halt{Kind: "usage_limit", Code: "usage_limit_exceeded", DetectedAt: created}
	for _, j := range []Job{
		{ID: "original", Status: "halted", Halt: halt, CreatedAt: created},
		{ID: "unrelated", Status: "completed", CreatedAt: created.Add(time.Minute)},
		{ID: "replacement", Status: "running", FallbackFor: "original", CreatedAt: created.Add(2 * time.Minute)},
	} {
		dir, _ := jobDir(j.ID)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := writeJob(j); err != nil {
			t.Fatal(err)
		}
	}
	assertOpen := func() {
		t.Helper()
		s, err := collectSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		j, err := readJob("original")
		if err != nil || j.AcknowledgedAt != nil {
			t.Fatalf("unresolved halt was cleared: %+v, %v", j, err)
		}
		found := false
		for _, shown := range s.Jobs {
			found = found || shown.ID == "original"
		}
		if !found {
			t.Fatal("unresolved halt missing from snapshot")
		}
	}
	assertOpen()
	for _, status := range []string{"queued", "failed"} {
		j, _ := readJob("replacement")
		j.Status = status
		if err := writeJob(j); err != nil {
			t.Fatal(err)
		}
		assertOpen()
	}
	j, _ := readJob("replacement")
	j.Status = "completed"
	if err := writeJob(j); err != nil {
		t.Fatal(err)
	}
	s, err := collectSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	original, err := readJob("original")
	if err != nil || original.AcknowledgedAt == nil || original.Halt == nil || original.Status != "halted" {
		t.Fatalf("resolved halt history: %+v, %v", original, err)
	}
	for _, shown := range s.Jobs {
		if shown.ID == "original" && shown.AcknowledgedAt == nil {
			t.Fatalf("snapshot kept alert: %+v", shown)
		}
	}
	if strings.Contains(strings.Join(renderWatchFrame(s, "test", map[string]TokenUsage{}), "\n"), "JOB original HALTED") {
		t.Fatal("resolved halt still rendered as an alert")
	}
	// Simulate simultaneous monitor samples that each saw the same open halt.
	original.AcknowledgedAt = nil
	if err := writeJob(original); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			jobs := map[string]Job{"original": original, "replacement": j}
			errs <- reconcileFallbackHalts(jobs)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	original, err = readJob("original")
	if err != nil || original.AcknowledgedAt == nil {
		t.Fatalf("concurrent acknowledgement missing: %+v, %v", original, err)
	}
	acknowledgedAt := *original.AcknowledgedAt
	if _, err := collectSnapshot(); err != nil {
		t.Fatal(err)
	}
	original, err = readJob("original")
	if err != nil || original.AcknowledgedAt == nil || !original.AcknowledgedAt.Equal(acknowledgedAt) {
		t.Fatalf("acknowledgement changed on repeat: %+v, %v", original, err)
	}
}

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
	claudeRows += "{\"type\":\"ai-title\",\"aiTitle\":\"Older title\"}\n"
	claudeRows += "{\"type\":\"ai-title\",\"aiTitle\":\"Widget monitoring\"}\n"
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
	index := "{\"id\":\"" + codexID + "\",\"thread_name\":\"Old task\"}\n"
	index += "{\"id\":\"" + codexID + "\",\"thread_name\":\"activity-hooks\"}\n"
	if err := os.WriteFile(filepath.Join(home, ".codex", "session_index.jsonl"), []byte(index), 0600); err != nil {
		t.Fatal(err)
	}
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
	if s.Agents[0].Task != "Widget monitoring" || s.Agents[1].Task != "activity-hooks" {
		t.Fatalf("tasks: Claude=%q Codex=%q", s.Agents[0].Task, s.Agents[1].Task)
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
	a.ReportOrigin = "watchdog"
	classifyAgent(&a, nil)
	if a.ActivityState != "unverified" {
		t.Fatalf("watchdog claimed readiness: %+v", a)
	}
	a.ReportOrigin = "event"
	classifyAgent(&a, nil)
	if a.ActivityState != "ready" {
		t.Fatalf("completed turn was not ready: %+v", a)
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

func TestOldWaitingReportIsNotCurrent(t *testing.T) {
	now := time.Now().UTC()
	_, current := activityCurrent(filepath.Join(t.TempDir(), "session.json"), activityFile{
		Phase: "waiting", SampledAt: now.Add(-2 * time.Hour),
	}, now)
	if current {
		t.Fatal("old waiting report remained visible")
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

func TestSafeTask(t *testing.T) {
	got := safeTask("  Title\nwith\x00 control\ttext  ")
	if got != "Title with control text" {
		t.Fatalf("safeTask = %q", got)
	}
	long := safeTask(strings.Repeat("x", 300))
	if len(long) > 120 {
		t.Fatalf("task length = %d", len(long))
	}
}

func TestActivityCurrentDelayedWatcher(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-test.json")
	script := filepath.Join(dir, "claude-activity-hook.py")
	if err := os.WriteFile(script, []byte("sleep 10\n"), 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	a := activityFile{ID: "claude-test", Phase: "working", SampledAt: now.Add(-20 * time.Second)}
	if _, ok := activityCurrent(path, a, now); ok {
		t.Fatal("delayed heartbeat accepted without watcher")
	}
	cmd := exec.Command("sh", script, "--watch", path, strconv.Itoa(os.Getpid()), a.ID)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if err := os.WriteFile(strings.TrimSuffix(path, ".json")+".watch.pid", []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}
	var host int
	var ok bool
	for attempt := 0; attempt < 20; attempt++ {
		host, ok = activityCurrent(path, a, now)
		if ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ok || host != os.Getpid() {
		t.Fatalf("validated watcher rejected: host=%d ok=%v", host, ok)
	}
	deadHost := exec.Command("sh", script, "--watch", path, "99999999", a.ID)
	if err := deadHost.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deadHost.Process.Kill(); _ = deadHost.Wait() }()
	if err := os.WriteFile(strings.TrimSuffix(path, ".json")+".watch.pid", []byte(strconv.Itoa(deadHost.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok := activityCurrent(path, a, now); ok {
		t.Fatal("dead host accepted")
	}
	a.SampledAt = now.Add(-31 * time.Second)
	if _, ok := activityCurrent(path, a, now); ok {
		t.Fatal("stale watcher accepted")
	}
}
