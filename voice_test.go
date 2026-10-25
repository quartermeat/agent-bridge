package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testVoiceFrame(level int16) []byte {
	b := make([]byte, voiceFrameSamples*2)
	for i := 0; i < len(b); i += 2 {
		binary.LittleEndian.PutUint16(b[i:], uint16(level))
	}
	return b
}
func testVoicePCM(level int16, count int) []byte { return bytes.Repeat(testVoiceFrame(level), count) }
func TestVoiceUtteranceBoundaries(t *testing.T) {
	input := append(testVoicePCM(0, 5), testVoicePCM(4000, 10)...)
	input = append(input, testVoicePCM(0, 35)...)
	input = append(input, testVoicePCM(4000, 10)...)
	reader := bytes.NewReader(input)
	got, err := readVoiceUtterance(context.Background(), reader, 0.018)
	if err != nil || len(got) == 0 || len(got) >= len(input) {
		t.Fatalf("boundary: %d bytes, %v", len(got), err)
	}
	rest, err := readVoiceUtterance(context.Background(), reader, 0.018)
	if err != io.EOF || rest != nil {
		t.Fatalf("second utterance: %d bytes, %v", len(rest), err)
	}
	long := testVoicePCM(4000, 800)
	got, err = readVoiceUtterance(context.Background(), bytes.NewReader(long), 0.018)
	if err != nil || len(got) > voiceRate*2*int(voiceMaxDuration/time.Second) {
		t.Fatalf("duration bound: %d bytes, %v", len(got), err)
	}
}
func TestVoiceWakeGate(t *testing.T) {
	var heard []string
	d := voiceDeps{reply: func(_ context.Context, prompt string) (string, error) {
		heard = append(heard, prompt)
		return "Hello.", nil
	}, speak: func(_ context.Context, text string) error {
		if text != "Hello." {
			t.Fatal(text)
		}
		return nil
	}}
	cfg := voiceConfig{wakeWord: "hey agent"}
	for _, s := range []string{"noise", "hey agency do it", "Hey Agent, first", "hey agent second"} {
		if err := handleVoiceText(context.Background(), cfg, d, s); err != nil {
			t.Fatal(err)
		}
	}
	if len(heard) != 2 || heard[0] != "first" || heard[1] != "second" {
		t.Fatal(heard)
	}
}
func TestVoicePrivateAudioAndNoSpeechLogs(t *testing.T) {
	var path string
	var logs []string
	d := voiceDeps{transcribe: func(_ context.Context, p string) (string, error) {
		path = p
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != 0600 {
			t.Fatalf("audio permission: %v %v", st, err)
		}
		dir, _ := os.Stat(filepath.Dir(p))
		if dir.Mode().Perm() != 0700 {
			t.Fatalf("directory permission: %v", dir.Mode())
		}
		return "hey agent secret question", nil
	}, reply: func(_ context.Context, _ string) (string, error) { return "secret reply", nil }, speak: func(context.Context, string) error { return nil }, logf: func(s string, a ...any) { logs = append(logs, s) }}
	if err := handleVoicePCM(context.Background(), voiceConfig{wakeWord: "hey agent"}, d, testVoicePCM(4000, 5)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("audio directory remains: %v", err)
	}
	if strings.Contains(strings.Join(logs, " "), "secret") {
		t.Fatal("speech logged")
	}
}
func TestVoiceOneShotAndErrors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "input.wav")
	if err := writeVoiceWAV(file, testVoicePCM(4000, 5)); err != nil {
		t.Fatal(err)
	}
	calls := 0
	d := voiceDeps{transcribe: func(_ context.Context, p string) (string, error) {
		if p != file {
			t.Fatal(p)
		}
		return "hello", nil
	}, reply: func(context.Context, string) (string, error) { calls++; return "ok", nil }, speak: func(context.Context, string) error { calls++; return nil }}
	cfg := voiceConfig{file: file, wakeWord: "hey agent"}
	if err := runVoiceFile(context.Background(), cfg, d); err != nil || calls != 0 {
		t.Fatalf("wake gate: %v %d", err, calls)
	}
	cfg.wakeWord = ""
	if err := runVoiceFile(context.Background(), cfg, d); err != nil || calls != 2 {
		t.Fatalf("one shot: %v %d", err, calls)
	}
	d.reply = func(context.Context, string) (string, error) { return "", errors.New("secret failure") }
	if err := handleVoiceText(context.Background(), cfg, d, "hello"); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error: %v", err)
	}
}
func TestVoiceCaptureFailureAndCancellation(t *testing.T) {
	attempts := 0
	ctx, cancel := context.WithCancel(context.Background())
	d := voiceDeps{
		capture: func(context.Context, string) (io.ReadCloser, func() error, error) {
			attempts++
			return nil, nil, errors.New("device failed")
		},
		logf:  func(string, ...any) {},
		pause: func(context.Context, time.Duration) error { cancel(); return context.Canceled },
	}
	if err := runVoice(ctx, voiceConfig{source: "fake", threshold: 0.018}, d); err != nil || attempts != 1 {
		t.Fatalf("capture retry: %v attempts=%d", err, attempts)
	}
	cancelled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err := readVoiceUtterance(cancelled, bytes.NewReader(testVoicePCM(4000, 4)), 0.018); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	waited := false
	d.capture = func(context.Context, string) (io.ReadCloser, func() error, error) {
		return io.NopCloser(bytes.NewReader(append(testVoicePCM(4000, 5), testVoicePCM(0, 35)...))), func() error { waited = true; return nil }, nil
	}
	pcm, err := captureVoiceUtterance(context.Background(), d, "fake", 0.018)
	if err != nil || len(pcm) == 0 || !waited {
		t.Fatalf("capture cleanup: %d bytes, %v, waited=%v", len(pcm), err, waited)
	}
}

