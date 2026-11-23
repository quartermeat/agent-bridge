package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixture(t *testing.T, id, agent string) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("AGENT_BRIDGE_STATE_DIR", root)
	dir := filepath.Join(root, "jobs", id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"id": id, "launcher": "direct", "status": "running", "agent": agent, "events_path": "/etc/passwd", "result_path": "/etc/passwd"})
	if err := os.WriteFile(filepath.Join(dir, "job.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}
func TestTargetIdentityAndIsolation(t *testing.T) {
	dir := fixture(t, "job-1", "codex")
	for _, id := range []string{"../job-1", "job-1/x", ".hidden", "job 1"} {
		if _, _, err := target(id); err == nil {
			t.Fatalf("accepted %q", id)
		}
	}
	if _, _, err := target("job-1"); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"id": "other", "launcher": "direct", "status": "running"})
	_ = os.WriteFile(filepath.Join(dir, "job.json"), b, 0600)
	if _, _, err := target("job-1"); err == nil {
		t.Fatal("mismatched ID accepted")
	}
	b, _ = json.Marshal(map[string]string{"id": "job-1", "launcher": "systemd"})
	_ = os.WriteFile(filepath.Join(dir, "job.json"), b, 0600)
	if _, _, err := target("job-1"); err == nil {
		t.Fatal("non-direct job accepted")
	}
	_ = os.Remove(filepath.Join(dir, "job.json"))
	_ = os.Symlink("/etc/passwd", filepath.Join(dir, "job.json"))
	if _, _, err := target("job-1"); err == nil {
		t.Fatal("symlink accepted")
	}
}
func TestLiveOutputAndFiltering(t *testing.T) {
	dir := fixture(t, "one", "codex")
	lines := []string{`{"type":"item.completed","item":{"type":"agent_message","text":"First progress"}}`, `{"type":"item.completed","item":{"type":"command_execution","aggregated_output":"tool secret"}}`, `{"type":"item.completed","item":{"type":"reasoning","text":"reasoning secret"}}`, `{"type":"turn.started","prompt":"prompt secret"}`}
	_ = os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0600)
	_ = os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("input secret"), 0600)
	_ = os.WriteFile(filepath.Join(dir, "stderr.log"), []byte("stderr secret"), 0600)
	other := filepath.Join(filepath.Dir(dir), "other")
	_ = os.MkdirAll(other, 0700)
	_ = os.WriteFile(filepath.Join(other, "events.jsonl"), []byte(`{"type":"item.completed","item":{"type":"agent_message","text":"other job secret"}}`), 0600)
	v := &viewer{id: "one", dir: dir, token: strings.Repeat("a", 64)}
	s := httptest.NewServer(http.HandlerFunc(v.handle))
	defer s.Close()
	u := s.URL + "/v/" + v.token + "/data"
	get := func() string {
		t.Helper()
		r, e := http.Get(u)
		if e != nil {
			t.Fatal(e)
		}
		defer r.Body.Close()
		if r.StatusCode != 200 {
			t.Fatalf("status %d", r.StatusCode)
		}
		var body struct{ ID, Status, Output string }
		if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
			t.Fatal(e)
		}
		if body.ID != "one" || body.Status != "running" {
			t.Fatalf("wrong target: %+v", body)
		}
		return body.Output
	}
	if out := get(); out != "First progress" {
		t.Fatalf("unexpected output %q", out)
	}
	f, _ := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = fmt.Fprintln(f, `{"type":"item.completed","item":{"type":"agent_message","text":"Second progress"}}`)
	_ = f.Close()
	if out := get(); out != "First progress\n\nSecond progress" {
		t.Fatalf("no live update: %q", out)
	}
	_ = os.WriteFile(filepath.Join(dir, "result.txt"), []byte("Final answer"), 0600)
	if out := get(); !strings.HasSuffix(out, "Final answer") {
		t.Fatal(out)
	}
	_ = os.Remove(filepath.Join(dir, "result.txt"))
	_ = os.Symlink(filepath.Join(dir, "prompt.txt"), filepath.Join(dir, "result.txt"))
	if out := get(); strings.Contains(out, "input secret") {
		t.Fatalf("followed result symlink: %q", out)
	}
	for _, path := range []string{"/", "/v/" + strings.Repeat("b", 64), "/v/" + v.token + "/../other", "/v/" + v.token + "/data/other"} {
		r, _ := http.Get(s.URL + path)
		r.Body.Close()
		if r.StatusCode != 404 {
			t.Fatalf("path %q: %d", path, r.StatusCode)
		}
	}
	req, _ := http.NewRequest("POST", u, nil)
	r, _ := http.DefaultClient.Do(req)
	r.Body.Close()
	if r.StatusCode != 404 {
		t.Fatalf("POST: %d", r.StatusCode)
	}
}
func TestClaudeTextOnly(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"content":[{"type":"thinking","text":"hidden"},{"type":"text","text":"Visible"},{"type":"tool_use","text":"tool hidden"}]}}`)
	if got := visibleEvent(line, "claude"); got != "Visible" {
		t.Fatal(got)
	}
}
func TestRepeatedLaunchReusesInstance(t *testing.T) {
	dir := fixture(t, "repeat", "codex")
	token := strings.Repeat("a", 64)
	v := &viewer{id: "repeat", dir: dir, token: token}
	server := httptest.NewServer(http.HandlerFunc(v.handle))
	defer server.Close()
	var port int
	_, _ = fmt.Sscanf(server.URL, "http://127.0.0.1:%d", &port)
	stateDir := filepath.Join(filepath.Dir(filepath.Dir(dir)), "viewers")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(stateDir, "repeat.json")
	data, _ := json.Marshal(instance{Port: port, Token: token})
	if err := os.WriteFile(state, data, 0600); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(t.TempDir(), "am")
	log := filepath.Join(t.TempDir(), "calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\n"
	if err := os.WriteFile(stub, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WIDGET_JOB_VIEW_AM", stub)
	if err := run([]string{"repeat"}); err != nil {
		t.Fatal(err)
	}
	first, ok := readInstance(state)
	if !ok || !healthy(first) {
		t.Fatal("first server not healthy")
	}
	if err := run([]string{"repeat"}); err != nil {
		t.Fatal(err)
	}
	second, ok := readInstance(state)
	if !ok || first != second {
		t.Fatalf("server replaced: %+v %+v", first, second)
	}
	b, _ := os.ReadFile(log)
	calls := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(calls) != 2 || calls[0] != calls[1] || !strings.Contains(calls[0], "com.android.browser.application_id widget-job-view.repeat") {
		t.Fatalf("browser calls: %q", b)
	}
}
func TestConcurrentReuseAndIdleLifecycle(t *testing.T) {
	dir := fixture(t, "concurrent", "codex")
	token := strings.Repeat("c", 64)
	v := &viewer{id: "concurrent", dir: dir, token: token}
	v.touch()
	server := httptest.NewServer(http.HandlerFunc(v.handle))
	defer server.Close()
	var port int
	_, _ = fmt.Sscanf(server.URL, "http://127.0.0.1:%d", &port)
	stateDir := filepath.Join(filepath.Dir(filepath.Dir(dir)), "viewers")
	_ = os.MkdirAll(stateDir, 0700)
	meta := filepath.Join(stateDir, "concurrent.json")
	data, _ := json.Marshal(instance{Port: port, Token: token})
	_ = os.WriteFile(meta, data, 0600)
	stub := filepath.Join(t.TempDir(), "am")
	_ = os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0700)
	t.Setenv("WIDGET_JOB_VIEW_AM", stub)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- run([]string{"concurrent"}) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, ok := readInstance(meta)
	if !ok || got.Token != token {
		t.Fatalf("instance changed: %+v", got)
	}
	v.last.Store(time.Now().Add(-idleTimeout - time.Second).UnixNano())
	if !v.expired() {
		t.Fatal("old viewer did not expire")
	}
	r, err := http.Get(server.URL + "/v/" + token + "/health")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if v.expired() {
		t.Fatal("active viewer expired")
	}
}

func TestStatusNameAndBoundedEventTail(t *testing.T) {
	dir := fixture(t, "named", "codex")
	writeJob := func(status string) {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"id": "named", "launcher": "direct", "status": status, "agent": "codex", "name": "Release <A> 🚀"})
		if err := os.WriteFile(filepath.Join(dir, "job.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeJob("starting")
	log := filepath.Join(dir, "events.jsonl")
	f, err := os.Create(log)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"item.completed","item":{"type":"agent_message","text":"old secret"}}` + "\n")
	_, _ = f.WriteString(strings.Repeat("x", int(eventTailBytes)))
	_, _ = f.WriteString("\n" + `{"type":"item.completed","item":{"type":"agent_message","text":"Recent café 🚀"}}` + "\n")
	_, _ = f.WriteString(`{"type":"item.completed","item":{"type":"agent_message","text":"unfinished`)
	_ = f.Close()
	v := &viewer{id: "named", dir: dir, token: strings.Repeat("d", 64)}
	s := httptest.NewServer(http.HandlerFunc(v.handle))
	defer s.Close()
	base := s.URL + "/v/" + v.token
	r, err := http.Get(base)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if !strings.Contains(string(page), "Job Release &lt;A&gt; 🚀") || strings.Contains(string(page), "Job named</h1>") {
		t.Fatalf("page name: %s", page)
	}
	check := func(wantStatus string) {
		t.Helper()
		r, err := http.Get(base + "/data")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var data struct{ Name, Status, Output string }
		if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
			t.Fatal(err)
		}
		if data.Name != "Release <A> 🚀" || data.Status != wantStatus || data.Output != "Recent café 🚀" {
			t.Fatalf("snapshot: %+v", data)
		}
	}
	check("starting")
	writeJob("completed")
	check("completed")
}
