package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseVoiceApproval(t *testing.T) {
	cases := map[string]voiceApprovalVerdict{
		"Approve.":                  voiceVerdictApprove,
		"approved":                  voiceVerdictApprove,
		"Yes, approve!":             voiceVerdictApprove,
		"CONFIRM":                   voiceVerdictApprove,
		"Hey agent, approve.":       voiceVerdictApprove,
		"Agent approve":             voiceVerdictApprove,
		"yes":                       voiceVerdictApprove,
		"Yeah.":                     voiceVerdictApprove,
		"Yep":                       voiceVerdictApprove,
		"Yes please.":               voiceVerdictApprove,
		"Sure!":                     voiceVerdictApprove,
		"OK":                        voiceVerdictApprove,
		"Okay.":                     voiceVerdictApprove,
		"confirmed":                 voiceVerdictApprove,
		"Do it.":                    voiceVerdictApprove,
		"Go ahead.":                 voiceVerdictApprove,
		"go for it":                 voiceVerdictApprove,
		"Yes, do it.":               voiceVerdictApprove,
		"Hey agent, yes.":           voiceVerdictApprove,
		"deny":                      voiceVerdictDeny,
		"No.":                       voiceVerdictDeny,
		"Nope":                      voiceVerdictDeny,
		"denied":                    voiceVerdictDeny,
		"stop":                      voiceVerdictDeny,
		"cancel":                    voiceVerdictDeny,
		"Don't.":                    voiceVerdictDeny,
		"don’t":                     voiceVerdictDeny,
		"Do not.":                   voiceVerdictDeny,
		"Hey agent, no.":            voiceVerdictDeny,
		"I approve":                 voiceVerdictUnclear,
		"approve and also delete x": voiceVerdictUnclear,
		"yes but not that":          voiceVerdictUnclear,
		"don't approve":             voiceVerdictUnclear,
		"no, hey agent, approve":    voiceVerdictUnclear,
		"what is that":              voiceVerdictUnclear,
		"[BLANK_AUDIO]":             voiceVerdictUnclear,
		"you":                       voiceVerdictUnclear,
		"":                          voiceVerdictUnclear,
	}
	for in, want := range cases {
		if got := parseVoiceApproval(in, "hey agent"); got != want {
			t.Errorf("%q: got %v want %v", in, got, want)
		}
	}
}

func TestVoiceApprovalSummary(t *testing.T) {
	pipeline := `python3 -c "import json,sys; print(json.load(sys.stdin)['x'])" < /tmp/a.json | grep -v '^#' && echo $((1+2))`
	pipelineJSON, _ := json.Marshal(map[string]string{"command": pipeline})
	describedJSON, _ := json.Marshal(map[string]string{"command": pipeline, "description": "Count recent voice transcript entries."})
	cases := []struct {
		tool, input, spoken, detail string
	}{
		{"Bash", string(describedJSON), "Count recent voice transcript entries", "Bash: " + pipeline},
		{"Bash", string(pipelineJSON), "run a shell command, python3", "Bash: " + pipeline},
		{"Bash", `{"command":"FOO=1 /home/u/.local/bin/agent-bridge list --json"}`, "run a shell command, agent-bridge", "Bash: FOO=1 /home/u/.local/bin/agent-bridge list --json"},
		{"Bash", `{"command":"$(rm x)"}`, "run a shell command", "Bash: $(rm x)"},
		{"Bash", `{"command":"ls","description":"List files; then && run $(x)"}`, "List files then run x", "Bash: ls"},
		{"Edit", `{"file_path":"/home/u/x/main.go"}`, "edit main.go", "Edit: /home/u/x/main.go"},
		{"Write", `{"file_path":"/tmp/new.txt"}`, "write new.txt", "Write: /tmp/new.txt"},
		{"WebFetch", `{"url":"https://example.com:8080/a?b"}`, "fetch example.com", "WebFetch: https://example.com:8080/a?b"},
		{"TodoWrite", `{}`, "use todo write", "TodoWrite: {}"},
		{"mcp__srv__do_thing", `{"x":1}`, "use do thing from srv", `mcp__srv__do_thing: {"x":1}`},
	}
	for _, c := range cases {
		spoken, detail := voiceApprovalSummary(voiceApprovalRequest{ToolName: c.tool, Input: json.RawMessage(c.input)})
		if spoken != c.spoken || detail != c.detail {
			t.Errorf("%s: %q / %q", c.tool, spoken, detail)
		}
		if strings.ContainsAny(spoken, "|&$()<>;\"'`=/*{}[]") {
			t.Errorf("%s: spoken summary contains code characters: %q", c.tool, spoken)
		}
	}
}

