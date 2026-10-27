package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Snapshot combines bridge activity with machine-local GPU and model usage.
type Snapshot struct {
	Version        int                `json:"version"`
	SampledAt      time.Time          `json:"sampled_at"`
	Agents         []AgentUsage       `json:"agents"`
	Jobs           []Job              `json:"jobs"`
	Sources        []SourceStatus     `json:"sources"`
	GPUs           []GPU              `json:"gpus"`
	GPUProcesses   []GPUProcess       `json:"gpu_processes"`
	LocalModels    []LocalModel       `json:"local_models"`
	ProviderLimits []ProviderLimit    `json:"provider_limits"`
	Steinberger    *SteinbergerStatus `json:"steinberger,omitempty"`
	RepoAttention  *RepoAttentionFeed `json:"repo_attention,omitempty"`
}

type SourceStatus struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Detail    string `json:"detail,omitempty"`
}

type AgentUsage struct {
	ID            string       `json:"id"`
	Provider      string       `json:"provider"`
	Name          string       `json:"name"`
	Phase         string       `json:"phase"`
	Detail        string       `json:"detail,omitempty"`
	ReportOrigin  string       `json:"report_origin,omitempty"`
	ActivityState string       `json:"activity_state"`
	StateDetail   string       `json:"state_detail"`
	ActiveJobIDs  []string     `json:"active_job_ids"`
	HaltedJobIDs  []string     `json:"halted_job_ids"`
	Halt          *Halt        `json:"halt,omitempty"`
	Target        *AgentTarget `json:"target,omitempty"`
	HostPID       int          `json:"host_pid,omitempty"`
	SampledAt     time.Time    `json:"sampled_at"`
	SessionID     string       `json:"session_id,omitempty"`
	Cwd           string       `json:"cwd,omitempty"`
	Model         string       `json:"model,omitempty"`
	SpawnedBy     *Spawner     `json:"spawned_by"`
	UsageKnown    bool         `json:"usage_known"`
	Usage         TokenUsage   `json:"usage"`
}

type AgentTarget struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type TokenUsage struct {
	Input      int64 `json:"input_tokens"`
	CacheRead  int64 `json:"cache_read_input_tokens"`
	CacheWrite int64 `json:"cache_write_input_tokens"`
	Output     int64 `json:"output_tokens"`
	Reasoning  int64 `json:"reasoning_output_tokens,omitempty"`
}

type activityFile struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Phase        string       `json:"phase"`
	Detail       string       `json:"detail"`
	ReportOrigin string       `json:"report_origin"`
	Target       *AgentTarget `json:"target"`
	SampledAt    time.Time    `json:"sampledAt"`
	Model        string       `json:"model,omitempty"`
	SessionID    string       `json:"session_id,omitempty"`
	UsageKnown   bool         `json:"usage_known,omitempty"`
	Usage        TokenUsage   `json:"usage,omitempty"`
}

func monitorHome() (string, error) {
	if v := os.Getenv("AGENT_BRIDGE_HOME"); v != "" {
		return v, nil
	}
	return os.UserHomeDir()
}

func activityDir(home string) string {
	if v := os.Getenv("AGENT_BRIDGE_ACTIVITY_DIR"); v != "" {
		return v
	}
	return filepath.Join(home, ".local", "state", "digital-terrarium", "agents")
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:6])
}

