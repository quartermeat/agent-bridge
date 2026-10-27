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
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

var localNow = time.Now
var localWaitLease = waitForModelLease
var localComplete = func(ctx context.Context, model, system, prompt string) (string, TokenUsage, error) {
	base, err := ollamaBase()
	if err != nil {
		return "", TokenUsage{}, err
	}
	text, usage, _, code, err := ollamaCompletion(ctx, base, model, system, prompt)
	if code == "model_not_found" {
		return text, usage, localModelNotFound{err}
	}
	return text, usage, err
}

type localModelNotFound struct{ error }

type localValidity struct {
	Checked []string `json:"checked"`
	Failed  []string `json:"failed"`
	Result  string   `json:"result"`
}
type localAttempt struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Code       string    `json:"code,omitempty"`
	Failed     []string  `json:"failed,omitempty"`
	RunS       float64   `json:"run_s"`
	PausedS    float64   `json:"paused_s"`
}
type localTask struct {
	ID               string         `json:"id"`
	Class            string         `json:"class"`
	State            string         `json:"state"`
	Labels           []string       `json:"labels,omitempty"`
	Fields           []string       `json:"fields,omitempty"`
	Expect           string         `json:"expect,omitempty"`
	Gold             bool           `json:"gold"`
	Correct          bool           `json:"correct"`
	SubmittedBy      string         `json:"submitted_by"`
	Reason           string         `json:"reason,omitempty"`
	Model            string         `json:"model,omitempty"`
	TemplateVersion  int            `json:"template_version"`
	CreatedAt        time.Time      `json:"created_at"`
	StartedAt        time.Time      `json:"started_at,omitempty"`
	FinishedAt       time.Time      `json:"finished_at,omitempty"`
	DeadlineAt       time.Time      `json:"deadline_at"`
	BudgetS          float64        `json:"budget_s"`
	TimeoutS         float64        `json:"timeout_s"`
	MaxAttempts      int            `json:"max_attempts"`
	Attempts         int            `json:"attempts"`
	AttemptLog       []localAttempt `json:"attempt_log"`
	QueueWaitS       float64        `json:"queue_wait_s"`
	PausedS          float64        `json:"paused_s"`
	RunS             float64        `json:"run_s"`
	WithinBudget     bool           `json:"within_budget"`
	InputBytes       int            `json:"input_bytes"`
	InputHash        string         `json:"input_hash"`
	OutputBytes      int            `json:"output_bytes"`
	OutputHash       string         `json:"output_hash"`
	PromptTokens     int64          `json:"prompt_tokens"`
	OutputTokens     int64          `json:"output_tokens"`
	Validity         localValidity  `json:"validity"`
	CanceledBy       string         `json:"canceled_by,omitempty"`
	Code             string         `json:"code,omitempty"`
	OutputRetained   bool           `json:"output_retained"`
	TelemetryWritten bool           `json:"telemetry_written,omitempty"`
}
type localTelemetry struct {
	Spec              string        `json:"spec"`
	ID                string        `json:"id"`
	Class             string        `json:"class"`
	TemplateVersion   int           `json:"template_version"`
	Model             string        `json:"model"`
	SubmittedBy       string        `json:"submitted_by"`
	Arm               string        `json:"arm"`
	Tier              string        `json:"tier"`
	CreatedAt         time.Time     `json:"created_at"`
	StartedAt         time.Time     `json:"started_at"`
	FinishedAt        time.Time     `json:"finished_at"`
	BudgetS           float64       `json:"budget_s"`
	TimeoutS          float64       `json:"timeout_s"`
	Attempts          int           `json:"attempts"`
	QueueWaitS        float64       `json:"queue_wait_s"`
	PausedS           float64       `json:"paused_s"`
	RunS              float64       `json:"run_s"`
	WithinBudget      bool          `json:"within_budget"`
	InputBytes        int           `json:"input_bytes"`
	InputHash         string        `json:"input_hash"`
	OutputBytes       int           `json:"output_bytes"`
	OutputHash        string        `json:"output_hash"`
	PromptTokens      int64         `json:"prompt_tokens"`
	OutputTokens      int64         `json:"output_tokens"`
	State             string        `json:"state"`
	Validity          localValidity `json:"validity"`
	Gold              bool          `json:"gold"`
	Correct           bool          `json:"correct"`
	ClaimedConfidence any           `json:"claimed_confidence"`
	Review            any           `json:"review"`
	CanceledBy        string        `json:"canceled_by,omitempty"`
	Code              string        `json:"code,omitempty"`
}

