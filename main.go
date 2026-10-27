package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Job struct {
	ID             string      `json:"id"`
	Name           string      `json:"name"`
	Provider       string      `json:"provider,omitempty"`
	Agent          string      `json:"agent"`
	Role           string      `json:"role"`
	Profile        string      `json:"profile,omitempty"`
	Model          string      `json:"model,omitempty"`
	Effort         string      `json:"effort,omitempty"`
	TimeLimit      string      `json:"time_limit,omitempty"`
	Substitution   string      `json:"substitution,omitempty"`
	FallbackFor    string      `json:"fallback_for,omitempty"`
	Depth          int         `json:"depth"`
	SpawnedBy      *Spawner    `json:"spawned_by"`
	RequestedBy    *Spawner    `json:"requested_by,omitempty"`
	SessionID      string      `json:"session_id,omitempty"`
	Cwd            string      `json:"cwd"`
	Sandbox        string      `json:"sandbox"`
	Launcher       string      `json:"launcher"`
	Status         string      `json:"status"`
	CreatedAt      time.Time   `json:"created_at"`
	UpdatedAt      time.Time   `json:"updated_at"`
	WorkerPID      int         `json:"worker_pid,omitempty"`
	WorkerStart    string      `json:"worker_start,omitempty"`
	CodexPID       int         `json:"codex_pid,omitempty"`
	CodexStart     string      `json:"codex_start,omitempty"`
	ExitCode       *int        `json:"exit_code,omitempty"`
	Error          string      `json:"error,omitempty"`
	EventsPath     string      `json:"events_path"`
	StderrPath     string      `json:"stderr_path"`
	ResultPath     string      `json:"result_path"`
	ContextFiles   []string    `json:"context_files,omitempty"`
	Usage          *TokenUsage `json:"usage,omitempty"`
	Halt           *Halt       `json:"halt,omitempty"`
	AcknowledgedAt *time.Time  `json:"acknowledged_at,omitempty"`
}

