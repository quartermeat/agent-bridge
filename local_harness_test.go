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
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func harnessTestCall(s *harnessSession, ctx context.Context, name string, args map[string]string) string {
	b, _ := json.Marshal(args)
	c := harnessCall{}
	c.Function.Name, c.Function.Arguments = name, b
	return s.executeTool(ctx, c)
}

func TestHarnessBoundedFileTools(t *testing.T) {
	dir := t.TempDir()
	s := harnessSession{Cwd: dir}
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), bytes.Repeat([]byte("hit\n"), harnessFileLimit/4+1), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "binary"), []byte("hit\x00more"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".cache"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".cache", "hidden"), []byte("hit"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := harnessTestCall(&s, context.Background(), "grep", map[string]string{"path": dir, "pattern": "hit"}); !strings.Contains(got, "1 large") || !strings.Contains(got, "1 binary") || !strings.Contains(got, "1 excluded directories") || strings.Contains(got, "hidden:1") {
		t.Fatal(got)
	}
	if got := harnessTestCall(&s, context.Background(), "grep", map[string]string{"path": filepath.Join(dir, ".cache"), "pattern": "hit"}); !strings.Contains(got, "hidden:1") {
		t.Fatal(got)
	}
	if got := harnessTestCall(&s, context.Background(), "read_file", map[string]string{"path": "binary"}); !strings.Contains(got, "binary file refused") {
		t.Fatal(got)
	}
	if err := os.WriteFile(filepath.Join(dir, "read.txt"), bytes.Repeat([]byte("a\n"), harnessReadLimit/2+10), 0600); err != nil {
		t.Fatal(err)
	}
	if got := harnessTestCall(&s, context.Background(), "read_file", map[string]string{"path": "read.txt"}); !strings.Contains(got, "[truncated:") {
		t.Fatal(len(got))
	}
}

func TestHarnessListEntryLimit(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i <= harnessEntryLimit; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%04d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	s := harnessSession{Cwd: dir}
	got := harnessTestCall(&s, context.Background(), "list_files", map[string]string{"path": "."})
	if !strings.Contains(got, "entry limit reached") || len(strings.Split(got, "\n")) != harnessEntryLimit+1 {
		t.Fatal(len(strings.Split(got, "\n")))
	}
}

type deadlineAfterChecks struct {
	context.Context
	checks int
}

type contextBlockingReader struct{ ctx context.Context }

func (r contextBlockingReader) Read([]byte) (int, error) { <-r.ctx.Done(); return 0, r.ctx.Err() }
func (r contextBlockingReader) Close() error             { return nil }

func (c *deadlineAfterChecks) Err() error {
	c.checks++
	if c.checks > 4 {
		return context.DeadlineExceeded
	}
	return nil
}

func TestHarnessGrepPartialDeadlineAndCancel(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("hit\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := &deadlineAfterChecks{Context: context.Background()}
	got := harnessGrep(ctx, dir, "hit")
	if !strings.Contains(got, "a:1:hit") || !strings.Contains(got, "partial matches") || !strings.Contains(got, "deadline exceeded") {
		t.Fatal(got)
	}
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := harnessGrep(cancelCtx, dir, "hit"); !strings.Contains(got, "canceled") {
		t.Fatal(got)
	}
}

func TestHarnessCommandCancel(t *testing.T) {
	s := harnessSession{Cwd: t.TempDir(), Write: true}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	got := harnessTestCall(&s, ctx, "run_command", map[string]string{"command": "sleep 10 & wait"})
	if !strings.Contains(got, "canceled") || time.Since(start) > 2*time.Second {
		t.Fatal(got, time.Since(start))
	}
}

func TestHarnessStreamStall(t *testing.T) {
	t.Setenv("AGENT_BRIDGE_STATE_DIR", t.TempDir())
	fakeOllama(t, func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: contextBlockingReader{r.Context()}, Header: make(http.Header)}, nil
	})
	old := harnessStreamStallLimit
	harnessStreamStallLimit = 30 * time.Millisecond
	defer func() { harnessStreamStallLimit = old }()
	s := harnessSession{Model: "m", Cwd: t.TempDir()}
	_, err := s.turn(context.Background(), "hello", "http://ollama.test")
	if err == nil || !strings.Contains(err.Error(), "stream stalled") {
		t.Fatal(err)
	}
}