func collectSnapshot() (Snapshot, error) {
	home, err := monitorHome()
	if err != nil {
		return Snapshot{}, err
	}
	now := time.Now().UTC()
	s := Snapshot{Version: 1, SampledAt: now, Agents: []AgentUsage{}, Jobs: []Job{}, Sources: []SourceStatus{}, GPUs: []GPU{}, GPUProcesses: []GPUProcess{}, LocalModels: []LocalModel{}, ProviderLimits: []ProviderLimit{}}
	// Claude exposes no stable local rate-limit data, so only Codex is listed.
	if limits := readCodexLimits(home, limitsNow()); limits != nil {
		s.ProviderLimits = limits
		s.Sources = append(s.Sources, SourceStatus{Name: "codex-limits", Available: true})
	} else {
		s.Sources = append(s.Sources, SourceStatus{Name: "codex-limits", Detail: "no rate_limits event in recent Codex sessions"})
	}
	dir := activityDir(home)
	entries, err := os.ReadDir(dir)
	if err != nil {
		s.Sources = append(s.Sources, SourceStatus{Name: "activity-hooks", Detail: err.Error()})
	} else {
		s.Sources = append(s.Sources, SourceStatus{Name: "activity-hooks", Available: true})
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				continue
			}
			var a activityFile
			if json.Unmarshal(b, &a) != nil || a.ID == "" || a.SampledAt.After(now.Add(5*time.Second)) {
				continue
			}
			hostPID, current := activityCurrent(filepath.Join(dir, entry.Name()), a, now)
			if !current {
				continue
			}
			provider := ""
			switch {
			case strings.HasPrefix(a.ID, "claude-"):
				provider = "claude"
			case strings.HasPrefix(a.ID, "codex-"):
				provider = "codex"
			case strings.HasPrefix(a.ID, "local-"):
				provider = "local"
			default:
				continue
			}
			s.Agents = append(s.Agents, AgentUsage{ID: a.ID, Provider: provider, Name: a.Name, Phase: a.Phase, Detail: a.Detail, ReportOrigin: a.ReportOrigin, Target: a.Target, HostPID: hostPID, SampledAt: a.SampledAt, Model: a.Model, SessionID: a.SessionID, UsageKnown: a.UsageKnown, Usage: a.Usage})
		}
	}
	for i := range s.Agents {
		a := &s.Agents[i]
		if a.Provider == "local" {
			continue
		}
		path := findSessionFile(home, a.Provider, a.ID, now)
		if path == "" {
			continue
		}
		var err error
		if a.Provider == "claude" {
			err = readClaudeUsage(path, a)
		} else {
			err = readCodexUsage(path, a)
		}
		a.UsageKnown = a.SessionID != ""
		if err != nil {
			s.Sources = append(s.Sources, SourceStatus{Name: a.Provider + "-usage", Detail: err.Error()})
		}
	}
	r, err := root()
	if err != nil {
		return s, err
	}
	jobEntries, err := os.ReadDir(filepath.Join(r, "jobs"))
	if err == nil {
		s.Sources = append(s.Sources, SourceStatus{Name: "bridge-jobs", Available: true})
		for _, e := range jobEntries {
			if !e.IsDir() {
				continue
			}
			j, err := readJob(e.Name())
			if err != nil {
				continue
			}
			j = current(j)
			if j.Status == "running" || j.Status == "starting" || j.Status == "queued" || (j.Status == "halted" && j.AcknowledgedAt == nil) || now.Sub(j.UpdatedAt) < time.Hour {
				s.Jobs = append(s.Jobs, j)
			}
		}
	} else {
		s.Sources = append(s.Sources, SourceStatus{Name: "bridge-jobs", Detail: err.Error()})
	}
	sort.Slice(s.Agents, func(i, j int) bool { return s.Agents[i].ID < s.Agents[j].ID })
	sort.Slice(s.Jobs, func(i, j int) bool { return s.Jobs[i].CreatedAt.After(s.Jobs[j].CreatedAt) })
	for i := range s.Agents {
		for _, j := range s.Jobs {
			if j.SessionID != "" && s.Agents[i].SessionID == j.SessionID && s.Agents[i].Provider == "codex" {
				s.Agents[i].SpawnedBy = j.SpawnedBy
				break
			}
		}
		classifyAgent(&s.Agents[i], s.Jobs)
	}
	collectGPU(&s)
	for _, model := range s.LocalModels {
		found := false
		for _, a := range s.Agents {
			if a.Provider == "local" && a.Model == model.Name && a.ActivityState == "active" {
				found = true
				break
			}
		}
		if !found {
			s.Agents = append(s.Agents, AgentUsage{ID: "local-ollama-" + model.Name, Provider: "local", Name: "ollama", Model: model.Name, Phase: "loaded", ActivityState: "loaded", StateDetail: "model loaded by Ollama", SampledAt: now, ActiveJobIDs: []string{}, HaltedJobIDs: []string{}})
		}
	}
	sort.Slice(s.Agents, func(i, j int) bool { return s.Agents[i].ID < s.Agents[j].ID })
	if status, err := cachedSteinberger(now); err == nil {
		s.Steinberger = &status
		s.Sources = append(s.Sources, SourceStatus{Name: "steinberger", Available: true})
	} else {
		s.Sources = append(s.Sources, SourceStatus{Name: "steinberger", Detail: err.Error()})
	}
	if feed, err := readRepoAttentionFeed(home, now); err == nil {
		s.RepoAttention = &feed
		s.Sources = append(s.Sources, SourceStatus{Name: "repo-attention-feed", Available: true})
	} else {
		s.Sources = append(s.Sources, SourceStatus{Name: "repo-attention-feed", Detail: err.Error()})
	}
	return s, nil
}