func localQueueDir() (string, error) {
	r, e := root()
	if e != nil {
		return "", e
	}
	return filepath.Join(r, "local-queue"), nil
}
func localEnsureDir() (string, error) {
	d, e := localQueueDir()
	if e != nil {
		return "", e
	}
	if e = os.MkdirAll(filepath.Join(d, "tasks"), 0700); e != nil {
		return "", e
	}
	return d, nil
}
func localTaskPath(d, id string) (string, error) {
	if !regexp.MustCompile(`^lq-[a-f0-9]{16}$`).MatchString(id) {
		return "", errors.New("invalid local task ID")
	}
	return filepath.Join(d, "tasks", id), nil
}
func localReadTask(d, id string) (localTask, error) {
	p, e := localTaskPath(d, id)
	if e != nil {
		return localTask{}, e
	}
	b, e := os.ReadFile(filepath.Join(p, "task.json"))
	if e != nil {
		return localTask{}, e
	}
	var t localTask
	e = json.Unmarshal(b, &t)
	return t, e
}
func localWriteTask(d string, t localTask) error {
	p, e := localTaskPath(d, t.ID)
	if e != nil {
		return e
	}
	return writeJSONPrivate(filepath.Join(p, "task.json"), t)
}
func localRandom(n int) ([]byte, error) {
	b := make([]byte, n)
	_, e := rand.Read(b)
	return b, e
}
func localSalt(d string) ([]byte, error) {
	p := filepath.Join(d, "salt")
	b, e := os.ReadFile(p)
	if e == nil {
		return b, nil
	}
	if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	b, e = localRandom(32)
	if e != nil {
		return nil, e
	}
	f, e := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(e, os.ErrExist) {
		return os.ReadFile(p)
	}
	if e != nil {
		return nil, e
	}
	if _, e = f.Write(b); e != nil {
		f.Close()
		return nil, e
	}
	e = f.Close()
	return b, e
}
func localHash(salt, b []byte) string {
	h := sha256.New()
	h.Write(salt)
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}
func localTier(n int) string {
	if n < 2048 {
		return "S"
	}
	if n < 8192 {
		return "M"
	}
	return "L"
}
func localSeconds(d time.Duration) float64 { return d.Seconds() }
func localFinal(s string) bool {
	switch s {
	case "done", "failed", "timeout", "canceled", "expired":
		return true
	}
	return false
}
func localRecord(d string, t *localTask) error {
	rec := localTelemetry{Spec: "local-task/v1", ID: t.ID, Class: t.Class, TemplateVersion: t.TemplateVersion, Model: t.Model, SubmittedBy: t.SubmittedBy, Arm: "unassigned", Tier: localTier(t.InputBytes), CreatedAt: t.CreatedAt, StartedAt: t.StartedAt, FinishedAt: t.FinishedAt, BudgetS: t.BudgetS, TimeoutS: t.TimeoutS, Attempts: t.Attempts, QueueWaitS: t.QueueWaitS, PausedS: t.PausedS, RunS: t.RunS, WithinBudget: t.WithinBudget, InputBytes: t.InputBytes, InputHash: t.InputHash, OutputBytes: t.OutputBytes, OutputHash: t.OutputHash, PromptTokens: t.PromptTokens, OutputTokens: t.OutputTokens, State: t.State, Validity: t.Validity, Gold: t.Gold, Correct: t.Correct, CanceledBy: t.CanceledBy, Code: t.Code}
	b, e := json.Marshal(rec)
	if e != nil {
		return e
	}
	f, e := os.OpenFile(filepath.Join(d, "telemetry.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(append(b, '\n'))
	ce := f.Close()
	if e != nil {
		return e
	}
	return ce
}

// localFinish persists the final state, drops the input, then appends telemetry.
// A telemetry failure is logged once and never undoes or retries the final state.
func localFinish(d string, t *localTask) error {
	if t.Code == "" && t.State == "timeout" {
		t.Code = "timeout"
	}
	t.FinishedAt = localNow().UTC()
	t.WithinBudget = t.RunS <= t.BudgetS
	if e := localWriteTask(d, *t); e != nil {
		return e
	}
	p, _ := localTaskPath(d, t.ID)
	if e := os.Remove(filepath.Join(p, "input")); e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	if e := localRecord(d, t); e != nil {
		fmt.Fprintln(os.Stderr, "local-queue: telemetry append failed")
	}
	return nil
}
func localValidateList(raw, kind string) ([]string, error) {
	if raw == "" {
		return nil, fmt.Errorf("%s required", kind)
	}
	a := strings.Split(raw, ",")
	if kind == "labels" {
		if len(a) < 2 || len(a) > 20 {
			return nil, errors.New("labels must contain 2 to 20 entries")
		}
	} else if len(a) < 1 || len(a) > 12 {
		return nil, errors.New("fields must contain 1 to 12 entries")
	}
	seen := map[string]bool{}
	for _, v := range a {
		if seen[v] {
			return nil, fmt.Errorf("duplicate %s", kind)
		}
		seen[v] = true
		if kind == "labels" {
			if len(v) < 1 || len(v) > 40 || strings.ContainsAny(v, "\r\n") || strings.TrimSpace(v) != v {
				return nil, errors.New("invalid label")
			}
		} else if !regexp.MustCompile(`^[a-z0-9_]{1,32}$`).MatchString(v) {
			return nil, errors.New("invalid field")
		}
	}
	return a, nil
}
func localSubmit(args []string) error {
	f := flag.NewFlagSet("local-queue submit", flag.ContinueOnError)
	class := f.String("class", "", "")
	path := f.String("input-file", "", "")
	labels := f.String("labels", "", "")
	fields := f.String("fields", "", "")
	expect := f.String("expect", "", "")
	budget := f.Duration("budget", time.Minute, "")
	timeout := f.Duration("timeout", 0, "")
	attempts := f.Int("max-attempts", 2, "")
	by := f.String("submitted-by", "unknown", "")
	reason := f.String("reason", "", "")
	if e := f.Parse(args); e != nil {
		return e
	}
	if f.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	if *class != "label" && *class != "extract" {
		return errors.New("class must be label or extract")
	}
	if *budget < time.Second || *budget > 10*time.Minute {
		return errors.New("budget must be 1s..10m")
	}
	if *timeout == 0 {
		*timeout = *budget
	}
	if *timeout <= 0 || *timeout > 10*time.Minute {
		return errors.New("timeout must be positive and at most 10m")
	}
	if *attempts < 1 || *attempts > 3 {
		return errors.New("max-attempts must be 1..3")
	}
	if len(*reason) > 200 {
		return errors.New("reason exceeds 200 characters")
	}
	if *path == "" {
		return errors.New("input-file required")
	}
	var opts []string
	var e error
	if *class == "label" {
		opts, e = localValidateList(*labels, "labels")
	} else {
		opts, e = localValidateList(*fields, "fields")
	}
	if e != nil {
		return e
	}
	if *expect != "" {
		if *class == "label" {
			found := false
			for _, v := range opts {
				if v == *expect {
					found = true
				}
			}
			if !found {
				return errors.New("expect must be a label")
			}
		} else {
			var obj map[string]string
			if e = json.Unmarshal([]byte(*expect), &obj); e != nil || len(obj) != len(opts) {
				return errors.New("expect must be an object with the requested string fields")
			}
			for _, v := range opts {
				if _, ok := obj[v]; !ok {
					return errors.New("expect fields mismatch")
				}
			}
		}
	}
	resolved, e := filepath.EvalSymlinks(*path)
	if e != nil {
		return e
	}
	resolved, e = filepath.Abs(resolved)
	if e != nil {
		return e
	}
	home, e := os.UserHomeDir()
	if e != nil {
		return e
	}
	private := filepath.Join(home, "private")
	if resolved == private || strings.HasPrefix(resolved, private+string(os.PathSeparator)) {
		return errors.New("input-file under ~/private is forbidden")
	}
	info, e := os.Stat(resolved)
	if e != nil {
		return e
	}
	if !info.Mode().IsRegular() {
		return errors.New("input-file must be regular")
	}
	file, e := os.Open(resolved)
	if e != nil {
		return e
	}
	b, e := io.ReadAll(io.LimitReader(file, 16385))
	file.Close()
	if e != nil {
		return e
	}
	if len(b) == 0 {
		return errors.New("input-file is empty")
	}
	if len(b) > 16384 {
		return errors.New("input-file exceeds 16384 bytes")
	}
	d, e := localEnsureDir()
	if e != nil {
		return e
	}
	salt, e := localSalt(d)
	if e != nil {
		return e
	}
	idBytes, e := localRandom(8)
	if e != nil {
		return e
	}
	id := "lq-" + hex.EncodeToString(idBytes)
	p, _ := localTaskPath(d, id)
	if e = os.Mkdir(p, 0700); e != nil {
		return e
	}
	if e = os.WriteFile(filepath.Join(p, "input"), b, 0600); e != nil {
		return e
	}
	now := localNow().UTC()
	t := localTask{ID: id, Class: *class, State: "queued", SubmittedBy: *by, Reason: *reason, TemplateVersion: 1, CreatedAt: now, DeadlineAt: now.Add(3 * *budget), BudgetS: budget.Seconds(), TimeoutS: timeout.Seconds(), MaxAttempts: *attempts, InputBytes: len(b), InputHash: localHash(salt, b), Gold: *expect != "", Expect: *expect, Validity: localValidity{Checked: []string{}, Failed: []string{}, Result: "unchecked"}}
	if *class == "label" {
		t.Labels = opts
	} else {
		t.Fields = opts
	}
	if e = localWriteTask(d, t); e != nil {
		return e
	}
	fmt.Println(id)
	return nil
}
func localCheck(t localTask, output string) (localValidity, bool) {
	v := localValidity{Failed: []string{}, Result: "valid"}
	fail := func(s string) {
		for _, x := range v.Failed {
			if x == s {
				return
			}
		}
		v.Failed = append(v.Failed, s)
	}
	if len(output) > 8192 {
		fail("output_size")
	}
	s := strings.TrimSpace(output)
	var correct bool
	if t.Class == "label" {
		v.Checked = []string{"parsed", "label_in_set", "output_size"}
		if s == "" {
			fail("parsed")
		}
		found := false
		for _, x := range t.Labels {
			if x == s {
				found = true
			}
		}
		if !found {
			fail("label_in_set")
		}
		correct = t.Gold && s == t.Expect
	} else {
		v.Checked = []string{"parsed", "fields_match", "values_strings", "values_short", "output_size"}
		if strings.HasPrefix(s, "```") {
			lines := strings.SplitN(s, "\n", 2)
			if len(lines) == 2 && (lines[0] == "```" || lines[0] == "```json") && strings.HasSuffix(strings.TrimSpace(lines[1]), "```") {
				s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(lines[1]), "```"))
			}
		}
		var obj map[string]json.RawMessage
		if e := json.Unmarshal([]byte(s), &obj); e != nil || obj == nil {
			fail("parsed")
		} else {
			if len(obj) != len(t.Fields) {
				fail("fields_match")
			} else {
				for _, x := range t.Fields {
					if _, ok := obj[x]; !ok {
						fail("fields_match")
						break
					}
				}
			}
			for _, raw := range obj {
				var val string
				if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &val) != nil {
					fail("values_strings")
					continue
				}
				if len(val) > 500 {
					fail("values_short")
				}
			}
			if t.Gold {
				var gold map[string]any
				var got map[string]any
				_ = json.Unmarshal([]byte(t.Expect), &gold)
				_ = json.Unmarshal([]byte(s), &got)
				gb, _ := json.Marshal(gold)
				ob, _ := json.Marshal(got)
				correct = bytes.Equal(gb, ob)
			}
		}
	}
	if len(v.Failed) > 0 {
		v.Result = "invalid"
	}
	return v, correct
}
func localPrompt(t localTask, input, failed string) (string, string) {
	var sys string
	if t.Class == "label" {
		sys = "Answer with exactly one label from this list and nothing else: " + strings.Join(t.Labels, ", ")
	} else {
		sys = "Return one JSON object with exactly these string fields and nothing else: " + strings.Join(t.Fields, ", ")
	}
	prompt := input
	if failed != "" {
		prompt += "\n\nPrevious response failed check: " + failed + ". Try again."
	}
	return sys, prompt
}
func localCancelMarker(d, id string) (string, error) {
	p, e := localTaskPath(d, id)
	if e != nil {
		return "", e
	}
	b, e := os.ReadFile(filepath.Join(p, "cancel"))
	return string(b), e
}
func localAll(d string) ([]localTask, error) {
	entries, e := os.ReadDir(filepath.Join(d, "tasks"))
	if e != nil {
		return nil, e
	}
	out := []localTask{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		t, e := localReadTask(d, entry.Name())
		if e != nil {
			return nil, e
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}
func localRecover(d string) error {
	all, e := localAll(d)
	if e != nil {
		return e
	}
	for _, t := range all {
		if t.State != "running" {
			continue
		}
		now := localNow().UTC()
		t.AttemptLog = append(t.AttemptLog, localAttempt{StartedAt: now, FinishedAt: now, Code: "interrupted"})
		if t.Attempts < t.MaxAttempts {
			t.State = "queued"
			t.Code = "interrupted"
			if e = localWriteTask(d, t); e != nil {
				return e
			}
		} else {
			t.State = "failed"
			t.Code = "interrupted"
			if e = localFinish(d, &t); e != nil {
				return e
			}
		}
	}
	return nil
}
func localProcess(ctx context.Context, d string, t localTask, model string) error {
	if by, e := localCancelMarker(d, t.ID); e == nil {
		t.CanceledBy = by
		t.State = "canceled"
		return localFinish(d, &t)
	}
	now := localNow()
	if !now.Before(t.DeadlineAt) {
		t.State = "expired"
		return localFinish(d, &t)
	}
	p, _ := localTaskPath(d, t.ID)
	input, e := os.ReadFile(filepath.Join(p, "input"))
	if e != nil {
		return e
	}
	t.Model = model
	if t.StartedAt.IsZero() {
		t.StartedAt = now.UTC()
		t.QueueWaitS = localSeconds(now.Sub(t.CreatedAt))
	}
	t.State = "running"
	if e = localWriteTask(d, t); e != nil {
		return e
	}
	failed := ""
	for t.Attempts < t.MaxAttempts {
		if !localNow().Before(t.DeadlineAt) {
			t.State = "timeout"
			t.Code = "deadline"
			break
		}
		cctx, cancel := context.WithCancel(ctx)
		watchDone := make(chan struct{})
		go func() {
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			defer close(watchDone)
			for {
				select {
				case <-cctx.Done():
					return
				case <-ticker.C:
					if _, e := localCancelMarker(d, t.ID); e == nil {
						cancel()
						return
					}
				}
			}
		}()
		start := time.Now()
		e = localWaitLease(cctx)
		paused := time.Since(start).Seconds()
		t.PausedS += paused
		if e != nil {
			cancel()
			<-watchDone
			if by, ce := localCancelMarker(d, t.ID); ce == nil {
				t.CanceledBy = by
				t.State = "canceled"
			} else if ctx.Err() != nil {
				t.State = "queued"
				return localWriteTask(d, t)
			} else {
				t.State = "failed"
				t.Code = "model_error"
			}
			break
		}
		limit := time.Duration(t.TimeoutS * float64(time.Second))
		if remaining := t.DeadlineAt.Sub(localNow()); remaining < limit {
			limit = remaining
		}
		attemptCtx, cancelAttempt := context.WithTimeout(cctx, limit)
		t.Attempts++
		a := localAttempt{StartedAt: localNow().UTC(), PausedS: paused}
		if e = localWriteTask(d, t); e != nil {
			cancelAttempt()
			cancel()
			<-watchDone
			return e
		}
		sys, prompt := localPrompt(t, string(input), failed)
		start = time.Now()
		output, usage, completeErr := localComplete(context.WithValue(attemptCtx, localActivityRoleKey{}, "queue"), model, sys, prompt)
		runS := time.Since(start).Seconds()
		cancelAttempt()
		cancel()
		<-watchDone
		t.RunS += runS
		t.PromptTokens += usage.Input
		t.OutputTokens += usage.Output
		a.FinishedAt = localNow().UTC()
		a.RunS = runS
		if by, ce := localCancelMarker(d, t.ID); ce == nil {
			t.CanceledBy = by
			t.State = "canceled"
			a.Code = "canceled"
		} else if ctx.Err() != nil {
			t.State = "queued"
			t.Attempts--
			a.Code = "interrupted"
		} else if !localNow().Before(t.DeadlineAt) || errors.Is(completeErr, context.DeadlineExceeded) || errors.Is(attemptCtx.Err(), context.DeadlineExceeded) {
			t.State = "timeout"
			a.Code = "timeout"
		} else if completeErr != nil {
			t.State = "failed"
			a.Code = "model_error"
			var missing localModelNotFound
			if errors.As(completeErr, &missing) {
				a.Code = "model_not_found"
			}
			t.Code = a.Code
		} else {
			t.OutputBytes = len(output)
			salt, se := localSalt(d)
			if se != nil {
				return se
			}
			t.OutputHash = localHash(salt, []byte(output))
			if e = os.WriteFile(filepath.Join(p, "output"), []byte(output), 0600); e != nil {
				return e
			}
			t.OutputRetained = true
			t.Validity, t.Correct = localCheck(t, output)
			a.Failed = t.Validity.Failed
			if t.Validity.Result == "valid" {
				t.State = "done"
			} else if t.Attempts >= t.MaxAttempts {
				t.State = "done"
			} else {
				failed = t.Validity.Failed[0]
			}
		}
		t.AttemptLog = append(t.AttemptLog, a)
		if t.State == "queued" {
			return localWriteTask(d, t)
		}
		if t.State == "timeout" && t.Attempts < t.MaxAttempts && localNow().Before(t.DeadlineAt) {
			t.State = "running"
			failed = "timeout"
		}
		if localFinal(t.State) {
			break
		}
		if e = localWriteTask(d, t); e != nil {
			return e
		}
	}
	if !localFinal(t.State) {
		t.State = "failed"
		t.Code = "attempts_exhausted"
	}
	return localFinish(d, &t)
}
func localRun(ctx context.Context, once bool, model string) error {
	d, e := localEnsureDir()
	if e != nil {
		return e
	}
	lock, e := os.OpenFile(filepath.Join(d, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		return errors.New("another local-queue runner holds the lock")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if e = localRecover(d); e != nil {
		return e
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		all, e := localAll(d)
		if e != nil {
			return e
		}
		found := false
		for _, t := range all {
			if t.State != "queued" {
				continue
			}
			found = true
			if e = localProcess(ctx, d, t, model); e != nil {
				return e
			}
			break
		}
		if found {
			continue
		}
		if once {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}
func localCancel(args []string) error {
	f := flag.NewFlagSet("local-queue cancel", flag.ContinueOnError)
	by := f.String("by", "unknown", "")
	if e := f.Parse(args); e != nil {
		return e
	}
	if f.NArg() != 1 {
		return errors.New("usage: local-queue cancel [--by AGENT_ID] ID")
	}
	d, e := localEnsureDir()
	if e != nil {
		return e
	}
	t, e := localReadTask(d, f.Arg(0))
	if e != nil {
		return e
	}
	if localFinal(t.State) {
		return errors.New("task already finished")
	}
	p, _ := localTaskPath(d, t.ID)
	if e = os.WriteFile(filepath.Join(p, "cancel"), []byte(*by), 0600); e != nil {
		return e
	}
	if t.State == "queued" {
		t.CanceledBy = *by
		t.State = "canceled"
		return localFinish(d, &t)
	}
	return nil
}

type localStatus struct {
	Depth   int            `json:"depth"`
	Counts  map[string]int `json:"counts"`
	Running *struct {
		ID       string  `json:"id"`
		Class    string  `json:"class"`
		ElapsedS float64 `json:"elapsed_s"`
	} `json:"running"`
	OldestQueuedAgeS float64 `json:"oldest_queued_age_s"`
}

func localStatusCommand(args []string) error {
	f := flag.NewFlagSet("local-queue status", flag.ContinueOnError)
	asJSON := f.Bool("json", false, "")
	if e := f.Parse(args); e != nil {
		return e
	}
	if f.NArg() > 1 {
		return errors.New("usage: local-queue status [--json] [ID]")
	}
	d, e := localEnsureDir()
	if e != nil {
		return e
	}
	if f.NArg() == 1 {
		t, e := localReadTask(d, f.Arg(0))
		if e != nil {
			return e
		}
		v := struct {
			ID         string    `json:"id"`
			Class      string    `json:"class"`
			State      string    `json:"state"`
			CreatedAt  time.Time `json:"created_at"`
			StartedAt  time.Time `json:"started_at"`
			FinishedAt time.Time `json:"finished_at"`
			DeadlineAt time.Time `json:"deadline_at"`
			Attempts   int       `json:"attempts"`
			QueueWaitS float64   `json:"queue_wait_s"`
			PausedS    float64   `json:"paused_s"`
			RunS       float64   `json:"run_s"`
			CanceledBy string    `json:"canceled_by,omitempty"`
		}{t.ID, t.Class, t.State, t.CreatedAt, t.StartedAt, t.FinishedAt, t.DeadlineAt, t.Attempts, t.QueueWaitS, t.PausedS, t.RunS, t.CanceledBy}
		return localPrint(v, *asJSON)
	}
	all, e := localAll(d)
	if e != nil {
		return e
	}
	s := localStatus{Counts: map[string]int{"queued": 0, "running": 0, "done": 0, "failed": 0, "timeout": 0, "canceled": 0, "expired": 0}}
	now := localNow()
	for _, t := range all {
		s.Counts[t.State]++
		if t.State == "queued" {
			s.Depth++
			age := now.Sub(t.CreatedAt).Seconds()
			if age > s.OldestQueuedAgeS {
				s.OldestQueuedAgeS = age
			}
		}
		if t.State == "running" {
			s.Running = &struct {
				ID       string  `json:"id"`
				Class    string  `json:"class"`
				ElapsedS float64 `json:"elapsed_s"`
			}{t.ID, t.Class, now.Sub(t.StartedAt).Seconds()}
		}
	}
	return localPrint(s, *asJSON)
}
func localPrint(v any, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(v)
	}
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	fmt.Println(string(b))
	return nil
}

type localReportRow struct {
	Class            string  `json:"class"`
	Tier             string  `json:"tier"`
	Tasks            int     `json:"tasks"`
	Valid            int     `json:"valid"`
	ValidRate        float64 `json:"valid_rate"`
	GoldTasks        int     `json:"gold_tasks"`
	GoldCorrect      int     `json:"gold_correct"`
	WithinBudgetRate float64 `json:"within_budget_rate"`
	P50RunS          float64 `json:"p50_run_s"`
	P90RunS          float64 `json:"p90_run_s"`
	PromptTokens     int64   `json:"prompt_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	Timeout          int     `json:"timeout"`
	Failed           int     `json:"failed"`
	Canceled         int     `json:"canceled"`
	Expired          int     `json:"expired"`
}

func localReport(args []string) error {
	f := flag.NewFlagSet("local-queue report", flag.ContinueOnError)
	asJSON := f.Bool("json", false, "")
	if e := f.Parse(args); e != nil {
		return e
	}
	if f.NArg() != 0 {
		return errors.New("usage: local-queue report [--json]")
	}
	d, e := localEnsureDir()
	if e != nil {
		return e
	}
	b, e := os.ReadFile(filepath.Join(d, "telemetry.jsonl"))
	if errors.Is(e, os.ErrNotExist) {
		b = nil
	} else if e != nil {
		return e
	}
	rows := map[string]*localReportRow{}
	times := map[string][]float64{}
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var t localTelemetry
		if e = json.Unmarshal(line, &t); e != nil {
			return e
		}
		key := t.Class + "/" + t.Tier
		r := rows[key]
		if r == nil {
			r = &localReportRow{Class: t.Class, Tier: t.Tier}
			rows[key] = r
		}
		r.Tasks++
		if t.Validity.Result == "valid" {
			r.Valid++
		}
		if t.Gold {
			r.GoldTasks++
			if t.Correct {
				r.GoldCorrect++
			}
		}
		if t.WithinBudget {
			r.WithinBudgetRate++
		}
		r.PromptTokens += t.PromptTokens
		r.OutputTokens += t.OutputTokens
		times[key] = append(times[key], t.RunS)
		switch t.State {
		case "timeout":
			r.Timeout++
		case "failed":
			r.Failed++
		case "canceled":
			r.Canceled++
		case "expired":
			r.Expired++
		}
	}
	out := []localReportRow{}
	for key, r := range rows {
		r.ValidRate = float64(r.Valid) / float64(r.Tasks)
		r.WithinBudgetRate /= float64(r.Tasks)
		sort.Float64s(times[key])
		r.P50RunS = localPercentile(times[key], .5)
		r.P90RunS = localPercentile(times[key], .9)
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Class == out[j].Class {
			return out[i].Tier < out[j].Tier
		}
		return out[i].Class < out[j].Class
	})
	result := struct {
		Rows []localReportRow `json:"rows"`
		Note string           `json:"note"`
	}{out, "Review grades are not recorded in this milestone."}
	return localPrint(result, *asJSON)
}

// Nearest rank: percentile p selects sorted element ceil(p*n), with ranks starting at one.
func localPercentile(a []float64, p float64) float64 {
	if len(a) == 0 {
		return 0
	}
	rank := int(math.Ceil(math.Round(p*float64(len(a))*1e9) / 1e9))
	if rank < 1 {
		rank = 1
	}
	return a[rank-1]
}
func localQueueCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: agent-bridge local-queue submit|run|status|cancel|report")
	}
	switch args[0] {
	case "submit":
		return localSubmit(args[1:])
	case "run":
		f := flag.NewFlagSet("local-queue run", flag.ContinueOnError)
		once := f.Bool("once", false, "")
		model := f.String("model", launcherModel(), "")
		if e := f.Parse(args[1:]); e != nil {
			return e
		}
		if f.NArg() != 0 {
			return errors.New("usage: local-queue run [--once] [--model NAME]")
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return localRun(ctx, *once, *model)
	case "status":
		return localStatusCommand(args[1:])
	case "cancel":
		return localCancel(args[1:])
	case "report":
		return localReport(args[1:])
	default:
		return errors.New("usage: agent-bridge local-queue submit|run|status|cancel|report")
	}
}