func TestCarlArgvDispatch(t *testing.T) {
	for _, executable := range []string{"carl", "/home/user/.local/bin/carl"} {
		if err := runAs(executable, []string{"--help"}); !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("%s --help: %v", executable, err)
		}
		if err := runAs(executable, []string{"--model", "example", "extra"}); err == nil || err.Error() != "unexpected arguments" {
			t.Fatalf("%s --model: %v", executable, err)
		}
	}
	if err := runAs("agent-bridge", []string{"local", "--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("agent-bridge local --help: %v", err)
	}
}

func TestHarnessToolLoopAndLimits(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_BRIDGE_STATE_DIR", t.TempDir())
	if err := os.WriteFile(filepath.Join(dir, "note.md"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	n := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			fmt.Fprint(w, `{"model_info":{"test.context_length":32768}}`)
			return
		}
		if r.URL.Path != "/api/chat" {
			t.Errorf("path %s", r.URL.Path)
		}
		n++
		w.Header().Set("Content-Type", "application/x-ndjson")
		if n == 1 {
			fmt.Fprintln(w, `{"message":{"role":"assistant","tool_calls":[{"function":{"name":"read_file","arguments":{"path":"note.md"}}}]},"done":true,"prompt_eval_count":3,"eval_count":2}`)
		} else {
			fmt.Fprintln(w, `{"message":{"role":"assistant","content":"done"},"done":true,"prompt_eval_count":4,"eval_count":1}`)
		}
	}))
	defer server.Close()
	fakeOllama(t, server.Client().Transport.RoundTrip)
	var out, events bytes.Buffer
	s := harnessSession{Model: "m", Cwd: dir, Role: "epm", Output: &out, Events: &events, Interactive: true}
	got, err := s.turn(context.Background(), "read", server.URL)
	if err != nil || got != "done" || n != 2 || s.Usage.Input != 7 || s.Usage.Output != 3 || !strings.Contains(events.String(), "hello") || !strings.Contains(out.String(), "Carl: thinking") || !strings.Contains(out.String(), "Carl: read_file note.md") {
		t.Fatalf("got %q err %v n %d usage %+v events %q", got, err, n, s.Usage, events.String())
	}
}

func TestHarnessHostAccessConfirmAndAudit(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	t.Setenv("AGENT_BRIDGE_STATE_DIR", t.TempDir())
	s := harnessSession{Cwd: dir, Role: "epm", Write: true, Interactive: true}
	call := func(name string, args map[string]string) string {
		b, _ := json.Marshal(args)
		c := harnessCall{}
		c.Function.Name, c.Function.Arguments = name, b
		return s.tool(context.Background(), c)
	}
	target := filepath.Join(outside, "code.go")
	s.Confirm = func(action string) bool { return false }
	if got := call("write_file", map[string]string{"path": target, "content": "abc"}); !strings.Contains(got, "user denied") {
		t.Fatal(got)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if got := call("run_command", map[string]string{"command": "touch " + target}); !strings.Contains(got, "user denied") {
		t.Fatal(got)
	}
	s.Yes = true
	if got := call("write_file", map[string]string{"path": target, "content": "abc"}); got != "ok" {
		t.Fatal(got)
	}
	if got := call("read_file", map[string]string{"path": target}); got != "abc" {
		t.Fatal(got)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "host-link")); err != nil {
		t.Fatal(err)
	}
	if got := call("read_file", map[string]string{"path": "host-link/code.go"}); got != "abc" {
		t.Fatal(got)
	}
	if got := call("list_files", map[string]string{"path": outside}); !strings.Contains(got, "code.go") {
		t.Fatal(got)
	}
	if got := call("grep", map[string]string{"path": outside, "pattern": "abc"}); !strings.Contains(got, "abc") {
		t.Fatal(got)
	}
	if got := call("edit_file", map[string]string{"path": target, "old": "abc", "new": "xyz"}); got != "ok" {
		t.Fatal(got)
	}
	if got := call("run_command", map[string]string{"command": "pwd"}); strings.TrimSpace(got) != dir {
		t.Fatal(got)
	}
	s.Write = false
	if got := call("write_file", map[string]string{"path": target, "content": "no"}); !strings.Contains(got, "denied") {
		t.Fatal(got)
	}
	if got := call("run_command", map[string]string{"command": "pwd"}); !strings.Contains(got, "denied") {
		t.Fatal(got)
	}
	p, err := harnessAuditPath()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("audit mode: %v %v", info, err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(b), []byte{'\n'})
	if len(lines) != 11 {
		t.Fatalf("audit entries: %d", len(lines))
	}
	var entry map[string]any
	if err := json.Unmarshal(lines[2], &entry); err != nil {
		t.Fatal(err)
	}
	if entry["tool"] != "write_file" || entry["preceding_tool_result_source"] != "run_command" || entry["epm_violation"] != true {
		t.Fatal(entry)
	}
}

