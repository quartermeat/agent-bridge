package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func phoneCall(name, args string) harnessCall {
	var c harnessCall
	c.Function.Name = name
	c.Function.Arguments = []byte(args)
	return c
}

func TestPhoneProfileValidationAndReadBoundary(t *testing.T) {
	p, e := loadProfiles()
	if e != nil {
		t.Fatal(e)
	}
	phone := p["ollama-phone"]
	if phone.Model != "qwen3:1.7b" || phone.NumCtx != 4096 || phone.NumThread != 3 || phone.NumPredict != 512 || phone.Think == nil || *phone.Think || phone.Tools != "read" || phone.MaxRounds != 6 {
		t.Fatalf("%+v", phone)
	}
	for _, field := range []string{"num_ctx: 0", "num_thread: -1", "num_predict: bad", "think: maybe", "keep_alive: 0s", "max_rounds: 0", "tools: command"} {
		data := "profiles:\n  bad:\n    agent: ollama\n    model: m\n    effort: low\n    time_limit: 1m\n    " + field + "\n"
		if _, e := parseProfiles([]byte(data)); e == nil {
			t.Fatalf("accepted %s", field)
		}
	}
	data := "profiles:\n  bad:\n    agent: codex\n    model: m\n    effort: low\n    time_limit: 1m\n    think: false\n"
	if _, e := parseProfiles([]byte(data)); e == nil {
		t.Fatal("accepted non-Ollama option")
	}
	dir := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("private"), 0600)
	if e := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "link")); e != nil {
		t.Fatal(e)
	}
	s := harnessSession{Cwd: dir, Role: "coder", Tools: "read"}
	if got := s.executeTool(context.Background(), phoneCall("read_file", `{"path":"link"}`)); !strings.Contains(got, "outside job cwd") {
		t.Fatal(got)
	}
	if got := s.executeTool(context.Background(), phoneCall("run_command", `{"command":"true"}`)); !strings.Contains(got, "denied") {
		t.Fatal(got)
	}
	if got := s.executeTool(context.Background(), phoneCall("search", `{"pattern":"x"}`)); !strings.Contains(got, "denied") {
		t.Fatal(got)
	}
}

func TestPhoneRequestAndReportRepair(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_BRIDGE_STATE_DIR", dir)
	t.Setenv("AGENT_BRIDGE_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte("0.32.0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			fmt.Fprint(w, `{}`)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body["think"] != false || body["keep_alive"] != "2m0s" {
			t.Errorf("options %v", body)
		}
		options := body["options"].(map[string]any)
		if options["num_ctx"] != float64(4096) || options["num_thread"] != float64(3) || options["num_predict"] != float64(512) {
			t.Errorf("options %v", options)
		}
		count++
		message := map[string]any{"role": "assistant"}
		switch count {
		case 1:
			if len(body["tools"].([]any)) != 3 || body["format"] != nil {
				t.Errorf("gather request %v", body)
			}
			msgs := body["messages"].([]any)
			if strings.Contains(fmt.Sprint(msgs[0]), "report") {
				t.Error("report instructions in gather")
			}
			message["tool_calls"] = []any{map[string]any{"function": map[string]any{"name": "read_file", "arguments": map[string]any{"path": "VERSION"}}}}
		case 2:
			message["content"] = "The version is 0.32.0."
		case 3, 4:
			if body["tools"] != nil || body["format"] == nil || body["stream"] != false {
				t.Errorf("report request %v", body)
			}
			message["content"] = "bad"
			if count == 4 {
				message["content"] = `{"status":"done","summary":"The version is 0.32.0.","checks":"pass"}`
			}
		default:
			t.Errorf("unexpected request %d", count)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"message": message, "done": true})
	}))
	defer server.Close()
	fakeOllama(t, server.Client().Transport.RoundTrip)
	t.Setenv("OLLAMA_HOST", server.URL)
	jobdir := filepath.Join(dir, "jobs", "phone")
	os.MkdirAll(jobdir, 0700)
	j := Job{ID: "phone", Agent: "ollama", Role: "coder", Model: "qwen3:1.7b", Cwd: dir, Sandbox: "read-only", Tools: "read", NumCtx: 4096, NumThread: 3, NumPredict: 512, Think: new(bool), KeepAlive: "2m", MaxRounds: 6, TimeLimit: "1m", EventsPath: filepath.Join(jobdir, "events"), ResultPath: filepath.Join(jobdir, "result")}
	if err := writeJob(j); err != nil {
		t.Fatal(err)
	}
	var ev, stderr bytes.Buffer
	if err := runOllamaJob(&j, []byte("task: v1\ngoal: Read the version from VERSION.\nowns: ["+filepath.Join(dir, "VERSION")+"]\ndetail: /outside/task.md"), &ev, &stderr); err != nil {
		t.Fatal(err)
	}
	result, _ := os.ReadFile(j.ResultPath)
	if count != 4 || j.Status != "completed" || !validPhoneReport(string(result)) || !strings.Contains(string(result), `summary: "The version is 0.32.0."`) || !strings.Contains(string(result), "checks: none") {
		t.Fatalf("count %d job %+v result %s", count, j, result)
	}
	if !strings.Contains(ev.String(), `"type":"tool.call"`) || !strings.Contains(ev.String(), `"type":"report.reply"`) {
		t.Fatalf("events %s", ev.String())
	}
}

