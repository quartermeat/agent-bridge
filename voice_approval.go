package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

const (
	voiceApprovalServer   = "voice_approval"
	voiceApprovalTool     = "mcp__voice_approval__approve"
	voiceApprovalWindow   = 25 * time.Second
	voiceApprovalAttempts = 2
	voiceApprovalPrompt   = "Say yes or no."
	voiceApprovalReprompt = "Sorry, yes or no?"
	voiceApprovalMaxReq   = 1 << 20
	// voiceApprovalWait bounds how long the MCP server waits for the daemon,
	// including time queued behind another approval.
	voiceApprovalWait  = 10 * time.Minute
	voiceApprovalNoMsg = "voice approval unavailable"
)

// voiceApprovalRequest is both the permission-prompt tool input and the socket request.
type voiceApprovalRequest struct {
	ToolName  string          `json:"tool_name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
}

// voiceApprovalDecision is the socket response from the daemon.
type voiceApprovalDecision struct {
	Allow   bool   `json:"allow"`
	Message string `json:"message,omitempty"`
}

func voiceApprovalSocketPath() (string, error) {
	r, err := root()
	if err != nil {
		return "", err
	}
	return filepath.Join(r, "voice", "approval.sock"), nil
}

// voiceApprovalVerdict classifies a normalized spoken reply.
type voiceApprovalVerdict int

const (
	voiceVerdictUnclear voiceApprovalVerdict = iota // noise: listen again
	voiceVerdictApprove
	voiceVerdictDeny
)

// Approval vocabulary, matched against the whole normalized utterance.
var (
	voiceApprovePhrases = voicePhraseSet("yes", "yeah", "yep", "yes please", "sure", "ok", "okay", "approve", "approved", "confirm", "confirmed", "do it", "go ahead", "go for it", "yes approve", "yes do it")
	voiceDenyPhrases    = voicePhraseSet("no", "nope", "deny", "denied", "stop", "cancel", "don't", "do not")
)

func voicePhraseSet(phrases ...string) map[string]bool {
	set := make(map[string]bool, len(phrases))
	for _, p := range phrases {
		set[normalizeVoicePhrase(p)] = true
	}
	return set
}

func normalizeVoicePhrase(text string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		if r == '\'' || r == '’' {
			return -1 // "don't" and "don’t" both become "dont"
		}
		return ' '
	}, text)), " ")
}

// parseVoiceApproval matches the whole utterance (optionally after a leading wake
// phrase) against the approve and deny vocabularies. Noise and anything else,
// including longer sentences that merely contain "yes", are unclear.
func parseVoiceApproval(transcript, wake string) voiceApprovalVerdict {
	if prompt, ok := stripLeadingWakeWord(transcript, wake); ok && strings.TrimSpace(wake) != "" {
		transcript = prompt
	}
	if voiceFollowUpNoise(transcript) {
		return voiceVerdictUnclear
	}
	phrase := normalizeVoicePhrase(transcript)
	switch {
	case voiceApprovePhrases[phrase]:
		return voiceVerdictApprove
	case voiceDenyPhrases[phrase]:
		return voiceVerdictDeny
	}
	return voiceVerdictUnclear
}

func (v voiceApprovalVerdict) String() string {
	switch v {
	case voiceVerdictApprove:
		return "approve"
	case voiceVerdictDeny:
		return "deny"
	}
	return "unclear"
}

// voiceSpeakable keeps letters, digits, spaces and light punctuation so the spoken
// summary never reads out code, quoting or shell operators.
func voiceSpeakable(text string, maxWords int) string {
	text = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == ' ', r == ',', r == '-', r == '\'', r == '.':
			return r
		}
		return ' '
	}, text)
	words := strings.Fields(text)
	if maxWords > 0 && len(words) > maxWords {
		words = words[:maxWords]
	}
	return strings.Trim(strings.Join(words, " "), " .,-")
}

// voiceShellProgram returns the first program of a shell command (skipping
// VAR=value assignments) as a bare name, or "" when it is not a plain word.
func voiceShellProgram(cmd string) string {
	for _, w := range strings.Fields(cmd) {
		if strings.Contains(w, "=") && !strings.HasPrefix(w, "=") {
			continue
		}
		name := filepath.Base(w)
		for _, r := range name {
			if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.') {
				return ""
			}
		}
		if len(name) > 40 {
			return ""
		}
		return name
	}
	return ""
}

// voiceHumanizeName turns "TodoWrite" or "do_thing" into "todo write" / "do thing".
func voiceHumanizeName(name string) string {
	var b strings.Builder
	prevLower := false
	for _, r := range name {
		switch {
		case r == '_' || r == '-' || r == ' ':
			b.WriteRune(' ')
			prevLower = false
			continue
		case unicode.IsUpper(r):
			if prevLower {
				b.WriteRune(' ')
			}
			b.WriteRune(unicode.ToLower(r))
			prevLower = false
			continue
		}
		b.WriteRune(r)
		prevLower = unicode.IsLetter(r) || unicode.IsDigit(r)
	}
	return voiceSpeakable(b.String(), 8)
}

// voiceApprovalSummary returns a short plain-language spoken summary (never raw
// code) and the full transcript detail.
func voiceApprovalSummary(req voiceApprovalRequest) (spoken, detail string) {
	var in map[string]any
	_ = json.Unmarshal(req.Input, &in)
	str := func(k string) string {
		s, _ := in[k].(string)
		return s
	}
	switch req.ToolName {
	case "Bash":
		cmd := str("command")
		detail = "Bash: " + cmd
		if d := voiceSpeakable(str("description"), 20); d != "" {
			return d, detail
		}
		if prog := voiceSpeakable(voiceShellProgram(cmd), 1); prog != "" {
			return "run a shell command, " + prog, detail
		}
		return "run a shell command", detail
	case "Edit", "MultiEdit", "NotebookEdit":
		p := str("file_path")
		if p == "" {
			p = str("notebook_path")
		}
		return voiceFileSummary("edit", p), req.ToolName + ": " + p
	case "Write":
		return voiceFileSummary("write", str("file_path")), "Write: " + str("file_path")
	case "WebFetch":
		host := ""
		if u, err := url.Parse(str("url")); err == nil {
			host = u.Hostname()
		}
		if host = voiceSpeakable(host, 1); host == "" {
			return "fetch a web page", "WebFetch: " + str("url")
		}
		return "fetch " + host, "WebFetch: " + str("url")
	case "WebSearch":
		return "search the web for " + voiceSpeakable(str("query"), 12), "WebSearch: " + str("query")
	}
	raw := string(req.Input)
	if len(raw) > 2000 {
		raw = raw[:2000] + "…"
	}
	name := req.ToolName
	if parts := strings.SplitN(name, "__", 3); len(parts) == 3 && parts[0] == "mcp" {
		return "use " + voiceHumanizeName(parts[2]) + " from " + voiceHumanizeName(parts[1]), name + ": " + raw
	}
	return "use " + voiceHumanizeName(name), name + ": " + raw
}

func voiceFileSummary(verb, path string) string {
	name := voiceSpeakable(filepath.Base(path), 6)
	if path == "" || name == "" {
		return verb + " a file"
	}
	return verb + " " + name
}

// voiceApprover asks for spoken approval. It runs while the main loop is blocked
// inside Reply, so it may capture from the microphone directly.
type voiceApprover struct {
	mu     sync.Mutex
	cfg    voiceConfig
	d      voiceDeps
	window time.Duration
}

func (a *voiceApprover) decide(ctx context.Context, req voiceApprovalRequest) voiceApprovalDecision {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.d.audio != nil {
		a.d.audio.Lock()
		defer a.d.audio.Unlock()
	}
	spoken, detail := voiceApprovalSummary(req)
	voiceRecord(a.d, "system", "approval_request", detail)
	voiceLog(a.d, "voice approval requested tool=%s", voiceSafeToolName(req.ToolName))
	allow := a.listen(ctx, spoken)
	result, msg := "Denied.", "The user denied this action by voice."
	if allow {
		result, msg = "Approved.", ""
	}
	if a.d.speak != nil {
		_ = a.d.speak(ctx, result)
	}
	voiceRecord(a.d, "system", "approval_result", result+" "+detail)
	voiceLog(a.d, "voice approval result approved=%t", allow)
	return voiceApprovalDecision{Allow: allow, Message: msg}
}

// voiceApprovalSpeech is the full spoken approval prompt.
func voiceApprovalSpeech(spoken string) string {
	if spoken == "" {
		spoken = "an action"
	}
	return "Approval needed: " + spoken + ". " + voiceApprovalPrompt
}

// listen speaks the prompt, then listens. Capture starts only after the prompt
// (and tone) finish playing, so speech that began during playback is never heard.
// One unclear reply gets "Sorry, yes or no?"; a second unclear reply, silence,
// an error, or the end of the window denies.
func (a *voiceApprover) listen(ctx context.Context, spoken string) bool {
	if a.d.speak == nil || a.d.capture == nil || a.d.transcribe == nil {
		return false
	}
	window := a.window
	if window <= 0 {
		window = voiceApprovalWindow
	}
	ctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	prompt := voiceApprovalSpeech(spoken)
	for i := 0; i < voiceApprovalAttempts; i++ {
		if i > 0 {
			prompt = voiceApprovalReprompt
		}
		if err := a.d.speak(ctx, prompt); err != nil {
			voiceRecord(a.d, "system", "approval_verdict", "deny (prompt playback failed)")
			return false
		}
		if a.d.tone != nil {
			_ = a.d.tone(ctx)
		}
		text, err := a.hear(ctx)
		if err != nil {
			voiceRecord(a.d, "system", "approval_verdict", "deny (nothing heard in time)")
			return false
		}
		verdict := parseVoiceApproval(text, a.cfg.wakeWord)
		heard := strings.TrimSpace(text)
		if heard == "" {
			heard = "(silence)"
		}
		voiceRecord(a.d, "user", "approval_heard", heard)
		voiceRecord(a.d, "system", "approval_verdict", verdict.String())
		switch verdict {
		case voiceVerdictApprove:
			return true
		case voiceVerdictDeny:
			return false
		}
	}
	return false
}

func (a *voiceApprover) hear(ctx context.Context) (string, error) {
	pcm, err := captureVoiceUtterance(ctx, a.d, a.cfg.source, a.cfg.threshold, a.cfg.maxUtterance)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "agent-bridge-voice-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "approval.wav")
	if err = writeVoiceWAVLimit(path, pcm, a.cfg.maxUtterance); err != nil {
		return "", err
	}
	text, err := a.d.transcribe(ctx, path)
	var failure voiceFailure
	if errors.As(err, &failure) && failure.code == "empty transcription" {
		return "", nil
	}
	return text, err
}

// voiceSafeToolName keeps only identifier characters for the journal.
func voiceSafeToolName(name string) string {
	if len(name) > 64 {
		name = name[:64]
	}
	return strings.Map(func(r rune) rune {
		if r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') {
			return r
		}
		return '?'
	}, name)
}

// serveVoiceApprovals listens on a private unix socket and answers each request
// from a same-uid peer with decide. It returns a stop function.
func serveVoiceApprovals(path string, decide func(context.Context, voiceApprovalRequest) voiceApprovalDecision) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("approval socket path is not a socket")
		}
		_ = os.Remove(path)
	}
	old := syscall.Umask(0177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		ln.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				handleVoiceApprovalConn(ctx, conn, decide)
			}()
		}
	}()
	return func() {
		cancel()
		ln.Close()
		_ = os.Remove(path)
		wg.Wait()
	}, nil
}

func voicePeerUID(conn net.Conn) (uint32, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, errors.New("not a unix connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *syscall.Ucred
	var credErr error
	if err = raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, credErr
	}
	return cred.Uid, nil
}

func handleVoiceApprovalConn(ctx context.Context, conn net.Conn, decide func(context.Context, voiceApprovalRequest) voiceApprovalDecision) {
	deny := voiceApprovalDecision{Message: voiceApprovalNoMsg}
	uid, err := voicePeerUID(conn)
	if err != nil || uid != uint32(os.Getuid()) {
		_ = json.NewEncoder(conn).Encode(deny)
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReader(io.LimitReader(conn, voiceApprovalMaxReq)).ReadBytes('\n')
	var req voiceApprovalRequest
	if err != nil || json.Unmarshal(line, &req) != nil || req.ToolName == "" {
		_ = json.NewEncoder(conn).Encode(deny)
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	_ = json.NewEncoder(conn).Encode(decide(ctx, req))
}

// requestVoiceApproval asks the daemon over its socket; any failure denies.
func requestVoiceApproval(ctx context.Context, path string, req voiceApprovalRequest) voiceApprovalDecision {
	deny := voiceApprovalDecision{Message: voiceApprovalNoMsg}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return deny
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	b, err := json.Marshal(req)
	if err != nil {
		return deny
	}
	if _, err = conn.Write(append(b, '\n')); err != nil {
		return deny
	}
	var got voiceApprovalDecision
	if err = json.NewDecoder(io.LimitReader(conn, voiceApprovalMaxReq)).Decode(&got); err != nil {
		return deny
	}
	if !got.Allow && got.Message == "" {
		got.Message = "The user denied this action by voice."
	}
	return got
}

// voiceApprovalMCPConfig is the --mcp-config JSON that runs this binary as the
// permission-prompt tool server.
func voiceApprovalMCPConfig(self, socket string) string {
	b, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{voiceApprovalServer: map[string]any{
		"type": "stdio", "command": self, "args": []string{"voice-approval-mcp", "--socket", socket}}}})
	return string(b)
}

func voiceApprovalMCPCommand(args []string) error {
	f := flag.NewFlagSet("voice-approval-mcp", flag.ContinueOnError)
	socket := f.String("socket", "", "Daemon approval socket")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *socket == "" {
		path, err := voiceApprovalSocketPath()
		if err != nil {
			return err
		}
		*socket = path
	}
	return serveVoiceApprovalMCP(context.Background(), os.Stdin, os.Stdout, *socket)
}

type mcpMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// serveVoiceApprovalMCP is a minimal stdio MCP server exposing one "approve" tool
// for Claude Code's --permission-prompt-tool.
func serveVoiceApprovalMCP(ctx context.Context, in io.Reader, out io.Writer, socket string) error {
	enc := json.NewEncoder(out)
	var mu sync.Mutex
	send := func(v any) {
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(v)
	}
	reply := func(id json.RawMessage, result any) {
		send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	s := bufio.NewScanner(in)
	s.Buffer(make([]byte, 64<<10), 8<<20)
	var wg sync.WaitGroup
	defer wg.Wait()
	for s.Scan() {
		var m mcpMessage
		if err := json.Unmarshal(s.Bytes(), &m); err != nil {
			send(map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "parse error"}})
			continue
		}
		if len(m.ID) == 0 || string(m.ID) == "null" {
			continue // notifications need no answer
		}
		switch m.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(m.Params, &p)
			if p.ProtocolVersion == "" {
				p.ProtocolVersion = "2024-11-05"
			}
			reply(m.ID, map[string]any{"protocolVersion": p.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]string{"name": "agent-bridge-voice-approval", "version": "0.18.0"}})
		case "ping":
			reply(m.ID, map[string]any{})
		case "tools/list":
			reply(m.ID, map[string]any{"tools": []any{map[string]any{
				"name":        "approve",
				"description": "Asks the user for spoken approval of a tool call. Used only by Claude Code's permission prompt.",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
					"tool_name": map[string]any{"type": "string"}, "input": map[string]any{"type": "object"}, "tool_use_id": map[string]any{"type": "string"}},
					"required": []string{"tool_name", "input"}}}}})
		case "tools/call":
			var p struct {
				Name      string               `json:"name"`
				Arguments voiceApprovalRequest `json:"arguments"`
			}
			if json.Unmarshal(m.Params, &p) != nil || p.Name != "approve" {
				send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32602, "message": "unknown tool"}})
				continue
			}
			wg.Add(1)
			go func(id json.RawMessage, req voiceApprovalRequest) {
				defer wg.Done()
				reply(id, voiceApprovalToolResult(ctx, socket, req))
			}(m.ID, p.Arguments)
		default:
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
		}
	}
	return s.Err()
}

func voiceApprovalToolResult(ctx context.Context, socket string, req voiceApprovalRequest) map[string]any {
	ctx, cancel := context.WithTimeout(ctx, voiceApprovalWait)
	defer cancel()
	var body any = map[string]any{"behavior": "deny", "message": voiceApprovalNoMsg}
	if req.ToolName != "" {
		d := requestVoiceApproval(ctx, socket, req)
		if d.Allow {
			input := req.Input
			if len(input) == 0 || string(input) == "null" {
				input = json.RawMessage("{}")
			}
			body = map[string]any{"behavior": "allow", "updatedInput": input}
		} else {
			body = map[string]any{"behavior": "deny", "message": d.Message}
		}
	}
	text, _ := json.Marshal(body)
	return map[string]any{"content": []any{map[string]string{"type": "text", "text": string(text)}}}
}
