package main

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

func renderWatchFrame(s Snapshot, origin string, previous map[string]TokenUsage) []string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "Claude / Codex / local activity — %s UTC (%s; Ctrl-C to exit)\n\n", s.SampledAt.Format("15:04:05"), origin)
	if s.Steinberger != nil && s.Steinberger.Health != "healthy" {
		fmt.Fprintf(&b, "*** STEINBERGER UNHEALTHY: %s ***\n", s.Steinberger.Reason)
	}
	for _, line := range watchLimitLines(s) {
		fmt.Fprintln(&b, line)
	}
	for _, a := range s.Agents {
		if a.Halt != nil {
			fmt.Fprintf(&b, "*** %s HALTED: %s (%s) ***\n", a.Name, haltLabel(a.Halt), a.Halt.Code)
		}
	}
	for _, j := range s.Jobs {
		if j.Halt != nil && j.AcknowledgedAt == nil {
			fmt.Fprintf(&b, "*** JOB %s HALTED: %s (%s) ***\n", j.ID, haltLabel(j.Halt), j.Halt.Code)
			if j.SpawnedBy != nil && j.SpawnedBy.Provider == "claude" && j.Halt.Kind == "usage_limit" {
				if j.Halt.Scope == "account" {
					fmt.Fprintln(&b, "    Claude decision pending: finish scoped task or defer until Codex resets; viewer is read-only.")
				} else if j.Halt.Scope == "unknown" {
					fmt.Fprintln(&b, "    Claude decision pending: verify limit scope, then finish or defer; viewer is read-only.")
				}
			}
		}
	}
	if len(s.Agents) == 0 {
		fmt.Fprintln(&b, "No fresh agent activity.")
	}
	for _, a := range s.Agents {
		prior, seen := previous[a.ID]
		fallback := agentFallback(a, s.Jobs)
		marker := ""
		if fallback != nil {
			marker = " ↺"
		}
		fmt.Fprintf(&b, "%-7s %-20s %-20s %s%s\n", a.Provider, a.ActivityState, a.Model, a.Cwd, marker)
		if !a.UsageKnown {
			fmt.Fprint(&b, "  usage pending")
		} else {
			inputDelta, outputDelta := int64(0), int64(0)
			if seen {
				inputDelta, outputDelta = max(0, a.Usage.Input-prior.Input), max(0, a.Usage.Output-prior.Output)
			}
			fmt.Fprintf(&b, "  input %d (+%d)  cache read %d  cache write %d  output %d (+%d)", a.Usage.Input, inputDelta, a.Usage.CacheRead, a.Usage.CacheWrite, a.Usage.Output, outputDelta)
		}
		fmt.Fprintf(&b, "  %s (hook: %s)", a.StateDetail, a.Phase)
		if a.SpawnedBy != nil {
			fmt.Fprintf(&b, "  spawned by %s", a.SpawnedBy.AgentID)
		}
		if a.Target != nil && a.Target.Name != "" {
			fmt.Fprintf(&b, " → %s", a.Target.Name)
		}
		if fallback != nil {
			fmt.Fprintf(&b, "  %s", fallbackLabel(*fallback, s.Jobs))
		}
		fmt.Fprintln(&b)
		previous[a.ID] = a.Usage
	}
	fmt.Fprintln(&b, "\nBridge jobs:")
	if len(s.Jobs) == 0 {
		fmt.Fprintln(&b, "  none in the last hour")
	}
	for _, j := range s.Jobs {
		fmt.Fprintf(&b, "  %-24s %-10s %s  %s/%s depth %d", j.ID, j.Status, j.Name, j.Agent, j.Role, j.Depth)
		if j.Profile != "" {
			fmt.Fprintf(&b, "  %s %s/%s limit %s", j.Profile, j.Model, j.Effort, j.TimeLimit)
		}
		if j.Substitution != "" {
			fmt.Fprintf(&b, "  substituted from %s", j.Substitution)
		}
		if j.FallbackFor != "" {
			fmt.Fprintf(&b, "  %s", fallbackLabel(j, s.Jobs))
		}
		if j.Usage != nil {
			fmt.Fprintf(&b, "  input %d output %d", j.Usage.Input, j.Usage.Output)
		}
		if j.SpawnedBy != nil {
			fmt.Fprintf(&b, "  spawned by %s", j.SpawnedBy.AgentID)
		}
		if j.Halt != nil {
			fmt.Fprintf(&b, "  %s", haltLabel(j.Halt))
		}
		fmt.Fprintln(&b)
	}
	fmt.Fprintln(&b, "\nSteinberger:")
	if s.Steinberger == nil {
		fmt.Fprintln(&b, "  status unavailable")
	} else {
		fmt.Fprintf(&b, "  %s  scan timer %s  watchdog timer %s  scan result %s\n", s.Steinberger.Health, s.Steinberger.ScanTimer, s.Steinberger.WatchdogTimer, s.Steinberger.ScanResult)
		if !s.Steinberger.LastSuccess.IsZero() {
			fmt.Fprintf(&b, "  last successful scan %s ago\n", s.SampledAt.Sub(s.Steinberger.LastSuccess).Round(time.Second))
		}
	}
	if s.RepoAttention == nil {
		fmt.Fprintln(&b, "  repository activity feed unavailable")
	} else {
		fmt.Fprintf(&b, "  %d repositories; feed sampled %s ago\n", s.RepoAttention.Repositories, s.SampledAt.Sub(s.RepoAttention.SampledAt).Round(time.Second))
		fmt.Fprintln(&b, "  recent commits:")
		if len(s.RepoAttention.Commits) == 0 {
			fmt.Fprintln(&b, "    none in the last seven days")
		}
		for _, commit := range s.RepoAttention.Commits {
			fmt.Fprintf(&b, "    %-22s %.7s  %s  (%s ago)\n", filepath.Base(commit.Repo), commit.Hash, commit.Subject, s.SampledAt.Sub(commit.CommittedAt).Round(time.Minute))
		}
	}
	renderVRAM(&b, s)
	return strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
}

func watchLimitLines(s Snapshot) []string {
	lines := limitLines(s)
	for _, l := range s.ProviderLimits {
		if !l.Stale {
			continue
		}
		for i := range lines {
			if strings.Contains(lines[i], l.Provider+" ") {
				lines[i] = strings.Replace(lines[i], l.Window+" reset", fmt.Sprintf("%s reset (stale, %.0f%%)", l.Window, l.UsedPercent), 1)
				break
			}
		}
	}
	return lines
}

func agentFallback(a AgentUsage, jobs []Job) *Job {
	for i := range jobs {
		j := &jobs[i]
		if j.FallbackFor != "" && j.SessionID != "" && j.SessionID == a.SessionID && j.Status == "running" {
			return j
		}
	}
	return nil
}

func fallbackLabel(j Job, jobs []Job) string {
	label := "fallback for " + j.FallbackFor
	for _, original := range jobs {
		if original.ID == j.FallbackFor && original.Name != "" {
			return label + " (" + original.Name + ")"
		}
	}
	return label
}
