package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
)

// TestMain keeps every test away from the host's real Ollama and GPU tools so
// snapshots never pick up live local-model rows. Tests that need data install
// their own fakes and restore these on cleanup.
func TestMain(m *testing.M) {
	runNvidiaSMI = func(ctx context.Context, args ...string) ([]byte, error) {
		return nil, errors.New("nvidia-smi disabled in tests")
	}
	ollamaClient = fakeHTTP(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("ollama disabled in tests")
	})
	ollamaHTTPClient = &http.Client{Transport: failingTransport{}}
	// Closed port for any child process (built binaries) that reads OLLAMA_HOST.
	os.Setenv("OLLAMA_HOST", "127.0.0.1:1")
	// Never read the host's real Codex sessions for rate limits.
	os.Setenv("CODEX_HOME", "/nonexistent/agent-bridge-test-codex")
	os.Exit(m.Run())
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("ollama disabled in tests")
}
