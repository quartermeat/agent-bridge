package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Replaced by a fake transport in tests; requests still use the real HTTP path.
var grokHTTPClient = &http.Client{}

func ask(args []string) error {
	f := flag.NewFlagSet("ask", flag.ContinueOnError)
	model := f.String("model", "", "xAI model ID")
	system := f.String("system", "", "System instructions")
	name := f.String("name", "", "Short job label")
	promptFile := f.String("prompt-file", "", "Prompt file; default: stdin")
	listModels := f.Bool("list-models", false, "List available xAI model IDs")
	spawnedBy := f.String("spawned-by", "", "Agent ID from snapshot; default: detect invoking agent")
	timeout := f.Duration("timeout", 120*time.Second, "Request timeout")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("pass prompt on stdin or with --prompt-file")
	}
	if *timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	if !*listModels && *model == "" {
		*model = os.Getenv("AGENT_BRIDGE_GROK_MODEL")
	}
	if !*listModels && strings.TrimSpace(*model) == "" {
		return errors.New("no Grok model selected; run agent-bridge ask --list-models and pass --model")
	}
	key, err := grokAPIKey()
	if err != nil {
		return err
	}
	base := strings.TrimRight(os.Getenv("AGENT_BRIDGE_XAI_BASE_URL"), "/")
	if base == "" {
		base = "https://api.x.ai/v1"
	}
	if *listModels {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		return grokListModels(ctx, base, key)
	}
	var prompt []byte
	if *promptFile != "" {
		prompt, err = os.ReadFile(*promptFile)
	} else {
		prompt, err = io.ReadAll(io.LimitReader(os.Stdin, 1<<20+1))
	}
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(prompt)) == 0 || len(prompt) > 1<<20 {
		return errors.New("prompt must contain non-whitespace text and be at most 1 MiB")
	}
	spawner, err := resolveSpawner(*spawnedBy)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	r, err := root()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(r, "jobs"), 0700); err != nil {
		return err
	}
	id := fmt.Sprintf("%s-%d", time.Now().UTC().Format("20060102T150405"), os.Getpid())
	dir, err := jobDir(id)
	if err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	j := Job{ID: id, Name: *name, Provider: "grok", SpawnedBy: spawner, Cwd: cwd, Sandbox: "none", Launcher: "inline", Status: "running", CreatedAt: time.Now().UTC(), EventsPath: filepath.Join(dir, "events.jsonl"), StderrPath: filepath.Join(dir, "stderr.log"), ResultPath: filepath.Join(dir, "result.txt")}
	if err := writeJob(j); err != nil {
		return err
	}
	events, err := os.OpenFile(j.EventsPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return finishError(&j, err)
	}
	defer events.Close()
	writeEvent := func(v any) error {
		return json.NewEncoder(events).Encode(v)
	}
	if err := writeEvent(map[string]any{"type": "request_started", "model": *model}); err != nil {
		return finishError(&j, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	result, usage, status, code, halt, err := grokCompletion(ctx, base, key, *model, *system, string(prompt))
	if err != nil {
		if eventErr := writeEvent(map[string]any{"type": "error", "http_status": status, "code": code}); eventErr != nil {
			return finishError(&j, eventErr)
		}
		j.Error = err.Error()
		j.Status = "failed"
		j.Halt = halt
		if halt != nil {
			j.Status = "halted"
		}
		exit := 1
		j.ExitCode = &exit
		if writeErr := writeJob(j); writeErr != nil {
			return writeErr
		}
		return err
	}
	if err := writeEvent(map[string]any{"type": "response_received", "http_status": status, "usage": usage}); err != nil {
		return finishError(&j, err)
	}
	if err := os.WriteFile(j.ResultPath, []byte(result), 0600); err != nil {
		return finishError(&j, err)
	}
	j.Usage = &usage
	if _, err := fmt.Fprintln(os.Stdout, result); err != nil {
		return finishError(&j, err)
	}
	j.Status = "completed"
	exit := 0
	j.ExitCode = &exit
	return writeJob(j)
}

func grokAPIKey() (string, error) {
	if key := strings.TrimSpace(os.Getenv("XAI_API_KEY")); key != "" {
		return key, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, ".config", "agent-bridge", "xai-api-key")
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", errors.New("xAI API key missing: set XAI_API_KEY or create ~/.config/agent-bridge/xai-api-key; see https://console.x.ai")
	}
	if err != nil {
		return "", fmt.Errorf("cannot inspect xAI API key file: %w", err)
	}
	if info.Mode().Perm()&0077 != 0 {
		return "", errors.New("xAI API key file allows group or other access; chmod 600 ~/.config/agent-bridge/xai-api-key")
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("xAI API key file must be a regular file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("cannot read xAI API key file: %w", err)
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return "", errors.New("xAI API key missing: set XAI_API_KEY or create ~/.config/agent-bridge/xai-api-key; see https://console.x.ai")
	}
	return key, nil
}

func grokRequest(ctx context.Context, method, url, key string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, errors.New("invalid xAI API URL")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := grokHTTPClient.Do(req)
	if err != nil {
		return nil, errors.New("xAI request failed")
	}
	return resp, nil
}

func grokListModels(ctx context.Context, base, key string) error {
	resp, err := grokRequest(ctx, http.MethodGet, base+"/models", key, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("xAI models request failed: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return errors.New("invalid xAI models response")
	}
	for _, m := range body.Data {
		if m.ID != "" {
			fmt.Println(m.ID)
		}
	}
	return nil
}

func grokCompletion(ctx context.Context, base, key, model, system, prompt string) (string, TokenUsage, int, string, *Halt, error) {
	messages := []map[string]string{}
	if system != "" {
		messages = append(messages, map[string]string{"role": "system", "content": system})
	}
	messages = append(messages, map[string]string{"role": "user", "content": prompt})
	body, err := json.Marshal(map[string]any{"model": model, "messages": messages, "stream": false})
	if err != nil {
		return "", TokenUsage{}, 0, "", nil, errors.New("cannot encode xAI request")
	}
	resp, err := grokRequest(ctx, http.MethodPost, base+"/chat/completions", key, bytes.NewReader(body))
	if err != nil {
		return "", TokenUsage{}, 0, "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var failure struct {
			Error json.RawMessage `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&failure)
		code, message := errorFields(failure.Error)
		code = safeGrokCode(code, key)
		halt := grokHalt(resp.StatusCode, code, message)
		if code != "" {
			return "", TokenUsage{}, resp.StatusCode, code, halt, fmt.Errorf("xAI request failed: HTTP %d (%s)", resp.StatusCode, code)
		}
		return "", TokenUsage{}, resp.StatusCode, code, halt, fmt.Errorf("xAI request failed: HTTP %d", resp.StatusCode)
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			PromptDetails    struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionDetails struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&completion); err != nil || len(completion.Choices) == 0 {
		return "", TokenUsage{}, resp.StatusCode, "", nil, errors.New("invalid xAI completion response")
	}
	result := completion.Choices[0].Message.Content
	if result == "" {
		return "", TokenUsage{}, resp.StatusCode, "", nil, errors.New("xAI completion contained no text")
	}
	usage := TokenUsage{Input: completion.Usage.PromptTokens, CacheRead: completion.Usage.PromptDetails.CachedTokens, Output: completion.Usage.CompletionTokens, Reasoning: completion.Usage.CompletionDetails.ReasoningTokens}
	return result, usage, resp.StatusCode, "", nil, nil
}

func safeGrokCode(code, key string) string {
	if len(code) > 100 || key != "" && strings.Contains(code, key) {
		return ""
	}
	for _, c := range code {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return ""
		}
	}
	return code
}

func grokHalt(status int, code, message string) *Halt {
	// Credit errors take precedence over an HTTP 429 rate status.
	combined := strings.ToLower(code + " " + message)
	exhausted := strings.Contains(combined, "credit") || strings.Contains(combined, "billing") || strings.Contains(combined, "insufficient_quota") || strings.Contains(combined, "quota exceeded") || strings.Contains(combined, "quota exhausted")
	if exhausted || status == http.StatusPaymentRequired {
		h := classifyUsageError(code, message, "grok_api", time.Now().UTC())
		if h == nil || h.Kind != "usage_limit" {
			h = &Halt{Kind: "usage_limit", Code: "credit_or_quota_exhausted", Source: "grok_api", DetectedAt: time.Now().UTC()}
		}
		h.Scope = "account"
		return h
	}
	if h := classifyUsageError(code, message, "grok_api", time.Now().UTC()); h != nil {
		return h
	}
	if status == http.StatusTooManyRequests {
		return &Halt{Kind: "rate_limit", Scope: "temporary", Code: "rate_limit_exceeded", Source: "grok_api", DetectedAt: time.Now().UTC()}
	}
	return nil
}