type Spawner struct {
	AgentID   string `json:"agent_id"`
	Provider  string `json:"provider"`
	SessionID string `json:"session_id,omitempty"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: agent-bridge start|request|request-status|launcher|local-model|local-queue|approvals|approve|deny|ask|list|show|wait|ack|stop|snapshot|latest|watch|daemon|speak|voice|voice-log")
	}
	switch args[0] {
	case "start":
		return start(args[1:])
	case "request":
		return requestCommand(args[1:])
	case "request-status":
		return requestStatusCommand(args[1:])
	case "launcher":
		return launcherCommand(args[1:])
	case "local-model":
		return localModelCommand(args[1:])
	case "local-queue":
		return localQueueCommand(args[1:])
	case "approvals":
		return approvalsCommand(args[1:])
	case "approve", "deny":
		return resolveApprovalCommand(args[0], args[1:])
	case "ask":
		return ask(args[1:])
	case "list":
		return list(args[1:])
	case "show":
		return show(args[1:])
	case "wait":
		return wait(args[1:])
	case "ack":
		return ack(args[1:])
	case "stop":
		return stop(args[1:])
	case "snapshot":
		return snapshotCommand(args[1:])
	case "latest":
		return latestCommand(args[1:])
	case "watch":
		return watchCommand(args[1:])
	case "daemon":
		return daemonCommand(args[1:])
	case "speak":
		return speak(args[1:])
	case "voice":
		return voiceCommand(args[1:])
	case "voice-log":
		return voiceLogCommand(args[1:])
	case "voice-approval-mcp":
		// Hidden: launched by Claude Code as the voice permission-prompt tool.
		return voiceApprovalMCPCommand(args[1:])
	case "_worker":
		if len(args) != 2 {
			return errors.New("worker needs a job ID")
		}
		return worker(args[1])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func root() (string, error) {
	if override := os.Getenv("AGENT_BRIDGE_STATE_DIR"); override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "agent-bridge"), nil
}

func jobDir(id string) (string, error) {
	if len(id) == 0 || strings.ContainsAny(id, `/\.`) {
		return "", errors.New("invalid job ID")
	}
	r, err := root()
	if err != nil {
		return "", err
	}
	return filepath.Join(r, "jobs", id), nil
}

func readJob(id string) (Job, error) {
	dir, err := jobDir(id)
	if err != nil {
		return Job{}, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "job.json"))
	if err != nil {
		return Job{}, err
	}
	var j Job
	err = json.Unmarshal(b, &j)
	if j.Agent == "" {
		j.Agent = "codex"
	}
	if j.Role == "" {
		j.Role = "coder"
	}
	return j, err
}

func writeJob(j Job) error {
	dir, err := jobDir(j.ID)
	if err != nil {
		return err
	}
	j.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "job.json.tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "job.json"))
}

func startRole(agent, role string) string {
	if role != "" {
		return role
	}
	if agent == "claude" {
		return "epm"
	}
	return "coder"
}

func start(args []string) error {
	f := flag.NewFlagSet("start", flag.ContinueOnError)
	cwd := f.String("cwd", "", "Git worktree or project directory")
	name := f.String("name", "", "Short job label")
	write := f.Bool("write", false, "Allow workspace-write sandbox")
	agent := f.String("agent", "codex", "Backend: codex for coder, claude for EPM")
	role := f.String("role", "", "Role: coder or epm (default: epm with --agent claude, coder with --agent codex)")
	profileName := f.String("profile", "", "Named job profile")
	promptFile := f.String("prompt-file", "", "Prompt file; default: stdin")
	spawnedBy := f.String("spawned-by", "", "Agent ID from snapshot; default: detect invoking agent")
	parentJob := f.String("parent-job", "", "Requesting bridge job ID")
	fallbackFor := f.String("fallback-for", "", "Usage-halted Codex coder job ID this Claude coder replaces (required for --agent claude --role coder)")
	var contextPaths contextFlags
	f.Var(&contextPaths, "context", "Context file or directory relative to AGENT_BRIDGE_CONTEXT_DIR (repeatable, comma-separated)")
	contextWork := f.Bool("context-work", false, "Allow work-sensitive context selected by --context")
	if err := f.Parse(args); err != nil {
		return err
	}
	*role = startRole(*agent, *role)
	if err := validAgentRoleFB(*agent, *role, *fallbackFor); err != nil {
		return err
	}
	if *fallbackFor != "" {
		if *agent != "claude" || *role != "coder" {
			return errors.New("--fallback-for is only for --agent claude --role coder")
		}
		s, err := fallbackSnapshot()
		if err != nil {
			return err
		}
		if err := checkFallback(*fallbackFor, s); err != nil {
			return err
		}
		if *cwd == "" {
			orig, _ := readJob(*fallbackFor)
			*cwd = orig.Cwd
		}
	}
	if *cwd == "" {
		return errors.New("--cwd is required")
	}
	if *profileName == "" {
		*profileName = defaultProfile(*agent, *role)
	}
	profiles, err := loadProfiles()
	if err != nil {
		return err
	}
	profile, ok := profiles[*profileName]
	if !ok {
		return fmt.Errorf("unknown profile %q", *profileName)
	}
	if profile.Agent != *agent {
		return errors.New("profile agent does not match --agent")
	}
	if *role == "epm" && *profileName != "claude-epm" && *profileName != "claude-deep" {
		return errors.New("EPM needs an EPM-capable profile")
	}
	if *role == "coder" && *profileName == "claude-epm" {
		return errors.New("claude-epm is for EPM jobs")
	}
	if f.NArg() != 0 {
		return errors.New("pass prompt on stdin or with --prompt-file")
	}
	abs, err := filepath.Abs(*cwd)
	if err != nil {
		return err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return errors.New("--cwd must be a directory")
	}
	var prompt []byte
	if *promptFile != "" {
		prompt, err = os.ReadFile(*promptFile)
	} else {
		prompt, err = io.ReadAll(io.LimitReader(os.Stdin, 1<<20+1))
	}
	if err != nil {
		return err
	}
	if len(prompt) == 0 || len(prompt) > 1<<20 {
		return errors.New("prompt must be 1 byte to 1 MiB")
	}
	selected, err := selectContext(contextPaths, *contextWork)
	if err != nil {
		return err
	}
	var spawner *Spawner
	if *parentJob != "" {
		parent, e := readJob(*parentJob)
		if e != nil || parent.Status != "running" || parent.Agent != "claude" || parent.Role != "epm" || !alive(parent.WorkerPID, parent.WorkerStart) {
			return errors.New("parent job is not a running Claude EPM")
		}
		spawner = &Spawner{AgentID: parent.ID, Provider: parent.Agent, SessionID: parent.SessionID}
	} else {
		spawner, err = resolveSpawner(*spawnedBy)
		if err != nil {
			return err
		}
	}
	r, err := root()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(r, "jobs"), 0700); err != nil {
		return err
	}
	id := fmt.Sprintf("%s-%d", time.Now().UTC().Format("20060102T150405"), os.Getpid())
	dir, _ := jobDir(id)
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	contextFiles, err := copyContext(dir, selected)
	if err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	if len(contextFiles) != 0 {
		prompt = contextPrompt(prompt, dir, contextFiles)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompt.txt"), prompt, 0600); err != nil {
		return err
	}
	sandbox := "read-only"
	if *write {
		sandbox = "workspace-write"
	}
	launcher := "systemd"
	if os.Getenv("AGENT_BRIDGE_LAUNCH_MODE") == "direct" {
		launcher = "direct"
	}
	depth := 0
	if spawner != nil {
		depth = 1
	}
	if *parentJob != "" {
		parent, _ := readJob(*parentJob)
		depth = parent.Depth + 1
	} else if parent := ancestorJob(os.Getppid()); parent != nil {
		depth = parent.Depth + 1
	}
	j := Job{ID: id, Name: *name, Agent: *agent, Role: *role, Profile: profile.Name, Model: profile.Model, Effort: profile.Effort, TimeLimit: profile.TimeLimit.String(), Depth: depth, SpawnedBy: spawner, FallbackFor: *fallbackFor, Cwd: abs, Sandbox: sandbox, Launcher: launcher, Status: "queued", CreatedAt: time.Now().UTC(), EventsPath: filepath.Join(dir, "events.jsonl"), StderrPath: filepath.Join(dir, "stderr.log"), ResultPath: filepath.Join(dir, "result.txt"), ContextFiles: contextFiles}
	if err := writeJob(j); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if os.Getenv("AGENT_BRIDGE_LAUNCH_MODE") != "direct" {
		binEnv, defaultBin := "AGENT_BRIDGE_CODEX_BIN", "codex"
		if *agent == "claude" {
			binEnv, defaultBin = "AGENT_BRIDGE_CLAUDE_BIN", "claude"
		}
		bin := os.Getenv(binEnv)
		if bin == "" {
			bin = defaultBin
		}
		if bin, err = exec.LookPath(bin); err != nil {
			return err
		}
		cmd := exec.Command("systemd-run", "--user", "--quiet", "--collect", "--service-type=exec", "--unit=agent-bridge-"+id,
			"--setenv=AGENT_BRIDGE_STATE_DIR="+r, "--setenv="+binEnv+"="+bin, self, "_worker", id)
		if out, err := cmd.CombinedOutput(); err != nil {
			j.Status, j.Error = "failed", strings.TrimSpace(string(out))
			_ = writeJob(j)
			return fmt.Errorf("systemd-run: %w: %s", err, strings.TrimSpace(string(out)))
		}
		fmt.Println(id)
		return nil
	}
	cmd := exec.Command(self, "_worker", id)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		j.Status, j.Error = "failed", err.Error()
		_ = writeJob(j)
		return err
	}
	fmt.Println(id)
	return cmd.Process.Release()
}

func worker(id string) error {
	syscall.Umask(0077)
	j, err := readJob(id)
	if err != nil {
		return err
	}
	dir, _ := jobDir(id)
	j.WorkerPID = os.Getpid()
	j.WorkerStart = processStart(j.WorkerPID)
	j.Status = "starting"
	if err := writeJob(j); err != nil {
		return err
	}
	defer os.Remove(filepath.Join(dir, "prompt.txt"))
	stdin, err := os.Open(filepath.Join(dir, "prompt.txt"))
	if err != nil {
		return finishError(&j, err)
	}
	defer stdin.Close()
	events, err := os.OpenFile(j.EventsPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return finishError(&j, err)
	}
	defer events.Close()
	stderr, err := os.OpenFile(j.StderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return finishError(&j, err)
	}
	defer stderr.Close()
	var cmd *exec.Cmd
	if j.Agent == "claude" {
		bin := os.Getenv("AGENT_BRIDGE_CLAUDE_BIN")
		if bin == "" {
			bin = "claude"
		}
		tools := "Read,Grep,Glob"
		availableTools := tools
		if j.Sandbox == "workspace-write" {
			tools = "Read,Grep,Glob,Edit,Write,Bash(go test *),Bash(go vet *),Bash(make ci),Bash(gofmt *),Bash(git status *),Bash(git diff *),Bash(git log *),Bash(git add *)"
			availableTools = "Read,Grep,Glob,Edit,Write,Bash"
		}
		args := []string{"-p", "--output-format", "stream-json", "--verbose", "--permission-mode", "dontAsk", "--tools=" + availableTools, "--allowed-tools=" + tools}
		if j.Model != "" {
			args = append(args, "--model", j.Model, "--effort", j.Effort)
		}
		cmd = exec.Command(bin, args...)
		cmd.Dir = j.Cwd
	} else {
		bin := os.Getenv("AGENT_BRIDGE_CODEX_BIN")
		if bin == "" {
			bin = "codex"
		}
		args := []string{"exec", "--json", "--sandbox", j.Sandbox, "--cd", j.Cwd, "--output-last-message", j.ResultPath}
		if j.Model != "" {
			args = append(args, "-m", j.Model, "-c", "model_reasoning_effort="+j.Effort)
		}
		cmd = exec.Command(bin, append(args, "-")...)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, events, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return finishError(&j, err)
	}
	j.CodexPID = cmd.Process.Pid
	j.CodexStart = processStart(j.CodexPID)
	j.Status = "running"
	if err := writeJob(j); err != nil {
		syscall.Kill(-j.CodexPID, syscall.SIGTERM)
		return err
	}
	limit, _ := time.ParseDuration(j.TimeLimit)
	var timedOut bool
	if limit > 0 {
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err = <-done:
		case <-time.After(limit):
			timedOut = true
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			select {
			case err = <-done:
			case <-time.After(3 * time.Second):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				err = <-done
			}
		}
	} else {
		err = cmd.Wait()
	}
	if j.Agent == "claude" {
		if e := extractClaudeResult(j.EventsPath, j.ResultPath); e != nil && err == nil {
			err = e
		}
	}
	code := 0
	if err != nil {
		var e *exec.ExitError
		if errors.As(err, &e) {
			code = e.ExitCode()
		} else {
			return finishError(&j, err)
		}
	}
	j.ExitCode = &code
	if timedOut {
		j.Status = "halted"
		j.Halt = &Halt{Kind: "time_limit", Code: "time_limit", Source: "launcher", DetectedAt: time.Now().UTC()}
	} else if _, err := os.Stat(filepath.Join(dir, "stop")); err == nil {
		j.Status = "stopped"
	} else if j.Halt = detectJobHalt(j.EventsPath, j.StderrPath, time.Now().UTC(), code); j.Halt != nil {
		j.Status = "halted"
	} else if code == 0 {
		j.Status = "completed"
	} else {
		j.Status = "failed"
	}
	return writeJob(j)
}

func finishError(j *Job, err error) error {
	j.Status, j.Error = "failed", err.Error()
	_ = writeJob(*j)
	return err
}

func processStart(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	end := strings.LastIndexByte(string(b), ')')
	if end < 0 {
		return ""
	}
	fields := strings.Fields(string(b[end+1:]))
	if len(fields) < 20 {
		return ""
	}
	return fields[19]
}

func processParent(pid int) int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	end := strings.LastIndexByte(string(b), ')')
	if end < 0 {
		return 0
	}
	fields := strings.Fields(string(b[end+1:]))
	if len(fields) < 2 {
		return 0
	}
	parent, _ := strconv.Atoi(fields[1])
	return parent
}

func resolveSpawner(explicit string) (*Spawner, error) {
	s, err := collectSnapshot()
	if err != nil {
		return nil, err
	}
	if explicit != "" {
		for _, a := range s.Agents {
			if a.ID == explicit {
				return &Spawner{AgentID: a.ID, Provider: a.Provider, SessionID: a.SessionID}, nil
			}
		}
		return nil, fmt.Errorf("spawning agent %q is not active in snapshot", explicit)
	}
	byPID := make(map[int]AgentUsage, len(s.Agents))
	for _, a := range s.Agents {
		if a.HostPID > 0 {
			byPID[a.HostPID] = a
		}
	}
	for pid, depth := os.Getppid(), 0; pid > 1 && depth < 32; pid, depth = processParent(pid), depth+1 {
		if a, ok := byPID[pid]; ok {
			return &Spawner{AgentID: a.ID, Provider: a.Provider, SessionID: a.SessionID}, nil
		}
	}
	return nil, nil
}

func alive(pid int, start string) bool {
	return pid > 0 && start != "" && processStart(pid) == start
}

func current(j Job) Job {
	if (j.Status == "queued" || j.Status == "starting" || j.Status == "running") && j.WorkerPID != 0 && !alive(j.WorkerPID, j.WorkerStart) && time.Since(j.UpdatedAt) > 5*time.Second {
		j.Status = "lost"
	}
	_ = scanJSONLines(j.EventsPath, func(line []byte) {
		var event struct {
			Type      string `json:"type"`
			ThreadID  string `json:"thread_id"`
			SessionID string `json:"session_id"`
			Usage     struct {
				Input           int64 `json:"input_tokens"`
				Cached          int64 `json:"cached_input_tokens"`
				CacheReadClaude int64 `json:"cache_read_input_tokens"`
				CacheWrite      int64 `json:"cache_write_input_tokens"`
				Output          int64 `json:"output_tokens"`
				Reasoning       int64 `json:"reasoning_output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(line, &event) != nil {
			return
		}
		if event.Type == "thread.started" {
			j.SessionID = event.ThreadID
			return
		}
		if event.Type == "result" && j.Agent == "claude" {
			j.SessionID = event.SessionID
			j.Usage = &TokenUsage{Input: event.Usage.Input, CacheRead: event.Usage.CacheReadClaude, CacheWrite: event.Usage.CacheWrite, Output: event.Usage.Output}
			return
		}
		if event.Type != "turn.completed" {
			return
		}
		j.Usage = &TokenUsage{Input: event.Usage.Input, CacheRead: event.Usage.Cached, CacheWrite: event.Usage.CacheWrite, Output: event.Usage.Output, Reasoning: event.Usage.Reasoning}
	})
	if j.Status == "failed" && j.Halt == nil && j.Provider == "" {
		code := 1
		if j.ExitCode != nil {
			code = *j.ExitCode
		}
		j.Halt = detectJobHalt(j.EventsPath, j.StderrPath, j.UpdatedAt, code)
		if j.Halt != nil {
			j.Status = "halted"
		}
	}
	return j
}