func TestHarnessJobReadOnly(t *testing.T) {
	t.Setenv("AGENT_BRIDGE_STATE_DIR", t.TempDir())
	s := harnessSession{Cwd: t.TempDir(), Role: "coder", JobID: "job", Write: false}
	for _, name := range []string{"write_file", "run_command"} {
		c := harnessCall{}
		c.Function.Name = name
		c.Function.Arguments = json.RawMessage(`{"path":"a","content":"x","command":"pwd"}`)
		if got := s.tool(context.Background(), c); !strings.Contains(got, "denied") {
			t.Fatal(got)
		}
	}
}

func TestHarnessFileToolsExpandHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, "note"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	s := harnessSession{Cwd: t.TempDir(), Write: true, Yes: true}
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		args map[string]string
		want string
	}{
		{"read_file", map[string]string{"path": "~/note"}, "hello"},
		{"list_files", map[string]string{"path": "~"}, "note"},
		{"grep", map[string]string{"path": "~/", "pattern": "hello"}, "note:1:hello"},
		{"search", map[string]string{"path": "~/", "pattern": "hello"}, "note:1:hello"},
		{"write_file", map[string]string{"path": "~/new", "content": "new"}, "ok"},
		{"edit_file", map[string]string{"path": "~/new", "old": "new", "new": "edited"}, "ok"},
	} {
		if got := harnessTestCall(&s, ctx, tc.name, tc.args); !strings.Contains(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
	if b, err := os.ReadFile(filepath.Join(home, "new")); err != nil || string(b) != "edited" {
		t.Fatalf("home write: %q, %v", b, err)
	}
}

func TestHarnessModelListing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			fmt.Fprint(w, `{"models":[{"name":"a"},{"name":"b"}]}`)
		case "/api/ps":
			fmt.Fprint(w, `{"models":[{"name":"b"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	fakeOllama(t, server.Client().Transport.RoundTrip)
	all, loaded, err := harnessModels(context.Background(), server.URL)
	if err != nil || len(all) != 2 || !loaded["b"] || loaded["a"] {
		t.Fatalf("%v %v %v", all, loaded, err)
	}
}

func TestHarnessIterationCap(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_BRIDGE_STATE_DIR", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			fmt.Fprint(w, `{}`)
			return
		}
		var body struct {
			Tools []any `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Tools) == 0 {
			fmt.Fprintln(w, `{"message":{"role":"assistant","content":"Here is what I know."},"done":true}`)
			return
		}
		fmt.Fprintln(w, `{"message":{"role":"assistant","tool_calls":[{"function":{"name":"list_files","arguments":{"path":"."}}}]},"done":true}`)
	}))
	defer server.Close()
	fakeOllama(t, server.Client().Transport.RoundTrip)
	s := harnessSession{Model: "m", Cwd: dir, Role: "epm"}
	got, err := s.turn(context.Background(), "loop", server.URL)
	if err != nil || got != "Here is what I know." {
		t.Fatal(got, err)
	}
}

