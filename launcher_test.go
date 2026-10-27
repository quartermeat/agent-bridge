package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func launchFixture(t *testing.T) (launchRecord, Snapshot, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENT_BRIDGE_HOME", home)
	t.Setenv("AGENT_BRIDGE_ACTIVITY_DIR", filepath.Join(home, "activity"))
	t.Setenv("AGENT_BRIDGE_STATE_DIR", filepath.Join(home, "state"))
	t.Setenv("AGENT_BRIDGE_SPOOL_DIR", filepath.Join(home, "spool"))
	repo := filepath.Join(home, "work", "sample")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main", repo}, {"-C", repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-qm", "initial"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	prompt := filepath.Join(home, "task.yaml")
	if err := os.WriteFile(prompt, []byte("task: v1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r := launchRecord{ID: "abcdef012345abcdef012345", Request: launchRequest{Launch: "v1", Role: "epm", Agent: "claude", Repo: repo, Task: "review", PromptFile: prompt, Reason: "Review work"}, Requester: requester{AgentID: "claude-live"}, CreatedAt: time.Now().UTC()}
	s := Snapshot{Agents: []AgentUsage{{ID: "claude-live", Provider: "claude"}}}
	return r, s, home
}
func TestLaunchRules(t *testing.T) {
	r, s, home := launchFixture(t)
	if err := checkLaunchRules(r, s); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	parentID := "parent"
	parentDir, _ := jobDir(parentID)
	if err := os.MkdirAll(parentDir, 0700); err != nil {
		t.Fatal(err)
	}
	parent := Job{ID: parentID, Agent: "claude", Role: "epm", Depth: 2, Status: "running", WorkerPID: os.Getpid(), WorkerStart: processStart(os.Getpid())}
	if err := writeJob(parent); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change func(*launchRecord, *Snapshot)
	}{
		{"repo outside work", func(r *launchRecord, _ *Snapshot) { r.Request.Repo = home }},
		{"unknown requester", func(r *launchRecord, _ *Snapshot) { r.Requester.AgentID = "missing" }},
		{"coder job", func(r *launchRecord, _ *Snapshot) {
			r.Requester = requester{JobID: parentID}
			parent.Role = "coder"
			_ = writeJob(parent)
		}},
		{"nesting depth", func(r *launchRecord, _ *Snapshot) {
			r.Requester = requester{JobID: parentID}
			parent.Depth = 3
			_ = writeJob(parent)
		}},
		{"coordinator coder", func(r *launchRecord, _ *Snapshot) { r.Request.Role = "coder" }},
		{"codex epm", func(r *launchRecord, _ *Snapshot) { r.Request.Agent = "codex" }},
		{"claude coder", func(r *launchRecord, _ *Snapshot) { r.Requester = requester{JobID: parentID}; r.Request.Role = "coder" }},
		{"auto coder claude profile", func(r *launchRecord, _ *Snapshot) {
			r.Requester = requester{JobID: parentID}
			r.Request.Role, r.Request.Agent, r.Request.Profile = "coder", "auto", "claude-std"
		}},
		{"missing prompt", func(r *launchRecord, _ *Snapshot) { r.Request.PromptFile = filepath.Join(home, "missing") }},
		{"bad context", func(r *launchRecord, _ *Snapshot) { r.Request.Context = []string{"../secret"} }},
		{"bad task", func(r *launchRecord, _ *Snapshot) { r.Request.Task = "BAD" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent.Role = "epm"
			parent.Depth = 2
			if err := writeJob(parent); err != nil {
				t.Fatal(err)
			}
			rr := r
			rr.Request = r.Request
			ss := s
			ss.Jobs = append([]Job(nil), s.Jobs...)
			tc.change(&rr, &ss)
			if err := checkLaunchRules(rr, ss); err == nil {
				t.Fatal("rule passed")
			}
		})
	}
}

func TestLauncherCapsQueue(t *testing.T) {
	s := Snapshot{Jobs: []Job{{Launcher: "launcher", Agent: "claude", Profile: "claude-std", Status: "running"}, {Launcher: "launcher", Agent: "claude", Profile: "claude-quick", Status: "running"}}}
	if launcherSlotAvailable(s, "claude-epm") {
		t.Fatal("Claude cap ignored")
	}
	if !launcherSlotAvailable(s, "codex-std") {
		t.Fatal("unrelated provider blocked")
	}
}

func TestLaunchWaitsForCapacity(t *testing.T) {
	r, _, home := launchFixture(t)
	activity := filepath.Join(home, "activity")
	if err := os.MkdirAll(activity, 0700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		dir, _ := jobDir(id)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := writeJob(Job{ID: id, Agent: "claude", Launcher: "launcher", Status: "running", WorkerPID: os.Getpid(), WorkerStart: processStart(os.Getpid())}); err != nil {
			t.Fatal(err)
		}
	}
	fakeOllama(t, func(*http.Request) (*http.Response, error) {
		return grokResponse(200, `{"message":{"content":"{\"verdict\":\"approve\",\"agent\":\"claude\",\"role\":\"epm\",\"rationale\":\"fits\"}"}}`), nil
	})
	spool, err := spoolDir()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(activityFile{ID: "claude-live", Name: "Claude", Phase: "waiting", SampledAt: time.Now().UTC()})
	if err := os.WriteFile(filepath.Join(activity, "claude-live.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := processLaunch(spool, r, false); !errors.Is(err, errCapWait) {
		t.Fatalf("capacity: %v", err)
	}
	if _, err := readResult(spool, r.ID); !os.IsNotExist(err) {
		t.Fatalf("queued result: %v", err)
	}
	if _, err := os.Stat(filepath.Join(spool, r.ID+".selected.json")); err != nil {
		t.Fatal(err)
	}
}
func TestVerdictParsing(t *testing.T) {
	r, _, _ := launchFixture(t)
	good := `{"verdict":"approve","agent":"claude","role":"epm","rationale":"fits"}`
	if _, err := parseModelVerdict(good, r.Request, false); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"bad", good + " trailing", `{"verdict":"approve","agent":"codex","role":"epm","rationale":"bad"}`, `{"verdict":"approve","agent":"codex","role":"coder","rationale":"bad"}`, `{"verdict":"approve","agent":"claude","role":"epm"}`} {
		if _, err := parseModelVerdict(body, r.Request, false); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
	fakeOllama(t, func(_ *http.Request) (*http.Response, error) {
		return grokResponse(200, `{"message":{"content":"`+strings.ReplaceAll(good, `"`, `\"`)+`"}}`), nil
	})
	v, hash, err := modelLaunchVerdict(context.Background(), r)
	if err != nil || v.Verdict != "approve" || hash == "" {
		t.Fatalf("verdict %+v hash %q err %v", v, hash, err)
	}
}

func TestCoderVerdictRequiresCodex(t *testing.T) {
	r := launchRequest{Role: "coder", Agent: "auto"}
	for _, body := range []string{
		`{"verdict":"approve","agent":"claude","role":"coder","profile":"claude-std","rationale":"fits"}`,
		`{"verdict":"approve","agent":"claude","role":"coder","profile":"codex-std","rationale":"fits"}`,
	} {
		if _, err := parseModelVerdict(body, r, true); err == nil {
			t.Fatalf("accepted Claude coder verdict %s", body)
		}
	}
	if _, err := parseModelVerdict(`{"verdict":"approve","agent":"codex","role":"coder","rationale":"fits"}`, r, true); err != nil {
		t.Fatal(err)
	}
}

func TestCoderRequestRejectsClaude(t *testing.T) {
	r, _, _ := launchFixture(t)
	for _, agent := range []string{"claude", "auto"} {
		profile := ""
		if agent == "auto" {
			profile = "\nprofile: claude-std"
		}
		body := []byte("launch: v1\nrole: coder\nagent: " + agent + profile + "\nrepo: " + r.Request.Repo + "\ntask: review\nprompt_file: " + r.Request.PromptFile + "\nreason: Review work\n")
		parsed, err := parseLaunchYAML(body)
		if agent == "claude" && err == nil {
			t.Fatal("accepted Claude coder request")
		}
		if agent == "auto" {
			if err != nil {
				t.Fatal(err)
			}
			r.Request = parsed
			r.Requester = requester{JobID: "parent"}
			parentDir, _ := jobDir("parent")
			if err := os.MkdirAll(parentDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := writeJob(Job{ID: "parent", Agent: "claude", Role: "epm", Status: "running", WorkerPID: os.Getpid(), WorkerStart: processStart(os.Getpid())}); err != nil {
				t.Fatal(err)
			}
			if err := checkLaunchRules(r, Snapshot{}); err == nil {
				t.Fatal("accepted auto coder with Claude profile")
			}
		}
	}
}

func TestForcedCoderApprovalQueuesOnCodexUsageLimit(t *testing.T) {
	r, _, home := launchFixture(t)
	r.Request.Role, r.Request.Agent = "coder", "auto"
	r.Requester = requester{JobID: "parent"}
	parentDir, _ := jobDir("parent")
	if err := os.MkdirAll(parentDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJob(Job{ID: "parent", Agent: "claude", Role: "epm", Status: "running", WorkerPID: os.Getpid(), WorkerStart: processStart(os.Getpid())}); err != nil {
		t.Fatal(err)
	}
	haltDir, _ := jobDir("exhausted")
	if err := os.MkdirAll(haltDir, 0700); err != nil {
		t.Fatal(err)
	}
	exhausted := Job{ID: "exhausted", Agent: "codex", Role: "coder", Status: "halted", Halt: &Halt{Kind: "usage_limit"}, UpdatedAt: time.Now().UTC()}
	if err := writeJob(exhausted); err != nil {
		t.Fatal(err)
	}
	dir, err := spoolDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := processLaunch(dir, r, true); err != nil {
		t.Fatal(err)
	}
	result, err := readResult(dir, r.ID)
	if err != nil || result.Status != "queued" || !strings.Contains(result.Rationale, "Codex usage limit") {
		t.Fatalf("usage queue result %+v: %v", result, err)
	}
	var selected modelVerdict
	b, err := os.ReadFile(filepath.Join(dir, r.ID+".selected.json"))
	if err != nil || json.Unmarshal(b, &selected) != nil || selected.Agent != "codex" || selected.Role != "coder" {
		t.Fatalf("queued verdict %+v: %v", selected, err)
	}
	if _, err := os.Stat(filepath.Join(home, "worktrees", "sample-review")); !os.IsNotExist(err) {
		t.Fatalf("worktree created while exhausted: %v", err)
	}
	if got := substituteProfile("codex-std", Snapshot{Jobs: []Job{exhausted}}); got != "" {
		t.Fatalf("Codex substituted to %q", got)
	}
	// A later dispatcher pass may retry the saved verdict when the usage halt clears.
	exhausted.Halt = nil
	if err := writeJob(exhausted); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_BRIDGE_LAUNCHER_MAX_JOBS", "1")
	capacityDir, _ := jobDir("capacity")
	if err := os.MkdirAll(capacityDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJob(Job{ID: "capacity", Agent: "codex", Role: "coder", Launcher: "launcher", Status: "running", WorkerPID: os.Getpid(), WorkerStart: processStart(os.Getpid())}); err != nil {
		t.Fatal(err)
	}
	if err := processLaunch(dir, r, false); !errors.Is(err, errCapWait) {
		t.Fatalf("retry after usage limit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, r.ID+".selected.json")); err != nil {
		t.Fatal(err)
	}
}

func TestForcedApprovalRejectsClaudeCoderOverride(t *testing.T) {
	r, _, _ := launchFixture(t)
	r.Request.Role, r.Request.Agent = "coder", "auto"
	r.Request.Profile = "claude-std"
	r.Requester = requester{JobID: "parent"}
	parentDir, _ := jobDir("parent")
	if err := os.MkdirAll(parentDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJob(Job{ID: "parent", Agent: "claude", Role: "epm", Status: "running", WorkerPID: os.Getpid(), WorkerStart: processStart(os.Getpid())}); err != nil {
		t.Fatal(err)
	}
	dir, err := spoolDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := processLaunch(dir, r, true); err != nil {
		t.Fatal(err)
	}
	result, err := readResult(dir, r.ID)
	if err != nil || result.Status != "rejected" {
		t.Fatalf("Claude coder override result %+v: %v", result, err)
	}
	r.Request.Profile = ""
	if err := writeJSONPrivate(filepath.Join(dir, r.ID+".request.json"), r); err != nil {
		t.Fatal(err)
	}
	if err := resolveApprovalCommand("approve", []string{"--profile", "claude-std", r.ID}); err == nil {
		t.Fatal("accepted Claude coder approval profile")
	}
}
func TestSpoolRoundTripAndEscalation(t *testing.T) {
	r, _, _ := launchFixture(t)
	dir, err := spoolDir()
	if err != nil {
		t.Fatal(err)
	}
	yaml := []byte("launch: v1\nrole: epm\nagent: claude\nrepo: " + r.Request.Repo + "\ntask: review\nprompt_file: " + r.Request.PromptFile + "\nreason: Review work\n")
	parsed, err := parseLaunchYAML(yaml)
	if err != nil {
		t.Fatal(err)
	}
	r.Request = parsed
	if err := os.WriteFile(filepath.Join(dir, r.ID+".yaml"), yaml, 0600); err != nil {
		t.Fatal(err)
	}
	p, _ := requestPath(dir, r.ID)
	if err := writeJSONPrivate(p, r); err != nil {
		t.Fatal(err)
	}
	if got, err := readRecord(dir, r.ID); err != nil || got.Request.Task != "review" {
		t.Fatalf("record %+v: %v", got, err)
	}
	if err := setResult(dir, launchResult{ID: r.ID, Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	pending, _ := pendingPath(r.ID)
	if err := writeJSONPrivate(pending, r); err != nil {
		t.Fatal(err)
	}
	if err := resolveApprovalCommand("deny", []string{r.ID}); err != nil {
		t.Fatal(err)
	}
	result, err := readResult(dir, r.ID)
	if err != nil || result.Status != "denied" {
		t.Fatalf("result %+v: %v", result, err)
	}
	if err := setResult(dir, launchResult{ID: r.ID, Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONPrivate(pending, r); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(pending, old, old); err != nil {
		t.Fatal(err)
	}
	if err := expirePending(30 * time.Minute); err != nil {
		t.Fatal(err)
	}
	result, err = readResult(dir, r.ID)
	if err != nil || result.Status != "denied" || !strings.Contains(result.Rationale, "timed out") {
		t.Fatalf("timeout %+v: %v", result, err)
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(pending), "..", "decisions.jsonl"))
	if err != nil || len(data) == 0 {
		t.Fatalf("decisions: %v", err)
	}
	var row map[string]any
	if json.Unmarshal(data[:strings.IndexByte(string(data), '\n')], &row) != nil {
		t.Fatal("invalid decision JSON")
	}
}
func TestClaudeWorkerFake(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_BRIDGE_STATE_DIR", dir)
	fake := filepath.Join(dir, "claude")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + filepath.Join(dir, "args") + "'\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"result\",\"result\":\"finished\",\"session_id\":\"claude-session\"}'\n"
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_BRIDGE_CLAUDE_BIN", fake)
	id := "fakeclaude"
	path, _ := jobDir(id)
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	j := Job{ID: id, Agent: "claude", Role: "epm", Cwd: dir, Sandbox: "read-only", Status: "queued", CreatedAt: time.Now().UTC(), EventsPath: filepath.Join(path, "events.jsonl"), StderrPath: filepath.Join(path, "stderr.log"), ResultPath: filepath.Join(path, "result.txt")}
	if err := writeJob(j); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "prompt.txt"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := worker(id); err != nil {
		t.Fatal(err)
	}
	j, err := readJob(id)
	if err != nil || j.Status != "completed" {
		t.Fatalf("job %+v: %v", j, err)
	}
	b, err := os.ReadFile(j.ResultPath)
	if err != nil || string(b) != "finished" {
		t.Fatalf("result %q: %v", b, err)
	}
	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil || !strings.Contains(string(args), "Read,Grep,Glob") || strings.Contains(string(args), "Edit") {
		t.Fatalf("args %q: %v", args, err)
	}
}

func TestApproveLaunchesWorktree(t *testing.T) {
	r, _, home := launchFixture(t)
	dir, err := spoolDir()
	if err != nil {
		t.Fatal(err)
	}
	yaml := []byte("launch: v1\nrole: epm\nagent: claude\nrepo: " + r.Request.Repo + "\ntask: review\nprompt_file: " + r.Request.PromptFile + "\nreason: Review work\n")
	if err := os.WriteFile(filepath.Join(dir, r.ID+".yaml"), yaml, 0600); err != nil {
		t.Fatal(err)
	}
	p, _ := requestPath(dir, r.ID)
	if err := writeJSONPrivate(p, r); err != nil {
		t.Fatal(err)
	}
	if err := setResult(dir, launchResult{ID: r.ID, Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	pending, _ := pendingPath(r.ID)
	if err := writeJSONPrivate(pending, r); err != nil {
		t.Fatal(err)
	}
	activity := filepath.Join(home, "activity")
	if err := os.Mkdir(activity, 0700); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(home, "fake-claude")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"result\",\"result\":\"done\"}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, "agent-bridge")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	record, _ := json.Marshal(map[string]any{"id": "claude-live", "name": "Claude", "phase": "waiting", "sampledAt": time.Now().UTC()})
	if err := os.WriteFile(filepath.Join(activity, "claude-live.json"), record, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "approve", r.ID)
	cmd.Env = append(os.Environ(), "HOME="+home, "AGENT_BRIDGE_HOME="+home, "AGENT_BRIDGE_STATE_DIR="+filepath.Join(home, "state"), "AGENT_BRIDGE_SPOOL_DIR="+dir, "AGENT_BRIDGE_ACTIVITY_DIR="+activity, "AGENT_BRIDGE_CLAUDE_BIN="+fake, "AGENT_BRIDGE_LAUNCH_MODE=direct")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("approve: %v %s", err, out)
	}
	var result launchResult
	if err := json.Unmarshal(out, &result); err != nil || result.Status != "approved" || result.JobID == "" {
		t.Fatalf("result %s: %v", out, err)
	}
	wait := exec.Command(bin, "wait", "--interval", "100ms", result.JobID)
	wait.Env = cmd.Env
	if out, err := wait.CombinedOutput(); err != nil {
		t.Fatalf("wait: %v %s", err, out)
	}
	j, err := readJob(result.JobID)
	if err != nil || j.Agent != "claude" || j.Role != "epm" || j.Launcher != "launcher" {
		t.Fatalf("job %+v: %v", j, err)
	}
	if _, err := os.Stat(filepath.Join(home, "worktrees", "sample-review")); err != nil {
		t.Fatal(err)
	}
}

func TestLaunchFallbackRequest(t *testing.T) {
	r, s, _ := launchFixture(t)
	halt := &Halt{Kind: "usage_limit", Code: "usage_limit_exceeded", Source: "test", DetectedAt: time.Now().UTC()}
	dir, _ := jobDir("halted")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJob(Job{ID: "halted", Agent: "codex", Role: "coder", Status: "halted", Halt: halt, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	pdir, _ := jobDir("parent")
	if err := os.MkdirAll(pdir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJob(Job{ID: "parent", Agent: "claude", Role: "epm", Status: "running", WorkerPID: os.Getpid(), WorkerStart: processStart(os.Getpid())}); err != nil {
		t.Fatal(err)
	}
	r.Requester = requester{JobID: "parent"}
	r.Request.Role, r.Request.Agent, r.Request.FallbackFor = "coder", "claude", "halted"
	if err := checkLaunchRules(r, s); err != nil {
		t.Fatalf("eligible fallback: %v", err)
	}
	r.Request.FallbackFor = ""
	if err := checkLaunchRules(r, s); err == nil {
		t.Fatal("Claude coder without fallback_for passed")
	}
	r.Request.FallbackFor = "halted"
	s.Agents = append(s.Agents, AgentUsage{Provider: "claude", ActivityState: "halted_usage"})
	if err := checkLaunchRules(r, s); err == nil || !strings.Contains(err.Error(), "Claude usage limit") {
		t.Fatalf("exhausted Claude: %v", err)
	}
	// Model verdict may not drop the fallback agent or add the field.
	if _, err := parseModelVerdict(`{"verdict":"approve","agent":"codex","role":"coder","rationale":"x"}`, r.Request, true); err == nil {
		t.Fatal("verdict moved fallback to Codex")
	}
	if _, err := parseModelVerdict(`{"verdict":"approve","agent":"claude","role":"coder","fallback_for":"x","rationale":"x"}`, r.Request, true); err == nil {
		t.Fatal("model supplied fallback_for")
	}
	v, err := parseModelVerdict(`{"verdict":"approve","agent":"claude","role":"coder","rationale":"x"}`, r.Request, true)
	if err != nil || v.Profile != "claude-std" {
		t.Fatalf("fallback verdict = %+v, %v", v, err)
	}
	r.Request.FallbackFor = ""
	if _, err := parseModelVerdict(`{"verdict":"approve","agent":"claude","role":"coder","rationale":"x"}`, r.Request, true); err == nil {
		t.Fatal("Claude coder verdict without fallback_for")
	}
	req, err := parseLaunchYAML([]byte("launch: v1\nrole: coder\nagent: claude\nrepo: /a\ntask: t\nprompt_file: /p\nreason: r\nfallback_for: halted\n"))
	if err != nil || req.FallbackFor != "halted" {
		t.Fatalf("parse: %+v %v", req, err)
	}
	if _, err := parseLaunchYAML([]byte("launch: v1\nrole: coder\nagent: claude\nrepo: /a\ntask: t\nprompt_file: /p\nreason: r\n")); err == nil {
		t.Fatal("Claude coder request without fallback_for parsed")
	}
}
