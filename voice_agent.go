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
	"strings"
	"sync"
	"syscall"
	"time"
)

// voiceAgent serializes turns on one app-server connection. Only its thread ID is
// stored by agent-bridge; Codex owns the conversation history.
type voiceAgent struct {
	mu        sync.Mutex
	dial      func() (io.ReadWriteCloser, error)
	conn      io.ReadWriteCloser
	events    chan agentEvent
	done      chan struct{}
	pending   []agentEvent
	nextID    int
	threadID  string
	statePath string
	stateErr  error
}
type agentEvent struct {
	msg agentMessage
	err error
}
type agentMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

const voiceDeveloperInstructions = "Respond for spoken playback. Keep answers concise and natural. For simple questions, answer directly without unnecessary tools."

type codexPipe struct {
	io.ReadCloser
	writer   io.WriteCloser
	cmd      *exec.Cmd
	once     sync.Once
	waitDone chan struct{}
	waitErr  error
}

func (p *codexPipe) Read(b []byte) (int, error) {
	n, err := p.ReadCloser.Read(b)
	if err != nil && p.waitDone != nil {
		select {
		case <-p.waitDone:
			if p.waitErr != nil {
				return n, voiceProcessFailure("app-server stopped", p.waitErr)
			}
			return n, voiceFailure{code: "app-server stopped"}
		case <-time.After(250 * time.Millisecond):
		}
	}
	return n, err
}
func (p *codexPipe) Write(b []byte) (int, error) { return p.writer.Write(b) }
func (p *codexPipe) Close() error {
	p.once.Do(func() {
		_ = p.writer.Close()
		_ = p.ReadCloser.Close()
		// The app-server can leave tool processes behind even after its parent exits.
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		if p.waitDone != nil {
			<-p.waitDone
		} else {
			_ = p.cmd.Wait()
		}
	})
	return nil
}
func dialCodex() (io.ReadWriteCloser, error) {
	bin := os.Getenv("AGENT_BRIDGE_CODEX_BIN")
	if bin == "" {
		bin = "codex"
	}
	cmd := exec.Command(bin, "app-server", "--listen", "stdio://")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = io.Discard // Never log server output, which can contain user text.
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		_ = in.Close()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		_ = in.Close()
		_ = out.Close()
		return nil, voiceProcessFailure("app-server start failed", err)
	}
	p := &codexPipe{ReadCloser: out, writer: in, cmd: cmd, waitDone: make(chan struct{})}
	go func() { p.waitErr = cmd.Wait(); close(p.waitDone) }()
	return p, nil
}
func newVoiceAgent() *voiceAgent {
	r, err := root()
	if err != nil {
		return &voiceAgent{dial: dialCodex, stateErr: err}
	}
	return &voiceAgent{dial: dialCodex, statePath: filepath.Join(r, "voice", "thread-id")}
}
func (a *voiceAgent) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closeLocked()
}
func (a *voiceAgent) closeLocked() error {
	if a.conn == nil {
		return nil
	}
	close(a.done)
	err := a.conn.Close()
	a.conn = nil
	a.events = nil
	a.done = nil
	a.pending = nil
	return err
}
func (a *voiceAgent) readLoop(conn io.Reader, events chan<- agentEvent, done <-chan struct{}) {
	post := func(e agentEvent) bool {
		select {
		case events <- e:
			return true
		case <-done:
			return false
		}
	}
	s := bufio.NewScanner(conn)
	s.Buffer(make([]byte, 64<<10), 4<<20)
	for s.Scan() {
		var msg agentMessage
		if err := json.Unmarshal(s.Bytes(), &msg); err != nil {
			post(agentEvent{err: voiceFailure{code: "invalid app-server message"}})
			return
		}
		if !post(agentEvent{msg: msg}) {
			return
		}
	}
	err := s.Err()
	if err == nil {
		err = io.EOF
	}
	post(agentEvent{err: err})
}
func (a *voiceAgent) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = a.conn.Write(b)
	return err
}
func (a *voiceAgent) receive(ctx context.Context) (agentMessage, error) {
	if len(a.pending) > 0 {
		e := a.pending[0]
		a.pending = a.pending[1:]
		return e.msg, e.err
	}
	select {
	case <-ctx.Done():
		return agentMessage{}, ctx.Err()
	case e := <-a.events:
		return e.msg, e.err
	}
}
func (a *voiceAgent) respondToRequest(m agentMessage) error {
	if len(m.ID) == 0 || m.Method == "" {
		return nil
	}
	var result any
	switch m.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		result = map[string]any{"decision": "decline"}
	case "item/permissions/requestApproval":
		result = map[string]any{"permissions": map[string]any{}}
	default:
		// Unknown interactive requests must fail closed instead of hanging the turn.
		if err := a.send(map[string]any{"id": json.RawMessage(m.ID), "error": map[string]any{"code": -32601, "message": "Voice client cannot approve this request"}}); err != nil {
			return err
		}
		return errors.New("voice client cannot handle agent request")
	}
	return a.send(map[string]any{"id": json.RawMessage(m.ID), "result": result})
}
func (a *voiceAgent) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	a.nextID++
	id := a.nextID
	if err := a.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		var m agentMessage
		var err error
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case event := <-a.events:
			m, err = event.msg, event.err
		}
		if err != nil {
			return nil, err
		}
		if len(m.ID) > 0 && m.Method != "" {
			if err := a.respondToRequest(m); err != nil {
				return nil, err
			}
			continue
		}
		if len(m.ID) > 0 {
			var got int
			if json.Unmarshal(m.ID, &got) == nil && got == id {
				if len(m.Error) > 0 && string(m.Error) != "null" {
					return nil, voiceFailure{code: "app-server " + method + " failed"}
				}
				return m.Result, nil
			}
		}
		// Keep lifecycle notifications that arrive before a request response.
		if m.Method == "turn/completed" || m.Method == "item/completed" {
			a.pending = append(a.pending, agentEvent{msg: m})
		}
	}
}
func (a *voiceAgent) connect(ctx context.Context) error {
	if a.conn != nil {
		return nil
	}
	conn, err := a.dial()
	if err != nil {
		return err
	}
	a.conn = conn
	a.events = make(chan agentEvent, 32)
	a.done = make(chan struct{})
	a.pending = nil
	go a.readLoop(conn, a.events, a.done)
	_, err = a.request(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "agent_bridge_voice", "title": "Agent Bridge Voice", "version": "0.15.0"}})
	if err != nil {
		a.closeLocked()
		return err
	}
	if err = a.send(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		a.closeLocked()
		return err
	}
	if a.threadID == "" && a.statePath != "" {
		b, readErr := os.ReadFile(a.statePath)
		if readErr == nil {
			a.threadID = strings.TrimSpace(string(b))
			if len(a.threadID) > 128 || strings.ContainsAny(a.threadID, "\r\n \t") {
				a.closeLocked()
				return errors.New("invalid saved voice thread")
			}
		}
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			a.closeLocked()
			return readErr
		}
	}
	if a.threadID != "" {
		_, err = a.request(ctx, "thread/resume", map[string]any{"threadId": a.threadID, "excludeTurns": true, "developerInstructions": voiceDeveloperInstructions})
	} else {
		cwd, _ := os.Getwd()
		var raw json.RawMessage
		raw, err = a.request(ctx, "thread/start", map[string]any{"cwd": cwd, "approvalPolicy": "never", "sandbox": "read-only", "serviceName": "agent_bridge_voice", "developerInstructions": voiceDeveloperInstructions})
		if err == nil {
			var response struct {
				Thread struct {
					ID string `json:"id"`
				} `json:"thread"`
			}
			if json.Unmarshal(raw, &response) != nil || response.Thread.ID == "" {
				err = errors.New("missing voice thread ID")
			} else {
				a.threadID = response.Thread.ID
				err = a.saveThreadID()
			}
		}
	}
	if err != nil {
		a.closeLocked()
		return err
	}
	return nil
}
func (a *voiceAgent) saveThreadID() error {
	if a.statePath == "" {
		return nil
	}
	dir := filepath.Dir(a.statePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".thread-id-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.WriteString(a.threadID + "\n")
	}
	if err == nil {
		err = tmp.Close()
	} else {
		_ = tmp.Close()
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), a.statePath)
}
func (a *voiceAgent) Reply(ctx context.Context, transcript string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stateErr != nil {
		return "", a.stateErr
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if err := a.connect(ctx); err != nil {
		return "", err
	}
	raw, err := a.request(ctx, "turn/start", map[string]any{"threadId": a.threadID, "input": []any{map[string]string{"type": "text", "text": transcript}}, "approvalPolicy": "never", "sandboxPolicy": map[string]string{"type": "readOnly"}})
	if err != nil {
		a.closeLocked()
		return "", err
	}
	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(raw, &started) != nil || started.Turn.ID == "" {
		a.closeLocked()
		return "", errors.New("invalid turn response")
	}
	var final string
	for {
		m, err := a.receive(ctx)
		if err != nil {
			a.closeLocked()
			return "", err
		}
		if len(m.ID) > 0 && m.Method != "" {
			if err = a.respondToRequest(m); err != nil {
				a.closeLocked()
				return "", err
			}
			continue
		}
		switch m.Method {
		case "item/completed":
			var p struct {
				ThreadID string `json:"threadId"`
				TurnID   string `json:"turnId"`
				Item     struct {
					Type  string `json:"type"`
					Phase string `json:"phase"`
					Text  string `json:"text"`
				} `json:"item"`
			}
			if json.Unmarshal(m.Params, &p) == nil && p.ThreadID == a.threadID && p.TurnID == started.Turn.ID && p.Item.Type == "agentMessage" && (p.Item.Phase == "final_answer" || p.Item.Phase == "") {
				final = p.Item.Text
			}
		case "turn/completed":
			var p struct {
				ThreadID string `json:"threadId"`
				Turn     struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				} `json:"turn"`
			}
			if json.Unmarshal(m.Params, &p) == nil && p.ThreadID == a.threadID && p.Turn.ID == started.Turn.ID {
				if p.Turn.Status != "completed" {
					return "", voiceFailure{code: "agent turn failed"}
				}
				if strings.TrimSpace(final) == "" {
					return "", voiceFailure{code: "agent returned no final message"}
				}
				return final, nil
			}
		}
	}
}