func TestPhoneNoEvidenceBlocked(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_BRIDGE_STATE_DIR", dir)
	t.Setenv("AGENT_BRIDGE_HOME", dir)
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			fmt.Fprint(w, `{}`)
			return
		}
		count++
		fmt.Fprint(w, `{"message":{"role":"assistant","content":"I guess 1.0.0"},"done":true}`)
	}))
	defer server.Close()
	fakeOllama(t, server.Client().Transport.RoundTrip)
	t.Setenv("OLLAMA_HOST", server.URL)
	jobdir := filepath.Join(dir, "jobs", "none")
	os.MkdirAll(jobdir, 0700)
	j := Job{ID: "none", Agent: "ollama", Role: "coder", Model: "m", Cwd: dir, Sandbox: "read-only", Tools: "read", NumCtx: 4096, MaxRounds: 3, TimeLimit: "1m", EventsPath: filepath.Join(jobdir, "events"), ResultPath: filepath.Join(jobdir, "result")}
	writeJob(j)
	var ev, stderr bytes.Buffer
	if err := runOllamaJob(&j, []byte("Read version"), &ev, &stderr); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(j.ResultPath)
	if count < 2 || !strings.Contains(string(b), "status: blocked") || strings.Contains(string(b), "1.0.0") {
		t.Fatalf("count %d result %s", count, b)
	}
}

func TestPhoneStopChild(t *testing.T) {
	if os.Getenv("PHONE_STOP_CHILD") == "1" {
		if err := worker("phone-stop"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	dir := t.TempDir()
	t.Setenv("AGENT_BRIDGE_STATE_DIR", dir)
	t.Setenv("AGENT_BRIDGE_HOME", dir)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			fmt.Fprint(w, `{}`)
			return
		}
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	jobdir := filepath.Join(dir, "jobs", "phone-stop")
	if err := os.MkdirAll(jobdir, 0700); err != nil {
		t.Fatal(err)
	}
	j := Job{ID: "phone-stop", Agent: "ollama", Role: "coder", Model: "m", Cwd: dir, Sandbox: "read-only", Tools: "read", NumCtx: 4096, TimeLimit: "1m", EventsPath: filepath.Join(jobdir, "events.jsonl"), StderrPath: filepath.Join(jobdir, "stderr.log"), ResultPath: filepath.Join(jobdir, "result.txt")}
	if err := writeJob(j); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobdir, "prompt.txt"), []byte("Read VERSION"), 0600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "agent-bridge")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	cmd := exec.Command(bin, "_worker", "phone-stop")
	var childOutput bytes.Buffer
	cmd.Stdout, cmd.Stderr = &childOutput, &childOutput
	cmd.Env = append(os.Environ(), "PHONE_STOP_CHILD=1", "AGENT_BRIDGE_STATE_DIR="+dir, "AGENT_BRIDGE_HOME="+dir, "OLLAMA_HOST="+server.URL)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("child exited before request: %v output %s", err, childOutput.String())
	case <-time.After(10 * time.Second):
		got, _ := readJob("phone-stop")
		t.Fatalf("child did not request Ollama: job %+v output %s", got, childOutput.String())
	}
	if err := stop([]string{"phone-stop"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child did not stop")
	}
	got, err := readJob("phone-stop")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "stopped" {
		t.Fatalf("status %s error %s", got.Status, got.Error)
	}
	if _, err := os.Stat(filepath.Join(jobdir, "prompt.txt")); !os.IsNotExist(err) {
		t.Fatalf("prompt cleanup: %v", err)
	}
}

