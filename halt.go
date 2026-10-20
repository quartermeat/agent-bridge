package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"
)

// Halt records a confirmed resource limit without storing provider error text,
// which can contain request details.
type Halt struct {
	Kind       string    `json:"kind"`
	Scope      string    `json:"scope"`
	Code       string    `json:"code"`
	Source     string    `json:"source"`
	DetectedAt time.Time `json:"detected_at"`
}

func classifyUsageError(code, message, source string, at time.Time) *Halt {
	c := strings.ToLower(strings.TrimSpace(code))
	m := strings.ToLower(message)
	kind := ""
	scope := "unknown"
	switch c {
	case "context_length_exceeded", "context_window_exceeded", "max_tokens_exceeded", "token_limit_exceeded":
		kind, scope = "context_limit", "session"
	case "session_budget_exceeded":
		kind, scope = "usage_limit", "session"
	case "usage_limit_exceeded":
		kind = "usage_limit"
	case "organization_usage_limit_exceeded", "credit_balance_exhausted", "credits_exhausted", "insufficient_credits", "insufficient_quota", "billing_hard_limit_reached", "organization_spend_limit_exceeded", "project_spend_limit_exceeded":
		kind, scope = "usage_limit", "account"
	case "rate_limit_exceeded", "tokens_per_minute_exceeded":
		kind, scope = "rate_limit", "temporary"
	}
	if kind == "" {
		switch {
		case strings.Contains(m, "context window") && (strings.Contains(m, "exceed") || strings.Contains(m, "full")),
			strings.Contains(m, "maximum context length"),
			strings.Contains(m, "context length exceeded"):
			kind, scope, c = "context_limit", "session", "context_length_exceeded"
		case strings.Contains(m, "usage limit") && (strings.Contains(m, "reach") || strings.Contains(m, "exceed") || strings.Contains(m, "hit")),
			strings.Contains(m, "out of tokens"),
			strings.Contains(m, "credit balance exhausted"),
			strings.Contains(m, "quota exceeded"):
			kind, c = "usage_limit", "usage_limit_exceeded"
		case strings.Contains(m, "rate limit exceeded") || strings.Contains(m, "too many tokens per minute"):
			kind, scope, c = "rate_limit", "temporary", "rate_limit_exceeded"
		}
	}
	if kind == "" {
		return nil
	}
	return &Halt{Kind: kind, Scope: scope, Code: c, Source: source, DetectedAt: at}
}

func detectJobHalt(eventsPath, stderrPath string, at time.Time, exitCode int) *Halt {
	var halt *Halt
	_ = scanJSONLines(eventsPath, func(line []byte) {
		var event struct {
			Type    string          `json:"type"`
			Error   json.RawMessage `json:"error"`
			Message string          `json:"message"`
		}
		if json.Unmarshal(line, &event) != nil {
			return
		}
		if event.Type == "turn.completed" {
			halt = nil
			return
		}
		if event.Type != "turn.failed" && event.Type != "error" {
			return
		}
		code, message := errorFields(event.Error)
		if message == "" {
			message = event.Message
		}
		if found := classifyUsageError(code, message, "codex_event", at); found != nil {
			halt = found
		}
	})
	if halt != nil || exitCode == 0 {
		return halt
	}
	// The CLI can put a fatal error on stderr without a structured turn event.
	// Only inspect its final 64 KiB, never prompts or tool output.
	f, err := os.Open(stderrPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	if info.Size() > 64<<10 {
		if _, err := f.Seek(info.Size()-(64<<10), io.SeekStart); err != nil {
			return nil
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(b), "\n") {
		var row struct {
			Error   json.RawMessage `json:"error"`
			Code    string          `json:"code"`
			Message string          `json:"message"`
		}
		if json.Unmarshal([]byte(line), &row) == nil {
			code, message := errorFields(row.Error)
			if code == "" {
				code, message = row.Code, row.Message
			}
			if found := classifyUsageError(code, message, "codex_stderr", at); found != nil {
				return found
			}
		}
		if found := classifyUsageError("", line, "codex_stderr", at); found != nil {
			return found
		}
	}
	return nil
}

func errorFields(raw json.RawMessage) (string, string) {
	if len(raw) == 0 {
		return "", ""
	}
	var object struct {
		Code      string `json:"code"`
		Type      string `json:"type"`
		Message   string `json:"message"`
		Formatted string `json:"formatted"`
	}
	if json.Unmarshal(raw, &object) == nil {
		message := object.Message
		if object.Formatted != "" {
			message += " " + object.Formatted
		}
		if object.Code != "" {
			return object.Code, message
		}
		return object.Type, message
	}
	var message string
	if json.Unmarshal(raw, &message) == nil {
		return "", message
	}
	return "", ""
}

func haltLabel(h *Halt) string {
	if h == nil {
		return ""
	}
	switch h.Kind {
	case "context_limit":
		return "CONTEXT WINDOW FULL"
	case "usage_limit":
		return "USAGE LIMIT REACHED"
	case "rate_limit":
		return "RATE LIMITED"
	default:
		return "HALTED"
	}
}
