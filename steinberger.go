package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type SteinbergerStatus struct {
	Health        string    `json:"health"`
	Reason        string    `json:"reason,omitempty"`
	CheckedAt     time.Time `json:"checked_at"`
	LastSuccess   time.Time `json:"last_success,omitempty"`
	ScanTimer     string    `json:"scan_timer"`
	WatchdogTimer string    `json:"watchdog_timer"`
	ScanResult    string    `json:"scan_result"`
}

type CommitActivity struct {
	Repo        string    `json:"repo"`
	Hash        string    `json:"hash"`
	Subject     string    `json:"subject"`
	CommittedAt time.Time `json:"committed_at"`
}

type RepoActivity struct {
	Repo         string    `json:"repo"`
	LastObserved time.Time `json:"last_observed"`
	Score        float64   `json:"score"`
}

type RepoAttentionFeed struct {
	Version      int              `json:"version"`
	SampledAt    time.Time        `json:"sampled_at"`
	Repositories int              `json:"repositories"`
	ActiveRepos  []RepoActivity   `json:"active_repos"`
	Commits      []CommitActivity `json:"commits"`
}

type unitRunner func(string) (map[string]string, error)

func systemdProperties(unit string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "--user", "show", unit, "-p", "ActiveState", "-p", "Result", "-p", "ExecMainExitTimestamp").Output()
	if err != nil {
		return nil, err
	}
	properties := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			properties[key] = value
		}
	}
	if properties["ActiveState"] == "" {
		return nil, errors.New("missing ActiveState")
	}
	return properties, nil
}

func checkSteinberger(run unitRunner, now time.Time) (SteinbergerStatus, error) {
	status := SteinbergerStatus{CheckedAt: now, Health: "unknown"}
	scanTimer, err := run("repo-attention.timer")
	if err != nil {
		return status, fmt.Errorf("repo-attention.timer: %w", err)
	}
	watchdogTimer, err := run("steinberger-wd.timer")
	if err != nil {
		return status, fmt.Errorf("steinberger-wd.timer: %w", err)
	}
	scan, err := run("repo-attention.service")
	if err != nil {
		return status, fmt.Errorf("repo-attention.service: %w", err)
	}
	status.ScanTimer, status.WatchdogTimer, status.ScanResult = scanTimer["ActiveState"], watchdogTimer["ActiveState"], scan["Result"]
	if stamp := scan["ExecMainExitTimestamp"]; stamp != "" && status.ScanResult == "success" {
		status.LastSuccess, _ = time.ParseInLocation("Mon 2006-01-02 15:04:05 MST", stamp, time.Local)
	}
	switch {
	case status.ScanTimer != "active":
		status.Reason = "repo-attention.timer is " + status.ScanTimer
	case status.WatchdogTimer != "active":
		status.Reason = "steinberger-wd.timer is " + status.WatchdogTimer
	case status.ScanResult != "success":
		status.Reason = "repo-attention.service result is " + status.ScanResult
	case status.LastSuccess.IsZero():
		status.Reason = "scan success time unavailable"
	case now.Sub(status.LastSuccess) > 15*time.Minute:
		status.Reason = "last successful scan is older than 15 minutes"
	default:
		status.Health = "healthy"
	}
	if status.Health != "healthy" {
		status.Health = "unhealthy"
	}
	return status, nil
}

var steinbergerCache struct {
	sync.Mutex
	at     time.Time
	status SteinbergerStatus
	err    error
}

func cachedSteinberger(now time.Time) (SteinbergerStatus, error) {
	steinbergerCache.Lock()
	defer steinbergerCache.Unlock()
	if !steinbergerCache.at.IsZero() && now.Sub(steinbergerCache.at) < 30*time.Second {
		return steinbergerCache.status, steinbergerCache.err
	}
	steinbergerCache.status, steinbergerCache.err = checkSteinberger(systemdProperties, now)
	steinbergerCache.at = now
	return steinbergerCache.status, steinbergerCache.err
}

func readRepoAttentionFeed(home string, now time.Time) (RepoAttentionFeed, error) {
	path := filepath.Join(home, ".local", "state", "repo-attention", "activity-feed.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return RepoAttentionFeed{}, err
	}
	var feed RepoAttentionFeed
	if err := json.Unmarshal(b, &feed); err != nil {
		return RepoAttentionFeed{}, err
	}
	if feed.Version != 1 || feed.SampledAt.IsZero() || now.Sub(feed.SampledAt) > 15*time.Minute || feed.SampledAt.After(now.Add(5*time.Second)) {
		return RepoAttentionFeed{}, errors.New("repository activity feed is stale or invalid")
	}
	// A damaged feed must not reorder the viewer or print unbounded lists.
	if len(feed.Commits) > 10 || len(feed.ActiveRepos) > 8 {
		return RepoAttentionFeed{}, errors.New("repository activity feed exceeds limits")
	}
	sort.Slice(feed.Commits, func(i, j int) bool { return feed.Commits[i].CommittedAt.After(feed.Commits[j].CommittedAt) })
	return feed, nil
}
