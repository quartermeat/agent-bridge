package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var limitsBase = time.Date(2026, 10, 26, 23, 0, 0, 0, time.UTC)

func rateLine(primary, secondary float64, resetsAt time.Time) string {
	return fmt.Sprintf(`{"type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"primary":{"used_percent":%v,"window_minutes":300,"resets_at":%d},"secondary":{"used_percent":%v,"window_minutes":10080,"resets_at":%d},"credits":{"has_credits":false}}}}`, primary, resetsAt.Unix(), secondary, resetsAt.Add(48*time.Hour).Unix())
}

func writeRollout(t *testing.T, codexHome, day, name string, mtime time.Time, lines ...string) string {
	t.Helper()
	dir := filepath.Join(codexHome, "sessions", day)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return p
}

func limitsFixture(t *testing.T) string {
	t.Helper()
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("AGENT_BRIDGE_DEFER_PERCENT", "")
	clock := limitsBase
	limitsNow = func() time.Time { return clock }
	codexLimitsCache = limitsCache{}
	t.Cleanup(func() { limitsNow = time.Now; codexLimitsCache = limitsCache{} })
	return codexHome
}

func TestReadCodexLimitsUsesLastEventOfNewestFile(t *testing.T) {
	home := limitsFixture(t)
	reset := limitsBase.Add(time.Hour)
	writeRollout(t, home, "2026/10/25", "rollout-old.jsonl", limitsBase.Add(-24*time.Hour), rateLine(5, 5, reset))
	writeRollout(t, home, "2026/10/26", "rollout-new.jsonl", limitsBase,
		`{"type":"session_meta","payload":{}}`, rateLine(40, 10, reset), `{"type":"event_msg","payload":{"type":"agent_message"}}`, rateLine(97, 30, reset))
	got := readCodexLimits(t.TempDir(), limitsBase)
	if len(got) != 2 {
		t.Fatalf("limits %+v", got)
	}
	if got[0].Provider != "codex" || got[0].Window != "5h" || got[0].UsedPercent != 97 || !got[0].ResetsAt.Equal(reset) || got[0].Stale {
		t.Fatalf("primary %+v", got[0])
	}
	if got[1].Window != "7d" || got[1].UsedPercent != 30 {
		t.Fatalf("secondary %+v", got[1])
	}
}

func TestReadCodexLimitsSkipsNewestFileWithoutEvent(t *testing.T) {
	home := limitsFixture(t)
	reset := limitsBase.Add(time.Hour)
	writeRollout(t, home, "2026/10/26", "rollout-a.jsonl", limitsBase.Add(-time.Hour), rateLine(61, 2, reset))
	writeRollout(t, home, "2026/10/26", "rollout-b.jsonl", limitsBase, `{"type":"session_meta","payload":{}}`)
	got := readCodexLimits(t.TempDir(), limitsBase)
	if len(got) == 0 || got[0].UsedPercent != 61 {
		t.Fatalf("limits %+v", got)
	}
}

func TestReadCodexLimitsTailOnly(t *testing.T) {
	home := limitsFixture(t)
	reset := limitsBase.Add(time.Hour)
	// An early event followed by well over the tail window of other rows: it must not be found.
	filler := `{"type":"event_msg","payload":{"type":"agent_message","text":"` + strings.Repeat("x", 4000) + `"}}`
	lines := []string{rateLine(11, 11, reset)}
	for i := 0; i < limitsTailBytes/4000+50; i++ {
		lines = append(lines, filler)
	}
	p := writeRollout(t, home, "2026/10/26", "rollout-big.jsonl", limitsBase, lines...)
	if rl, err := tailRateLimits(p); err != nil || rl != nil {
		t.Fatalf("read beyond tail: %+v %v", rl, err)
	}
	// A recent event after the filler is found even though the cut line is partial.
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, rateLine(83, 12, reset))
	f.Close()
	rl, err := tailRateLimits(p)
	if err != nil || rl == nil || rl.Primary.UsedPercent != 83 {
		t.Fatalf("tail %+v %v", rl, err)
	}
}

func TestReadCodexLimitsIgnoresHalfWrittenLastLine(t *testing.T) {
	home := limitsFixture(t)
	p := writeRollout(t, home, "2026/10/26", "rollout-x.jsonl", limitsBase, rateLine(72, 1, limitsBase.Add(time.Hour)))
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString(`{"payload":{"rate_limits":{"primary":{"used_per`)
	f.Close()
	got := readCodexLimits(t.TempDir(), limitsBase)
	if len(got) == 0 || got[0].UsedPercent != 72 {
		t.Fatalf("limits %+v", got)
	}
}

