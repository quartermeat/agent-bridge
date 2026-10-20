package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTokenFeedCountsSessionGrowthOnceAcrossRestart(t *testing.T) {
	t.Setenv("AGENT_BRIDGE_STATE_DIR", t.TempDir())
	f, err := loadTokenFeed()
	if err != nil {
		t.Fatal(err)
	}
	s := Snapshot{Agents: []AgentUsage{
		{Provider: "claude", SessionID: "claude-one", UsageKnown: true, Usage: TokenUsage{Input: 100, CacheRead: 80, Output: 20}},
		{Provider: "codex", SessionID: "codex-one", UsageKnown: true, Usage: TokenUsage{Input: 30, Output: 4}},
	}}
	f.addSnapshot(s)
	f.addSnapshot(s)
	if f.Claude != 120 || f.Codex != 34 {
		t.Fatalf("duplicate or missing tokens: %+v", f)
	}
	if err := f.publish(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(os.Getenv("AGENT_BRIDGE_STATE_DIR"), "token-feed.tsv"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("feed permissions: %v, %v", info, err)
	}
	restarted, err := loadTokenFeed()
	if err != nil {
		t.Fatal(err)
	}
	s.Agents[0].Usage.Input = 105
	s.Agents[1].Usage.Output = 10
	restarted.addSnapshot(s)
	if restarted.Claude != 125 || restarted.Codex != 40 {
		t.Fatalf("growth after restart: %+v", restarted)
	}
	// A stale or truncated session total must not subtract or recount tokens.
	s.Agents[0].Usage.Input = 1
	restarted.addSnapshot(s)
	if restarted.Claude != 125 {
		t.Fatalf("regressed session total changed feed: %+v", restarted)
	}
}
