package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

// voiceTranscriptMaxBytes rotates the transcript into a single ".1" backup.
const voiceTranscriptMaxBytes = 5 << 20

// voiceTranscriptEntry is one JSONL line of the private voice transcript.
type voiceTranscriptEntry struct {
	TS   string `json:"ts"`
	Role string `json:"role"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// voiceTranscript appends conversation entries to <state root>/voice/transcript.jsonl.
// Unlike the journal, it holds user and agent text, so it stays 0600 in a 0700 dir.
type voiceTranscript struct {
	mu    sync.Mutex
	path  string
	limit int64
	now   func() time.Time
}

func voiceTranscriptPath() (string, error) {
	r, err := root()
	if err != nil {
		return "", err
	}
	return filepath.Join(r, "voice", "transcript.jsonl"), nil
}

func newVoiceTranscript(path string) *voiceTranscript {
	return &voiceTranscript{path: path, limit: voiceTranscriptMaxBytes, now: time.Now}
}

// Append writes one entry, rotating first when the file would exceed its limit.
func (t *voiceTranscript) Append(role, kind, text string) error {
	if t == nil || t.path == "" {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	line, err := json.Marshal(voiceTranscriptEntry{TS: t.now().Format(time.RFC3339), Role: role, Kind: kind, Text: text})
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if err = os.MkdirAll(filepath.Dir(t.path), 0700); err != nil {
		return err
	}
	if info, statErr := os.Stat(t.path); statErr == nil && info.Size() > 0 && info.Size()+int64(len(line)) > t.limit {
		if err = os.Rename(t.path, t.path+".1"); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(t.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(line)
	return errors.Join(err, f.Close())
}

// voiceRecord appends to the transcript when one is configured; failures are logged
// with a fixed label so transcript text never reaches the journal.
func voiceRecord(d voiceDeps, role, kind, text string) {
	if d.record == nil {
		return
	}
	if err := d.record(role, kind, text); err != nil {
		voiceLog(d, "voice transcript write failed")
	}
}

func voiceLogCommand(args []string) error {
	f := flag.NewFlagSet("voice-log", flag.ContinueOnError)
	follow := f.Bool("follow", false, "Keep printing new entries")
	lines := f.Int("lines", 50, "Number of recent entries to print")
	asJSON := f.Bool("json", false, "Print raw JSONL entries")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || *lines < 0 {
		return errors.New("usage: agent-bridge voice-log [--follow] [--lines 50] [--json]")
	}
	path, err := voiceTranscriptPath()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runVoiceLog(ctx, path, os.Stdout, *lines, *follow, *asJSON, 500*time.Millisecond)
}

// runVoiceLog prints the last n entries (the rotated backup included) and, when
// following, tails the live file across rotation and truncation.
func runVoiceLog(ctx context.Context, path string, w io.Writer, n int, follow, asJSON bool, poll time.Duration) error {
	var recent [][]byte
	for _, p := range []string{path + ".1", path} {
		b, err := os.ReadFile(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for _, line := range bytes.Split(b, []byte("\n")) {
			if len(bytes.TrimSpace(line)) > 0 {
				recent = append(recent, line)
			}
		}
	}
	if len(recent) > n {
		recent = recent[len(recent)-n:]
	}
	for _, line := range recent {
		writeVoiceLogLine(w, line, asJSON)
	}
	if !follow {
		if len(recent) == 0 {
			fmt.Fprintln(w, "No voice transcript yet: "+path)
		}
		return nil
	}
	f, err := os.Open(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if f != nil {
		if _, err = f.Seek(0, io.SeekEnd); err != nil {
			f.Close()
			return err
		}
	}
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	var partial []byte
	drain := func() {
		if f == nil {
			return
		}
		b, _ := io.ReadAll(f)
		partial = append(partial, b...)
		for {
			i := bytes.IndexByte(partial, '\n')
			if i < 0 {
				break
			}
			if line := partial[:i]; len(bytes.TrimSpace(line)) > 0 {
				writeVoiceLogLine(w, line, asJSON)
			}
			partial = partial[i+1:]
		}
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		drain()
		info, statErr := os.Stat(path)
		switch {
		case statErr != nil:
			// Rotated away and not recreated yet; keep the old handle.
		case f == nil:
			f, _ = os.Open(path)
			partial = nil
			continue
		default:
			cur, _ := f.Stat()
			if cur == nil || !os.SameFile(cur, info) {
				drain()
				f.Close()
				f, _ = os.Open(path)
				partial = nil
				continue
			}
			if pos, _ := f.Seek(0, io.SeekCurrent); info.Size() < pos {
				_, _ = f.Seek(0, io.SeekStart)
				partial = nil
				continue
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func writeVoiceLogLine(w io.Writer, line []byte, asJSON bool) {
	if asJSON {
		fmt.Fprintf(w, "%s\n", line)
		return
	}
	var e voiceTranscriptEntry
	if json.Unmarshal(line, &e) != nil {
		return
	}
	fmt.Fprintln(w, formatVoiceLogEntry(e))
}

// formatVoiceLogEntry renders one entry as a chat line in local time. Control
// characters are dropped so agent text cannot drive the terminal.
func formatVoiceLogEntry(e voiceTranscriptEntry) string {
	stamp := "--:--"
	if ts, err := time.Parse(time.RFC3339, e.TS); err == nil {
		stamp = ts.Local().Format("15:04")
	}
	text := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, strings.TrimSpace(e.Text))
	text = strings.ReplaceAll(text, "\n", "\n      ")
	var who string
	switch {
	case e.Kind == "ignored":
		who = "(ignored) you"
	case e.Kind == "approval_heard":
		who = "you (approval)"
	case e.Kind == "approval_request" || e.Kind == "approval_result" || e.Kind == "approval_verdict":
		who = "⚠ approval"
	case e.Kind == "error":
		who = "! error"
	case e.Role == "user":
		who = "you"
	case e.Role == "agent":
		who = "agent"
	default:
		who = "·"
	}
	return stamp + " " + who + ": " + text
}