func list(args []string) error {
	if len(args) != 0 {
		return errors.New("list takes no arguments")
	}
	r, err := root()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(r, "jobs"))
	if os.IsNotExist(err) {
		fmt.Println("[]")
		return nil
	}
	if err != nil {
		return err
	}
	jobs := make([]Job, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			if j, err := readJob(e.Name()); err == nil {
				jobs = append(jobs, current(j))
			}
		}
	}
	sort.Slice(jobs, func(i, k int) bool { return jobs[i].CreatedAt.After(jobs[k].CreatedAt) })
	return json.NewEncoder(os.Stdout).Encode(jobs)
}

func show(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: agent-bridge show ID")
	}
	j, err := readJob(args[0])
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(current(j))
}

func wait(args []string) error {
	f := flag.NewFlagSet("wait", flag.ContinueOnError)
	interval := f.Duration("interval", 2*time.Second, "Status polling interval (at least 100ms)")
	if err := f.Parse(args); err != nil {
		return err
	}
	ids := f.Args()
	if len(ids) == 0 || *interval < 100*time.Millisecond {
		return errors.New("usage: agent-bridge wait [--interval 2s] ID [ID ...]")
	}
	for _, id := range ids {
		if _, err := readJob(id); err != nil {
			return err
		}
	}
	for {
		jobs := make([]Job, 0, len(ids))
		allDone := true
		for _, id := range ids {
			j, err := readJob(id)
			if err != nil {
				return err
			}
			j = current(j)
			jobs = append(jobs, j)
			if j.Status == "queued" || j.Status == "starting" || j.Status == "running" {
				allDone = false
			}
		}
		if allDone {
			if err := json.NewEncoder(os.Stdout).Encode(jobs); err != nil {
				return err
			}
			for _, j := range jobs {
				if j.Status != "completed" {
					if j.Halt != nil {
						return fmt.Errorf("job %s halted: %s (%s)", j.ID, haltLabel(j.Halt), j.Halt.Code)
					}
					return fmt.Errorf("job %s ended with status %s", j.ID, j.Status)
				}
			}
			return nil
		}
		time.Sleep(*interval)
	}
}

func ack(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: agent-bridge ack ID")
	}
	j, err := readJob(args[0])
	if err != nil {
		return err
	}
	j = current(j)
	if j.Status != "halted" {
		return fmt.Errorf("job is %s, not halted", j.Status)
	}
	if j.AcknowledgedAt == nil {
		now := time.Now().UTC()
		j.AcknowledgedAt = &now
		if err := writeJob(j); err != nil {
			return err
		}
	}
	return json.NewEncoder(os.Stdout).Encode(j)
}

func stop(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: agent-bridge stop ID")
	}
	j, err := readJob(args[0])
	if err != nil {
		return err
	}
	if j.Status != "running" {
		return fmt.Errorf("job is %s", j.Status)
	}
	dir, _ := jobDir(j.ID)
	if err := os.WriteFile(filepath.Join(dir, "stop"), nil, 0600); err != nil {
		return err
	}
	if !alive(j.WorkerPID, j.WorkerStart) || !alive(j.CodexPID, j.CodexStart) {
		return errors.New("job process is no longer running")
	}
	return syscall.Kill(-j.CodexPID, syscall.SIGTERM)
}
