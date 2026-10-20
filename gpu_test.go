package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

type fakeHTTP func(*http.Request) (*http.Response, error)

func (f fakeHTTP) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestParseNvidiaSMI(t *testing.T) {
	gpus, err := parseGPUCSV([]byte("0, NVIDIA A100, 38000, 40960, 92\n1, NVIDIA RTX, [N/A], 24576, [N/A]\n"))
	if err != nil || len(gpus) != 2 || gpus[0].MemoryUsedMiB != 38000 || gpus[1].MemoryUsedMiB != 0 || gpus[1].UtilizationPercent != 0 {
		t.Fatalf("GPUs = %+v, %v", gpus, err)
	}
	processes, err := parseGPUProcessesCSV([]byte("123, python, 2048\n456, ollama, [N/A]\n"))
	if err != nil || len(processes) != 2 || processes[1].UsedMiB != 0 {
		t.Fatalf("processes = %+v, %v", processes, err)
	}
	if _, err := parseGPUCSV([]byte("bad,row\n")); err == nil {
		t.Fatal("malformed GPU output accepted")
	}
}

func TestGPUUnavailableDoesNotBreakSnapshot(t *testing.T) {
	oldRunner, oldClient := runNvidiaSMI, ollamaClient
	t.Cleanup(func() { runNvidiaSMI, ollamaClient = oldRunner, oldClient })
	runNvidiaSMI = func(context.Context, ...string) ([]byte, error) { return nil, os.ErrNotExist }
	ollamaClient = fakeHTTP(func(*http.Request) (*http.Response, error) { return nil, errors.New("connection refused") })
	home := t.TempDir()
	t.Setenv("AGENT_BRIDGE_HOME", home)
	t.Setenv("AGENT_BRIDGE_STATE_DIR", home)
	t.Setenv("AGENT_BRIDGE_ACTIVITY_DIR", home)
	s, err := collectSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if s.Version != 1 || s.GPUs == nil || s.GPUProcesses == nil || s.LocalModels == nil {
		t.Fatalf("invalid empty snapshot: %+v", s)
	}
	statuses := map[string]SourceStatus{}
	for _, source := range s.Sources {
		statuses[source.Name] = source
	}
	for _, name := range []string{"nvidia-smi", "ollama"} {
		if statuses[name].Available || statuses[name].Detail == "" {
			t.Fatalf("%s status = %+v", name, statuses[name])
		}
	}
}

func TestOllamaPS(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "127.0.0.1:11434")
	client := fakeHTTP(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "http://127.0.0.1:11434/api/ps" {
			t.Fatalf("URL = %s", r.URL)
		}
		if _, ok := r.Context().Deadline(); !ok {
			t.Fatal("Ollama request has no deadline")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"models":[{"name":"llama3:latest","size":4000000000,"size_vram":3000000000,"expires_at":"2026-10-19T12:00:00Z"}]}`))}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	models, err := ollamaModels(ctx, client)
	if err != nil || len(models) != 1 || models[0].Provider != "ollama" || models[0].VRAMBytes != 3000000000 || models[0].ExpiresAt.IsZero() {
		t.Fatalf("models = %+v, %v", models, err)
	}
	_, err = ollamaModels(ctx, fakeHTTP(func(*http.Request) (*http.Response, error) { return nil, errors.New("connection refused") }))
	if err == nil {
		t.Fatal("unreachable Ollama reported available")
	}
}

func TestCollectGPUAssociatesKnownPID(t *testing.T) {
	oldRunner, oldClient := runNvidiaSMI, ollamaClient
	t.Cleanup(func() { runNvidiaSMI, ollamaClient = oldRunner, oldClient })
	runNvidiaSMI = func(_ context.Context, args ...string) ([]byte, error) {
		if strings.HasPrefix(args[0], "--query-gpu=") {
			return []byte("0, A100, 1024, 40960, 25\n"), nil
		}
		return []byte("42, python, 1024\n"), nil
	}
	ollamaClient = fakeHTTP(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"models":[]}`))}, nil
	})
	s := Snapshot{Agents: []AgentUsage{{ID: "agent", HostPID: 42}}, Jobs: []Job{{ID: "job", CodexPID: 42}}, GPUs: []GPU{}, GPUProcesses: []GPUProcess{}, LocalModels: []LocalModel{}}
	collectGPU(&s)
	if len(s.GPUs) != 1 || len(s.GPUProcesses) != 1 || s.GPUProcesses[0].AgentID != "agent" || s.GPUProcesses[0].JobID != "job" {
		t.Fatalf("GPU snapshot: %+v", s)
	}
	for _, status := range s.Sources {
		if !status.Available {
			t.Fatalf("source unavailable: %+v", status)
		}
	}
}

func TestWatchVRAMRendering(t *testing.T) {
	s := Snapshot{GPUs: []GPU{{Index: 0, Name: "A100", MemoryUsedMiB: 36864, MemoryTotalMiB: 40960, UtilizationPercent: 80}}, GPUProcesses: []GPUProcess{{PID: 42, Name: "python", UsedMiB: 16000}}, LocalModels: []LocalModel{{Provider: "ollama", Name: "llama3", VRAMBytes: 2 << 30}}}
	var out bytes.Buffer
	renderVRAM(&out, s)
	for _, want := range []string{"VRAM 36864/40960 MiB (90%)", "util 80%", "HIGH VRAM", "PID 42 python: 16000 MiB", "llama3: VRAM 2048 MiB"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in %q", want, out.String())
		}
	}
}
