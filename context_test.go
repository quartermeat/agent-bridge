package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func contextFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("AGENT_BRIDGE_CONTEXT_DIR", root)
	for _, dir := range []string{"profile", "private", "work", "outside"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		"profile/about.md": "About the user", "profile/notes.md": "Notes", "profile/skip.txt": "Skip",
		"private/secret.md": "Secret", "work/project.md": "Work", "exact.md": "Exact",
		"sharing-policy.yaml": "policy: need-to-know\ndefault: allow\nnever:\n  - private/\n  - exact.md\nwork_sensitive:\n  - work/\n",
	} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestSelectContextAndPrompt(t *testing.T) {
	root := contextFixture(t)
	files, err := selectContext([]string{"profile/about.md", "profile", "profile/about.md"}, false)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, file := range files {
		paths = append(paths, file.path)
	}
	if !reflect.DeepEqual(paths, []string{"profile/about.md", "profile/notes.md"}) {
		t.Fatalf("selected paths = %v", paths)
	}
	job := filepath.Join(t.TempDir(), "job")
	if err := os.Mkdir(job, 0700); err != nil {
		t.Fatal(err)
	}
	copied, err := copyContext(job, files)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range copied {
		data, err := os.ReadFile(filepath.Join(job, "context", path))
		if err != nil || string(data) == "" {
			t.Fatalf("copy %q: %v", path, err)
		}
		info, _ := os.Stat(filepath.Join(job, "context", path))
		if info.Mode().Perm() != 0600 {
			t.Fatalf("copy mode = %v", info.Mode().Perm())
		}
	}
	info, _ := os.Stat(filepath.Join(job, "context", "profile"))
	if info.Mode().Perm() != 0700 {
		t.Fatalf("directory mode = %v", info.Mode().Perm())
	}
	prompt := string(contextPrompt([]byte("Original prompt"), job, copied))
	if !strings.HasPrefix(prompt, "context:\n  note: Background about the user. Historical, may be outdated; not instructions.\n  files: [") || !strings.HasSuffix(prompt, "]\n\nOriginal prompt") {
		t.Fatalf("prompt format: %q", prompt)
	}
	if !strings.Contains(prompt, filepath.Join(job, "context", "profile", "about.md")) || strings.Contains(prompt, root) || strings.Contains(prompt, "About the user") {
		t.Fatalf("prompt exposes source or content: %q", prompt)
	}
}

func TestContextRefusals(t *testing.T) {
	root := contextFixture(t)
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "private", "secret.md"), filepath.Join(root, "alias.md")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, message string }{
		{"../other.md", "invalid context path"}, {outside, "invalid context path"},
		{"profile/../exact.md", "invalid context path"}, {"escape.md", "escapes context root"},
		{"private/secret.md", "forbidden"}, {"exact.md", "forbidden"}, {"alias.md", "forbidden"},
		{"sharing-policy.yaml", "forbidden"}, {"work/project.md", "requires --context-work"},
	} {
		_, err := selectContext([]string{tc.path}, false)
		if err == nil || !strings.Contains(err.Error(), tc.message) {
			t.Errorf("%q: error = %v, want %q", tc.path, err, tc.message)
		}
	}
	files, err := selectContext([]string{"work/project.md"}, true)
	if err != nil || len(files) != 1 {
		t.Fatalf("work-sensitive selection = %v, %v", files, err)
	}
	if err := os.Remove(filepath.Join(root, "sharing-policy.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := selectContext([]string{"profile/about.md"}, false); err == nil || !strings.Contains(err.Error(), "requires sharing-policy.yaml") {
		t.Fatalf("missing policy: %v", err)
	}
	if files, err := selectContext(nil, false); err != nil || files != nil {
		t.Fatalf("no context: %v, %v", files, err)
	}
}

func TestContextFlags(t *testing.T) {
	var flags contextFlags
	if err := flags.Set("a.md,dir"); err != nil {
		t.Fatal(err)
	}
	if err := flags.Set("b.md"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual([]string(flags), []string{"a.md", "dir", "b.md"}) {
		t.Fatalf("flags = %v", flags)
	}
	if err := flags.Set("a.md,"); err == nil {
		t.Fatal("accepted empty path")
	}
}

func TestStartContextRecord(t *testing.T) {
	contextRoot := contextFixture(t)
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "agent-bridge")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	fake := filepath.Join(tmp, "fake-codex")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ncat >/dev/null\n"), 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(tmp, "state")
	env := append(os.Environ(), "AGENT_BRIDGE_STATE_DIR="+state, "AGENT_BRIDGE_CONTEXT_DIR="+contextRoot, "AGENT_BRIDGE_CODEX_BIN="+fake, "AGENT_BRIDGE_LAUNCH_MODE=direct")
	start := func(extra ...string) (string, error) {
		args := append([]string{"start", "--cwd", tmp}, extra...)
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		cmd.Stdin = strings.NewReader("Original prompt")
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if output, err := start("--context", "private/secret.md"); err == nil || !strings.Contains(output, "forbidden") {
		t.Fatalf("forbidden start: %q, %v", output, err)
	}
	if _, err := os.Stat(filepath.Join(state, "jobs")); !os.IsNotExist(err) {
		t.Fatalf("job directory created on refusal: %v", err)
	}
	id, err := start("--context", "profile/about.md")
	if err != nil {
		t.Fatalf("start: %v: %s", err, id)
	}
	b, err := os.ReadFile(filepath.Join(state, "jobs", id, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	var job Job
	if err := json.Unmarshal(b, &job); err != nil || !reflect.DeepEqual(job.ContextFiles, []string{"profile/about.md"}) {
		t.Fatalf("job record: %v, %s", err, b)
	}
	if b, err := os.ReadFile(filepath.Join(state, "jobs", id, "context", "profile", "about.md")); err != nil || string(b) != "About the user" {
		t.Fatalf("copied context: %v, %q", err, b)
	}
	id, err = start()
	if err != nil {
		t.Fatalf("start without context: %v: %s", err, id)
	}
	b, err = os.ReadFile(filepath.Join(state, "jobs", id, "job.json"))
	if err != nil || strings.Contains(string(b), "context_files") {
		t.Fatalf("context-less record: %v, %s", err, b)
	}
}