func classifyAgent(a *AgentUsage, jobs []Job) {
	a.ActiveJobIDs = []string{}
	a.HaltedJobIDs = []string{}
	workerRunning, childRunning := false, false
	for _, j := range jobs {
		isWorker := a.Provider == "codex" && j.SessionID != "" && j.SessionID == a.SessionID
		isParent := j.SpawnedBy != nil && j.SpawnedBy.AgentID == a.ID || j.RequestedBy != nil && j.RequestedBy.AgentID == a.ID
		if j.Status == "halted" && j.AcknowledgedAt == nil && (isWorker || isParent) {
			a.HaltedJobIDs = append(a.HaltedJobIDs, j.ID)
		}
		if j.Status != "queued" && j.Status != "starting" && j.Status != "running" {
			continue
		}
		if isWorker {
			workerRunning = true
			a.ActiveJobIDs = append(a.ActiveJobIDs, j.ID)
		} else if isParent {
			childRunning = true
			a.ActiveJobIDs = append(a.ActiveJobIDs, j.ID)
		}
	}
	switch {
	case a.Halt != nil:
		a.ActivityState, a.StateDetail = "halted_usage", haltLabel(a.Halt)
	case a.Phase == "error":
		a.ActivityState, a.StateDetail = "error", a.Detail
	case a.Phase == "waiting" && strings.HasPrefix(a.Detail, "approval requested"):
		a.ActivityState, a.StateDetail = "approval_required", a.Detail
	case workerRunning:
		a.ActivityState, a.StateDetail = "running_job", "bridge job still running"
	case a.Phase == "thinking" || a.Phase == "working" || a.Phase == "tool":
		a.ActivityState, a.StateDetail = "active", a.Detail
	case childRunning:
		a.ActivityState = "has_running_children"
		a.StateDetail = fmt.Sprintf("%d spawned bridge job(s) still running", len(a.ActiveJobIDs))
	case len(a.HaltedJobIDs) > 0 && (a.Phase == "waiting" || a.Phase == "idle"):
		a.ActivityState, a.StateDetail = "halted_usage", "spawned job halted by token or usage limit"
	case a.ReportOrigin == "event" && ((a.Phase == "waiting" && a.Detail == "ready for direction") || (a.Phase == "idle" && a.Detail == "session ready")):
		a.ActivityState, a.StateDetail = "ready", "turn completed; waiting for direction"
	case a.Provider == "local" && a.Phase == "idle":
		a.ActivityState, a.StateDetail = "idle", a.Detail
	default:
		a.ActivityState, a.StateDetail = "unverified", "hook is quiet; readiness unverified"
	}
}

func activityCurrent(path string, a activityFile, now time.Time) (int, bool) {
	age := now.Sub(a.SampledAt)
	if strings.HasPrefix(a.ID, "local-") {
		if a.Phase == "idle" {
			return 0, age <= time.Hour
		}
		return 0, a.Phase == "working" && age <= 5*time.Second
	}
	switch a.Phase {
	case "thinking", "working", "tool":
		if age > 5*time.Second {
			return 0, false
		}
	case "idle", "waiting", "error":
		// A watcher tracks its host process, which may serve several sessions.
		// An old session must not remain visible indefinitely just because that
		// shared process is still alive.
		if age > time.Hour {
			return 0, false
		}
	default:
		return 0, false
	}
	pidBytes, err := os.ReadFile(strings.TrimSuffix(path, ".json") + ".watch.pid")
	if os.IsNotExist(err) {
		return 0, age <= 5*time.Second
	}
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return 0, false
	}
	args := bytes.Split(bytes.TrimSuffix(cmdline, []byte{0}), []byte{0})
	if len(args) != 6 {
		return 0, false
	}
	script := filepath.Base(string(args[1]))
	valid := (script == "claude-activity-hook.py" || script == "codex-activity-hook.py") &&
		string(args[2]) == "--watch" && string(args[3]) == path && string(args[5]) == a.ID
	if !valid {
		return 0, false
	}
	hostPID, _ := strconv.Atoi(string(args[4]))
	return hostPID, true
}

