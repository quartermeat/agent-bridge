package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestVoiceTranscriptAppendRotatePerms(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "voice")
	path := filepath.Join(dir, "transcript.jsonl")
	tr := newVoiceTranscript(path)
	tr.now = func() time.Time { return time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC) }
	if err := tr.Append("user", "prompt", "hello"); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0700 {
		t.Fatalf("dir mode %v", info.Mode())
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0600 {
		t.Fatalf("file mode %v", info.Mode())
	}
	b, _ := os.ReadFile(path)
	var e voiceTranscriptEntry
	if err := json.Unmarshal(bytes.TrimSpace(b), &e); err != nil || e != (voiceTranscriptEntry{TS: "2026-10-25T12:00:00Z", Role: "user", Kind: "prompt", Text: "hello"}) {
		t.Fatalf("entry %+v %v", e, err)
	}
	tr.limit = int64(len(b)) + 10
	if err := tr.Append("agent", "reply", "second line pushes over"); err != nil {
		t.Fatal(err)
	}
	old, _ := os.ReadFile(path + ".1")
	cur, _ := os.ReadFile(path)
	if !bytes.Equal(old, b) || !strings.Contains(string(cur), "second line") || strings.Contains(string(cur), "hello") {
		t.Fatalf("rotation old=%q cur=%q", old, cur)
	}
	// A further rotation replaces the single backup.
	if err := tr.Append("agent", "reply", "third"); err != nil {
		t.Fatal(err)
	}
	if old, _ = os.ReadFile(path + ".1"); !strings.Contains(string(old), "second line") {
		t.Fatalf("backup %q", old)
	}
}

func TestVoiceTurnRecordsTranscript(t *testing.T) {
	var got []string
	d := voiceDeps{
		reply:  func(context.Context, string) (string, error) { return "Done.", nil },
		speak:  func(context.Context, string) error { return nil },
		record: func(role, kind, text string) error { got = append(got, role+"/"+kind+"/"+text); return nil },
	}
	cfg := voiceConfig{wakeWord: "hey agent"}
	for _, s := range []string{"background chatter", "hey agent open the log"} {
		if err := handleVoiceText(context.Background(), cfg, d, s); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"user/ignored/background chatter", "user/prompt/open the log", "system/status/Wake phrase heard; listening.", "system/status/Agent turn started.", "agent/reply/Done."}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("recorded %q", got)
	}
}

func TestFormatVoiceLogEntry(t *testing.T) {
	ts := time.Date(2026, 10, 25, 9, 7, 0, 0, time.Local).Format(time.RFC3339)
	cases := map[voiceTranscriptEntry]string{
		{TS: ts, Role: "user", Kind: "prompt", Text: "hi"}:                        "09:07 you: hi",
		{TS: ts, Role: "user", Kind: "ignored", Text: "tv noise"}:                 "09:07 (ignored) you: tv noise",
		{TS: ts, Role: "user", Kind: "approval_heard", Text: "yeah"}:              "09:07 you (approval): yeah",
		{TS: ts, Role: "system", Kind: "approval_verdict", Text: "approve"}:       "09:07 ⚠ approval: approve",
		{TS: ts, Role: "agent", Kind: "reply", Text: "yo\x1b[2J"}:                 "09:07 agent: yo[2J",
		{TS: ts, Role: "system", Kind: "approval_request", Text: "Bash: touch x"}: "09:07 ⚠ approval: Bash: touch x",
		{TS: ts, Role: "system", Kind: "status", Text: "Agent turn started."}:     "09:07 ·: Agent turn started.",
		{TS: "bad", Role: "system", Kind: "error", Text: "agent reply failed"}:    "--:-- ! error: agent reply failed",
		{TS: ts, Role: "agent", Kind: "reply", Text: "line one\nline two"}:        "09:07 agent: line one\n      line two",
	}
	for e, want := range cases {
		if got := formatVoiceLogEntry(e); got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestVoiceLogTailAndFollowAcrossRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "voice", "transcript.jsonl")
	tr := newVoiceTranscript(path)
	for i := 0; i < 5; i++ {
		if err := tr.Append("user", "prompt", "old"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := runVoiceLog(context.Background(), path, &out, 2, false, false, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); strings.Count(s, "\n") != 2 || !strings.Contains(s, "you: oldd") || !strings.Contains(s, "you: olde") {
		t.Fatalf("tail %q", s)
	}
	out.Reset()
	if err := runVoiceLog(context.Background(), path, &out, 1, false, true, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), `{"ts":`) {
		t.Fatalf("json %q", out.String())
	}

	ctx, cancel := context.WithCancel(context.Background())
	buf := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- runVoiceLog(ctx, path, buf, 0, true, false, 5*time.Millisecond) }()
	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for !strings.Contains(buf.String(), want) {
			if time.Now().After(deadline) {
				t.Fatalf("missing %q in %q", want, buf.String())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	time.Sleep(30 * time.Millisecond)
	_ = tr.Append("agent", "reply", "live one")
	waitFor("agent: live one")
	tr.limit = 1 // force rotation on the next append
	_ = tr.Append("agent", "reply", "after rotation")
	waitFor("agent: after rotation")
	_ = tr.Append("system", "approval_result", "Denied. Bash: rm x")
	waitFor("⚠ approval: Denied. Bash: rm x")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "old") {
		t.Fatalf("follow with --lines 0 printed history: %q", buf.String())
	}
}

func TestVoiceLogMissingFile(t *testing.T) {
	var out bytes.Buffer
	if err := runVoiceLog(context.Background(), filepath.Join(t.TempDir(), "none.jsonl"), &out, 50, false, false, time.Millisecond); err != nil || !strings.Contains(out.String(), "No voice transcript yet") {
		t.Fatalf("%q %v", out.String(), err)
	}
}
