package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
)

// Replaced by a fake transport in tests; requests still use the real HTTP path.
var ollamaHTTPClient = &http.Client{}

func ollamaBase() (string, error) {
	base := strings.TrimSpace(os.Getenv("OLLAMA_HOST"))
	if base == "" {
		base = "http://127.0.0.1:11434"
	} else if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid OLLAMA_HOST; use host:port or an http(s) URL")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func ollamaRequest(ctx context.Context, method, endpoint, base string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, base+endpoint, body)
	if err != nil {
		return nil, errors.New("invalid Ollama URL")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := ollamaHTTPClient.Do(req)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("Ollama not reachable at %s; start it with `ollama serve`", base)
		}
		return nil, fmt.Errorf("Ollama request failed: %w", err)
	}
	return resp, nil
}

func ollamaListModels(ctx context.Context, base string) error {
	resp, err := ollamaRequest(ctx, http.MethodGet, "/api/tags", base, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Ollama models request failed: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return errors.New("invalid Ollama models response")
	}
	for _, m := range body.Models {
		if m.Name != "" {
			fmt.Println(m.Name)
		}
	}
	return nil
}

func ollamaCompletion(ctx context.Context, base, model, system, prompt string) (string, TokenUsage, int, string, error) {
	messages := []map[string]string{}
	if system != "" {
		messages = append(messages, map[string]string{"role": "system", "content": system})
	}
	messages = append(messages, map[string]string{"role": "user", "content": prompt})
	body, err := json.Marshal(map[string]any{"model": model, "messages": messages, "stream": false})
	if err != nil {
		return "", TokenUsage{}, 0, "", errors.New("cannot encode Ollama request")
	}
	resp, err := ollamaRequest(ctx, http.MethodPost, "/api/chat", base, bytes.NewReader(body))
	if err != nil {
		return "", TokenUsage{}, 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", TokenUsage{}, resp.StatusCode, "model_not_found", fmt.Errorf("Ollama model %q not found; run `ollama pull %s`", model, model)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", TokenUsage{}, resp.StatusCode, "", fmt.Errorf("Ollama request failed: HTTP %d", resp.StatusCode)
	}
	var completion struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		PromptEvalCount int64 `json:"prompt_eval_count"`
		EvalCount       int64 `json:"eval_count"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&completion); err != nil {
		return "", TokenUsage{}, resp.StatusCode, "", errors.New("invalid Ollama chat response")
	}
	if completion.Message.Content == "" {
		return "", TokenUsage{}, resp.StatusCode, "", errors.New("Ollama chat response contained no text")
	}
	return completion.Message.Content, TokenUsage{Input: completion.PromptEvalCount, Output: completion.EvalCount}, resp.StatusCode, "", nil
}