func TestPhoneEvidenceRules(t *testing.T) {
	s := &harnessSession{}
	s.recordPhoneTool("read_file", []byte(`{"path":"missing"}`), "error: open missing: no such file", false)
	s.recordPhoneTool("list_files", []byte(`{"path":"."}`), "a.go", true)
	got := phoneReportStatus(phoneAnswer{Status: "done", Summary: "a.go"}, s)
	if got.Status != "blocked" || !strings.Contains(got.Summary, "open missing") {
		t.Fatalf("%+v", got)
	}
	s = &harnessSession{}
	listed := &harnessSession{}
	listed.recordPhoneTool("list_files", []byte(`{"path":"."}`), "a.go", true)
	if got := phoneReportStatus(phoneAnswer{Status: "done", Summary: "a.go"}, listed); got.Status != "done" {
		t.Fatalf("%+v", got)
	}
	s.recordPhoneTool("grep", []byte(`{"pattern":"x"}`), "[skipped: 0 large, 0 binary, 0 symlink, 0 excluded directories, 0 errors; scanned 5 bytes]", true)
	if got := phoneReportStatus(phoneAnswer{Status: "done", Summary: "x"}, s); got.Status != "partial" {
		t.Fatalf("%+v", got)
	}
	s.recordPhoneTool("grep", []byte(`{"pattern":"x"}`), "a.go:1:x\n[skipped: 0 large, 0 binary, 0 symlink, 0 excluded directories, 0 errors; scanned 5 bytes]", true)
	if got := phoneReportStatus(phoneAnswer{Status: "done", Summary: "a.go"}, s); got.Status != "done" {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(phoneReportInput("Find x", "Files read successfully", s.PhoneEvidenceText()), "a.go:1:x") {
		t.Fatal("missing tool evidence")
	}
	if !phoneBoilerplate("Files read successfully") || !phoneBoilerplate("Task completed.") || phoneBoilerplate("The answer is a.go") {
		t.Fatal("boilerplate classification")
	}
}

func TestPhoneBoilerplateRetryAndRoundLimit(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_BRIDGE_STATE_DIR", dir)
	t.Setenv("AGENT_BRIDGE_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\nconst Answer = 42\n"), 0600); err != nil {
		t.Fatal(err)
	}
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			fmt.Fprint(w, `{}`)
			return
		}
		count++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		message := map[string]any{"role": "assistant"}
		switch count {
		case 1, 2:
			message["tool_calls"] = []any{map[string]any{"function": map[string]any{"name": "read_file", "arguments": map[string]any{"path": "a.go"}}}}
		case 3, 4:
			if body["tools"] != nil || body["format"] == nil {
				t.Errorf("report phase missing: %v", body)
			}
			if count == 3 {
				messages := body["messages"].([]any)
				user := fmt.Sprint(messages[1])
				if !strings.Contains(user, "Goal: Find the answer") || !strings.Contains(user, "const Answer = 42") || !strings.Contains(user, "Gather answer:") {
					t.Errorf("report input %s", user)
				}
			}
			message["content"] = `{"status":"done","summary":"Files read successfully","checks":"none"}`
		default:
			t.Errorf("extra request %d", count)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"message": message, "done": true})
	}))
	defer server.Close()
	fakeOllama(t, server.Client().Transport.RoundTrip)
	t.Setenv("OLLAMA_HOST", server.URL)
	jobdir := filepath.Join(dir, "jobs", "rounds")
	if err := os.MkdirAll(jobdir, 0700); err != nil {
		t.Fatal(err)
	}
	j := Job{ID: "rounds", Agent: "ollama", Role: "coder", Model: "m", Cwd: dir, Sandbox: "read-only", Tools: "read", NumCtx: 4096, MaxRounds: 2, TimeLimit: "1m", EventsPath: filepath.Join(jobdir, "events"), ResultPath: filepath.Join(jobdir, "result")}
	if err := writeJob(j); err != nil {
		t.Fatal(err)
	}
	var ev, stderr bytes.Buffer
	if err := runOllamaJob(&j, []byte("task: v1\ngoal: Find the answer"), &ev, &stderr); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(j.ResultPath)
	if count != 4 || j.Status != "completed" || !strings.Contains(string(b), "status: partial") {
		t.Fatalf("count %d job %s result %s", count, j.Status, b)
	}
}
