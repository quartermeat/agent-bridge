package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type lqEnv struct {
	dir    string
	offset atomic.Int64
	ticks  atomic.Int64
}

// lqSetup isolates state and home, replaces the injectable vars and restores them.
func lqSetup(t *testing.T) *lqEnv {
	t.Helper()
	t.Setenv("AGENT_BRIDGE_STATE_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	env := &lqEnv{}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	oldNow, oldWait, oldComplete := localNow, localWaitLease, localComplete
	localNow = func() time.Time {
		return base.Add(time.Duration(env.offset.Load()) + time.Duration(env.ticks.Add(1))*time.Millisecond)
	}
	localWaitLease = func(context.Context) error { return nil }
	localComplete = func(context.Context, string, string, string) (string, TokenUsage, error) {
		t.Error("unexpected model call")
		return "", TokenUsage{}, errors.New("unexpected")
	}
	t.Cleanup(func() { localNow, localWaitLease, localComplete = oldNow, oldWait, oldComplete })
	d, err := localEnsureDir()
	if err != nil {
		t.Fatal(err)
	}
	env.dir = d
	return env
}

func lqCapture(t *testing.T, f func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	ferr := f()
	w.Close()
	os.Stdout = old
	return strings.TrimSpace(<-done), ferr
}

func lqInput(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "in.txt")
	if err := os.WriteFile(p, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func lqSubmit(t *testing.T, input string, args ...string) string {
	t.Helper()
	id, err := lqCapture(t, func() error {
		return localSubmit(append([]string{"--input-file", lqInput(t, input)}, args...))
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func lqLabel(t *testing.T, input string, args ...string) string {
	return lqSubmit(t, input, append([]string{"--class", "label", "--labels", "red,green,blue"}, args...)...)
}

func lqExtract(t *testing.T, input string, args ...string) string {
	return lqSubmit(t, input, append([]string{"--class", "extract", "--fields", "a,b"}, args...)...)
}

func lqRun(t *testing.T) {
	t.Helper()
	if err := localRun(context.Background(), true, "test-model"); err != nil {
		t.Fatal(err)
	}
}

func lqTask(t *testing.T, env *lqEnv, id string) localTask {
	t.Helper()
	task, err := localReadTask(env.dir, id)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func lqFake(outputs ...string) func(context.Context, string, string, string) (string, TokenUsage, error) {
	var n atomic.Int64
	return func(context.Context, string, string, string) (string, TokenUsage, error) {
		i := int(n.Add(1)) - 1
		if i >= len(outputs) {
			i = len(outputs) - 1
		}
		return outputs[i], TokenUsage{Input: 10, Output: 2}, nil
	}
}

func lqTelemetryLines(t *testing.T, env *lqEnv) []localTelemetry {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(env.dir, "telemetry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []localTelemetry
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte{'\n'}) {
		var rec localTelemetry
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatal(err)
		}
		out = append(out, rec)
	}
	return out
}

func TestLocalQueueSerialFIFO(t *testing.T) {
	env := lqSetup(t)
	var ids []string
	for _, in := range []string{"one", "two", "three"} {
		ids = append(ids, lqLabel(t, in))
	}
	var running, maxRunning atomic.Int64
	var mu sync.Mutex
	var order []string
	localComplete = func(_ context.Context, _, _, prompt string) (string, TokenUsage, error) {
		n := running.Add(1)
		if n > maxRunning.Load() {
			maxRunning.Store(n)
		}
		mu.Lock()
		order = append(order, strings.SplitN(prompt, "\n", 2)[0])
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		running.Add(-1)
		return "red", TokenUsage{}, nil
	}
	lqRun(t)
	if maxRunning.Load() != 1 {
		t.Fatalf("max concurrent = %d", maxRunning.Load())
	}
	if strings.Join(order, ",") != "one,two,three" {
		t.Fatalf("order = %v", order)
	}
	for _, id := range ids {
		if s := lqTask(t, env, id).State; s != "done" {
			t.Fatalf("%s state %s", id, s)
		}
	}
}

func TestLocalQueueLabelValidity(t *testing.T) {
	env := lqSetup(t)
	cases := []struct {
		out, result, failed string
	}{
		{"red", "valid", ""},
		{"  green\n", "valid", ""},
		{"purple", "invalid", "label_in_set"},
		{"", "invalid", "parsed"},
		{"Red", "invalid", "label_in_set"},
	}
	for _, c := range cases {
		id := lqLabel(t, "text", "--max-attempts", "1")
		localComplete = lqFake(c.out)
		lqRun(t)
		task := lqTask(t, env, id)
		if task.State != "done" || task.Validity.Result != c.result {
			t.Fatalf("%q: state %s result %s", c.out, task.State, task.Validity.Result)
		}
		if c.failed != "" && !containsString(task.Validity.Failed, c.failed) {
			t.Fatalf("%q: failed %v", c.out, task.Validity.Failed)
		}
	}
}

func containsString(a []string, s string) bool {
	for _, x := range a {
		if x == s {
			return true
		}
	}
	return false
}

func TestLocalQueueExtractValidity(t *testing.T) {
	env := lqSetup(t)
	long := strings.Repeat("x", 501)
	cases := []struct {
		name, out, failed string
	}{
		{"valid", `{"a":"1","b":"2"}`, ""},
		{"fenced", "```json\n{\"a\":\"1\",\"b\":\"2\"}\n```", ""},
		{"extra key", `{"a":"1","b":"2","c":"3"}`, "fields_match"},
		{"missing key", `{"a":"1"}`, "fields_match"},
		{"non-string", `{"a":1,"b":"2"}`, "values_strings"},
		{"null value", `{"a":null,"b":"2"}`, "values_strings"},
		{"long value", `{"a":"` + long + `","b":"2"}`, "values_short"},
		{"not json", `a=1`, "parsed"},
	}
	for _, c := range cases {
		id := lqExtract(t, "text", "--max-attempts", "1")
		localComplete = lqFake(c.out)
		lqRun(t)
		task := lqTask(t, env, id)
		if c.failed == "" && task.Validity.Result != "valid" {
			t.Fatalf("%s: %v", c.name, task.Validity)
		}
		if c.failed != "" && (task.Validity.Result != "invalid" || !containsString(task.Validity.Failed, c.failed)) {
			t.Fatalf("%s: %v", c.name, task.Validity)
		}
	}
}

func TestLocalQueueGoldCorrectness(t *testing.T) {
	env := lqSetup(t)
	cases := []struct {
		out            string
		valid, correct bool
	}{{"red", true, true}, {"green", true, false}, {"nope", false, false}}
	for _, c := range cases {
		id := lqLabel(t, "text", "--max-attempts", "1", "--expect", "red")
		localComplete = lqFake(c.out)
		lqRun(t)
		task := lqTask(t, env, id)
		if !task.Gold || task.Correct != c.correct || (task.Validity.Result == "valid") != c.valid {
			t.Fatalf("%q: %+v %v", c.out, task.Validity, task.Correct)
		}
	}
	id := lqExtract(t, "text", "--max-attempts", "1", "--expect", `{"b":"2","a":"1"}`)
	localComplete = lqFake(`{"a":"1","b":"2"}`)
	lqRun(t)
	if !lqTask(t, env, id).Correct {
		t.Fatal("extract deep-equal should be correct")
	}
	recs := lqTelemetryLines(t, env)
	if len(recs) != 4 || recs[0].Correct != true || recs[1].Correct != false || !recs[1].Gold {
		t.Fatalf("telemetry %+v", recs)
	}
}

func TestLocalQueueRetryLimit(t *testing.T) {
	env := lqSetup(t)
	for _, max := range []string{"2", "3"} {
		id := lqLabel(t, "text", "--max-attempts", max)
		var prompts []string
		localComplete = func(_ context.Context, _, _, prompt string) (string, TokenUsage, error) {
			prompts = append(prompts, prompt)
			return "bad", TokenUsage{Input: 10, Output: 2}, nil
		}
		lqRun(t)
		task := lqTask(t, env, id)
		want := int(max[0] - '0')
		if len(prompts) != want || task.Attempts != want {
			t.Fatalf("max %s: calls %d attempts %d", max, len(prompts), task.Attempts)
		}
		if strings.Contains(prompts[0], "label_in_set") || !strings.Contains(prompts[1], "label_in_set") {
			t.Fatalf("retry prompt does not name the failed check: %q", prompts)
		}
		if task.State != "done" || task.Validity.Result != "invalid" {
			t.Fatalf("state %s %v", task.State, task.Validity)
		}
		if task.PromptTokens != int64(10*want) || task.OutputTokens != int64(2*want) {
			t.Fatalf("tokens %d/%d", task.PromptTokens, task.OutputTokens)
		}
	}
}

func TestLocalQueueTimeout(t *testing.T) {
	env := lqSetup(t)
	block := func(ctx context.Context, _, _, _ string) (string, TokenUsage, error) {
		<-ctx.Done()
		return "", TokenUsage{}, ctx.Err()
	}
	id := lqLabel(t, "text", "--budget", "1s", "--timeout", "50ms", "--max-attempts", "1")
	localComplete = block
	start := time.Now()
	lqRun(t)
	task := lqTask(t, env, id)
	if task.State != "timeout" || time.Since(start) > time.Second {
		t.Fatalf("state %s after %v", task.State, time.Since(start))
	}
	// A timed-out attempt with no budget left for a retry is final.
	id = lqLabel(t, "text", "--budget", "1s", "--timeout", "50ms", "--max-attempts", "3")
	localComplete = func(ctx context.Context, m, s, p string) (string, TokenUsage, error) {
		env.offset.Add(int64(time.Hour))
		return block(ctx, m, s, p)
	}
	lqRun(t)
	task = lqTask(t, env, id)
	if task.State != "timeout" || task.Attempts != 1 {
		t.Fatalf("state %s attempts %d", task.State, task.Attempts)
	}
	for _, rec := range lqTelemetryLines(t, env) {
		if rec.State != "timeout" {
			t.Fatalf("telemetry state %s", rec.State)
		}
	}
}

func TestLocalQueueCancelQueuedAndRunning(t *testing.T) {
	env := lqSetup(t)
	queued := lqLabel(t, "text")
	if err := localCancel([]string{"--by", "tester", queued}); err != nil {
		t.Fatal(err)
	}
	if task := lqTask(t, env, queued); task.State != "canceled" || task.CanceledBy != "tester" {
		t.Fatalf("%+v", task)
	}
	lqRun(t) // the fake fails the test if the canceled task is started

	running := lqLabel(t, "text")
	started := make(chan struct{})
	localComplete = func(ctx context.Context, _, _, _ string) (string, TokenUsage, error) {
		close(started)
		<-ctx.Done()
		return "", TokenUsage{}, ctx.Err()
	}
	done := make(chan error, 1)
	go func() { done <- localRun(context.Background(), true, "m") }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("task never started")
	}
	if err := localCancel([]string{"--by", "boss", running}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("running task was not canceled in time")
	}
	task := lqTask(t, env, running)
	if task.State != "canceled" || task.CanceledBy != "boss" {
		t.Fatalf("%+v", task)
	}
	if err := localCancel([]string{running}); err == nil {
		t.Fatal("canceling a finished task must fail")
	}
	if lqTask(t, env, running).CanceledBy != "boss" {
		t.Fatal("finished task changed")
	}
}

func TestLocalQueueExpiry(t *testing.T) {
	env := lqSetup(t)
	id := lqLabel(t, "text", "--budget", "1s")
	env.offset.Add(int64(time.Hour))
	lqRun(t)
	task := lqTask(t, env, id)
	if task.State != "expired" || task.Attempts != 0 {
		t.Fatalf("%+v", task)
	}
	if recs := lqTelemetryLines(t, env); len(recs) != 1 || recs[0].State != "expired" {
		t.Fatalf("%+v", recs)
	}
}

func TestLocalQueueRestartRecovery(t *testing.T) {
	env := lqSetup(t)
	retry := lqLabel(t, "text", "--max-attempts", "2")
	spent := lqLabel(t, "text", "--max-attempts", "1")
	for id, attempts := range map[string]int{retry: 1, spent: 1} {
		task := lqTask(t, env, id)
		task.State, task.Attempts = "running", attempts
		if err := localWriteTask(env.dir, task); err != nil {
			t.Fatal(err)
		}
	}
	if err := localRecover(env.dir); err != nil {
		t.Fatal(err)
	}
	if task := lqTask(t, env, retry); task.State != "queued" || len(task.AttemptLog) != 1 || task.AttemptLog[0].Code != "interrupted" {
		t.Fatalf("%+v", task)
	}
	if task := lqTask(t, env, spent); task.State != "failed" || task.Code != "interrupted" {
		t.Fatalf("%+v", task)
	}
}

func TestLocalQueueSingleRunner(t *testing.T) {
	env := lqSetup(t)
	f, err := os.OpenFile(filepath.Join(env.dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	err = localRun(context.Background(), true, "m")
	if err == nil || !strings.Contains(err.Error(), "another") {
		t.Fatalf("err = %v", err)
	}
}

func TestLocalQueueGPUYieldPause(t *testing.T) {
	env := lqSetup(t)
	id := lqLabel(t, "text")
	localWaitLease = func(context.Context) error { time.Sleep(80 * time.Millisecond); return nil }
	localComplete = func(context.Context, string, string, string) (string, TokenUsage, error) {
		time.Sleep(10 * time.Millisecond)
		return "red", TokenUsage{}, nil
	}
	lqRun(t)
	task := lqTask(t, env, id)
	if task.PausedS < 0.07 || task.RunS >= 0.07 || task.RunS <= 0 {
		t.Fatalf("paused %v run %v", task.PausedS, task.RunS)
	}
}

func TestLocalQueueTelemetryHasNoText(t *testing.T) {
	env := lqSetup(t)
	valid := lqSubmit(t, "INPUTSECRET one", "--class", "label", "--labels", "LABELSECRET1,LABELSECRET2", "--reason", "REASONSECRET", "--expect", "LABELSECRET1")
	localComplete = lqFake("LABELSECRET1")
	lqRun(t)
	invalid := lqSubmit(t, "INPUTSECRET two", "--class", "label", "--labels", "LABELSECRET1,LABELSECRET2", "--reason", "REASONSECRET", "--max-attempts", "1")
	localComplete = lqFake("OUTPUTSECRET")
	lqRun(t)
	if lqTask(t, env, valid).State != "done" || lqTask(t, env, invalid).State != "done" {
		t.Fatal("tasks did not finish")
	}
	b, err := os.ReadFile(filepath.Join(env.dir, "telemetry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	salt, err := os.ReadFile(filepath.Join(env.dir, "salt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"INPUTSECRET", "OUTPUTSECRET", "LABELSECRET", "REASONSECRET", string(salt), hex.EncodeToString(salt)} {
		if bytes.Contains(b, []byte(s)) {
			t.Fatalf("telemetry contains %q", s)
		}
	}
	for _, rec := range lqTelemetryLines(t, env) {
		if rec.Review != nil || rec.ClaimedConfidence != nil || rec.Arm != "unassigned" || rec.Spec != "local-task/v1" {
			t.Fatalf("%+v", rec)
		}
	}
}

func TestLocalQueuePermissions(t *testing.T) {
	env := lqSetup(t)
	id := lqLabel(t, "text")
	localComplete = lqFake("red")
	lqRun(t)
	dirs := []string{env.dir, filepath.Join(env.dir, "tasks"), filepath.Join(env.dir, "tasks", id)}
	for _, p := range dirs {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0700 {
			t.Fatalf("%s: %v %v", p, fi, err)
		}
	}
	files := []string{
		filepath.Join(env.dir, "tasks", id, "task.json"),
		filepath.Join(env.dir, "tasks", id, "output"),
		filepath.Join(env.dir, "telemetry.jsonl"),
		filepath.Join(env.dir, "salt"),
	}
	for _, p := range files {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0600 {
			t.Fatalf("%s: %v %v", p, fi, err)
		}
	}
	if _, err := os.Stat(filepath.Join(env.dir, "tasks", id, "input")); !os.IsNotExist(err) {
		t.Fatalf("input should be removed: %v", err)
	}
}

func TestLocalQueueSubmitRejects(t *testing.T) {
	lqSetup(t)
	home, _ := os.UserHomeDir()
	private := filepath.Join(home, "private")
	if err := os.MkdirAll(private, 0700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(private, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	ok := lqInput(t, "text")
	cases := map[string][]string{
		"oversize":      {"--class", "label", "--labels", "a,b", "--input-file", lqInput(t, strings.Repeat("x", 16385))},
		"empty":         {"--class", "label", "--labels", "a,b", "--input-file", lqInput(t, "")},
		"missing label": {"--class", "label", "--input-file", ok},
		"one label":     {"--class", "label", "--labels", "a", "--input-file", ok},
		"missing field": {"--class", "extract", "--input-file", ok},
		"bad field":     {"--class", "extract", "--fields", "Bad", "--input-file", ok},
		"bad class":     {"--class", "summarize", "--labels", "a,b", "--input-file", ok},
		"attempts 0":    {"--class", "label", "--labels", "a,b", "--max-attempts", "0", "--input-file", ok},
		"attempts 4":    {"--class", "label", "--labels", "a,b", "--max-attempts", "4", "--input-file", ok},
		"budget low":    {"--class", "label", "--labels", "a,b", "--budget", "500ms", "--input-file", ok},
		"budget high":   {"--class", "label", "--labels", "a,b", "--budget", "11m", "--input-file", ok},
		"directory":     {"--class", "label", "--labels", "a,b", "--input-file", t.TempDir()},
		"private":       {"--class", "label", "--labels", "a,b", "--input-file", secret},
		"private link":  {"--class", "label", "--labels", "a,b", "--input-file", link},
		"long reason":   {"--class", "label", "--labels", "a,b", "--reason", strings.Repeat("r", 201), "--input-file", ok},
	}
	for name, args := range cases {
		if _, err := lqCapture(t, func() error { return localSubmit(args) }); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestLocalQueueReport(t *testing.T) {
	env := lqSetup(t)
	got, err := lqCapture(t, func() error { return localReport([]string{"--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var empty struct {
		Rows []localReportRow `json:"rows"`
		Note string           `json:"note"`
	}
	if err := json.Unmarshal([]byte(got), &empty); err != nil || len(empty.Rows) != 0 || !strings.Contains(empty.Note, "not recorded") {
		t.Fatalf("empty report: %s %v", got, err)
	}
	var lines []byte
	add := func(class string, size int, state, result string, run float64, within, gold, correct bool) {
		b, _ := json.Marshal(localTelemetry{Class: class, Tier: localTier(size), State: state, Validity: localValidity{Result: result}, RunS: run, WithinBudget: within, Gold: gold, Correct: correct, PromptTokens: 10, OutputTokens: 1})
		lines = append(append(lines, b...), '\n')
	}
	for i := 1; i <= 10; i++ {
		result := "valid"
		if i > 8 {
			result = "invalid"
		}
		add("label", 100, "done", result, float64(i), i <= 9, i <= 2, i == 1)
	}
	add("label", 3000, "timeout", "unchecked", 5, false, false, false)
	add("label", 3000, "canceled", "unchecked", 0, true, false, false)
	add("label", 3000, "failed", "unchecked", 1, true, false, false)
	add("label", 3000, "expired", "unchecked", 0, true, false, false)
	if err := os.WriteFile(filepath.Join(env.dir, "telemetry.jsonl"), lines, 0600); err != nil {
		t.Fatal(err)
	}
	got, err = lqCapture(t, func() error { return localReport([]string{"--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Rows []localReportRow `json:"rows"`
	}
	if err := json.Unmarshal([]byte(got), &rep); err != nil || len(rep.Rows) != 2 {
		t.Fatalf("%s %v", got, err)
	}
	m, s := rep.Rows[0], rep.Rows[1] // rows sort by class, then tier name
	if s.Tier != "S" || s.Tasks != 10 || s.Valid != 8 || s.ValidRate != 0.8 || s.GoldTasks != 2 || s.GoldCorrect != 1 ||
		s.WithinBudgetRate != 0.9 || s.P50RunS != 5 || s.P90RunS != 9 || s.PromptTokens != 100 || s.OutputTokens != 10 {
		t.Fatalf("S row %+v", s)
	}
	if m.Tier != "M" || m.Tasks != 4 || m.Timeout != 1 || m.Canceled != 1 || m.Failed != 1 || m.Expired != 1 || m.P50RunS != 0 || m.P90RunS != 5 {
		t.Fatalf("M row %+v", m)
	}
	nearest := []struct {
		in   []float64
		p    float64
		want float64
	}{{[]float64{1, 2, 3, 4}, .5, 2}, {[]float64{1, 2, 3, 4}, .9, 4}, {[]float64{7}, .5, 7}, {nil, .9, 0}}
	for _, c := range nearest {
		if g := localPercentile(c.in, c.p); g != c.want {
			t.Errorf("percentile(%v, %v) = %v, want %v", c.in, c.p, g, c.want)
		}
	}
}

func TestLocalQueueStatusJSON(t *testing.T) {
	env := lqSetup(t)
	got, err := lqCapture(t, func() error { return localStatusCommand([]string{"--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"depth", "counts", "running", "oldest_queued_age_s"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing key %s in %s", k, got)
		}
	}
	id := lqLabel(t, "INPUTTEXT")
	got, err = lqCapture(t, func() error { return localStatusCommand([]string{"--json"}) })
	if err != nil || !strings.Contains(got, `"depth":1`) || !strings.Contains(got, `"queued":1`) {
		t.Fatalf("%s %v", got, err)
	}
	localComplete = lqFake("OUTPUTTEXT")
	lqRun(t)
	for _, args := range [][]string{{"--json"}, {"--json", id}, {id}} {
		got, err = lqCapture(t, func() error { return localStatusCommand(args) })
		if err != nil || strings.Contains(got, "INPUTTEXT") || strings.Contains(got, "OUTPUTTEXT") {
			t.Fatalf("%v: %s %v", args, got, err)
		}
	}
	if lqTask(t, env, id).State != "done" {
		t.Fatal("not done")
	}
}