// fakeApprovalDeps scripts transcriptions for successive approval utterances.
func fakeApprovalDeps(t *testing.T, heard []string, spoken *[]string, recorded *[]string) voiceDeps {
	i := 0
	var mu sync.Mutex
	return voiceDeps{
		capture: func(ctx context.Context, _ string) (io.ReadCloser, func() error, error) {
			if i >= len(heard) {
				<-ctx.Done()
				return io.NopCloser(bytes.NewReader(nil)), func() error { return nil }, nil
			}
			return io.NopCloser(bytes.NewReader(append(testVoicePCM(4000, 5), testVoicePCM(0, 40)...))), func() error { return nil }, nil
		},
		transcribe: func(context.Context, string) (string, error) {
			s := heard[i]
			i++
			if s == "" {
				return "", voiceFailure{code: "empty transcription"}
			}
			return s, nil
		},
		speak: func(_ context.Context, s string) error {
			mu.Lock()
			*spoken = append(*spoken, s)
			mu.Unlock()
			return nil
		},
		record: func(role, kind, text string) error { *recorded = append(*recorded, kind+":"+text); return nil },
		audio:  &sync.Mutex{},
	}
}

func TestVoiceApproverDecide(t *testing.T) {
	req := voiceApprovalRequest{ToolName: "Bash", Input: json.RawMessage(`{"command":"touch x","description":"Create file x"}`)}
	const prompt = "Approval needed: Create file x. Say yes or no."
	const reprompt = "Sorry, yes or no?"
	cases := []struct {
		name   string
		heard  []string
		allow  bool
		spoken []string
		trail  []string // approval_heard / approval_verdict records
	}{
		{"yes", []string{"Yes."}, true, []string{prompt}, []string{"approval_heard:Yes.", "approval_verdict:approve"}},
		{"go ahead", []string{"Hey agent, go ahead."}, true, []string{prompt}, []string{"approval_heard:Hey agent, go ahead.", "approval_verdict:approve"}},
		{"noise then yes", []string{"[BLANK_AUDIO]", "yeah"}, true, []string{prompt, reprompt}, []string{"approval_heard:[BLANK_AUDIO]", "approval_verdict:unclear", "approval_heard:yeah", "approval_verdict:approve"}},
		{"empty then approve", []string{"", "approved"}, true, []string{prompt, reprompt}, []string{"approval_heard:(silence)", "approval_verdict:unclear", "approval_heard:approved", "approval_verdict:approve"}},
		{"unclear then yes", []string{"what is that", "yes"}, true, []string{prompt, reprompt}, []string{"approval_heard:what is that", "approval_verdict:unclear", "approval_heard:yes", "approval_verdict:approve"}},
		{"no", []string{"no"}, false, []string{prompt}, []string{"approval_heard:no", "approval_verdict:deny"}},
		{"unclear then no", []string{"hmm maybe", "nope"}, false, []string{prompt, reprompt}, []string{"approval_heard:hmm maybe", "approval_verdict:unclear", "approval_heard:nope", "approval_verdict:deny"}},
		{"two unclear", []string{"um", "what is that", "yes"}, false, []string{prompt, reprompt}, []string{"approval_heard:um", "approval_verdict:unclear", "approval_heard:what is that", "approval_verdict:unclear"}},
		{"unclear then timeout", []string{"maybe"}, false, []string{prompt, reprompt}, []string{"approval_heard:maybe", "approval_verdict:unclear", "approval_verdict:deny (nothing heard in time)"}},
		{"timeout", nil, false, []string{prompt}, []string{"approval_verdict:deny (nothing heard in time)"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var spoken, recorded []string
			a := &voiceApprover{cfg: voiceConfig{wakeWord: "hey agent", threshold: 0.018}, d: fakeApprovalDeps(t, c.heard, &spoken, &recorded), window: 200 * time.Millisecond}
			got := a.decide(context.Background(), req)
			if got.Allow != c.allow || (!c.allow && got.Message == "") {
				t.Fatalf("decision %+v", got)
			}
			result := "Denied."
			if c.allow {
				result = "Approved."
			}
			wantSpoken := append(append([]string{}, c.spoken...), result)
			if strings.Join(spoken, "|") != strings.Join(wantSpoken, "|") {
				t.Fatalf("spoken %q want %q", spoken, wantSpoken)
			}
			want := append(append([]string{"approval_request:Bash: touch x"}, c.trail...), "approval_result:"+result+" Bash: touch x")
			if strings.Join(recorded, "|") != strings.Join(want, "|") {
				t.Fatalf("recorded %q want %q", recorded, want)
			}
		})
	}
}

