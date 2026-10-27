package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type launchRequest struct {
	Launch      string   `json:"launch"`
	Role        string   `json:"role"`
	Agent       string   `json:"agent"`
	Profile     string   `json:"profile,omitempty"`
	Repo        string   `json:"repo"`
	Task        string   `json:"task"`
	PromptFile  string   `json:"prompt_file"`
	Context     []string `json:"context,omitempty"`
	Reason      string   `json:"reason"`
	FallbackFor string   `json:"fallback_for,omitempty"`
}
type requester struct {
	AgentID string `json:"agent_id,omitempty"`
	JobID   string `json:"job_id,omitempty"`
}
type launchRecord struct {
	ID        string        `json:"id"`
	Request   launchRequest `json:"request"`
	Requester requester     `json:"requester"`
	CreatedAt time.Time     `json:"created_at"`
}
type launchResult struct {
	ID        string     `json:"id"`
	Status    string     `json:"status"`
	JobID     string     `json:"job_id,omitempty"`
	Rule      string     `json:"rule,omitempty"`
	Rationale string     `json:"rationale,omitempty"`
	SpawnedBy *Spawner   `json:"spawned_by,omitempty"`
	RetryAt   *time.Time `json:"retry_at,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
}
type modelVerdict struct {
	Verdict      string `json:"verdict"`
	Agent        string `json:"agent"`
	Role         string `json:"role"`
	Profile      string `json:"profile,omitempty"`
	Substitution string `json:"substitution,omitempty"`
	LocalID      string `json:"local_agent_id,omitempty"`
	Rationale    string `json:"rationale"`
	FallbackFor  string `json:"-"` // set by the launcher from the request, never by the model
}

var taskName = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
var launcherModelSerial sync.Mutex
var errCapWait = errors.New("launcher capacity reached")

func launcherSlotAvailable(s Snapshot, profile string) bool {
	total, claude, deep := 0, 0, 0
	for _, j := range s.Jobs {
		if j.Launcher != "launcher" || j.Status != "queued" && j.Status != "starting" && j.Status != "running" {
			continue
		}
		total++
		if j.Agent == "claude" {
			claude++
		}
		if j.Profile == "claude-deep" {
			deep++
		}
	}
	maxJobs := 4
	if raw := os.Getenv("AGENT_BRIDGE_LAUNCHER_MAX_JOBS"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			maxJobs = n
		}
	}
	if total >= maxJobs {
		return false
	}
	if strings.HasPrefix(profile, "claude-") && claude >= 2 {
		return false
	}
	return profile != "claude-deep" || deep < 1
}

func substituteProfile(profile string, s Snapshot) string {
	profiles := map[string]string{"claude-quick": "codex-quick", "claude-std": "codex-std", "claude-deep": "codex-deep"}
	provider := "codex"
	if strings.HasPrefix(profile, "claude-") {
		provider = "claude"
	}
	if !providerExhausted(provider, s) {
		return ""
	}
	return profiles[profile]
}

func providerExhausted(provider string, s Snapshot) bool {
	exhausted := false
	for _, a := range s.Agents {
		if a.Provider == provider && a.ActivityState == "halted_usage" {
			exhausted = true
		}
	}
	for _, j := range s.Jobs {
		if j.Agent == provider && j.Halt != nil && j.Halt.Kind == "usage_limit" && time.Since(j.UpdatedAt) < time.Hour {
			exhausted = true
		}
	}
	if _, _, ok := limitDeferral(provider, s); ok {
		exhausted = true
	}
	return exhausted
}

func validAgentRole(agent, role string) error { return validAgentRoleFB(agent, role, "") }

// fallbackSnapshot is replaced in tests.
var fallbackSnapshot = collectSnapshot

// checkFallback accepts a Claude coder only as the fallback for a Codex coder
// that halted on a usage limit while Claude has capacity.
func checkFallback(id string, s Snapshot) error {
	j, err := readJob(id)
	if err != nil {
		return fmt.Errorf("fallback job %q: %w", id, err)
	}
	if j.Agent != "codex" || j.Role != "coder" {
		return fmt.Errorf("fallback job %s is not a Codex coder", id)
	}
	if j.Halt == nil || j.Halt.Kind != "usage_limit" {
		return fmt.Errorf("fallback job %s did not halt on a usage limit", id)
	}
	if providerExhausted("claude", s) {
		return errors.New("Claude usage limit; no fallback capacity")
	}
	return nil
}

func validAgentRoleFB(agent, role, fallback string) error {
	if agent != "codex" && agent != "claude" {
		return fmt.Errorf("invalid agent %q", agent)
	}
	if role != "coder" && role != "epm" {
		return fmt.Errorf("invalid role %q", role)
	}
	if agent == "codex" && role == "epm" {
		return errors.New("Codex cannot run the epm role")
	}
	if agent == "claude" && role == "coder" && fallback == "" {
		return errors.New("Claude cannot run the coder role without --fallback-for a usage-halted Codex coder")
	}
	return nil
}
func extractClaudeResult(eventsPath, resultPath string) error {
	var result string
	err := scanJSONLines(eventsPath, func(line []byte) {
		var event struct {
			Type    string `json:"type"`
			Result  string `json:"result"`
			IsError bool   `json:"is_error"`
		}
		if json.Unmarshal(line, &event) == nil && event.Type == "result" && !event.IsError {
			result = event.Result
		}
	})
	if err != nil {
		return err
	}
	if result == "" {
		return errors.New("Claude returned no final result")
	}
	return os.WriteFile(resultPath, []byte(result), 0600)
}
func ancestorJob(pid int) *Job {
	r, err := root()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(r, "jobs"))
	if err != nil {
		return nil
	}
	byPID := map[int]Job{}
	for _, e := range entries {
		if e.IsDir() {
			if j, err := readJob(e.Name()); err == nil && j.WorkerPID > 0 && alive(j.WorkerPID, j.WorkerStart) {
				byPID[j.WorkerPID] = j
			}
		}
	}
	for n := 0; pid > 1 && n < 32; n++ {
		if j, ok := byPID[pid]; ok {
			return &j
		}
		pid = processParent(pid)
	}
	return nil
}
func spoolDir() (string, error) {
	dir := os.Getenv("AGENT_BRIDGE_SPOOL_DIR")
	if dir == "" {
		dir = fmt.Sprintf("/tmp/agent-bridge-%d/spool", os.Getuid())
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || !st.IsDir() || st.Mode().Perm() != 0700 {
		return "", errors.New("spool directory must be owned by the current user and mode 0700")
	}
	return dir, nil
}
func privateRegular(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || sys.Uid != uint32(os.Getuid()) || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
		return fmt.Errorf("%s must be a regular 0600 file owned by the current user", path)
	}
	return nil
}
func requestPath(dir, id string) (string, error) {
	if !taskName.MatchString(id) && !regexp.MustCompile(`^[a-f0-9]{24}$`).MatchString(id) {
		return "", errors.New("invalid request ID")
	}
	return filepath.Join(dir, id+".request.json"), nil
}
func newRequestID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func parseLaunchYAML(data []byte) (launchRequest, error) {
	var r launchRequest
	seen := map[string]bool{}
	lines := strings.Split(string(data), "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(lines[i], " ") || strings.HasPrefix(lines[i], "\t") {
			return r, fmt.Errorf("invalid indentation on line %d", i+1)
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return r, fmt.Errorf("invalid line %d", i+1)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if seen[key] {
			return r, fmt.Errorf("duplicate key %q", key)
		}
		seen[key] = true
		if key == "context" {
			if value == "" {
				for i+1 < len(lines) && strings.HasPrefix(lines[i+1], "  - ") {
					i++
					r.Context = append(r.Context, strings.TrimSpace(strings.TrimPrefix(lines[i], "  - ")))
				}
			} else if value == "[]" {
			} else if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
				for _, p := range strings.Split(value[1:len(value)-1], ",") {
					p = strings.TrimSpace(p)
					if p != "" {
						r.Context = append(r.Context, p)
					}
				}
			} else {
				return r, errors.New("invalid context list")
			}
			continue
		}
		if strings.HasPrefix(value, "\"") {
			v, e := strconv.Unquote(value)
			if e != nil {
				return r, e
			}
			value = v
		} else if strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'") {
			value = strings.ReplaceAll(value[1:len(value)-1], "''", "'")
		}
		switch key {
		case "launch":
			r.Launch = value
		case "role":
			r.Role = value
		case "agent":
			r.Agent = value
		case "profile":
			r.Profile = value
		case "repo":
			r.Repo = value
		case "task":
			r.Task = value
		case "prompt_file":
			r.PromptFile = value
		case "reason":
			r.Reason = value
		case "fallback_for":
			r.FallbackFor = value
		default:
			return r, fmt.Errorf("unknown key %q", key)
		}
	}
	if r.Launch != "v1" || (r.Role != "coder" && r.Role != "epm") || (r.Agent != "codex" && r.Agent != "claude" && r.Agent != "auto") || r.Repo == "" || r.Task == "" || r.PromptFile == "" || r.Reason == "" {
		return r, errors.New("missing or invalid launch fields")
	}
	if r.FallbackFor != "" && (r.Role != "coder" || r.Agent == "codex") {
		return r, errors.New("fallback_for is only for a Claude coder request")
	}
	if r.Agent != "auto" {
		if err := validAgentRoleFB(r.Agent, r.Role, r.FallbackFor); err != nil {
			return r, err
		}
	}
	if !taskName.MatchString(r.Task) {
		return r, errors.New("task must be short kebab case")
	}
	if r.Profile != "" && !taskName.MatchString(r.Profile) {
		return r, errors.New("invalid profile name")
	}
	if !filepath.IsAbs(r.Repo) || !filepath.IsAbs(r.PromptFile) {
		return r, errors.New("repo and prompt_file must be absolute")
	}
	for _, p := range r.Context {
		if err := cleanContextPath(p); err != nil {
			return r, err
		}
	}
	return r, nil
}
func detectRequester() (requester, error) {
	if j := ancestorJob(os.Getppid()); j != nil {
		return requester{JobID: j.ID}, nil
	}
	s, err := resolveSpawner("")
	if err != nil {
		return requester{}, err
	}
	if s == nil {
		return requester{}, errors.New("requester is not a live known agent or running job")
	}
	return requester{AgentID: s.AgentID}, nil
}
func writeJSONPrivate(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
func requestCommand(args []string) error {
	f := flag.NewFlagSet("request", flag.ContinueOnError)
	file := f.String("file", "", "Launch YAML")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *file == "" || f.NArg() != 0 {
		return errors.New("usage: agent-bridge request --file FILE")
	}
	b, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	r, err := parseLaunchYAML(b)
	if err != nil {
		return err
	}
	who, err := detectRequester()
	if err != nil {
		return err
	}
	dir, err := spoolDir()
	if err != nil {
		return err
	}
	id, err := newRequestID()
	if err != nil {
		return err
	}
	record := launchRecord{ID: id, Request: r, Requester: who, CreatedAt: time.Now().UTC()}
	path, _ := requestPath(dir, id)
	if err := os.WriteFile(filepath.Join(dir, id+".yaml"), b, 0600); err != nil {
		return err
	}
	if err := writeJSONPrivate(path, record); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]string{"id": id})
}
func readRecord(dir, id string) (launchRecord, error) {
	p, err := requestPath(dir, id)
	if err != nil {
		return launchRecord{}, err
	}
	if err := privateRegular(p); err != nil {
		return launchRecord{}, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return launchRecord{}, err
	}
	var r launchRecord
	err = json.Unmarshal(b, &r)
	if err == nil && r.ID != id {
		err = errors.New("request ID mismatch")
	}
	if err == nil {
		yamlPath := filepath.Join(dir, id+".yaml")
		if err = privateRegular(yamlPath); err == nil {
			var data []byte
			data, err = os.ReadFile(yamlPath)
			if err == nil {
				var parsed launchRequest
				parsed, err = parseLaunchYAML(data)
				if err == nil && mustJSON(parsed) != mustJSON(r.Request) {
					err = errors.New("request YAML and metadata differ")
				}
			}
		}
	}
	return r, err
}
func resultPath(dir, id string) (string, error) {
	if _, err := requestPath(dir, id); err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".result.json"), nil
}
func readResult(dir, id string) (launchResult, error) {
	p, err := resultPath(dir, id)
	if err != nil {
		return launchResult{}, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return launchResult{}, err
	}
	var r launchResult
	err = json.Unmarshal(b, &r)
	return r, err
}
func setResult(dir string, r launchResult) error {
	p, err := resultPath(dir, r.ID)
	if err != nil {
		return err
	}
	r.UpdatedAt = time.Now().UTC()
	return writeJSONPrivate(p, r)
}
func requestStatusCommand(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: agent-bridge request-status ID")
	}
	dir, err := spoolDir()
	if err != nil {
		return err
	}
	r, err := readResult(dir, args[0])
	if os.IsNotExist(err) {
		if _, checkErr := readRecord(dir, args[0]); checkErr != nil {
			return checkErr
		}
		r = launchResult{ID: args[0], Status: "queued"}
	} else if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(r)
}
func launcherStateDir() (string, error) {
	r, err := root()
	if err != nil {
		return "", err
	}
	p := filepath.Join(r, "launcher")
	if err := os.MkdirAll(p, 0700); err != nil {
		return "", err
	}
	return p, nil
}
func appendDecision(id, rule, promptHash string, v modelVerdict, outcome string) error {
	return appendDecisionRetry(id, rule, promptHash, v, outcome, nil)
}
func appendDecisionRetry(id, rule, promptHash string, v modelVerdict, outcome string, retryAt *time.Time) error {
	dir, err := launcherStateDir()
	if err != nil {
		return err
	}
	p := filepath.Join(dir, "decisions.jsonl")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	profiles, _ := loadProfiles()
	selected := profiles[v.Profile]
	row := map[string]any{"ts": time.Now().UTC(), "id": id, "rule": rule, "model_prompt_hash": promptHash, "model_verdict": v.Verdict, "agent": v.Agent, "role": v.Role, "profile": v.Profile, "model": selected.Model, "effort": selected.Effort, "time_limit": selected.TimeLimit.String(), "substitution": v.Substitution, "local_agent_id": v.LocalID, "rationale": v.Rationale, "fallback_for": v.FallbackFor, "outcome": outcome}
	if retryAt != nil {
		row["retry_at"] = retryAt
	}
	return json.NewEncoder(f).Encode(row)
}
func verdictSpawner(v modelVerdict) *Spawner {
	if v.LocalID == "" {
		return nil
	}
	return &Spawner{AgentID: v.LocalID, Provider: "local"}
}
func requesterDepth(w requester, s Snapshot) (int, error) {
	if w.JobID != "" {
		j, err := readJob(w.JobID)
		if err != nil {
			return 0, errors.New("requester job unknown")
		}
		if j.Status != "running" || !alive(j.WorkerPID, j.WorkerStart) {
			return 0, errors.New("requester job not running")
		}
		if j.Agent != "claude" || j.Role != "epm" {
			return 0, errors.New("coder or Codex job cannot request")
		}
		return j.Depth, nil
	}
	for _, a := range s.Agents {
		if a.ID == w.AgentID {
			return 0, nil
		}
	}
	return 0, errors.New("requester agent not live")
}
func checkLaunchRules(r launchRecord, s Snapshot) error {
	if r.Request.Agent != "auto" {
		if err := validAgentRoleFB(r.Request.Agent, r.Request.Role, r.Request.FallbackFor); err != nil {
			return err
		}
	}
	if r.Request.FallbackFor != "" {
		if r.Requester.JobID == "" {
			return errors.New("fallback_for needs a running Claude EPM requester")
		}
		if err := checkFallback(r.Request.FallbackFor, s); err != nil {
			return err
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	work := filepath.Join(home, "work")
	resolved, err := filepath.EvalSymlinks(r.Request.Repo)
	if err != nil {
		return fmt.Errorf("repo: %w", err)
	}
	rel, err := filepath.Rel(work, resolved)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("repo must be under ~/work")
	}
	cmd := exec.Command("git", "-C", resolved, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != resolved {
		return errors.New("repo must be a Git repository root")
	}
	depth, err := requesterDepth(r.Requester, s)
	if err != nil {
		return err
	}
	if r.Requester.JobID == "" && r.Request.Role != "epm" {
		return errors.New("coordinator may request only an EPM")
	}
	if r.Request.Agent == "codex" && r.Request.Role == "epm" {
		return errors.New("Codex cannot run the epm role")
	}
	if depth >= 3 {
		return errors.New("nesting depth exceeds 3")
	}
	if r.Request.Role == "epm" && r.Requester.JobID == "" && r.Request.Agent == "codex" {
		return errors.New("Codex cannot run the epm role")
	}
	profiles, err := loadProfiles()
	if err != nil {
		return err
	}
	if r.Request.Profile != "" {
		p, ok := profiles[r.Request.Profile]
		if !ok {
			return errors.New("unknown requested profile")
		}
		if r.Request.Agent != "auto" && p.Agent != r.Request.Agent {
			return errors.New("requested profile agent mismatch")
		}
		if err := validAgentRoleFB(p.Agent, r.Request.Role, r.Request.FallbackFor); err != nil {
			return err
		}
		if r.Requester.JobID != "" && r.Request.Profile == "claude-deep" {
			return errors.New("requested profile exceeds EPM ceiling")
		}
		if r.Request.Role == "epm" && r.Request.Profile != "claude-epm" && r.Request.Profile != "claude-deep" {
			return errors.New("EPM needs an EPM-capable profile")
		}
	}
	if _, err := selectContext(r.Request.Context, false); err != nil {
		return fmt.Errorf("context: %w", err)
	}
	if !filepath.IsAbs(r.Request.PromptFile) {
		return errors.New("prompt_file must be absolute")
	}
	st, err := os.Stat(r.Request.PromptFile)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("prompt_file must be regular")
	}
	if !taskName.MatchString(r.Request.Task) {
		return errors.New("invalid task name")
	}
	return nil
}
func parseModelVerdict(body string, r launchRequest, fromJob bool) (modelVerdict, error) {
	var v modelVerdict
	d := json.NewDecoder(strings.NewReader(strings.TrimSpace(body)))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		return v, err
	}
	var more any
	if d.Decode(&more) != io.EOF {
		return v, errors.New("extra model output")
	}
	if v.Verdict != "approve" && v.Verdict != "escalate" && v.Verdict != "reject" {
		return v, errors.New("invalid verdict")
	}
	if strings.TrimSpace(v.Rationale) == "" {
		return v, errors.New("missing rationale")
	}
	if v.LocalID != "" || v.Substitution != "" {
		return v, errors.New("model supplied reserved verdict fields")
	}
	if v.Verdict == "approve" {
		if err := validAgentRoleFB(v.Agent, v.Role, r.FallbackFor); err != nil {
			return v, err
		}
		if r.FallbackFor != "" && v.Agent != "claude" {
			return v, errors.New("model changed fallback agent")
		}
		if r.Agent != "auto" && v.Agent != r.Agent {
			return v, errors.New("model changed requested agent")
		}
		if v.Role != r.Role {
			return v, errors.New("model changed requested role")
		}
		if !fromJob && (v.Agent != "claude" || v.Role != "epm") {
			return v, errors.New("coordinator may launch only Claude EPM")
		}
		if v.Profile == "" {
			v.Profile = r.Profile
			if v.Profile == "" {
				v.Profile = defaultProfile(v.Agent, v.Role)
			}
		}
		profiles, err := loadProfiles()
		if err != nil {
			return v, err
		}
		p, ok := profiles[v.Profile]
		if !ok || p.Agent != v.Agent {
			return v, errors.New("unknown or mismatched verdict profile")
		}
		ceiling := r.Profile
		if ceiling == "" {
			ceiling = defaultProfile(v.Agent, v.Role)
		}
		if profileRank(v.Profile) < 0 || profileRank(ceiling) < 0 || profileRank(v.Profile) > profileRank(ceiling) {
			return v, errors.New("verdict profile exceeds requested profile")
		}
		if !profileAllowed(v.Profile, fromJob, false) || fromJob && v.Profile == "claude-deep" {
			return v, errors.New("verdict profile exceeds requester ceiling")
		}
		if v.Role == "epm" && v.Profile != "claude-epm" {
			return v, errors.New("EPM needs claude-epm profile")
		}
		if v.Role == "coder" && v.Profile == "claude-epm" {
			return v, errors.New("claude-epm is for EPM jobs")
		}
	}
	return v, nil
}
func modelLaunchVerdict(ctx context.Context, r launchRecord) (modelVerdict, string, error) {
	launcherModelSerial.Lock()
	defer launcherModelSerial.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, localActivityRoleKey{}, "dispatcher")
	localID := ""
	ctx = context.WithValue(ctx, localActivityIDKey{}, &localID)
	model := launcherModel()
	if err := waitForModelLease(ctx); err != nil {
		return modelVerdict{}, "", err
	}
	base, err := ollamaBase()
	if err != nil {
		return modelVerdict{}, "", err
	}
	prompt := fmt.Sprintf("Request: %s. From EPM: %t. Choose approve, escalate, or reject. On approve choose agent, requested role, and profile. Profile cost order: %s. Pick cheapest fitting profile within requested ceiling. Coordinator may only launch Claude EPM. EPM may launch Codex coder or Claude EPM. Never choose Claude coder; agent:auto for coder means Codex.", mustJSON(r.Request), r.Requester.JobID != "", strings.Join(profileOrder, " < "))
	h := sha256.Sum256([]byte(prompt))
	hash := hex.EncodeToString(h[:])
	body, _, _, _, err := ollamaCompletion(ctx, base, model, "Return only JSON with verdict, agent, role, profile, rationale. No markdown.", prompt)
	if err != nil {
		return modelVerdict{}, hash, err
	}
	v, err := parseModelVerdict(body, r.Request, r.Requester.JobID != "")
	v.LocalID = localID
	return v, hash, err
}
func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
func pendingDir() (string, error) {
	dir, err := launcherStateDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, "pending")
	err = os.MkdirAll(p, 0700)
	return p, err
}
func pendingPath(id string) (string, error) {
	if _, err := requestPath("", id); err != nil {
		return "", err
	}
	dir, err := pendingDir()
	return filepath.Join(dir, id+".json"), err
}
func notifyPending(id string) {
	if bin, err := exec.LookPath("notify-send"); err == nil {
		_ = exec.Command(bin, "Agent Bridge approval pending", id).Run()
	}
}
func dispatchLaunch(r launchRecord, v modelVerdict) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	repo := r.Request.Repo
	dest := filepath.Join(home, "worktrees", filepath.Base(repo)+"-"+r.Request.Task)
	branch := "worker/" + r.Request.Task
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return "", err
	}
	cmd := exec.Command("git", "-C", repo, "worktree", "add", "-b", branch, dest, "main")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git worktree add: %w: %s", err, bytes.TrimSpace(out))
	}
	args := []string{"start", "--cwd", dest, "--name", r.Request.Task, "--write", "--agent", v.Agent, "--role", v.Role, "--profile", v.Profile, "--prompt-file", r.Request.PromptFile}
	for _, p := range r.Request.Context {
		args = append(args, "--context", p)
	}
	if r.Request.FallbackFor != "" {
		args = append(args, "--fallback-for", r.Request.FallbackFor)
	}
	if r.Requester.JobID != "" {
		args = append(args, "--parent-job", r.Requester.JobID)
	}
	if r.Requester.AgentID != "" {
		args = append(args, "--spawned-by", r.Requester.AgentID)
	}
	// start validates the parent job and records its identity and depth.
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	out, err := exec.Command(self, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("start: %w: %s", err, bytes.TrimSpace(out))
	}
	id := strings.TrimSpace(string(out))
	j, err := readJob(id)
	if err != nil {
		return "", err
	}
	j.Launcher = "launcher"
	j.Substitution = v.Substitution
	if v.LocalID != "" {
		j.RequestedBy, j.SpawnedBy = j.SpawnedBy, &Spawner{AgentID: v.LocalID, Provider: "local"}
	}
	if err := writeJob(j); err != nil {
		return "", err
	}
	return id, nil
}
func processLaunch(dir string, r launchRecord, forced bool) error {
	if result, err := readResult(dir, r.ID); err == nil && result.Status != "queued" {
		return nil
	} else if err == nil && !forced && result.RetryAt != nil && limitsNow().Before(*result.RetryAt) {
		return nil // deferred until the provider window resets
	}
	s, err := collectSnapshot()
	if err != nil {
		return err
	}
	if err := checkLaunchRules(r, s); err != nil {
		res := launchResult{ID: r.ID, Status: "rejected", Rule: err.Error()}
		_ = appendDecision(r.ID, err.Error(), "", modelVerdict{FallbackFor: r.Request.FallbackFor}, "rejected")
		return setResult(dir, res)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var v modelVerdict
	var hash string
	if !forced {
		selectedPath := filepath.Join(dir, r.ID+".selected.json")
		if b, readErr := os.ReadFile(selectedPath); readErr == nil {
			if json.Unmarshal(b, &v) != nil {
				return errors.New("invalid queued verdict")
			}
		} else {
			v, hash, err = modelLaunchVerdict(ctx, r)
			if err != nil {
				v = modelVerdict{Verdict: "escalate", LocalID: v.LocalID, Rationale: "model unavailable or invalid: " + err.Error()}
			}
		}
	}
	if forced {
		v = modelVerdict{Verdict: "approve", Agent: r.Request.Agent, Role: r.Request.Role, Profile: r.Request.Profile, Rationale: "user approved"}
		if v.Agent == "auto" {
			if v.Role == "coder" && r.Request.FallbackFor == "" {
				v.Agent = "codex"
			} else if v.Role == "coder" {
				v.Agent = "claude"
			} else {
				v.Agent = "claude"
			}
		}
		if err := validAgentRoleFB(v.Agent, v.Role, r.Request.FallbackFor); err != nil {
			return err
		}
		if v.Profile == "" {
			v.Profile = defaultProfile(v.Agent, v.Role)
		}
	}
	v.FallbackFor = r.Request.FallbackFor
	switch v.Verdict {
	case "reject":
		_ = appendDecision(r.ID, "pass", hash, v, "rejected")
		return setResult(dir, launchResult{ID: r.ID, Status: "rejected", Rationale: v.Rationale})
	case "escalate":
		p, err := pendingPath(r.ID)
		if err != nil {
			return err
		}
		if err := writeJSONPrivate(p, r); err != nil {
			return err
		}
		notifyPending(r.ID)
		_ = appendDecision(r.ID, "pass", hash, v, "pending")
		return setResult(dir, launchResult{ID: r.ID, Status: "pending", Rationale: v.Rationale, SpawnedBy: verdictSpawner(v)})
	case "approve":
		if err := validAgentRoleFB(v.Agent, v.Role, r.Request.FallbackFor); err != nil {
			return setResult(dir, launchResult{ID: r.ID, Status: "rejected", Rule: err.Error()})
		}
		if !forced && v.Role == "coder" && v.Substitution == "" && r.Request.FallbackFor == "" {
			replacement := substituteProfile(v.Profile, s)
			profiles, _ := loadProfiles()
			if p, ok := profiles[replacement]; replacement != "" && ok {
				v.Substitution, v.Profile, v.Agent = v.Profile, replacement, p.Agent
			}
		}
		if providerExhausted(v.Agent, s) {
			if v.Role == "coder" && r.Request.FallbackFor == "" {
				v.Rationale = "Codex usage limit; queued for retry"
				var retryAt *time.Time
				if l, until, ok := limitDeferral(v.Agent, s); ok {
					v.Rationale = fmt.Sprintf("Codex %s window at %.0f%% (defer at %.0f%%); queued until reset", l.Window, l.UsedPercent, deferPercent())
					if !until.IsZero() {
						retryAt = &until
					}
				}
				if err := writeJSONPrivate(filepath.Join(dir, r.ID+".selected.json"), v); err != nil {
					return err
				}
				_ = appendDecisionRetry(r.ID, "pass", hash, v, "queued_usage_limit", retryAt)
				return setResult(dir, launchResult{ID: r.ID, Status: "queued", Rationale: v.Rationale, SpawnedBy: verdictSpawner(v), RetryAt: retryAt})
			}
			if !forced {
				v.Verdict, v.Rationale = "escalate", "provider usage limit has no allowed substitute"
				p, err := pendingPath(r.ID)
				if err != nil {
					return err
				}
				if err := writeJSONPrivate(p, r); err != nil {
					return err
				}
				notifyPending(r.ID)
				_ = appendDecision(r.ID, "pass", hash, v, "pending")
				return setResult(dir, launchResult{ID: r.ID, Status: "pending", Rationale: v.Rationale, SpawnedBy: verdictSpawner(v)})
			}
		}
		if !launcherSlotAvailable(s, v.Profile) {
			_ = writeJSONPrivate(filepath.Join(dir, r.ID+".selected.json"), v)
			_ = appendDecision(r.ID, "pass", hash, v, "queued_capacity")
			return errCapWait
		}
		id, err := dispatchLaunch(r, v)
		if err != nil {
			_ = appendDecision(r.ID, "pass", hash, v, "failed")
			return setResult(dir, launchResult{ID: r.ID, Status: "failed", Rationale: err.Error()})
		}
		_ = appendDecision(r.ID, "pass", hash, v, "approved")
		_ = os.Remove(filepath.Join(dir, r.ID+".selected.json"))
		return setResult(dir, launchResult{ID: r.ID, Status: "approved", JobID: id, Rationale: v.Rationale, SpawnedBy: verdictSpawner(v)})
	}
	return errors.New("unreachable verdict")
}
func expirePending(timeout time.Duration) error {
	dir, err := pendingDir()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	spool, err := spoolDir()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		p, _ := pendingPath(id)
		st, err := os.Stat(p)
		if err != nil || time.Since(st.ModTime()) < timeout {
			continue
		}
		_ = setResult(spool, launchResult{ID: id, Status: "denied", Rationale: "approval timed out"})
		_ = appendDecision(id, "pass", "", modelVerdict{Verdict: "escalate"}, "timeout_denied")
		_ = os.Remove(p)
	}
	return nil
}
func launcherOnce(timeout time.Duration) error {
	dir, err := spoolDir()
	if err != nil {
		return err
	}
	if err := expirePending(timeout); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool {
		a, ea := entries[i].Info()
		b, eb := entries[j].Info()
		if ea == nil && eb == nil && !a.ModTime().Equal(b.ModTime()) {
			return a.ModTime().Before(b.ModTime())
		}
		return entries[i].Name() < entries[j].Name()
	})
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".request.json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".request.json")
		if result, err := readResult(dir, id); err == nil && result.Status != "queued" {
			continue
		}
		r, err := readRecord(dir, id)
		if err != nil {
			_ = setResult(dir, launchResult{ID: id, Status: "rejected", Rule: err.Error()})
			_ = appendDecision(id, err.Error(), "", modelVerdict{}, "rejected")
			continue
		}
		if err := processLaunch(dir, r, false); errors.Is(err, errCapWait) {
			break
		} else if err != nil {
			return err
		}
	}
	return nil
}
func launcherCommand(args []string) error {
	f := flag.NewFlagSet("launcher", flag.ContinueOnError)
	once := f.Bool("once", false, "Process queued requests once")
	interval := f.Duration("interval", time.Second, "Spool polling interval")
	timeout := f.Duration("approval-timeout", 30*time.Minute, "Escalation timeout")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || *interval < 100*time.Millisecond || *timeout <= 0 {
		return errors.New("invalid launcher arguments")
	}
	for {
		if err := launcherOnce(*timeout); err != nil {
			return err
		}
		if *once {
			return nil
		}
		time.Sleep(*interval)
	}
}
func approvalsCommand(args []string) error {
	if len(args) != 0 {
		return errors.New("approvals takes no arguments")
	}
	dir, err := pendingDir()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	out := []launchResult{}
	spool, err := spoolDir()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			id := strings.TrimSuffix(e.Name(), ".json")
			if r, err := readResult(spool, id); err == nil && r.Status == "pending" {
				out = append(out, r)
			}
		}
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}
func resolveApprovalCommand(action string, args []string) error {
	profileOverride := ""
	if action == "approve" && len(args) == 3 && args[0] == "--profile" {
		profileOverride, args = args[1], args[2:]
	}
	if len(args) != 1 {
		return fmt.Errorf("usage: agent-bridge %s ID", action)
	}
	if err := expirePending(30 * time.Minute); err != nil {
		return err
	}
	dir, err := spoolDir()
	if err != nil {
		return err
	}
	r, err := readRecord(dir, args[0])
	if err != nil {
		return err
	}
	if profileOverride != "" {
		profiles, err := loadProfiles()
		if err != nil {
			return err
		}
		p, ok := profiles[profileOverride]
		if !ok {
			return errors.New("unknown override profile")
		}
		if r.Request.Role == "epm" && p.Agent != "claude" {
			return errors.New("EPM profile must use Claude")
		}
		if r.Request.Role == "epm" && profileOverride != "claude-epm" && profileOverride != "claude-deep" {
			return errors.New("EPM needs an EPM-capable profile")
		}
		if err := validAgentRoleFB(p.Agent, r.Request.Role, r.Request.FallbackFor); err != nil {
			return err
		}
		r.Request.Profile, r.Request.Agent = profileOverride, p.Agent
	}
	p, err := pendingPath(r.ID)
	if err != nil {
		return err
	}
	if err := privateRegular(p); err != nil {
		return err
	}
	status, err := readResult(dir, r.ID)
	if err != nil {
		return err
	}
	if status.Status != "pending" {
		return errors.New("request is not pending")
	}
	if action == "deny" {
		err = setResult(dir, launchResult{ID: r.ID, Status: "denied", Rationale: "user denied"})
		_ = appendDecision(r.ID, "pass", "", modelVerdict{Verdict: "escalate"}, "denied")
	} else {
		_ = os.Remove(p)
		err = os.Remove(filepath.Join(dir, r.ID+".result.json"))
		if err == nil {
			err = processLaunch(dir, r, true)
			if errors.Is(err, errCapWait) {
				err = setResult(dir, launchResult{ID: r.ID, Status: "queued", Rationale: "waiting for capacity"})
			}
		}
	}
	if err != nil {
		return err
	}
	_ = os.Remove(p)
	res, err := readResult(dir, r.ID)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(res)
}