func TestVoiceRejectsOversizedTextAndPlaybackFailure(t *testing.T) {
	cfg := voiceConfig{wakeWord: ""}
	called := false
	var spoken []string
	d := voiceDeps{
		reply: func(context.Context, string) (string, error) {
			called = true
			return strings.Repeat("x", voiceMaxReply+1), nil
		},
		speak: func(_ context.Context, s string) error { spoken = append(spoken, s); return nil },
	}
	if err := handleVoiceText(context.Background(), cfg, d, strings.Repeat("a", voiceMaxTranscript+1)); err == nil || called {
		t.Fatalf("transcript bound: %v called=%v", err, called)
	}
	if err := handleVoiceText(context.Background(), cfg, d, "hello"); err != nil || len(spoken) < 2 {
		t.Fatalf("long reply delivery: %v chunks=%d", err, len(spoken))
	}
	for _, chunk := range spoken {
		if len(chunk) > voiceMaxReply {
			t.Fatalf("oversized chunk: %d", len(chunk))
		}
	}
	spoken = nil
	d.reply = func(context.Context, string) (string, error) { return strings.Repeat("é", voiceMaxSpokenReply), nil }
	if err := handleVoiceText(context.Background(), cfg, d, "hello"); err != nil || !strings.Contains(spoken[len(spoken)-1], "Ask me to continue") {
		t.Fatalf("truncation notice: %v %v", err, spoken)
	}
	d.reply = func(context.Context, string) (string, error) { return "okay", nil }
	d.speak = func(context.Context, string) error { return errors.New("secret playback failure") }
	if err := handleVoiceText(context.Background(), cfg, d, "hello"); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("playback error: %v", err)
	}
}

func TestVoiceLevelTriggerAndFeedback(t *testing.T) {
	input := append(testVoicePCM(100, 5), testVoicePCM(4000, 4)...)
	input = append(input, testVoicePCM(0, 35)...)
	triggered := 0
	levels := 0
	_, err := readVoiceUtteranceObserved(context.Background(), bytes.NewReader(input), 0.018, func(level float64, trigger bool) {
		levels++
		if trigger {
			triggered++
			if level < 0.018 {
				t.Fatal("trigger below threshold")
			}
		}
	})
	if err != nil || triggered != 1 || levels < 5 {
		t.Fatalf("VAD: %v triggered=%d levels=%d", err, triggered, levels)
	}
	var spoken []string
	var logs []string
	d := voiceDeps{capture: func(context.Context, string) (io.ReadCloser, func() error, error) {
		return nil, nil, errors.New("secret device name")
	},
		speak: func(_ context.Context, s string) error { spoken = append(spoken, s); return nil },
		logf:  func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
		pause: func(context.Context, time.Duration) error { return nil }}
	if err := runVoice(context.Background(), voiceConfig{threshold: 0.018}, d); err == nil {
		t.Fatal("capture failure should exit after retries")
	}
	if len(spoken) != 1 || strings.Contains(strings.Join(logs, " "), "secret") {
		t.Fatalf("feedback=%v logs=%v", spoken, logs)
	}
}