// TestVoiceApproverListensAfterPlayback checks that capture never overlaps the
// spoken prompt or tone, so speech during playback cannot be taken as a reply.
func TestVoiceApproverListensAfterPlayback(t *testing.T) {
	var events []string
	var spoken, recorded []string
	d := fakeApprovalDeps(t, []string{"um", "yes"}, &spoken, &recorded)
	speak, capture := d.speak, d.capture
	playing := false
	d.speak = func(ctx context.Context, s string) error {
		playing = true
		events = append(events, "speak")
		defer func() { playing = false }()
		return speak(ctx, s)
	}
	d.tone = func(context.Context) error { events = append(events, "tone"); return nil }
	d.capture = func(ctx context.Context, src string) (io.ReadCloser, func() error, error) {
		if playing {
			t.Error("capture started during playback")
		}
		events = append(events, "capture")
		return capture(ctx, src)
	}
	a := &voiceApprover{cfg: voiceConfig{wakeWord: "hey agent", threshold: 0.018}, d: d, window: time.Second}
	if !a.decide(context.Background(), voiceApprovalRequest{ToolName: "Write", Input: json.RawMessage(`{"file_path":"/tmp/a.txt"}`)}).Allow {
		t.Fatal("want approve")
	}
	if got := strings.Join(events, ","); got != "speak,tone,capture,speak,tone,capture,speak" {
		t.Fatalf("events %s", got)
	}
	if spoken[0] != "Approval needed: write a.txt. Say yes or no." {
		t.Fatalf("prompt %q", spoken[0])
	}
}

