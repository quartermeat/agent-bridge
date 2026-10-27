package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProviderLimit is one rate-limit window of a provider. Stale means the window's
// reset time has passed, so the recorded usage no longer applies.
type ProviderLimit struct {
	Provider    string    `json:"provider"`
	Window      string    `json:"window"`
	UsedPercent float64   `json:"used_percent"`
	ResetsAt    time.Time `json:"resets_at"`
	Stale       bool      `json:"stale"`
}

const (
	defaultDeferPercent = 90
	limitsTailBytes     = 1 << 20
	limitsMaxCandidates = 8
)

// limitsNow is replaced in tests.
var limitsNow = time.Now

// deferPercent is the used percentage at or above which coder launches wait for a reset.
func deferPercent() float64 {
	if raw := os.Getenv("AGENT_BRIDGE_DEFER_PERCENT"); raw != "" {
		if n, err := strconv.ParseFloat(raw, 64); err == nil && n > 0 {
			return n
		}
	}
	return defaultDeferPercent
}

func codexSessionsDir(home string) string {
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return filepath.Join(v, "sessions")
	}
	return filepath.Join(home, ".codex", "sessions")
}

type rateWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int64   `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
}

type codexRateLimits struct {
	Primary   *rateWindow `json:"primary"`
	Secondary *rateWindow `json:"secondary"`
	Credits   *struct {
		HasCredits bool `json:"has_credits"`
	} `json:"credits"`
}

type limitsCache struct {
	sync.Mutex
	path   string
	size   int64
	mtime  time.Time
	limits *codexRateLimits
}

var codexLimitsCache limitsCache

// newestRollouts lists rollout files newest first. Sessions live under
// YYYY/MM/DD, so only the newest few day directories are listed.
func newestRollouts(base string) []string {
	type file struct {
		path  string
		mtime time.Time
	}
	var files []file
	days := 0
	for _, y := range sortedDirs(base) {
		for _, m := range sortedDirs(filepath.Join(base, y)) {
			for _, d := range sortedDirs(filepath.Join(base, y, m)) {
				dir := filepath.Join(base, y, m, d)
				entries, err := os.ReadDir(dir)
				if err != nil {
					continue
				}
				found := false
				for _, e := range entries {
					if e.IsDir() || !strings.HasPrefix(e.Name(), "rollout-") || !strings.HasSuffix(e.Name(), ".jsonl") {
						continue
					}
					if info, err := e.Info(); err == nil {
						files = append(files, file{filepath.Join(dir, e.Name()), info.ModTime()})
						found = true
					}
				}
				if found {
					days++
				}
				if days >= 2 {
					return sortRollouts(files, func(f file) (string, time.Time) { return f.path, f.mtime })
				}
			}
		}
	}
	return sortRollouts(files, func(f file) (string, time.Time) { return f.path, f.mtime })
}

func sortRollouts[T any](files []T, key func(T) (string, time.Time)) []string {
	sort.Slice(files, func(i, j int) bool {
		pi, ti := key(files[i])
		pj, tj := key(files[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return pi > pj
	})
	out := make([]string, 0, len(files))
	for _, f := range files {
		p, _ := key(f)
		out = append(out, p)
	}
	return out
}

// sortedDirs returns subdirectory names, descending.
func sortedDirs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names
}

// tailRateLimits returns the last rate_limits event in the final limitsTailBytes
// of the file, without reading the rest.
func tailRateLimits(path string) (*codexRateLimits, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	offset := max(int64(0), info.Size()-limitsTailBytes)
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		// The first line is probably cut in half.
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			return nil, nil
		}
	}
	lines := bytes.Split(data, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		if !bytes.Contains(lines[i], []byte(`"rate_limits"`)) {
			continue
		}
		var row struct {
			Payload struct {
				RateLimits *codexRateLimits `json:"rate_limits"`
			} `json:"payload"`
		}
		if json.Unmarshal(lines[i], &row) != nil {
			continue // possibly a half-written final line
		}
		if rl := row.Payload.RateLimits; rl != nil && (rl.Primary != nil || rl.Secondary != nil) {
			return rl, nil
		}
	}
	return nil, nil
}

func cachedTailRateLimits(path string) (*codexRateLimits, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	c := &codexLimitsCache
	c.Lock()
	defer c.Unlock()
	if c.path == path && c.size == info.Size() && c.mtime.Equal(info.ModTime()) {
		return c.limits, nil
	}
	rl, err := tailRateLimits(path)
	if err != nil {
		return nil, err
	}
	c.path, c.size, c.mtime, c.limits = path, info.Size(), info.ModTime(), rl
	return rl, nil
}

func windowLabel(minutes int64) string {
	switch {
	case minutes <= 0:
		return "?"
	case minutes%(60*24) == 0:
		return fmt.Sprintf("%dd", minutes/(60*24))
	case minutes%60 == 0:
		return fmt.Sprintf("%dh", minutes/60)
	}
	return fmt.Sprintf("%dm", minutes)
}

// readCodexLimits reads the newest Codex rate-limit event. A window whose reset
// time has passed is reported as stale at 0%.
func readCodexLimits(home string, now time.Time) []ProviderLimit {
	for i, path := range newestRollouts(codexSessionsDir(home)) {
		if i >= limitsMaxCandidates {
			break
		}
		rl, err := cachedTailRateLimits(path)
		if err != nil || rl == nil {
			continue
		}
		var out []ProviderLimit
		for _, w := range []*rateWindow{rl.Primary, rl.Secondary} {
			if w == nil {
				continue
			}
			l := ProviderLimit{Provider: "codex", Window: windowLabel(w.WindowMinutes), UsedPercent: w.UsedPercent}
			if w.ResetsAt > 0 {
				l.ResetsAt = time.Unix(w.ResetsAt, 0).UTC()
				if !now.Before(l.ResetsAt) {
					l.Stale, l.UsedPercent = true, 0
				}
			}
			out = append(out, l)
		}
		return out
	}
	return nil
}

// limitDeferral reports when a provider's most-used live window clears if any
// window is at or above the defer threshold. A zero time means the reset is unknown.
func limitDeferral(provider string, s Snapshot) (limit ProviderLimit, until time.Time, ok bool) {
	now := limitsNow()
	threshold := deferPercent()
	for _, l := range s.ProviderLimits {
		if l.Provider != provider || l.Stale || l.UsedPercent < threshold {
			continue
		}
		if !l.ResetsAt.IsZero() && !now.Before(l.ResetsAt) {
			continue
		}
		if !ok || l.ResetsAt.After(until) {
			limit, until = l, l.ResetsAt
		}
		ok = true
	}
	return limit, until, ok
}

func formatReset(t, now time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	if t.Sub(now) >= 20*time.Hour {
		return t.UTC().Format("Mon 15:04")
	}
	return t.UTC().Format("15:04")
}

// limitLines renders one compact line per provider; lines at or above the
// threshold start with "***" so the viewer highlights them.
func limitLines(s Snapshot) []string {
	var order []string
	byProvider := map[string][]ProviderLimit{}
	for _, l := range s.ProviderLimits {
		if _, seen := byProvider[l.Provider]; !seen {
			order = append(order, l.Provider)
		}
		byProvider[l.Provider] = append(byProvider[l.Provider], l)
	}
	threshold := deferPercent()
	var lines []string
	for _, p := range order {
		var parts []string
		hot := false
		for _, l := range byProvider[p] {
			if l.Stale {
				parts = append(parts, fmt.Sprintf("%s reset", l.Window))
				continue
			}
			parts = append(parts, fmt.Sprintf("%s %.0f%% → resets %s", l.Window, l.UsedPercent, formatReset(l.ResetsAt, s.SampledAt)))
			hot = hot || l.UsedPercent >= threshold
		}
		line := p + " " + strings.Join(parts, " · ")
		if hot {
			line = fmt.Sprintf("*** %s — coders deferred ***", line)
		}
		lines = append(lines, line)
	}
	return lines
}
