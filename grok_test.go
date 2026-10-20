package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

type grokTransport func(*http.Request) (*http.Response, error)

func (f grokTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func grokTestEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("AGENT_BRIDGE_HOME", dir)
	t.Setenv("AGENT_BRIDGE_ACTIVITY_DIR", filepath.Join(dir, "activity"))
	t.Setenv("AGENT_BRIDGE_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("AGENT_BRIDGE_GROK_MODEL", "")
	t.Setenv("AGENT_BRIDGE_XAI_BASE_URL", "https://example.test/v1")
	return dir
}

func fakeGrok(t *testing.T, fn grokTransport) {
	t.Helper()
	old := grokHTTPClient
	grokHTTPClient = &http.Client{Transport: fn}
	t.Cleanup(func() { grokHTTPClient = old })
}

func grokResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func captureAsk(t *testing.T, args []string, prompt string) (string, error) {
	t.Helper()
	in, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if _, err := in.WriteString(prompt); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = in, out
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()
	err = ask(args)
	if _, seekErr := out.Seek(0, io.SeekStart); seekErr != nil {
		t.Fatal(seekErr)
	}
	b, readErr := io.ReadAll(out)
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(b), err
}

func onlyGrokJob(t *testing.T) Job {
	t.Helper()
	r, err := root()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(r, "jobs"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("jobs: %v, %v", entries, err)
	}
	j, err := readJob(entries[0].Name())
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestGrokAskSuccess(t *testing.T) {
	dir := grokTestEnv(t)
	t.Setenv("XAI_API_KEY", "secret-key")
	if err := os.Mkdir(filepath.Join(dir, "activity"), 0700); err != nil {
		t.Fatal(err)
	}
	activity, _ := json.Marshal(map[string]any{"id": "claude-parent", "name": "Claude", "phase": "waiting", "sampledAt": time.Now().UTC()})
	if err := os.WriteFile(filepath.Join(dir, "activity", "claude-parent.json"), activity, 0600); err != nil {
		t.Fatal(err)
	}
	fakeGrok(t, func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.String() != "https://example.test/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer secret-key" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
		}
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal(b, &req) != nil || req.Model != "selected-model" || req.Stream || len(req.Messages) != 2 || req.Messages[0].Content != "system secret" || req.Messages[1].Content != "prompt secret" {
			t.Fatalf("bad request body: %s", b)
		}
		return grokResponse(200, `{"choices":[{"message":{"content":"response secret"}}],"usage":{"prompt_tokens":13,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}}`), nil
	})
	out, err := captureAsk(t, []string{"--model", "selected-model", "--system", "system secret", "--name", "trial", "--spawned-by", "claude-parent"}, "prompt secret")
	if err != nil || out != "response secret\n" {
		t.Fatalf("ask = %q, %v", out, err)
	}
	j := onlyGrokJob(t)
	if !regexp.MustCompile(`^\d{8}T\d{6}-\d+$`).MatchString(j.ID) {
		t.Fatalf("job ID %q does not match start ID format", j.ID)
	}
	if j.Provider != "grok" || j.Name != "trial" || j.Launcher != "inline" || j.Sandbox != "none" || j.Status != "completed" || j.Cwd == "" || j.SpawnedBy == nil || j.SpawnedBy.AgentID != "claude-parent" || j.Usage == nil || j.Usage.Input != 13 || j.Usage.Output != 7 || j.Usage.CacheRead != 3 || j.Usage.Reasoning != 2 || j.ExitCode == nil || *j.ExitCode != 0 {
		t.Fatalf("bad job: %+v", j)
	}
	result, err := os.ReadFile(j.ResultPath)
	if err != nil || string(result) != "response secret" {
		t.Fatalf("result: %q, %v", result, err)
	}
	events, err := os.ReadFile(j.EventsPath)
	if err != nil || !strings.Contains(string(events), "request_started") || !strings.Contains(string(events), "response_received") || strings.Contains(string(events), "prompt secret") || strings.Contains(string(events), "response secret") || strings.Contains(string(events), "system secret") || strings.Contains(string(events), "secret-key") {
		t.Fatalf("events: %q, %v", events, err)
	}
}