func TestHarnessJobResult(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_BRIDGE_STATE_DIR", dir)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			fmt.Fprint(w, `{}`)
			return
		}
		fmt.Fprintln(w, `{"message":{"role":"assistant","content":"report: v2"},"done":true,"prompt_eval_count":5,"eval_count":2}`)
	}))
	defer server.Close()
	fakeOllama(t, server.Client().Transport.RoundTrip)
	t.Setenv("OLLAMA_HOST", server.URL)
	jobDir := filepath.Join(dir, "jobs", "test-local")
	if err := os.MkdirAll(jobDir, 0700); err != nil {
		t.Fatal(err)
	}
	j := Job{ID: "test-local", Agent: "ollama", Role: "epm", Model: "m", Cwd: dir, Sandbox: "read-only", TimeLimit: "1m", EventsPath: filepath.Join(jobDir, "events.jsonl"), StderrPath: filepath.Join(jobDir, "stderr.log"), ResultPath: filepath.Join(jobDir, "result.txt")}
	if err := writeJob(j); err != nil {
		t.Fatal(err)
	}
	var events, stderr bytes.Buffer
	if err := runOllamaJob(&j, []byte("work"), &events, &stderr); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(j.ResultPath)
	if err != nil || string(b) != "report: v2" || j.Status != "completed" || j.Usage.Input != 5 {
		t.Fatalf("%q %v %+v", b, err, j)
	}
}

func TestHarnessRangesAliasAndErrors(t *testing.T) {
	dir := t.TempDir()
	s := harnessSession{Cwd: dir}
	var lines strings.Builder
	for i := 1; i <= 20000; i++ {
		fmt.Fprintf(&lines, "line %d\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "lines.txt"), []byte(lines.String()), 0600); err != nil {
		t.Fatal(err)
	}
	got := harnessTestCall(&s, context.Background(), "read_file", map[string]string{"path": "lines.txt", "line_start": "199", "line_end": "200"})
	if got != "line 199\nline 200" {
		t.Fatal(got)
	}
	numeric := harnessCall{}
	numeric.Function.Name = "read_file"
	numeric.Function.Arguments = json.RawMessage(`{"path":"lines.txt","line_start":199,"line_end":200}`)
	if got := s.executeTool(context.Background(), numeric); got != "line 199\nline 200" {
		t.Fatal(got)
	}
	if got = harnessTestCall(&s, context.Background(), "search", map[string]string{"path": dir, "pattern": "line 199"}); !strings.Contains(got, "lines.txt:199:line 199") {
		t.Fatal(got)
	}
	if got = harnessTestCall(&s, context.Background(), "read_file", map[string]string{"path": "lines.txt"}); !strings.Contains(got, "truncated:") || !strings.Contains(got, "total lines") || !strings.Contains(got, "line_start=") {
		t.Fatal(got)
	}
	call := harnessCall{}
	call.Function.Name = "missing"
	call.Function.Arguments = json.RawMessage(`{}`)
	if got = s.executeTool(context.Background(), call); !strings.Contains(got, "valid tools:") || !strings.Contains(got, "read_file") {
		t.Fatal(got)
	}
	call.Function.Name = "read_file"
	call.Function.Arguments = json.RawMessage(`{"path":"x","bogus":1}`)
	if got = s.executeTool(context.Background(), call); !strings.Contains(got, "accepted fields:") || !strings.Contains(got, "line_start") {
		t.Fatal(got)
	}
}

func harnessResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestHarnessLoopGuardAndContext(t *testing.T) {
	t.Setenv("AGENT_BRIDGE_STATE_DIR", t.TempDir())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note"), []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	requests := 0
	fakeOllama(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/show" {
			return harnessResponse(`{"model_info":{"test.context_length":8192}}`), nil
		}
		requests++
		var body struct {
			Tools   []any `json:"tools"`
			Options struct {
				NumCtx int `json:"num_ctx"`
			} `json:"options"`
			Messages []harnessMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Options.NumCtx != 8192 {
			t.Fatalf("num_ctx %d", body.Options.NumCtx)
		}
		if requests == 3 && !strings.Contains(body.Messages[len(body.Messages)-1].Content, "identical") {
			t.Fatal(body.Messages)
		}
		if requests == 4 {
			if len(body.Tools) != 0 {
				t.Fatal("loop guard sent tools")
			}
			return harnessResponse(`{"message":{"role":"assistant","content":"Known answer"},"done":true}`), nil
		}
		return harnessResponse(`{"message":{"role":"assistant","tool_calls":[{"function":{"name":"read_file","arguments":{"path":"note"}}}]},"done":true}`), nil
	})
	s := harnessSession{Model: "m", Cwd: dir}
	got, err := s.turn(context.Background(), "what am I working on?", "http://ollama.test")
	if err != nil || got != "Known answer" || requests != 4 {
		t.Fatal(got, err, requests)
	}
	if !strings.Contains(s.Messages[0].Content, "Recent projects") {
		t.Fatal(s.Messages[0])
	}
}

