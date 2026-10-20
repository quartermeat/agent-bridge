package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fakeOllama(t *testing.T, fn grokTransport) {
	t.Helper()
	old := ollamaHTTPClient
	ollamaHTTPClient = &http.Client{Transport: fn}
	t.Cleanup(func() { ollamaHTTPClient = old })
}

func ollamaTestEnv(t *testing.T) {
	t.Helper()
	grokTestEnv(t)
	t.Setenv("OLLAMA_HOST", "localhost:11434")
	t.Setenv("AGENT_BRIDGE_OLLAMA_MODEL", "")
}

func TestOllamaAskSuccess(t *testing.T) {
	ollamaTestEnv(t)
	fakeOllama(t, func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.String() != "http://localhost:11434/api/chat" || r.Header.Get("Authorization") != "" || r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("request: %v", r)
		}
		if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) < 590*time.Second {
			t.Fatalf("default timeout: %v, %v", deadline, ok)
		}
		b, _ := io.ReadAll(r.Body)
		var request struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Role, Content string
			} `json:"messages"`
		}
		if json.Unmarshal(b, &request) != nil || request.Model != "llama3" || request.Stream || len(request.Messages) != 2 || request.Messages[0].Role != "system" || request.Messages[0].Content != "system secret" || request.Messages[1].Role != "user" || request.Messages[1].Content != "prompt secret" {
			t.Fatalf("body: %s", b)
		}
		return grokResponse(200, `{"message":{"role":"assistant","content":"response secret"},"prompt_eval_count":13,"eval_count":7}`), nil
	})
	out, err := captureAsk(t, []string{"--provider", "ollama", "--model", "llama3", "--system", "system secret"}, "prompt secret")
	if err != nil || out != "response secret\n" {
		t.Fatalf("ask = %q, %v", out, err)
	}
	j := onlyGrokJob(t)
	if j.Provider != "ollama" || j.Status != "completed" || j.Launcher != "inline" || j.Sandbox != "none" || j.Usage == nil || j.Usage.Input != 13 || j.Usage.Output != 7 || j.Halt != nil || j.ExitCode == nil || *j.ExitCode != 0 {
		t.Fatalf("job: %+v", j)
	}
	result, err := os.ReadFile(j.ResultPath)
	if err != nil || string(result) != "response secret" {
		t.Fatalf("result: %q, %v", result, err)
	}
	events, err := os.ReadFile(j.EventsPath)
	if err != nil || !strings.Contains(string(events), "request_started") || !strings.Contains(string(events), "response_received") || strings.Contains(string(events), "prompt secret") || strings.Contains(string(events), "response secret") || strings.Contains(string(events), "system secret") {
		t.Fatalf("events: %q, %v", events, err)
	}
}

func TestOllamaListModelsAndHost(t *testing.T) {
	ollamaTestEnv(t)
	t.Setenv("OLLAMA_HOST", "https://example.test:1234/")
	fakeOllama(t, func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.String() != "https://example.test:1234/api/tags" || r.Header.Get("Authorization") != "" {
			t.Fatalf("request: %v", r)
		}
		return grokResponse(200, `{"models":[{"name":"one"},{"name":"two"}]}`), nil
	})
	out, err := captureAsk(t, []string{"--provider", "ollama", "--list-models"}, "")
	if err != nil || out != "one\ntwo\n" {
		t.Fatalf("models = %q, %v", out, err)
	}
}

func TestOllamaMissingAndEnvModel(t *testing.T) {
	ollamaTestEnv(t)
	if _, err := captureAsk(t, []string{"--provider", "ollama"}, "prompt"); err == nil || !strings.Contains(err.Error(), "--provider ollama --list-models") {
		t.Fatalf("missing model: %v", err)
	}
	t.Setenv("AGENT_BRIDGE_OLLAMA_MODEL", "env-model")
	fakeOllama(t, func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"model":"env-model"`) {
			t.Fatalf("body: %s", b)
		}
		return grokResponse(200, `{"message":{"content":"answer"}}`), nil
	})
	if out, err := captureAsk(t, []string{"--provider", "ollama", "--timeout", "1s"}, "prompt"); err != nil || out != "answer\n" {
		t.Fatalf("ask = %q, %v", out, err)
	}
}

func TestOllamaUnreachableAndNotFound(t *testing.T) {
	for _, tc := range []struct {
		name      string
		transport grokTransport
		want      string
	}{
		{"unreachable", func(r *http.Request) (*http.Response, error) { return nil, syscall.ECONNREFUSED }, "Ollama not reachable at http://localhost:11434; start it with `ollama serve`"},
		{"not found", func(r *http.Request) (*http.Response, error) {
			return grokResponse(404, `{"error":"model missing"}`), nil
		}, "Ollama model \"missing\" not found; run `ollama pull missing`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ollamaTestEnv(t)
			fakeOllama(t, tc.transport)
			_, err := captureAsk(t, []string{"--provider", "ollama", "--model", "missing"}, "private prompt")
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error: %v", err)
			}
			j := onlyGrokJob(t)
			if j.Provider != "ollama" || j.Status != "failed" || j.Halt != nil || j.ExitCode == nil || *j.ExitCode != 1 {
				t.Fatalf("job: %+v", j)
			}
			events, _ := os.ReadFile(j.EventsPath)
			if strings.Contains(string(events), "private prompt") {
				t.Fatalf("events: %q", events)
			}
		})
	}
}

func TestOllamaHostParsing(t *testing.T) {
	for _, tc := range []struct{ host, want string }{
		{"", "http://127.0.0.1:11434"},
		{"localhost:1234", "http://localhost:1234"},
		{"https://example.test:1234/", "https://example.test:1234"},
	} {
		t.Setenv("OLLAMA_HOST", tc.host)
		got, err := ollamaBase()
		if err != nil || got != tc.want {
			t.Fatalf("host %q: %q, %v", tc.host, got, err)
		}
	}
	t.Setenv("OLLAMA_HOST", "ftp://example.test")
	if _, err := ollamaBase(); err == nil || !strings.Contains(err.Error(), "OLLAMA_HOST") {
		t.Fatal(err)
	}
}