func shortSocketDir(t *testing.T) string {
	// Unix socket paths are limited to about 108 bytes; t.TempDir can exceed that.
	dir, err := os.MkdirTemp("", "abv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestVoiceApprovalNoSocketDenies(t *testing.T) {
	path := filepath.Join(shortSocketDir(t), "missing.sock")
	got := requestVoiceApproval(context.Background(), path, voiceApprovalRequest{ToolName: "Bash", Input: json.RawMessage(`{}`)})
	if got.Allow || got.Message != voiceApprovalNoMsg {
		t.Fatalf("%+v", got)
	}
	res := voiceApprovalToolResult(context.Background(), path, voiceApprovalRequest{ToolName: "Bash", Input: json.RawMessage(`{}`)})
	text := res["content"].([]any)[0].(map[string]string)["text"]
	if text != `{"behavior":"deny","message":"voice approval unavailable"}` {
		t.Fatalf("tool result %s", text)
	}
}

func TestVoiceApprovalSocketRoundTrip(t *testing.T) {
	path := filepath.Join(shortSocketDir(t), "approval.sock")
	var seen []string
	stop, err := serveVoiceApprovals(path, func(_ context.Context, r voiceApprovalRequest) voiceApprovalDecision {
		seen = append(seen, r.ToolName)
		return voiceApprovalDecision{Allow: r.ToolName == "Write"}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if info, _ := os.Stat(path); info.Mode().Perm() != 0600 {
		t.Fatalf("socket mode %v", info.Mode())
	}
	if d := requestVoiceApproval(context.Background(), path, voiceApprovalRequest{ToolName: "Write", Input: json.RawMessage(`{"file_path":"/x"}`)}); !d.Allow {
		t.Fatalf("%+v", d)
	}
	if d := requestVoiceApproval(context.Background(), path, voiceApprovalRequest{ToolName: "Bash", Input: json.RawMessage(`{}`)}); d.Allow || d.Message == "" {
		t.Fatalf("%+v", d)
	}
	if strings.Join(seen, ",") != "Write,Bash" {
		t.Fatalf("seen %v", seen)
	}
}

func TestVoiceApprovalMCPServer(t *testing.T) {
	path := filepath.Join(shortSocketDir(t), "approval.sock")
	stop, err := serveVoiceApprovals(path, func(_ context.Context, r voiceApprovalRequest) voiceApprovalDecision {
		var in map[string]string
		_ = json.Unmarshal(r.Input, &in)
		return voiceApprovalDecision{Allow: in["command"] == "ok"}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- serveVoiceApprovalMCP(context.Background(), inR, outW, path); outW.Close() }()
	responses := bufio.NewScanner(outR)
	call := func(msg string) map[string]any {
		t.Helper()
		if _, err := io.WriteString(inW, msg+"\n"); err != nil {
			t.Fatal(err)
		}
		if !responses.Scan() {
			t.Fatal("no response")
		}
		var m map[string]any
		if err := json.Unmarshal(responses.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	init := call(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	if r := init["result"].(map[string]any); r["protocolVersion"] != "2025-06-18" || r["capabilities"].(map[string]any)["tools"] == nil {
		t.Fatalf("initialize %v", init)
	}
	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
	list := call(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "approve" {
		t.Fatalf("tools %v", list)
	}
	text := func(m map[string]any) map[string]any {
		var body map[string]any
		s := m["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
		if err := json.Unmarshal([]byte(s), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	allow := text(call(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Bash","input":{"command":"ok"},"tool_use_id":"u1"}}}`))
	if allow["behavior"] != "allow" || allow["updatedInput"].(map[string]any)["command"] != "ok" {
		t.Fatalf("allow %v", allow)
	}
	deny := text(call(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Bash","input":{"command":"rm -rf x"}}}}`))
	if deny["behavior"] != "deny" || deny["message"] == "" {
		t.Fatalf("deny %v", deny)
	}
	if bad := call(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"other","arguments":{}}}`); bad["error"] == nil {
		t.Fatalf("unknown tool %v", bad)
	}
	inW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestVoiceClaudeArgsByAccess(t *testing.T) {
	read := newVoiceClaudeAccess(voiceAccessRead, "/s.sock")
	work := newVoiceClaudeAccess(voiceAccessWork, "/s.sock")
	if read.timeout != 3*time.Minute || work.timeout != 30*time.Minute {
		t.Fatalf("timeouts %v %v", read.timeout, work.timeout)
	}
	r := strings.Join(read.args("sid"), "\n")
	for _, want := range []string{"--tools=Read,Grep,Glob", "--disallowed-tools=" + voiceClaudeDenied, "--permission-mode\ndontAsk", "--append-system-prompt\n" + voiceDeveloperInstructions, "--resume\nsid"} {
		if !strings.Contains(r, want) {
			t.Fatalf("read args missing %q: %q", want, r)
		}
	}
	if strings.Contains(r, "--mcp-config") || strings.Contains(r, "permission-prompt-tool") {
		t.Fatalf("read args %q", r)
	}
	w := work.args("")
	joined := strings.Join(w, "\n")
	for _, want := range []string{"--allowed-tools=Read,Grep,Glob", "--permission-mode\nmanual", "--strict-mcp-config", "--permission-prompt-tool\n" + voiceApprovalTool, "--disallowed-tools=" + voiceApprovalTool, "--append-system-prompt\n" + voiceWorkInstructions} {
		if !strings.Contains(joined, want) {
			t.Fatalf("work args missing %q: %q", want, joined)
		}
	}
	if strings.Contains(joined, "--tools=") || strings.Contains(joined, "--resume") {
		t.Fatalf("work args %q", joined)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	for _, a := range w {
		if strings.HasPrefix(a, "--mcp-config=") {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(a, "--mcp-config=")), &cfg); err != nil {
				t.Fatal(err)
			}
		}
	}
	srv := cfg.MCPServers[voiceApprovalServer]
	if srv.Command == "" || strings.Join(srv.Args, " ") != "voice-approval-mcp --socket /s.sock" {
		t.Fatalf("mcp config %+v", cfg)
	}
}

func TestVoiceProgressSpeaksStillWorking(t *testing.T) {
	var mu sync.Mutex
	var spoken []string
	d := voiceDeps{audio: &sync.Mutex{}, speak: func(_ context.Context, s string) error {
		mu.Lock()
		spoken = append(spoken, s)
		mu.Unlock()
		return nil
	}, reply: func(context.Context, string) (string, error) {
		time.Sleep(120 * time.Millisecond)
		return "Done.", nil
	}}
	cfg := voiceConfig{wakeWord: "hey agent", stillWorkingAfter: 30 * time.Millisecond, stillWorkingEvery: time.Hour}
	if err := handleVoiceText(context.Background(), cfg, d, "hey agent go"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(spoken, "|") != "I'm listening.|Still working.|Done." {
		t.Fatalf("spoken %q", spoken)
	}
}