func findSessionFile(home, provider, activityID string, now time.Time) string {
	base := filepath.Join(home, ".codex", "sessions")
	if provider == "claude" {
		base = filepath.Join(home, ".claude", "projects")
	}
	var newest string
	var newestTime time.Time
	_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil || now.Sub(info.ModTime()) > 24*time.Hour {
			return nil
		}
		stem := strings.TrimSuffix(d.Name(), ".jsonl")
		if provider == "codex" {
			parts := strings.Split(stem, "-")
			if len(parts) < 11 {
				return nil
			}
			stem = strings.Join(parts[len(parts)-5:], "-")
		}
		identity := stem
		if provider == "claude" {
			identity = "claude:" + stem
		}
		if provider+"-"+shortHash(identity) == activityID && info.ModTime().After(newestTime) {
			newest, newestTime = path, info.ModTime()
		}
		return nil
	})
	return newest
}

func scanJSONLines(path string, visit func([]byte)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 128*1024)
	for {
		line, err := r.ReadBytes('\n')
		if err == nil {
			visit(line)
			continue
		}
		if errors.Is(err, io.EOF) {
			return nil
		} // ignore a line still being written
		return err
	}
}

func readClaudeUsage(path string, a *AgentUsage) error {
	byMessage := map[string]TokenUsage{}
	cwds := map[string]int{}
	err := scanJSONLines(path, func(line []byte) {
		var row struct {
			Type      string          `json:"type"`
			Subtype   string          `json:"subtype"`
			Timestamp string          `json:"timestamp"`
			Error     json.RawMessage `json:"error"`
			SessionID string          `json:"sessionId"`
			Cwd       string          `json:"cwd"`
			Message   struct {
				ID    string `json:"id"`
				Model string `json:"model"`
				Usage struct {
					Input      int64 `json:"input_tokens"`
					CacheRead  int64 `json:"cache_read_input_tokens"`
					CacheWrite int64 `json:"cache_creation_input_tokens"`
					Output     int64 `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &row) != nil {
			return
		}
		if row.Type == "system" && row.Subtype == "api_error" {
			code, message := errorFields(row.Error)
			at, err := time.Parse(time.RFC3339Nano, row.Timestamp)
			if err != nil {
				at = time.Now().UTC()
			}
			if found := classifyUsageError(code, message, "claude_api_error", at); found != nil {
				a.Halt = found
			}
			return
		}
		if row.Type != "assistant" || row.Message.ID == "" {
			return
		}
		a.Halt = nil // A later successful response resolved any prior API error.
		a.SessionID, a.Model = row.SessionID, row.Message.Model
		if row.Cwd != "" {
			cwds[row.Cwd]++
		}
		byMessage[row.Message.ID] = TokenUsage{Input: row.Message.Usage.Input + row.Message.Usage.CacheRead + row.Message.Usage.CacheWrite, CacheRead: row.Message.Usage.CacheRead, CacheWrite: row.Message.Usage.CacheWrite, Output: row.Message.Usage.Output}
	})
	for _, u := range byMessage {
		a.Usage.Input += u.Input
		a.Usage.CacheRead += u.CacheRead
		a.Usage.CacheWrite += u.CacheWrite
		a.Usage.Output += u.Output
	}
	for cwd, count := range cwds {
		if count > cwds[a.Cwd] {
			a.Cwd = cwd
		}
	}
	return err
}

func readCodexUsage(path string, a *AgentUsage) error {
	return scanJSONLines(path, func(line []byte) {
		var row struct {
			Type    string `json:"type"`
			Payload struct {
				Type  string `json:"type"`
				ID    string `json:"id"`
				Cwd   string `json:"cwd"`
				Model string `json:"model"`
				Info  struct {
					Total struct {
						Input      int64 `json:"input_tokens"`
						Cached     int64 `json:"cached_input_tokens"`
						CacheWrite int64 `json:"cache_write_input_tokens"`
						Output     int64 `json:"output_tokens"`
						Reasoning  int64 `json:"reasoning_output_tokens"`
					} `json:"total_token_usage"`
				} `json:"info"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &row) != nil {
			return
		}
		if row.Type == "session_meta" {
			a.SessionID, a.Cwd = row.Payload.ID, row.Payload.Cwd
		}
		if row.Type == "turn_context" {
			a.Model = row.Payload.Model
		}
		if row.Type == "event_msg" && row.Payload.Type == "token_count" {
			u := row.Payload.Info.Total
			a.Usage = TokenUsage{Input: u.Input, CacheRead: u.Cached, CacheWrite: u.CacheWrite, Output: u.Output, Reasoning: u.Reasoning}
		}
	})
}

func snapshotCommand(args []string) error {
	if len(args) != 0 {
		return errors.New("snapshot takes no arguments")
	}
	s, err := collectSnapshot()
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(s)
}
