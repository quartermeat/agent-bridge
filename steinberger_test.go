package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSteinbergerHealth(t *testing.T) {
	now := time.Date(2026, 10, 21, 22, 0, 0, 0, time.Local)
	last := now.Add(-5 * time.Minute).Format("Mon 2006-01-02 15:04:05 MST")
	states := map[string]map[string]string{
		"repo-attention.timer":   {"ActiveState": "active"},
		"steinberger-wd.timer":   {"ActiveState": "active"},
		"repo-attention.service": {"ActiveState": "inactive", "Result": "success", "ExecMainExitTimestamp": last},
	}
	run := func(unit string) (map[string]string, error) { return states[unit], nil }
	status, err := checkSteinberger(run, now)
	if err != nil || status.Health != "healthy" || status.LastSuccess.IsZero() {
		t.Fatalf("healthy status: %+v %v", status, err)
	}
	states["steinberger-wd.timer"]["ActiveState"] = "inactive"
	status, err = checkSteinberger(run, now)
	if err != nil || status.Health != "unhealthy" || !strings.Contains(status.Reason, "steinberger-wd.timer") {
		t.Fatalf("inactive watchdog timer: %+v %v", status, err)
	}
	states["steinberger-wd.timer"]["ActiveState"] = "active"
	states["repo-attention.service"]["ExecMainExitTimestamp"] = now.Add(-16 * time.Minute).Format("Mon 2006-01-02 15:04:05 MST")
	status, err = checkSteinberger(run, now)
	if err != nil || status.Health != "unhealthy" || !strings.Contains(status.Reason, "older than 15 minutes") {
		t.Fatalf("stale scan: %+v %v", status, err)
	}
}

func TestRepoAttentionFeedAge(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".local", "state", "repo-attention", "activity-feed.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	feed := RepoAttentionFeed{Version: 1, SampledAt: now, Repositories: 3, Commits: []CommitActivity{}}
	b, _ := json.Marshal(feed)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := readRepoAttentionFeed(home, now); err != nil || got.Repositories != 3 {
		t.Fatalf("fresh feed: %+v %v", got, err)
	}
	if _, err := readRepoAttentionFeed(home, now.Add(16*time.Minute)); err == nil {
		t.Fatal("stale feed accepted")
	}
}
