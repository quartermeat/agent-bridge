package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// voiceClaude answers each voice turn with one `claude -p` run. Claude Code owns
// the conversation history; agent-bridge stores only its session ID. In "read"
// access the run is read-only; in "work" access every tool beyond reading asks the
// voice approval MCP server, which relays to the daemon's spoken approval.
type voiceClaude struct {
	mu        sync.Mutex
	access    string
	mcpConfig string
	bin       string
	statePath string
	stateErr  error
	sessionID string
	loaded    bool
	timeout   time.Duration
}

const (
	voiceClaudeTools     = "Read,Grep,Glob"
	voiceClaudeDenied    = "Bash,Edit,Write,NotebookEdit,WebFetch,WebSearch"
	voiceClaudeMaxOutput = 256 << 10
	voiceAccessRead      = "read"
	voiceAccessWork      = "work"
	voiceWorkTimeout     = 30 * time.Minute
)

// voiceWorkInstructions replace voiceDeveloperInstructions in work access.
const voiceWorkInstructions = "You are operating the user's PC by voice. Spoken replies must be brief (one to three sentences). Actions that change anything are gated by spoken approval; describe what you will do before doing it."

// newVoiceClaude returns a read-only voice Claude backend.
func newVoiceClaude() *voiceClaude { return newVoiceClaudeAccess(voiceAccessRead, "") }

// newVoiceClaudeAccess returns a backend for access "read" or "work"; work routes
// permission prompts through `agent-bridge voice-approval-mcp --socket socket`.
func newVoiceClaudeAccess(access, socket string) *voiceClaude {
	bin := os.Getenv("AGENT_BRIDGE_CLAUDE_BIN")
	if bin == "" {
		bin = "claude"
	}
	c := &voiceClaude{bin: bin, access: voiceAccessRead, timeout: 3 * time.Minute}
	if access == voiceAccessWork {
		self, err := os.Executable()
		if err != nil {
			c.stateErr = err
			return c
		}
		c.access, c.timeout, c.mcpConfig = voiceAccessWork, voiceWorkTimeout, voiceApprovalMCPConfig(self, socket)
	}
	r, err := root()
	if err != nil {
		c.stateErr = err
		return c
	}
	c.statePath = filepath.Join(r, "voice", "claude-session-id")
	return c
}

// Close has nothing to release: each turn is a separate process.
func (c *voiceClaude) Close() error { return nil }

func (c *voiceClaude) Reply(ctx context.Context, transcript string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stateErr != nil {
		return "", c.stateErr
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.loadSession(); err != nil {
		return "", err
	}
	if c.sessionID != "" {
		reply, _, err := c.run(ctx, transcript, c.sessionID)
		if err == nil {
			return reply, nil
		}
		var failure voiceFailure
		if ctx.Err() != nil || (errors.As(err, &failure) && failure.code == "agent usage limit reached") {
			return "", err
		}
		// The saved session may be gone or unusable; start a fresh one once.
		c.sessionID = ""
	}
	reply, session, err := c.run(ctx, transcript, "")
	if err != nil {
		return "", err
	}
	if session != "" && validVoiceSessionID(session) {
		c.sessionID = session
		if err = saveVoiceStateID(c.statePath, ".claude-session-id-", session); err != nil {
			return "", voiceFailure{code: "save voice session failed"}
		}
	}
	return reply, nil
}

func (c *voiceClaude) loadSession() error {
	if c.loaded || c.statePath == "" {
		return nil
	}
	b, err := os.ReadFile(c.statePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return voiceFailure{code: "read voice session failed"}
	}
	c.loaded = true
	id := strings.TrimSpace(string(b))
	if id != "" && validVoiceSessionID(id) {
		c.sessionID = id
	}
	return nil
}

func validVoiceSessionID(id string) bool {
	if id == "" || len(id) > 128 || strings.HasPrefix(id, "-") {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func (c *voiceClaude) args(session string) []string {
	if c.access == voiceAccessWork {
		// No --tools restriction: the full tool set is available, but only reads are
		// pre-allowed; every other call reaches the spoken-approval prompt tool.
		args := []string{"-p", "--output-format", "json",
			"--allowed-tools=" + voiceClaudeTools,
			"--permission-mode", "manual",
			"--strict-mcp-config",
			"--mcp-config=" + c.mcpConfig,
			"--permission-prompt-tool", voiceApprovalTool,
			"--disallowed-tools=" + voiceApprovalTool,
			"--append-system-prompt", voiceWorkInstructions}
		if session != "" {
			args = append(args, "--resume", session)
		}
		return args
	}
	args := []string{"-p", "--output-format", "json",
		"--tools=" + voiceClaudeTools,
		"--allowed-tools=" + voiceClaudeTools,
		"--disallowed-tools=" + voiceClaudeDenied,
		"--permission-mode", "dontAsk",
		"--strict-mcp-config",
		"--append-system-prompt", voiceDeveloperInstructions}
	if session != "" {
		args = append(args, "--resume", session)
	}
	return args
}

// run executes one turn. The transcript travels on stdin so it never appears in
// argv, and stderr is discarded because it can echo user text.
func (c *voiceClaude) run(ctx context.Context, transcript, session string) (string, string, error) {
	cmd := exec.CommandContext(ctx, c.bin, c.args(session)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdin = strings.NewReader(transcript)
	cmd.Stderr = nil // null device
	out, err := limitedCommandOutput(cmd, voiceClaudeMaxOutput)
	if cmd.Process != nil {
		// Tool children can outlive the CLI; clear the whole group.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	var result struct {
		IsError   bool   `json:"is_error"`
		APIStatus int    `json:"api_error_status"`
		Subtype   string `json:"subtype"`
		Result    string `json:"result"`
		SessionID string `json:"session_id"`
	}
	parsed := json.Unmarshal(out, &result) == nil
	if parsed && result.IsError {
		if result.APIStatus == 429 {
			return "", "", voiceFailure{code: "agent usage limit reached"}
		}
		return "", "", claudeErrorFailure(result.Result)
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", "", voiceProcessFailure("agent process failed", ctx.Err())
		}
		var failure voiceFailure
		if errors.As(err, &failure) && failure.code == "transcription output too large" {
			return "", "", voiceFailure{code: "agent output too large"}
		}
		return "", "", voiceProcessFailure("agent process failed", err)
	}
	if !parsed {
		return "", "", voiceFailure{code: "invalid agent output"}
	}
	if strings.TrimSpace(result.Result) == "" {
		return "", "", voiceFailure{code: "agent returned no final message"}
	}
	return result.Result, result.SessionID, nil
}

// claudeErrorFailure maps an error result to a fixed label; the message itself is
// never returned because it may quote user text.
func claudeErrorFailure(message string) error {
	m := strings.ToLower(message)
	if strings.Contains(m, "usage limit") || strings.Contains(m, "limit reached") || strings.Contains(m, "rate limit") {
		return voiceFailure{code: "agent usage limit reached"}
	}
	return voiceFailure{code: "agent turn failed"}
}

// saveVoiceStateID atomically writes a private one-line ID file.
func saveVoiceStateID(path, pattern, id string) error {
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.WriteString(id + "\n")
	}
	if err == nil {
		err = tmp.Close()
	} else {
		_ = tmp.Close()
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