func TestReadCodexLimitsResetExpiry(t *testing.T) {
	home := limitsFixture(t)
	reset := limitsBase.Add(time.Hour)
	writeRollout(t, home, "2026/10/26", "rollout-a.jsonl", limitsBase, rateLine(97, 50, reset))
	live := readCodexLimits(t.TempDir(), reset.Add(-time.Second))
	if live[0].Stale || live[0].UsedPercent != 97 {
		t.Fatalf("before reset %+v", live[0])
	}
	expired := readCodexLimits(t.TempDir(), reset)
	if !expired[0].Stale || expired[0].UsedPercent != 0 {
		t.Fatalf("at reset %+v", expired[0])
	}
	if expired[1].Stale {
		t.Fatalf("weekly window reset early: %+v", expired[1])
	}
}

func TestLimitDeferralThreshold(t *testing.T) {
	limitsFixture(t)
	reset := limitsBase.Add(time.Hour)
	s := Snapshot{ProviderLimits: []ProviderLimit{{Provider: "codex", Window: "5h", UsedPercent: 89.9, ResetsAt: reset}}}
	if providerExhausted("codex", s) {
		t.Fatal("deferred below default threshold")
	}
	s.ProviderLimits[0].UsedPercent = 90
	if !providerExhausted("codex", s) {
		t.Fatal("not deferred at threshold")
	}
	if providerExhausted("claude", s) {
		t.Fatal("codex limit applied to claude")
	}
	t.Setenv("AGENT_BRIDGE_DEFER_PERCENT", "95")
	if providerExhausted("codex", s) {
		t.Fatal("threshold override ignored")
	}
	t.Setenv("AGENT_BRIDGE_DEFER_PERCENT", "")
	s.ProviderLimits[0].Stale = true
	if providerExhausted("codex", s) {
		t.Fatal("stale window deferred")
	}
}

func TestSnapshotProviderLimitsJSON(t *testing.T) {
	_, _, home := launchFixture(t)
	codexHome := limitsFixture(t)
	reset := limitsBase.Add(time.Hour)
	writeRollout(t, codexHome, "2026/10/26", "rollout-a.jsonl", limitsBase, rateLine(97, 12, reset))
	s, err := collectSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	_ = home
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		ProviderLimits []map[string]any `json:"provider_limits"`
	}
	if err := json.Unmarshal(b, &decoded); err != nil || len(decoded.ProviderLimits) != 2 {
		t.Fatalf("provider_limits %s: %v", b, err)
	}
	first := decoded.ProviderLimits[0]
	for _, key := range []string{"provider", "window", "used_percent", "resets_at", "stale"} {
		if _, ok := first[key]; !ok {
			t.Fatalf("missing %q in %v", key, first)
		}
	}
	if first["provider"] != "codex" || first["window"] != "5h" || first["used_percent"] != float64(97) || first["stale"] != false {
		t.Fatalf("row %v", first)
	}
	// No data still serializes as an empty list.
	t.Setenv("CODEX_HOME", t.TempDir())
	s, _ = collectSnapshot()
	if b, _ := json.Marshal(s); !strings.Contains(string(b), `"provider_limits":[]`) {
		t.Fatalf("empty limits not an array: %s", b)
	}
}

func TestWatchLimitLine(t *testing.T) {
	limitsFixture(t)
	reset := time.Date(2026, 10, 27, 0, 46, 0, 0, time.UTC)
	s := Snapshot{SampledAt: limitsBase, ProviderLimits: []ProviderLimit{
		{Provider: "codex", Window: "5h", UsedPercent: 97, ResetsAt: reset},
		{Provider: "codex", Window: "7d", UsedPercent: 30, ResetsAt: limitsBase.Add(72 * time.Hour)},
	}}
	lines := limitLines(s)
	if len(lines) != 1 {
		t.Fatalf("lines %q", lines)
	}
	if !strings.HasPrefix(lines[0], "***") || !strings.Contains(lines[0], "codex 5h 97% → resets 00:46") || !strings.Contains(lines[0], "7d 30% → resets Thu 23:00") {
		t.Fatalf("line %q", lines[0])
	}
	s.ProviderLimits[0].UsedPercent = 40
	if line := limitLines(s)[0]; strings.HasPrefix(line, "***") {
		t.Fatalf("highlighted below threshold: %q", line)
	}
	s.ProviderLimits[0].Stale = true
	if line := limitLines(s)[0]; !strings.Contains(line, "5h reset") {
		t.Fatalf("stale line %q", line)
	}
	frame := strings.Join(renderWatchFrame(s, "test", map[string]TokenUsage{}), "\n")
	if !strings.Contains(frame, "codex 5h reset") {
		t.Fatalf("frame lacks limit line:\n%s", frame)
	}
}

