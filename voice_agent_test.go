package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type fakeAgentPipe struct {
	reader *io.PipeReader
	writer *io.PipeWriter
	in     *io.PipeWriter
	out    *io.PipeReader
}

func (p *fakeAgentPipe) Read(b []byte) (int, error)  { return p.reader.Read(b) }
func (p *fakeAgentPipe) Write(b []byte) (int, error) { return p.in.Write(b) }
func (p *fakeAgentPipe) Close() error {
	_ = p.reader.Close()
	_ = p.writer.Close()
	_ = p.in.Close()
	return p.out.Close()
}
func fakeAgent(t *testing.T, handle func(map[string]json.RawMessage, func(any))) func() (io.ReadWriteCloser, error) {
	t.Helper()
	return func() (io.ReadWriteCloser, error) {
		serverRead, clientWrite := io.Pipe()
		clientRead, serverWrite := io.Pipe()
		p := &fakeAgentPipe{reader: clientRead, writer: serverWrite, in: clientWrite, out: serverRead}
		go func() {
			defer serverRead.Close()
			defer serverWrite.Close()
			s := bufio.NewScanner(serverRead)
			for s.Scan() {
				var msg map[string]json.RawMessage
				if json.Unmarshal(s.Bytes(), &msg) != nil {
					return
				}
				send := func(v any) { b, _ := json.Marshal(v); _, _ = serverWrite.Write(append(b, '\n')) }
				handle(msg, send)
			}
		}()
		return p, nil
	}
}
func method(m map[string]json.RawMessage) string {
	var s string
	_ = json.Unmarshal(m["method"], &s)
	return s
}
func response(m map[string]json.RawMessage, result any) any {
	return map[string]any{"id": json.RawMessage(m["id"]), "result": result}
}
func TestVoiceAgentThreadReuseAndResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "voice", "thread-id")
	starts, resumes, turns := 0, 0, 0
	handler := func(m map[string]json.RawMessage, send func(any)) {
		switch method(m) {
		case "initialize":
			send(response(m, map[string]any{}))
		case "thread/start":
			starts++
			send(response(m, map[string]any{"thread": map[string]string{"id": "thread-1"}}))
		case "thread/resume":
			resumes++
			send(response(m, map[string]any{"thread": map[string]string{"id": "thread-1"}}))
		case "turn/start":
			turns++
			var params struct {
				ThreadID string `json:"threadId"`
				Input    []struct {
					Text string `json:"text"`
				} `json:"input"`
				SandboxPolicy struct {
					Type string `json:"type"`
				} `json:"sandboxPolicy"`
			}
			_ = json.Unmarshal(m["params"], &params)
			if params.ThreadID != "thread-1" || params.SandboxPolicy.Type != "readOnly" || len(params.Input) != 1 {
				t.Errorf("turn settings: %+v", params)
			}
			id := "turn-" + string(rune('0'+turns))
			send(response(m, map[string]any{"turn": map[string]string{"id": id}}))
			send(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "thread-1", "turnId": id, "item": map[string]string{"type": "agentMessage", "phase": "commentary", "text": "ignore"}}})
			send(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "thread-1", "turnId": id, "item": map[string]string{"type": "agentMessage", "phase": "final_answer", "text": "reply"}}})
			send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread-1", "turn": map[string]string{"id": id, "status": "completed"}}})
		}
	}
	a := &voiceAgent{dial: fakeAgent(t, handler), statePath: path}
	for i := 0; i < 2; i++ {
		got, err := a.Reply(context.Background(), "private transcript")
		if err != nil || got != "reply" {
			t.Fatalf("reply %q %v", got, err)
		}
	}
	_ = a.Close()
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "thread-1\n" {
		t.Fatalf("state %q %v", b, err)
	}
	a = &voiceAgent{dial: fakeAgent(t, handler), statePath: path}
	got, err := a.Reply(context.Background(), "more")
	if err != nil || got != "reply" {
		t.Fatalf("resume %q %v", got, err)
	}
	_ = a.Close()
	if starts != 1 || resumes != 1 || turns != 3 {
		t.Fatalf("starts=%d resumes=%d turns=%d", starts, resumes, turns)
	}
}
func TestVoiceAgentDeniesApprovalAndFailure(t *testing.T) {
	denied := false
	a := &voiceAgent{dial: fakeAgent(t, func(m map[string]json.RawMessage, send func(any)) {
		switch method(m) {
		case "initialize":
			send(response(m, map[string]any{}))
		case "thread/start":
			send(response(m, map[string]any{"thread": map[string]string{"id": "t"}}))
		case "turn/start":
			send(response(m, map[string]any{"turn": map[string]string{"id": "u"}}))
			send(map[string]any{"id": 77, "method": "item/commandExecution/requestApproval", "params": map[string]any{"command": "danger"}})
		default:
			if string(m["id"]) == "77" {
				var result struct {
					Decision string `json:"decision"`
				}
				_ = json.Unmarshal(m["result"], &result)
				denied = result.Decision == "decline"
				send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "t", "turn": map[string]string{"id": "u", "status": "failed"}}})
			}
		}
	})}
	_, err := a.Reply(context.Background(), "secret request")
	_ = a.Close()
	if err == nil || !denied || strings.Contains(err.Error(), "secret") {
		t.Fatalf("denied=%v err=%v", denied, err)
	}
}
func TestVoiceAgentCancellationAndTimeout(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel bool
	}{{"cancel", true}, {"timeout", false}} {
		t.Run(tc.name, func(t *testing.T) {
			a := &voiceAgent{dial: fakeAgent(t, func(m map[string]json.RawMessage, send func(any)) {
				switch method(m) {
				case "initialize":
					send(response(m, map[string]any{}))
				case "thread/start":
					send(response(m, map[string]any{"thread": map[string]string{"id": "t"}}))
				case "turn/start":
					send(response(m, map[string]any{"turn": map[string]string{"id": "u"}}))
				}
			})}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			if tc.cancel {
				cancel()
			}
			defer cancel()
			_, err := a.Reply(ctx, "secret")
			_ = a.Close()
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
		})
	}
}

func TestCodexPipeClosesProcessGroup(t *testing.T) {
	// A shell stands in for the app-server and leaves a tool child running.
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cmd := exec.Command("sh", "-c", `sleep 60 & echo $! > "$VOICE_TEST_CHILD_PID"; wait`)
	cmd.Env = append(os.Environ(), "VOICE_TEST_CHILD_PID="+pidFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &codexPipe{ReadCloser: out, writer: in, cmd: cmd}
	t.Cleanup(func() { _ = p.Close() })
	var childPID int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b, readErr := os.ReadFile(pidFile)
		if readErr == nil {
			childPID, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID <= 0 {
		t.Fatal("child did not start")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b, readErr := os.ReadFile(filepath.Join("/proc", strconv.Itoa(childPID), "stat"))
		if os.IsNotExist(readErr) || (readErr == nil && strings.Contains(string(b), ") Z ")) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child %d survived close", childPID)
}
