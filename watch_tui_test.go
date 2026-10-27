package main

import (
	"strings"
	"testing"
	"time"
)

func TestWatchScreenClippingAndScroll(t *testing.T) {
	lines := []string{"zero", "one", "two", "three", "four", "five"}
	v := watchView{offset: 2}
	frame := watchScreen(lines, 12, 4, &v, "daemon", time.Unix(0, 0))
	if strings.Contains(frame, "zero") || strings.Contains(frame, "five") || !strings.Contains(frame, "two") || !strings.Contains(frame, "four") {
		t.Fatalf("bad viewport: %q", frame)
	}
	statusFrame := watchScreen(lines, 120, 4, &v, "daemon", time.Unix(0, 0))
	if !strings.Contains(statusFrame, "lines 3–5") {
		t.Fatalf("status range: %q", frame)
	}
	if !applyWatchKey(&v, "bottom", 6, 4) || v.offset != 3 {
		t.Fatalf("bottom: %+v", v)
	}
	applyWatchKey(&v, "pageUp", 6, 4)
	if v.offset != 1 {
		t.Fatalf("page up: %+v", v)
	}
	v.offset = 99
	watchScreen(lines, 20, 4, &v, "direct", time.Now())
	if v.offset != 3 {
		t.Fatalf("clamp: %+v", v)
	}
}
func TestANSITruncation(t *testing.T) {
	input := "\x1b[31mab界c\x1b[0m"
	got := truncateANSI(input, 4, true)
	if got != "\x1b[31mab界" {
		t.Fatalf("truncated %q", got)
	}
	if plainText(input) != "ab界c" {
		t.Fatalf("plain %q", plainText(input))
	}
	if truncateANSI("e\u0301x", 1, true) != "e\u0301" {
		t.Fatal("combining rune split")
	}
}
func TestWatchSelection(t *testing.T) {
	lines := []string{"title", "*** ALERT ***", "claude  active               model  /tmp", "codex   ready                model  /tmp", "local   loaded               model  /tmp", "", "Bridge jobs:", "  j1                       running    work", "  j2                       completed  done", "", "Steinberger:", "  healthy", "", "VRAM:", "  GPU 0"}
	active := strings.Join(selectWatchLines(lines, filterActive, false), "\n")
	if !strings.Contains(active, "running") || strings.Contains(active, "completed") || strings.Contains(active, "ready") {
		t.Fatalf("active: %s", active)
	}
	jobs := strings.Join(selectWatchLines(lines, filterJobs, false), "\n")
	if strings.Contains(jobs, "GPU 0") || !strings.Contains(jobs, "j1") {
		t.Fatalf("jobs: %s", jobs)
	}
	local := strings.Join(selectWatchLines(lines, filterLocal, false), "\n")
	if !strings.Contains(local, "local") || !strings.Contains(local, "GPU 0") || strings.Contains(local, "j1") {
		t.Fatalf("local: %s", local)
	}
	compact := strings.Join(selectWatchLines(lines, filterAll, true), "\n")
	if strings.Contains(compact, "ready") || strings.Contains(compact, "completed") || strings.Contains(compact, "GPU 0") || !strings.Contains(compact, "j1") {
		t.Fatalf("compact: %s", compact)
	}
}
func TestKeyDecoder(t *testing.T) {
	cases := map[string]string{"j": "down", "k": "up", " ": "pageDown", "g": "top", "G": "bottom", "c": "compact", "f": "filter", "q": "quit", "\x1b[A": "up", "\x1b[B": "down", "\x1b[5~": "pageUp", "\x1b[6~": "pageDown"}
	for input, want := range cases {
		d := keyDecoder{}
		got := ""
		for i := 0; i < len(input); i++ {
			got = d.feed(input[i])
		}
		if got != want {
			t.Errorf("%q: %q want %q", input, got, want)
		}
	}
}
func TestPlainFrameNoEscapes(t *testing.T) {
	s := Snapshot{SampledAt: time.Now(), GPUs: []GPU{{Index: 0, MemoryUsedMiB: 95, MemoryTotalMiB: 100}}}
	frame := plainText(strings.Join(renderWatchFrame(s, "direct", map[string]TokenUsage{}), "\n"))
	if strings.Contains(frame, "\x1b") || !strings.Contains(frame, "HIGH VRAM") || !strings.Contains(frame, "Bridge jobs:") {
		t.Fatalf("plain frame: %q", frame)
	}
}
