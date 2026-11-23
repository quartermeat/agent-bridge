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

func roleFixture(t *testing.T, role string, depth int) string {
	t.Helper()
	state := t.TempDir()
	t.Setenv("AGENT_BRIDGE_STATE_DIR", state)
	t.Setenv("AGENT_BRIDGE_JOB_ID", "")
	t.Setenv("AGENT_BRIDGE_JOB_ROLE", "")
	if role == "" {
		return state
	}
	ppid := os.Getppid()
	dir, _ := jobDir("live")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	agent := "codex"
	if role == "epm" {
		agent = "claude"
	}
	j := Job{ID: "live", Agent: agent, Role: role, Depth: depth, Status: "running", WorkerPID: ppid, WorkerStart: processStart(ppid), UpdatedAt: time.Now().UTC()}
	if err := writeJob(j); err != nil {
		t.Fatal(err)
	}
	return state
}

func roleStartArgs(t *testing.T, extra ...string) []string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "prompt")
	if err := os.WriteFile(p, []byte("Do it."), 0600); err != nil {
		t.Fatal(err)
	}
	return append([]string{"--cwd", t.TempDir(), "--prompt-file", p}, extra...)
}

func jobDirCount(t *testing.T, state string) int {
	t.Helper()
	e, err := os.ReadDir(filepath.Join(state, "jobs"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(e)
}

func TestStartRefusedByAncestry(t *testing.T) {
	state := roleFixture(t, "coder", 0)
	err := start(roleStartArgs(t))
	want := "bridge workers cannot launch jobs (job live, role coder); finish the task or report partial/blocked"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v", err)
	}
	if n := jobDirCount(t, state); n != 1 {
		t.Fatalf("job dirs = %d, want only the live job", n)
	}
	for _, f := range []func(bool) error{refuseWorkerLaunch} {
		if f(true) == nil || f(false) == nil {
			t.Fatal("coder allowed to launch")
		}
	}
}

func TestStartRefusedByEnv(t *testing.T) {
	state := roleFixture(t, "", 0)
	t.Setenv("AGENT_BRIDGE_JOB_ID", "other")
	t.Setenv("AGENT_BRIDGE_JOB_ROLE", "coder")
	err := start(roleStartArgs(t))
	if err == nil || !strings.Contains(err.Error(), "bridge workers cannot launch jobs (job other, role coder)") {
		t.Fatalf("err = %v", err)
	}
	if n := jobDirCount(t, state); n != 0 {
		t.Fatalf("job dirs = %d", n)
	}
}

func TestEPMMayRequestButNotStart(t *testing.T) {
	roleFixture(t, "epm", 1)
	if err := refuseWorkerLaunch(true); err != nil {
		t.Fatalf("request path refused: %v", err)
	}
	if err := refuseWorkerLaunch(false); err == nil {
		t.Fatal("EPM direct launch allowed")
	}
	if err := start(roleStartArgs(t, "--agent", "claude")); err == nil || !strings.Contains(err.Error(), "role epm") {
		t.Fatalf("start err = %v", err)
	}
}

func TestOutsideCallerAllowed(t *testing.T) {
	roleFixture(t, "", 0)
	if err := refuseWorkerLaunch(false); err != nil {
		t.Fatal(err)
	}
	if err := refuseWorkerLaunch(true); err != nil {
		t.Fatal(err)
	}
}

func TestStartDepthLimit(t *testing.T) {
	state := roleFixture(t, "", 0)
	dir, _ := jobDir("parent")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	parent := Job{ID: "parent", Agent: "claude", Role: "epm", Depth: 3, Status: "running", WorkerPID: pid, WorkerStart: processStart(pid), UpdatedAt: time.Now().UTC()}
	if err := writeJob(parent); err != nil {
		t.Fatal(err)
	}
	err := start(roleStartArgs(t, "--agent", "claude", "--parent-job", "parent"))
	if err == nil || !strings.Contains(err.Error(), "nesting depth") {
		t.Fatalf("err = %v", err)
	}
	if n := jobDirCount(t, state); n != 1 {
		t.Fatalf("job dirs = %d", n)
	}
}

func TestRoleBlockAndChildEnv(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "agent-bridge")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	activity := filepath.Join(tmp, "activity")
	if err := os.Mkdir(activity, 0700); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, agent, role, bin string
		launch                 []string
		wantLaunchLine         string
	}{
		{"codex coder", "codex", "coder", "AGENT_BRIDGE_CODEX_BIN", nil, "Launch no jobs"},
		{"claude coder", "claude", "coder", "AGENT_BRIDGE_CLAUDE_BIN", []string{"--fallback-for", "halted"}, "Launch no jobs"},
		{"claude epm", "claude", "epm", "AGENT_BRIDGE_CLAUDE_BIN", nil, "agent-bridge request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, _ := json.Marshal(map[string]any{"id": "claude-t", "name": "Claude", "phase": "waiting", "sampledAt": time.Now().UTC()})
			if err := os.WriteFile(filepath.Join(activity, "claude-t.json"), rec, 0600); err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(t.TempDir(), "state")
			out := t.TempDir()
			fake := filepath.Join(out, "fake")
			script := "#!/bin/sh\ncat > '" + out + "/prompt'\nprintf '%s %s %s' \"$AGENT_BRIDGE_JOB_ID\" \"$AGENT_BRIDGE_JOB_ROLE\" \"$AGENT_BRIDGE_JOB_DEPTH\" > '" + out + "/env'\n"
			if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			env := append(os.Environ(), "AGENT_BRIDGE_STATE_DIR="+state, "AGENT_BRIDGE_ACTIVITY_DIR="+activity, tc.bin+"="+fake, "AGENT_BRIDGE_LAUNCH_MODE=direct", "AGENT_BRIDGE_JOB_ID=", "AGENT_BRIDGE_JOB_ROLE=")
			if tc.launch != nil {
				dir := filepath.Join(state, "jobs", "halted")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("AGENT_BRIDGE_STATE_DIR", state)
				halt := &Halt{Kind: "usage_limit", Code: "usage_limit_exceeded", Source: "test", DetectedAt: time.Now().UTC()}
				if err := writeJob(Job{ID: "halted", Agent: "codex", Role: "coder", Cwd: tmp, Status: "halted", Halt: halt, UpdatedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
			}
			args := append([]string{"start", "--cwd", tmp, "--agent", tc.agent, "--role", tc.role, "--spawned-by", "claude-t"}, tc.launch...)
			cmd := exec.Command(bin, args...)
			cmd.Env = env
			cmd.Stdin = strings.NewReader("TASK BODY")
			b, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("start: %v: %s", err, b)
			}
			id := strings.TrimSpace(string(b))
			wait := exec.Command(bin, "wait", "--interval", "100ms", id)
			wait.Env = env
			// The fake emits no Claude result, so claude jobs end "failed"; only
			// the prompt and environment it captured matter here.
			_, _ = wait.CombinedOutput()
			prompt, err := os.ReadFile(filepath.Join(out, "prompt"))
			if err != nil {
				t.Fatal(err)
			}
			p := string(prompt)
			for _, s := range []string{"job " + id, "role " + tc.role, "not the interactive EPM", "overrides any EPM instructions", "yourself", "report: v2", tc.wantLaunchLine, "TASK BODY"} {
				if !strings.Contains(p, s) {
					t.Errorf("prompt missing %q:\n%s", s, p)
				}
			}
			if !strings.HasPrefix(p, "Bridge worker role:") {
				t.Errorf("role block not first:\n%s", p)
			}
			if tc.agent == "claude" && tc.role == "coder" && !strings.Contains(p, "Tool rules:") {
				t.Errorf("claude coder tool rules lost:\n%s", p)
			}
			e, err := os.ReadFile(filepath.Join(out, "env"))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := string(e), id+" "+tc.role+" 1"; got != want {
				t.Errorf("env = %q, want %q", got, want)
			}
		})
	}
}

