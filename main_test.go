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
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ncat >/dev/null\nprintf '{\"type\":\"thread.started\",\"thread_id\":\"worker-session\"}\\n{\"type\":\"turn.completed\"}\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(tmp, "state")
	activity := filepath.Join(tmp, "activity")
	if err := os.Mkdir(activity, 0700); err != nil {
		t.Fatal(err)
	}
	spawnerID := "claude-test-parent"
	activityRecord, _ := json.Marshal(map[string]any{"id": spawnerID, "name": "Claude", "phase": "waiting", "sampledAt": time.Now().UTC()})
	if err := os.WriteFile(filepath.Join(activity, spawnerID+".json"), activityRecord, 0600); err != nil {
		t.Fatal(err)
	}
	start := exec.Command(bin, "start", "--cwd", tmp, "--name", "smoke", "--spawned-by", spawnerID)
	start.Env = append(os.Environ(), "AGENT_BRIDGE_STATE_DIR="+state, "AGENT_BRIDGE_ACTIVITY_DIR="+activity, "AGENT_BRIDGE_CODEX_BIN="+fake, "AGENT_BRIDGE_LAUNCH_MODE=direct")
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
	if waited[0].SessionID != "worker-session" || waited[0].SpawnedBy == nil || waited[0].SpawnedBy.AgentID != spawnerID || waited[0].SpawnedBy.Provider != "claude" {
		t.Fatalf("missing worker or spawner identity: %+v", waited[0])
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
	if job.Role != "coder" || job.Profile != "codex-std" {
		t.Fatalf("default Codex role/profile = %q/%q", job.Role, job.Profile)
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

func TestEPMWriteScope(t *testing.T) {
	allowed, available := claudeTools(Job{Agent: "claude", Role: "epm", Sandbox: "workspace-write"})
	if strings.Contains(allowed, "Bash") || strings.Contains(available, "Bash") || strings.Contains(allowed, ",Edit,") || strings.Contains(allowed, ",Write,") {
		t.Fatalf("EPM tools are unrestricted: %q / %q", allowed, available)
	}
	for _, rule := range []string{"Edit(docs/**)", "Edit(**/*.md)", "Edit(README*)"} {
		if !strings.Contains(allowed, rule) {
			t.Errorf("missing %s in %q", rule, allowed)
		}
	}
}

func TestDocJobRejectsCodePaths(t *testing.T) {
	for _, path := range []string{"main.go", "src/server.py", "../README.md"} {
		if err := validateDocOwns("task: v1\nowns: [docs/PLAN.md, " + path + "]\n"); err == nil {
			t.Errorf("accepted %s", path)
		}
	}
	if err := validateDocOwns("owns:\n  - docs/PLAN.md\n  - README.md\n"); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeCoderToolRules(t *testing.T) {
	allowed, available := claudeTools(Job{Agent: "claude", Role: "coder", Sandbox: "workspace-write"})
	if available != "Read,Grep,Glob,Edit,Write,Bash" || !strings.Contains(allowed, claudeCoderBash) {
		t.Fatalf("coder allowlist changed: %q / %q", allowed, available)
	}
	for _, phrase := range []string{"one command per Bash call", "semicolons, &&, or pipes", "Read, Grep, or Glob instead of cat, grep, or find"} {
		if !strings.Contains(claudeCoderToolRules, phrase) {
			t.Errorf("missing coder tool rule %q", phrase)
		}
	}
}

func TestClaudeJobViolationAudit(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.Mkdir(filepath.Join(repo, "docs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docs", "plan.md"), []byte("plan"), 0600); err != nil {
		t.Fatal(err)
	}
	j := Job{ID: "epm-audit", Agent: "claude", Role: "epm", Cwd: repo, Status: "completed"}
	t.Setenv("AGENT_BRIDGE_STATE_DIR", t.TempDir())
	if err := auditClaudeJob(&j); err != nil || j.Status != "completed" {
		t.Fatalf("document audit: %+v: %v", j, err)
	}
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := auditClaudeJob(&j); err != nil {
		t.Fatal(err)
	}
	if j.Status != "violation" || len(j.Violations) != 1 || j.Violations[0] != "main.go" {
		t.Fatalf("violation audit: %+v", j)
	}
	encoded, err := json.Marshal(j)
	if err != nil || !strings.Contains(string(encoded), `"violations":["main.go"]`) {
		t.Fatalf("show JSON lacks violations: %s: %v", encoded, err)
	}
	decisions, err := os.ReadFile(filepath.Join(os.Getenv("AGENT_BRIDGE_STATE_DIR"), "launcher", "decisions.jsonl"))
	if err != nil || !strings.Contains(string(decisions), `"outcome":"violation"`) {
		t.Fatalf("missing audit decision: %s: %v", decisions, err)
	}
	j.Role, j.Status, j.Violations = "coder", "completed", nil
	if err := auditClaudeJob(&j); err != nil || j.Status != "completed" {
		t.Fatalf("coder should be exempt: %+v: %v", j, err)
	}
}

func TestClaudeJobViolationAuditWorker(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	state := t.TempDir()
	t.Setenv("AGENT_BRIDGE_STATE_DIR", state)
	fake := filepath.Join(state, "fake-claude")
	script := "#!/bin/sh\ncat >/dev/null\nprintf 'package main\\n' > main.go\nprintf '%s\\n' '{\"type\":\"result\",\"result\":\"done\"}'\n"
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_BRIDGE_CLAUDE_BIN", fake)
	id := "epm-worker-audit"
	dir, err := jobDir(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	j := Job{ID: id, Agent: "claude", Role: "epm", Cwd: repo, Sandbox: "workspace-write", Status: "queued", EventsPath: filepath.Join(dir, "events.jsonl"), StderrPath: filepath.Join(dir, "stderr.log"), ResultPath: filepath.Join(dir, "result.txt")}
	if err := writeJob(j); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("write docs only"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := worker(id); err != nil {
		t.Fatal(err)
	}
	job, err := readJob(id)
	if err != nil || job.Status != "violation" || len(job.Violations) != 1 || job.Violations[0] != "main.go" {
		t.Fatalf("worker audit: %+v: %v", job, err)
	}
}

func TestClaudeJobViolationAuditIgnoresPreexisting(t *testing.T) {
	for _, tc := range []struct {
		name, jobWrite string
		wantViolation  bool
	}{
		{name: "unchanged"},
		{name: "modified", jobWrite: "package main\n// job edit\n", wantViolation: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
				t.Fatalf("git init: %v: %s", err, out)
			}
			if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n// preexisting\n"), 0600); err != nil {
				t.Fatal(err)
			}
			state := t.TempDir()
			t.Setenv("AGENT_BRIDGE_STATE_DIR", state)
			fake := filepath.Join(state, "fake-claude")
			script := "#!/bin/sh\ncat >/dev/null\n"
			if tc.jobWrite != "" {
				script += "printf 'package main\\n// job edit\\n' > main.go\n"
			}
			script += "printf '%s\\n' '{\"type\":\"result\",\"result\":\"done\"}'\n"
			if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("AGENT_BRIDGE_CLAUDE_BIN", fake)
			id := "preexisting-audit"
			dir, err := jobDir(id)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			j := Job{ID: id, Agent: "claude", Role: "epm", Cwd: repo, Sandbox: "workspace-write", Status: "queued", EventsPath: filepath.Join(dir, "events.jsonl"), StderrPath: filepath.Join(dir, "stderr.log"), ResultPath: filepath.Join(dir, "result.txt")}
			if err := writeJob(j); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("work"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := worker(id); err != nil {
				t.Fatal(err)
			}
			got, err := readJob(id)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantViolation {
				if got.Status != "violation" || len(got.Violations) != 1 || got.Violations[0] != "main.go" {
					t.Fatalf("job = %+v", got)
				}
			} else if got.Status != "completed" || len(got.Violations) != 0 {
				t.Fatalf("job = %+v", got)
			}
		})
	}
}

func TestStartRole(t *testing.T) {
	for _, tt := range []struct {
		agent, role, want string
	}{
		{"claude", "", "epm"},
		{"codex", "", "coder"},
		{"claude", "epm", "epm"},
		{"claude", "coder", "coder"},
		{"codex", "coder", "coder"},
	} {
		if got := startRole(tt.agent, tt.role); got != tt.want {
			t.Errorf("startRole(%q, %q) = %q, want %q", tt.agent, tt.role, got, tt.want)
		}
	}
}

func TestStartClaudeDefaultsToEPM(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "agent-bridge")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	fake := filepath.Join(tmp, "fake-claude")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ncat >/dev/null\n"), 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(tmp, "state")
	activity := filepath.Join(tmp, "activity")
	if err := os.Mkdir(activity, 0700); err != nil {
		t.Fatal(err)
	}
	spawnerID := "claude-test-parent"
	activityRecord, _ := json.Marshal(map[string]any{"id": spawnerID, "name": "Claude", "phase": "waiting", "sampledAt": time.Now().UTC()})
	if err := os.WriteFile(filepath.Join(activity, spawnerID+".json"), activityRecord, 0600); err != nil {
		t.Fatal(err)
	}
	start := exec.Command(bin, "start", "--cwd", tmp, "--name", "epm-default", "--agent", "claude", "--spawned-by", spawnerID)
	start.Env = append(os.Environ(), "AGENT_BRIDGE_STATE_DIR="+state, "AGENT_BRIDGE_ACTIVITY_DIR="+activity, "AGENT_BRIDGE_CLAUDE_BIN="+fake, "AGENT_BRIDGE_LAUNCH_MODE=direct")
	start.Stdin = strings.NewReader("Report status.")
	out, err := start.CombinedOutput()
	if err != nil {
		t.Fatalf("start: %v: %s", err, out)
	}
	id := strings.TrimSpace(string(out))
	wait := exec.Command(bin, "wait", "--interval", "100ms", id)
	wait.Env = start.Env
	waitOut, waitErr := wait.Output()
	var waited []Job
	if err := json.Unmarshal(waitOut, &waited); err != nil || len(waited) != 1 {
		t.Fatalf("wait: %v, result: %v: %s", waitErr, err, waitOut)
	}
	if waited[0].Status != "completed" && waited[0].Status != "failed" {
		t.Fatalf("wait status = %q, error = %v", waited[0].Status, waitErr)
	}
	show := exec.Command(bin, "show", id)
	show.Env = start.Env
	showOut, err := show.Output()
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	var job Job
	if err := json.Unmarshal(showOut, &job); err != nil {
		t.Fatalf("show result: %v: %s", err, showOut)
	}
	if job.Role != "epm" || job.Agent != "claude" || job.Profile != "claude-epm" {
		t.Fatalf("default Claude role/agent/profile = %q/%q/%q", job.Role, job.Agent, job.Profile)
	}
}

func TestStartRejectsClaudeCoder(t *testing.T) {
	if err := start([]string{"--cwd", t.TempDir(), "--agent", "claude", "--role", "coder"}); err == nil || !strings.Contains(err.Error(), "Claude cannot run the coder role without --fallback-for") {
		t.Fatalf("Claude coder start: %v", err)
	}
	if err := start([]string{"--cwd", t.TempDir(), "--agent", "codex", "--role", "epm"}); err == nil || !strings.Contains(err.Error(), "Codex cannot run the epm role") {
		t.Fatalf("Codex EPM start: %v", err)
	}
	if err := start([]string{"--cwd", t.TempDir(), "--role", "boss"}); err == nil || !strings.Contains(err.Error(), "invalid role") {
		t.Fatalf("invalid role start: %v", err)
	}
	if err := validAgentRole("claude", "epm"); err != nil {
		t.Fatalf("Claude EPM: %v", err)
	}
	if err := validAgentRole("codex", "coder"); err != nil {
		t.Fatalf("Codex coder: %v", err)
	}
}

func writeFallbackJobs(t *testing.T, cwd string) {
	t.Helper()
	t.Setenv("AGENT_BRIDGE_STATE_DIR", t.TempDir())
	halt := &Halt{Kind: "usage_limit", Code: "usage_limit_exceeded", Source: "test", DetectedAt: time.Now().UTC()}
	for _, j := range []Job{
		{ID: "halted", Agent: "codex", Role: "coder", Cwd: cwd, Status: "halted", Halt: halt, UpdatedAt: time.Now().UTC()},
		{ID: "claudejob", Agent: "claude", Role: "epm", Cwd: cwd, Status: "halted", Halt: halt, UpdatedAt: time.Now().UTC()},
		{ID: "running", Agent: "codex", Role: "coder", Cwd: cwd, Status: "running", UpdatedAt: time.Now().UTC()},
		{ID: "timelimit", Agent: "codex", Role: "coder", Cwd: cwd, Status: "halted", Halt: &Halt{Kind: "time_limit", Code: "time_limit"}, UpdatedAt: time.Now().UTC()},
	} {
		dir, _ := jobDir(j.ID)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := writeJob(j); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStartFallbackRejections(t *testing.T) {
	cwd := t.TempDir()
	writeFallbackJobs(t, cwd)
	snap := Snapshot{}
	old := fallbackSnapshot
	fallbackSnapshot = func() (Snapshot, error) { return snap, nil }
	defer func() { fallbackSnapshot = old }()
	for name, tc := range map[string]struct{ id, want string }{
		"non-Codex job":  {"claudejob", "not a Codex coder"},
		"non-halted job": {"running", "did not halt on a usage limit"},
		"non-usage halt": {"timelimit", "did not halt on a usage limit"},
		"missing job":    {"nosuch", "fallback job"},
	} {
		err := start([]string{"--cwd", cwd, "--agent", "claude", "--role", "coder", "--fallback-for", tc.id})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := start([]string{"--cwd", cwd, "--agent", "codex", "--fallback-for", "halted"}); err == nil || !strings.Contains(err.Error(), "only for --agent claude --role coder") {
		t.Errorf("codex with --fallback-for: %v", err)
	}
	snap = Snapshot{Agents: []AgentUsage{{Provider: "claude", ActivityState: "halted_usage"}}}
	if err := start([]string{"--cwd", cwd, "--agent", "claude", "--role", "coder", "--fallback-for", "halted"}); err == nil || !strings.Contains(err.Error(), "Claude usage limit") {
		t.Errorf("Claude exhausted: %v", err)
	}
}

func TestStartFallbackAccepted(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "agent-bridge")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	fake := filepath.Join(tmp, "fake-claude")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ncat >/dev/null\n"), 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(tmp, "state")
	worktree := filepath.Join(tmp, "wt")
	if err := os.Mkdir(worktree, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_BRIDGE_STATE_DIR", state)
	halt := &Halt{Kind: "usage_limit", Code: "usage_limit_exceeded", Source: "test", DetectedAt: time.Now().UTC()}
	dir, _ := jobDir("halted")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJob(Job{ID: "halted", Agent: "codex", Role: "coder", Cwd: worktree, Status: "halted", Halt: halt, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	activity := filepath.Join(tmp, "activity")
	if err := os.Mkdir(activity, 0700); err != nil {
		t.Fatal(err)
	}
	rec, _ := json.Marshal(map[string]any{"id": "claude-epm-1", "name": "Claude", "phase": "waiting", "sampledAt": time.Now().UTC()})
	if err := os.WriteFile(filepath.Join(activity, "claude-epm-1.json"), rec, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "start", "--name", "fb", "--agent", "claude", "--role", "coder", "--fallback-for", "halted", "--spawned-by", "claude-epm-1")
	cmd.Env = append(os.Environ(), "AGENT_BRIDGE_STATE_DIR="+state, "AGENT_BRIDGE_ACTIVITY_DIR="+activity, "AGENT_BRIDGE_CLAUDE_BIN="+fake, "AGENT_BRIDGE_LAUNCH_MODE=direct")
	cmd.Stdin = strings.NewReader("Do the task.")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("start: %v: %s", err, out)
	}
	j, err := readJob(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	if j.Agent != "claude" || j.Role != "coder" || j.FallbackFor != "halted" || j.Profile != "claude-std" || j.Cwd != worktree {
		t.Fatalf("fallback job = %+v", j)
	}
}