func TestHarnessEmptyAndThinkingOnly(t *testing.T) {
	for _, thinking := range []string{"", "planning quietly"} {
		t.Run(thinking, func(t *testing.T) {
			t.Setenv("AGENT_BRIDGE_STATE_DIR", t.TempDir())
			s := harnessSession{Model: "m", Cwd: t.TempDir(), ContextLimit: harnessContextLimit}
			requests := 0
			fakeOllama(t, func(r *http.Request) (*http.Response, error) {
				requests++
				var body struct {
					Tools []any `json:"tools"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				if requests == 2 && len(body.Tools) != 0 {
					t.Fatal("retry sent tools")
				}
				b, _ := json.Marshal(map[string]any{"message": map[string]string{"role": "assistant", "thinking": thinking}, "done": true})
				return harnessResponse(string(b)), nil
			})
			got, err := s.turn(context.Background(), "hello", "http://ollama.test")
			if err != nil || requests != 2 || !strings.Contains(got, "couldn't produce an answer") {
				t.Fatal(got, err, requests)
			}
			if thinking != "" && !strings.Contains(got, thinking) {
				t.Fatal(got)
			}
		})
	}
}

func TestHarnessEmptyRetryAnswers(t *testing.T) {
	t.Setenv("AGENT_BRIDGE_STATE_DIR", t.TempDir())
	s := harnessSession{Model: "m", Cwd: t.TempDir(), ContextLimit: harnessContextLimit}
	requests := 0
	fakeOllama(t, func(r *http.Request) (*http.Response, error) {
		requests++
		var body struct {
			Tools []any `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if requests == 1 {
			return harnessResponse(`{"message":{"role":"assistant","thinking":"thinking"},"done":true}`), nil
		}
		if len(body.Tools) != 0 {
			t.Fatal("retry sent tools")
		}
		return harnessResponse(`{"message":{"role":"assistant","content":"Answer after retry"},"done":true}`), nil
	})
	got, err := s.turn(context.Background(), "hello", "http://ollama.test")
	if err != nil || got != "Answer after retry" || requests != 2 {
		t.Fatal(got, err, requests)
	}
}

func TestHarnessCompactionKeepsUserAndSystem(t *testing.T) {
	s := harnessSession{Messages: []harnessMessage{{Role: "system", Content: "rules"}, {Role: "user", Content: "question"}, {Role: "tool", Content: strings.Repeat("result", 4000)}}, PromptTokens: 5000}
	s.compact(4096)
	if s.Messages[0].Content != "rules" || s.Messages[1].Content != "question" || !strings.Contains(s.Messages[2].Content, "omitted") {
		t.Fatal(s.Messages)
	}
}
