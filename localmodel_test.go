package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestKeepModelRetriesAndLogsStateChanges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs []string
	attempts := 0
	err := keepModelLoop(ctx, 5*time.Millisecond, func(context.Context) error {
		attempts++
		if attempts <= 3 {
			return errors.New("Ollama unavailable")
		}
		if attempts == 5 {
			cancel()
		}
		return nil
	}, func(message string) { logs = append(logs, message) })
	if !errors.Is(err, context.Canceled) || attempts != 5 {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
	want := []string{"Ollama unavailable", "Ollama available; model pinned"}
	if !reflect.DeepEqual(logs, want) {
		t.Fatalf("logs=%q want=%q", logs, want)
	}
}

func modelTestEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGENT_BRIDGE_STATE_DIR", dir)
	t.Setenv("AGENT_BRIDGE_HOME", dir)
	t.Setenv("AGENT_BRIDGE_ACTIVITY_DIR", filepath.Join(dir, "activity"))
	return dir
}
func modelResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}
}
func TestModelLeaseLifecycle(t *testing.T) {
	modelTestEnv(t)
	dir, err := modelStateDir()
	if err != nil {
		t.Fatal(err)
	}
	first, err := createModelLease(dir, "render one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := createModelLease(dir, "render two")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("leases collided")
	}
	stale := filepath.Join(dir, "leases", "stale.json")
	if err := writeJSONPrivate(stale, modelLease{PID: os.Getpid(), Start: "wrong-start"}); err != nil {
		t.Fatal(err)
	}
	leases, err := liveModelLeases(dir)
	if err != nil || len(leases) != 2 {
		t.Fatalf("leases=%+v err=%v", leases, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale lease still exists: %v", err)
	}
	os.Remove(first)
	leases, err = liveModelLeases(dir)
	if err != nil || len(leases) != 1 {
		t.Fatalf("leases=%+v err=%v", leases, err)
	}
	os.Remove(second)
	leases, err = liveModelLeases(dir)
	if err != nil || len(leases) != 0 {
		t.Fatalf("leases=%+v err=%v", leases, err)
	}
}
func TestKeepModelDecisions(t *testing.T) {
	modelTestEnv(t)
	dir, err := modelStateDir()
	if err != nil {
		t.Fatal(err)
	}
	loaded := true
	generate := 0
	fakeOllama(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/ps" {
			if loaded {
				return modelResponse(`{"models":[{"name":"epm","size_vram":1024}]}`), nil
			}
			return modelResponse(`{"models":[]}`), nil
		}
		if r.URL.Path != "/api/generate" {
			t.Fatalf("path %s", r.URL.Path)
		}
		var body struct {
			Prompt    string `json:"prompt"`
			KeepAlive int    `json:"keep_alive"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Prompt != "" || body.KeepAlive != -1 {
			t.Fatalf("pin body %+v", body)
		}
		generate++
		loaded = true
		return modelResponse(`{}`), nil
	})
	if err := keepModelOnce(context.Background(), dir, "http://localhost", "epm"); err != nil {
		t.Fatal(err)
	}
	if generate != 0 {
		t.Fatal("re-pinned loaded model")
	}
	loaded = false
	if err := keepModelOnce(context.Background(), dir, "http://localhost", "epm"); err != nil {
		t.Fatal(err)
	}
	if generate != 1 {
		t.Fatalf("pin count %d", generate)
	}
	path, err := createModelLease(dir, "render")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	loaded = false
	if err := keepModelOnce(context.Background(), dir, "http://localhost", "epm"); err != nil {
		t.Fatal(err)
	}
	if generate != 1 {
		t.Fatal("pinned during yield")
	}
	var activity activityFile
	b, err := os.ReadFile(filepath.Join(os.Getenv("AGENT_BRIDGE_ACTIVITY_DIR"), "local-epm-model.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &activity); err != nil || activity.Phase != "idle" || activity.Detail != "yielded" {
		t.Fatalf("activity %+v %v", activity, err)
	}
}
func TestYieldOrderingAndStatus(t *testing.T) {
	modelTestEnv(t)
	events := []string{}
	loaded := true
	fakeOllama(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/generate":
			var body struct {
				KeepAlive int `json:"keep_alive"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.KeepAlive == 0 {
				events = append(events, "unload")
				loaded = false
			} else {
				events = append(events, "repin")
				loaded = true
			}
			return modelResponse(`{}`), nil
		case "/api/ps":
			events = append(events, "wait")
			if loaded {
				return modelResponse(`{"models":[{"name":"epm","size_vram":2048}]}`), nil
			}
			return modelResponse(`{"models":[]}`), nil
		}
		t.Fatalf("path %s", r.URL.Path)
		return nil, nil
	})
	err := yieldModel(context.Background(), "epm", "render", []string{"render"}, func([]string) error {
		events = append(events, "run")
		dir, _ := modelStateDir()
		leases, e := liveModelLeases(dir)
		if e != nil || len(leases) != 1 {
			t.Fatalf("lease during run %+v %v", leases, e)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"unload", "wait", "run", "repin"}) {
		t.Fatalf("events %v", events)
	}
	dir, _ := modelStateDir()
	leases, err := liveModelLeases(dir)
	if err != nil || len(leases) != 0 {
		t.Fatalf("lease after run %+v %v", leases, err)
	}
	s, err := currentModelStatus(context.Background(), "epm")
	if err != nil || !s.Loaded || s.Yielded || !s.Pinned || s.VRAMBytes != 2048 {
		t.Fatalf("status %+v %v", s, err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"leases":[]`) || !strings.Contains(string(b), `"vram_bytes":2048`) {
		t.Fatalf("JSON %s", b)
	}
	loaded = true
	s, err = currentModelStatus(context.Background(), "epm")
	if err != nil || !s.Pinned || s.VRAMBytes != 2048 {
		t.Fatalf("loaded status %+v %v", s, err)
	}
}
func TestLauncherWaitsForYield(t *testing.T) {
	modelTestEnv(t)
	dir, _ := modelStateDir()
	path, err := createModelLease(dir, "render")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := waitForModelLease(ctx); err == nil {
		t.Fatal("did not wait")
	}
	os.Remove(path)
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitForModelLease(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentYieldsRepinAfterLastLease(t *testing.T) {
	modelTestEnv(t)
	loaded := true
	pins := 0
	fakeOllama(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/ps" {
			if loaded {
				return modelResponse(`{"models":[{"name":"epm"}]}`), nil
			}
			return modelResponse(`{"models":[]}`), nil
		}
		var body struct {
			KeepAlive int `json:"keep_alive"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.KeepAlive == -1 {
			pins++
			loaded = true
		} else {
			loaded = false
		}
		return modelResponse(`{}`), nil
	})
	dir, err := modelStateDir()
	if err != nil {
		t.Fatal(err)
	}
	first, err := createModelLease(dir, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := createModelLease(dir, "second")
	if err != nil {
		t.Fatal(err)
	}
	if err := releaseModelLease(context.Background(), dir, "http://localhost", "epm", first); err != nil {
		t.Fatal(err)
	}
	if pins != 0 {
		t.Fatal("repinned while second yield active")
	}
	if err := releaseModelLease(context.Background(), dir, "http://localhost", "epm", second); err != nil {
		t.Fatal(err)
	}
	if pins != 1 {
		t.Fatalf("repins=%d", pins)
	}
}