func TestCoderDeferredUntilResetThenRetried(t *testing.T) {
	r, _, home := launchFixture(t)
	codexHome := limitsFixture(t)
	reset := limitsBase.Add(time.Hour)
	writeRollout(t, codexHome, "2026/10/26", "rollout-a.jsonl", limitsBase, rateLine(97, 20, reset))
	r.Request.Role, r.Request.Agent = "coder", "auto"
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
	if err != nil || result.Status != "queued" || result.RetryAt == nil || !result.RetryAt.Equal(reset) || !strings.Contains(result.Rationale, "97%") {
		t.Fatalf("deferred result %+v: %v", result, err)
	}
	if result.JobID != "" {
		t.Fatalf("job started while deferred: %+v", result)
	}
	// No Claude substitution: the saved verdict is still a Codex coder.
	var selected modelVerdict
	b, err := os.ReadFile(filepath.Join(dir, r.ID+".selected.json"))
	if err != nil || json.Unmarshal(b, &selected) != nil || selected.Agent != "codex" || selected.Role != "coder" {
		t.Fatalf("selected %+v: %v", selected, err)
	}
	if _, err := os.Stat(filepath.Join(home, "worktrees", "sample-review")); !os.IsNotExist(err) {
		t.Fatalf("worktree created while deferred: %v", err)
	}
	status, _ := json.Marshal(result)
	if !strings.Contains(string(status), `"retry_at"`) {
		t.Fatalf("request-status output lacks retry_at: %s", status)
	}
	decisions, err := os.ReadFile(filepath.Join(home, "state", "launcher", "decisions.jsonl"))
	if err != nil || !strings.Contains(string(decisions), `"outcome":"queued_usage_limit"`) || !strings.Contains(string(decisions), `"retry_at"`) {
		t.Fatalf("decision log %s: %v", decisions, err)
	}

	// Before retry_at an unforced pass leaves the request alone, even when a newer
	// rollout says the window has room.
	limitsNow = func() time.Time { return reset.Add(-time.Minute) }
	if err := processLaunch(dir, r, false); err != nil {
		t.Fatalf("pass before retry_at: %v", err)
	}
	if again, _ := readResult(dir, r.ID); again.Status != "queued" || again.RetryAt == nil {
		t.Fatalf("result changed before retry_at: %+v", again)
	}

	// After the reset the retry gets past the deferral; with the launcher full it
	// stops at the capacity wait instead of re-queuing on the usage limit.
	limitsNow = func() time.Time { return reset.Add(time.Second) }
	t.Setenv("AGENT_BRIDGE_LAUNCHER_MAX_JOBS", "1")
	capacityDir, _ := jobDir("capacity")
	if err := os.MkdirAll(capacityDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJob(Job{ID: "capacity", Agent: "codex", Role: "coder", Launcher: "launcher", Status: "running", WorkerPID: os.Getpid(), WorkerStart: processStart(os.Getpid())}); err != nil {
		t.Fatal(err)
	}
	if err := processLaunch(dir, r, false); !errors.Is(err, errCapWait) {
		t.Fatalf("retry after reset: %v", err)
	}
}

func TestEPMKeepsEscalateWhenProviderExhausted(t *testing.T) {
	r, _, _ := launchFixture(t)
	limitsFixture(t)
	// Codex data cannot affect a Claude EPM request; the Claude path is unchanged.
	s := Snapshot{ProviderLimits: []ProviderLimit{{Provider: "codex", Window: "5h", UsedPercent: 99, ResetsAt: limitsBase.Add(time.Hour)}}}
	if providerExhausted(r.Request.Agent, s) {
		t.Fatal("Codex window exhausted a Claude EPM request")
	}
}