func TestGrokListModelsAndKeyFile(t *testing.T) {
	dir := grokTestEnv(t)
	keyPath := filepath.Join(dir, ".config", "agent-bridge", "xai-api-key")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(" file-key \n"), 0600); err != nil {
		t.Fatal(err)
	}
	fakeGrok(t, func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer file-key" {
			t.Fatalf("unexpected models request: %v", r)
		}
		return grokResponse(200, `{"data":[{"id":"one"},{"id":"two"}]}`), nil
	})
	out, err := captureAsk(t, []string{"--list-models"}, "")
	if err != nil || out != "one\ntwo\n" {
		t.Fatalf("models = %q, %v", out, err)
	}
}

func TestGrokValidation(t *testing.T) {
	dir := grokTestEnv(t)
	if _, err := captureAsk(t, nil, "prompt"); err == nil || !strings.Contains(err.Error(), "--list-models") {
		t.Fatalf("missing model: %v", err)
	}
	if _, err := captureAsk(t, []string{"--model", "m"}, "prompt"); err == nil || !strings.Contains(err.Error(), "XAI_API_KEY") || !strings.Contains(err.Error(), "console.x.ai") {
		t.Fatalf("missing key: %v", err)
	}
	keyPath := filepath.Join(dir, ".config", "agent-bridge", "xai-api-key")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("private-key"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keyPath, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := captureAsk(t, []string{"--model", "m"}, "prompt"); err == nil || !strings.Contains(err.Error(), "chmod 600") || strings.Contains(err.Error(), "private-key") {
		t.Fatalf("insecure key: %v", err)
	}
	t.Setenv("XAI_API_KEY", "env-key")
	if _, err := captureAsk(t, []string{"--model", "m"}, " \n\t "); err == nil || !strings.Contains(err.Error(), "non-whitespace") {
		t.Fatalf("empty prompt: %v", err)
	}
}

func TestGrokPromptFileAndEnvModel(t *testing.T) {
	dir := grokTestEnv(t)
	t.Setenv("XAI_API_KEY", "key")
	t.Setenv("AGENT_BRIDGE_GROK_MODEL", "env-model")
	path := filepath.Join(dir, "question.txt")
	if err := os.WriteFile(path, []byte("file prompt"), 0600); err != nil {
		t.Fatal(err)
	}
	fakeGrok(t, func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"model":"env-model"`) || !strings.Contains(string(b), "file prompt") {
			t.Fatalf("request: %s", b)
		}
		return grokResponse(200, `{"choices":[{"message":{"content":"answer"}}]}`), nil
	})
	if out, err := captureAsk(t, []string{"--prompt-file", path}, "ignored stdin"); err != nil || out != "answer\n" {
		t.Fatalf("ask = %q, %v", out, err)
	}
}

func TestGrokHTTPFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body, status, kind, scope string
		httpStatus                      int
	}{
		{"rate", `{"error":{"code":"rate_limit_exceeded","message":"slow down"}}`, "halted", "rate_limit", "temporary", 429},
		{"credits", `{"error":{"code":"credit_balance_exhausted","message":"no credits"}}`, "halted", "usage_limit", "account", 402},
		{"quota429", `{"error":{"code":"insufficient_quota","message":"quota exhausted"}}`, "halted", "usage_limit", "account", 429},
		{"context", `{"error":{"code":"context_length_exceeded","message":"too long"}}`, "halted", "context_limit", "session", 400},
		{"generic", `{"error":{"code":"server_error","message":"internal failure"}}`, "failed", "", "", 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			grokTestEnv(t)
			t.Setenv("XAI_API_KEY", "secret-key")
			fakeGrok(t, func(r *http.Request) (*http.Response, error) { return grokResponse(tc.httpStatus, tc.body), nil })
			if out, err := captureAsk(t, []string{"--model", "m"}, "private prompt"); err == nil || out != "" || strings.Contains(err.Error(), "private prompt") {
				t.Fatalf("failure = %q, %v", out, err)
			}
			j := onlyGrokJob(t)
			if j.Status != tc.status || j.ExitCode == nil || *j.ExitCode != 1 {
				t.Fatalf("status = %+v", j)
			}
			if tc.kind == "" && j.Halt != nil || tc.kind != "" && (j.Halt == nil || j.Halt.Kind != tc.kind || j.Halt.Scope != tc.scope) {
				t.Fatalf("halt = %+v", j.Halt)
			}
			events, err := os.ReadFile(j.EventsPath)
			if err != nil || !strings.Contains(string(events), `"http_status":`) || strings.Contains(string(events), "private prompt") || strings.Contains(string(events), "secret-key") {
				t.Fatalf("events: %q, %v", events, err)
			}
		})
	}
}
