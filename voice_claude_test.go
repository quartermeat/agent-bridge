package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClaude installs a stand-in CLI that logs its argv and stdin and answers
// according to FAKE_CLAUDE_MODE.
func fakeClaude(t *testing.T, mode string) (logDir, stateDir string) {
	t.Helper()
	dir := t.TempDir()
	logDir = filepath.Join(dir, "log")
	stateDir = filepath.Join(dir, "state")
	if err := os.MkdirAll(logDir, 0700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "claude")
	body := `#!/bin/sh
n=$(ls "$FAKE_CLAUDE_LOG" | grep -c '^args' )
n=$((n+1))
printf '%s\n' "$@" > "$FAKE_CLAUDE_LOG/args$n"
cat > "$FAKE_CLAUDE_LOG/stdin$n"
case "$FAKE_CLAUDE_MODE" in
usage) echo '{"type":"result","subtype":"success","is_error":true,"result":"Claude AI usage limit reached for secret","session_id":"s"}'; exit 1;;
error) echo '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"boom secret","session_id":"s"}'; exit 0;;
sleep) sleep 30;;
esac
for a in "$@"; do
	if [ "$prev" = "--resume" ] && [ "$a" = "stale" ]; then echo 'No conversation found' >&2; exit 1; fi
	prev=$a
done
echo '{"type":"result","subtype":"success","is_error":false,"result":"spoken reply","session_id":"sess-1"}'
`
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_BRIDGE_CLAUDE_BIN", script)
	t.Setenv("AGENT_BRIDGE_STATE_DIR", stateDir)
	t.Setenv("FAKE_CLAUDE_LOG", logDir)
	t.Setenv("FAKE_CLAUDE_MODE", mode)
	return logDir, stateDir
}

func readLog(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestVoiceClaudeSessionAndStdin(t *testing.T) {
	logDir, stateDir := fakeClaude(t, "ok")
	c := newVoiceClaude()
	for i := 0; i < 2; i++ {
		got, err := c.Reply(context.Background(), "private transcript")
		if err != nil || got != "spoken reply" {
			t.Fatalf("reply %q %v", got, err)
		}
	}
	statePath := filepath.Join(stateDir, "voice", "claude-session-id")
	b, err := os.ReadFile(statePath)
	if err != nil || string(b) != "sess-1\n" {
		t.Fatalf("state %q %v", b, err)
	}
	if info, _ := os.Stat(statePath); info.Mode().Perm() != 0600 {
		t.Fatalf("state mode %v", info.Mode())
	}
	first, second := readLog(t, logDir, "args1"), readLog(t, logDir, "args2")
	if strings.Contains(first, "--resume") || !strings.Contains(second, "--resume\nsess-1\n") {
		t.Fatalf("resume args: %q / %q", first, second)
	}
	for _, want := range []string{"-p\n", "--output-format\njson\n", "--tools=Read,Grep,Glob\n", "--permission-mode\ndontAsk\n", "--append-system-prompt\n" + voiceDeveloperInstructions + "\n"} {
		if !strings.Contains(first, want) {
			t.Fatalf("missing %q in %q", want, first)
		}
	}
	if strings.Contains(first, "private transcript") {
		t.Fatal("transcript leaked into argv")
	}
	if got := readLog(t, logDir, "stdin1"); got != "private transcript" {
		t.Fatalf("stdin %q", got)
	}
	// A new instance resumes from the saved session.
	if _, err := newVoiceClaude().Reply(context.Background(), "more"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readLog(t, logDir, "args3"), "--resume\nsess-1\n") {
		t.Fatal("new instance did not resume")
	}
}

func TestVoiceClaudeStaleSessionStartsFresh(t *testing.T) {
	logDir, stateDir := fakeClaude(t, "ok")
	statePath := filepath.Join(stateDir, "voice", "claude-session-id")
	if err := saveVoiceStateID(statePath, ".x-", "stale"); err != nil {
		t.Fatal(err)
	}
	got, err := newVoiceClaude().Reply(context.Background(), "hello")
	if err != nil || got != "spoken reply" {
		t.Fatalf("reply %q %v", got, err)
	}
	if !strings.Contains(readLog(t, logDir, "args1"), "--resume\nstale\n") || strings.Contains(readLog(t, logDir, "args2"), "--resume") {
		t.Fatal("expected resume attempt then fresh session")
	}
	if b, _ := os.ReadFile(statePath); string(b) != "sess-1\n" {
		t.Fatalf("state %q", b)
	}
}

func TestVoiceClaudeErrorsUseFixedLabels(t *testing.T) {
	for mode, want := range map[string]string{"usage": "agent usage limit reached", "error": "agent turn failed"} {
		t.Run(mode, func(t *testing.T) {
			fakeClaude(t, mode)
			_, err := newVoiceClaude().Reply(context.Background(), "secret words")
			var failure voiceFailure
			if !errors.As(err, &failure) || failure.code != want || strings.Contains(err.Error(), "secret") {
				t.Fatalf("err %v", err)
			}
		})
	}
}

func TestVoiceClaudeTimeout(t *testing.T) {
	fakeClaude(t, "sleep")
	c := newVoiceClaude()
	c.timeout = 200 * time.Millisecond
	start := time.Now()
	_, err := c.Reply(context.Background(), "x")
	var failure voiceFailure
	if !errors.As(err, &failure) || failure.status != "timeout" || time.Since(start) > 5*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}

func TestCodexTurnUsageLimit(t *testing.T) {
	for name, info := range map[string]any{"string": "usageLimitExceeded", "object": map[string]any{"usageLimitExceeded": map[string]any{}}, "other": "internalServerError"} {
		t.Run(name, func(t *testing.T) {
			a := &voiceAgent{dial: fakeAgent(t, func(m map[string]json.RawMessage, send func(any)) {
				switch method(m) {
				case "initialize":
					send(response(m, map[string]any{}))
				case "thread/start":
					send(response(m, map[string]any{"thread": map[string]string{"id": "t"}}))
				case "turn/start":
					send(response(m, map[string]any{"turn": map[string]string{"id": "u"}}))
					send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "t", "turn": map[string]any{"id": "u", "status": "failed", "error": map[string]any{"message": "secret server text", "codexErrorInfo": info}}}})
				}
			})}
			_, err := a.Reply(context.Background(), "hi")
			_ = a.Close()
			want := "agent usage limit reached"
			if name == "other" {
				want = "agent turn failed"
			}
			if err == nil || err.Error() != want {
				t.Fatalf("err %v", err)
			}
		})
	}
}