func TestWorkerCommandPolicyCoversDispatch(t *testing.T) {
	// Keep this list in sync with every case in run. Subcommand rows cover
	// commands that mix read-only and mutating operations.
	cases := []struct {
		args       []string
		coder, epm bool
	}{
		{[]string{"start"}, false, false},
		{[]string{"local"}, false, false},
		{[]string{"request"}, false, true},
		{[]string{"request-status"}, true, true},
		{[]string{"launcher"}, false, false},
		{[]string{"local-model", "keep"}, false, false},
		{[]string{"local-model", "yield"}, true, true},
		{[]string{"local-model", "status"}, true, true},
		{[]string{"local-queue", "submit"}, false, false},
		{[]string{"local-queue", "run"}, false, false},
		{[]string{"local-queue", "status"}, true, true},
		{[]string{"local-queue", "cancel"}, false, false},
		{[]string{"local-queue", "report"}, true, true},
		{[]string{"schedule", "add"}, false, false},
		{[]string{"schedule", "remove"}, false, false},
		{[]string{"schedule", "run"}, false, false},
		{[]string{"schedule", "list"}, true, true},
		{[]string{"schedule", "show"}, true, true},
		{[]string{"approvals"}, true, true},
		{[]string{"approve"}, false, false},
		{[]string{"deny"}, false, false},
		{[]string{"ask"}, false, false},
		{[]string{"list"}, true, true},
		{[]string{"show"}, true, true},
		{[]string{"wait"}, true, true},
		{[]string{"ack"}, false, false},
		{[]string{"stop"}, false, false},
		{[]string{"snapshot"}, true, true},
		{[]string{"latest"}, true, true},
		{[]string{"watch"}, true, true},
		{[]string{"daemon"}, false, false},
		{[]string{"speak"}, false, false},
		{[]string{"voice"}, false, false},
		{[]string{"voice-log"}, true, true},
		{[]string{"voice-approval-mcp"}, false, false},
		{[]string{"_worker", "other"}, false, false},
		{[]string{"future-command"}, false, false},
		{[]string{"schedule", "future"}, false, false},
		{[]string{"local-queue", "future"}, false, false},
		{[]string{"local-model", "future"}, false, false},
	}
	for _, role := range []string{"coder", "epm", ""} {
		t.Run(role, func(t *testing.T) {
			roleFixture(t, role, 1)
			for _, tc := range cases {
				name := strings.Join(tc.args, " ")
				t.Run(name, func(t *testing.T) {
					want := true
					if role == "coder" {
						want = tc.coder
					}
					if role == "epm" {
						want = tc.epm
					}
					if got := refuseWorkerCommand(tc.args) == nil; got != want {
						t.Fatalf("guard allowed = %v, want %v", got, want)
					}
					if !want {
						if err := run(tc.args); err == nil || !strings.Contains(err.Error(), "cannot launch jobs") {
							t.Fatalf("run bypassed guard: %v", err)
						}
					}
				})
			}
		})
	}
}

func TestCarlWorkerGuard(t *testing.T) {
	for _, role := range []string{"coder", "epm"} {
		t.Run(role, func(t *testing.T) {
			roleFixture(t, role, 1)
			if err := runAs("/usr/bin/carl", nil); err == nil || !strings.Contains(err.Error(), "cannot launch jobs") {
				t.Fatalf("carl allowed: %v", err)
			}
			if err := runAs("/usr/bin/carl", []string{"audit", "--n", "1"}); err != nil {
				t.Fatalf("carl audit refused: %v", err)
			}
		})
	}
}
