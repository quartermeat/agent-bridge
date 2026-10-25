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
	long := testVoicePCM(4000, 1600)
	got, err = readVoiceUtterance(context.Background(), bytes.NewReader(long), 0.018)
	if err != nil || len(got) > voiceMaxBytes(0) {
		t.Fatalf("duration bound: %d bytes, %v", len(got), err)
	}
}
func TestVoiceWakeGate(t *testing.T) {
	var heard []string
	d := voiceDeps{reply: func(_ context.Context, prompt string) (string, error) {
		heard = append(heard, prompt)
		return "Hello.", nil
	}, speak: func(_ context.Context, text string) error {
		if text != "Hello." && text != "I'm listening." {
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
func TestStripWakeWord(t *testing.T) {
	tests := []struct {
		name, transcript, wake, prompt string
		match                          bool
	}{
		{"plain", "Hey agent what time is it?", "hey agent", "what time is it?", true},
		{"punctuation inside", "Hey, agent, what time is it?", "hey agent", "what time is it?", true},
		{"extra space", "  Hey,   agent:  what time is it?  ", "hey agent", "what time is it?", true},
		{"punctuation separator", "Hey—agent! what time is it?", "hey agent", "what time is it?", true},
		{"leading punctuation", "...Hey, agent, what time is it?", "hey agent", "what time is it?", true},
		{"wake only", "Hey, agent!", "hey agent", "", true},
		{"leading word", "Oh, hey agent, what time is it?", "hey agent", "", false},
		{"partial first word", "Heyday agent, what time is it?", "hey agent", "", false},
		{"partial last word", "Hey, agency, what time is it?", "hey agent", "", false},
		{"underscore suffix", "Hey, agent_name, what time is it?", "hey agent", "", false},
		{"missing word", "Hey, what time is it?", "hey agent", "", false},
		{"empty wake", "  Hey, agent, what time is it?  ", "", "Hey, agent, what time is it?", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompt, match := stripWakeWord(tt.transcript, tt.wake)
			if prompt != tt.prompt || match != tt.match {
				t.Fatalf("stripWakeWord() = %q, %t; want %q, %t", prompt, match, tt.prompt, tt.match)
			}
		})
	}
}

func TestVoiceQuietSpeechThreshold(t *testing.T) {
	input := append(testVoicePCM(66, 10), testVoicePCM(328, 4)...)
	input = append(input, testVoicePCM(66, 35)...)
	triggered := 0
	pcm, _, err := readVoiceUtteranceObserved(context.Background(), bytes.NewReader(input), 0.006, 0, func(_ float64, trigger bool) {
		if trigger {
			triggered++
		}
	})
	if err != nil || triggered != 1 || len(pcm) == 0 {
		t.Fatalf("quiet speech: %d bytes, %v, triggers=%d", len(pcm), err, triggered)
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
	if err := runVoiceFile(context.Background(), cfg, d); err != nil || calls != 3 {
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
	pcm, err := captureVoiceUtterance(context.Background(), d, "fake", 0.018, 0)
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
	_, _, err := readVoiceUtteranceObserved(context.Background(), bytes.NewReader(input), 0.018, 0, func(level float64, trigger bool) {
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

func TestVoiceAcknowledgementAndStageLogs(t *testing.T) {
	var order, logs []string
	d := voiceDeps{
		transcribe: func(context.Context, string) (string, error) { return "Hey agent, private question", nil },
		reply: func(_ context.Context, prompt string) (string, error) {
			if prompt != "private question" {
				t.Fatal(prompt)
			}
			order = append(order, "reply")
			return "private answer", nil
		},
		speak: func(_ context.Context, s string) error {
			order = append(order, s)
			return nil
		},
		logf: func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
	}
	if err := handleVoicePCM(context.Background(), voiceConfig{wakeWord: "hey agent"}, d, testVoicePCM(4000, 5)); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, "|"); got != "I'm listening.|reply|private answer" {
		t.Fatal(got)
	}
	got := strings.Join(logs, "|")
	for _, stage := range []string{"transcription complete wake_match=true", "acknowledgement playback start", "acknowledgement playback complete", "agent turn start", "agent turn complete", "speech playback start", "speech playback complete"} {
		if !strings.Contains(got, stage) {
			t.Fatalf("missing %q in %q", stage, got)
		}
	}
	if strings.Contains(got, "private") {
		t.Fatal(got)
	}
}

func TestVoiceEmptyTranscriptionIgnored(t *testing.T) {
	for _, tc := range []struct {
		name       string
		transcript string
		err        error
	}{{"blank", "   ", nil}, {"process empty", "", voiceFailure{code: "empty transcription"}}} {
		t.Run(tc.name, func(t *testing.T) {
			var logs []string
			d := voiceDeps{
				transcribe: func(context.Context, string) (string, error) { return tc.transcript, tc.err },
				reply:      func(context.Context, string) (string, error) { t.Fatal("agent called"); return "", nil },
				speak:      func(context.Context, string) error { t.Fatal("speech played"); return nil },
				logf:       func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
			}
			if err := handleVoicePCM(context.Background(), voiceConfig{}, d, testVoicePCM(4000, 5)); err != nil {
				t.Fatal(err)
			}
			if len(logs) != 1 || logs[0] != "voice transcription empty; ignored" {
				t.Fatal(logs)
			}
		})
	}
}

func TestVoiceDiagnosticClassification(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{voiceFailure{code: "empty transcription"}, "empty transcription"},
		{voiceFailure{code: "invalid audio"}, "invalid audio"},
		{voiceFailure{code: "audio too long"}, "audio too long"},
		{voiceFailure{code: "private audio file failed"}, "private audio file failed"},
		{voiceFailure{code: "transcript invalid or too long"}, "transcript invalid or too long"},
	} {
		if got := voiceDiagnostic(voiceProcessFailure("transcription failed", tc.err)); got != tc.want {
			t.Fatalf("%q != %q", got, tc.want)
		}
	}
	if err := writeVoiceWAV(filepath.Join(t.TempDir(), "bad.wav"), []byte{1}); voiceDiagnostic(err) != "invalid audio" {
		t.Fatal(err)
	}
}

func TestVoiceUtteranceCapIncludesPreroll(t *testing.T) {
	limit := 2 * time.Second
	// Minutes of silence must not grow the pre-roll past its ring size.
	input := append(testVoicePCM(0, 6000), testVoicePCM(4000, 400)...)
	pcm, capped, err := readVoiceUtteranceObserved(context.Background(), bytes.NewReader(input), 0.018, limit, nil)
	if err != nil || !capped || len(pcm) != voiceMaxBytes(limit) {
		t.Fatalf("cap: %d bytes capped=%t %v; want %d", len(pcm), capped, err, voiceMaxBytes(limit))
	}
	silent := bytes.Count(pcm, testVoiceFrame(0))
	if silent > voicePrerollFrames {
		t.Fatalf("pre-roll grew to %d silent frames", silent)
	}
	short := append(testVoicePCM(0, 3000), testVoicePCM(4000, 10)...)
	short = append(short, testVoicePCM(0, 35)...)
	pcm, capped, err = readVoiceUtteranceObserved(context.Background(), bytes.NewReader(short), 0.018, limit, nil)
	if err != nil || capped || len(pcm) != (voicePrerollFrames-3+10+35)*voiceFrameSamples*2 {
		t.Fatalf("short after silence: %d bytes capped=%t %v", len(pcm), capped, err)
	}
}

func TestVoiceUtteranceAtCapIsProcessed(t *testing.T) {
	var logs []string
	transcribed := false
	d := voiceDeps{
		capture: func(context.Context, string) (io.ReadCloser, func() error, error) {
			input := append(testVoicePCM(0, 500), testVoicePCM(4000, 3000)...)
			return io.NopCloser(bytes.NewReader(input)), func() error { return nil }, nil
		},
		transcribe: func(context.Context, string) (string, error) {
			transcribed = true
			return "hey agent long question", nil
		},
		reply: func(context.Context, string) (string, error) { return "answer", nil },
		speak: func(context.Context, string) error { return nil },
		logf:  func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
	}
	cfg := voiceConfig{wakeWord: "hey agent", maxUtterance: 2 * time.Second}
	pcm, err := captureVoiceUtterance(context.Background(), d, "fake", 0.018, cfg.maxUtterance)
	if err != nil || len(pcm) != voiceMaxBytes(cfg.maxUtterance) {
		t.Fatalf("capture: %d bytes %v", len(pcm), err)
	}
	if err = handleVoicePCM(context.Background(), cfg, d, pcm); err != nil || !transcribed {
		t.Fatalf("capped utterance dropped: %v transcribed=%t", err, transcribed)
	}
	if !strings.Contains(strings.Join(logs, "|"), "voice utterance hit length cap") {
		t.Fatal(logs)
	}
	if err = writeVoiceWAVLimit(filepath.Join(t.TempDir(), "x.wav"), append(pcm, 0, 0), cfg.maxUtterance); voiceDiagnostic(err) != "audio too long" {
		t.Fatalf("over cap: %v", err)
	}
}

type voiceFollowUpHarness struct {
	now     time.Time
	prompts []string
	spoken  []string
	logs    []string
	tones   int
	d       voiceDeps
	state   voiceFollowUpState
}

func newVoiceFollowUpHarness() *voiceFollowUpHarness {
	h := &voiceFollowUpHarness{now: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)}
	h.d = voiceDeps{
		reply: func(_ context.Context, p string) (string, error) {
			h.prompts = append(h.prompts, p)
			return "answer", nil
		},
		speak: func(_ context.Context, s string) error { h.spoken = append(h.spoken, s); return nil },
		logf:  func(format string, args ...any) { h.logs = append(h.logs, fmt.Sprintf(format, args...)) },
		now:   func() time.Time { return h.now },
		tone:  func(context.Context) error { h.tones++; return nil },
	}
	return h
}
func (h *voiceFollowUpHarness) say(t *testing.T, cfg voiceConfig, text string) {
	t.Helper()
	if err := handleVoiceTurn(context.Background(), cfg, h.d, &h.state, h.now, text); err != nil {
		t.Fatal(err)
	}
}

func TestVoiceFollowUpWindow(t *testing.T) {
	cfg := voiceConfig{wakeWord: "hey agent", followUp: 20 * time.Second}
	h := newVoiceFollowUpHarness()
	h.say(t, cfg, "what is the weather")
	if len(h.prompts) != 0 {
		t.Fatal("accepted without wake word before any reply")
	}
	h.say(t, cfg, "Hey agent, first question")
	if got := strings.Join(h.spoken, "|"); got != "I'm listening.|answer" {
		t.Fatal(got)
	}
	h.now = h.now.Add(10 * time.Second)
	h.spoken = nil
	h.say(t, cfg, "Um.") // noise: ignored, window kept
	h.say(t, cfg, "and a second question")
	if len(h.prompts) != 2 || h.prompts[1] != "and a second question" {
		t.Fatal(h.prompts)
	}
	if got := strings.Join(h.spoken, "|"); got != "answer" {
		t.Fatalf("follow-up acknowledged: %s", got)
	}
	h.now = h.now.Add(19 * time.Second) // window reset by the last reply
	h.say(t, cfg, "hey agent third one please")
	if len(h.prompts) != 3 || h.prompts[2] != "third one please" {
		t.Fatal(h.prompts)
	}
	h.now = h.now.Add(21 * time.Second)
	h.say(t, cfg, "too late for this one")
	if len(h.prompts) != 3 {
		t.Fatal("accepted after the window closed")
	}
	logs := strings.Join(h.logs, "|")
	if !strings.Contains(logs, "wake_match=false follow_up=true") || !strings.Contains(logs, "wake_match=true follow_up=false") {
		t.Fatal(logs)
	}
	for _, private := range []string{"question", "late", "third"} {
		if strings.Contains(logs, private) {
			t.Fatalf("transcript logged: %s", logs)
		}
	}
}

func TestVoiceFollowUpDisabledAndFailureClosesWindow(t *testing.T) {
	h := newVoiceFollowUpHarness()
	cfg := voiceConfig{wakeWord: "hey agent"}
	h.say(t, cfg, "hey agent first question")
	h.now = h.now.Add(time.Second)
	h.say(t, cfg, "a follow-up question")
	if len(h.prompts) != 1 || !h.state.until.IsZero() {
		t.Fatalf("disabled follow-up: %v until=%v", h.prompts, h.state.until)
	}
	cfg.followUp = 20 * time.Second
	h.say(t, cfg, "hey agent first question")
	h.d.reply = func(context.Context, string) (string, error) { return "", errors.New("down") }
	if err := handleVoiceTurn(context.Background(), cfg, h.d, &h.state, h.now, "this one fails"); err == nil {
		t.Fatal("expected agent failure")
	}
	if !h.state.until.IsZero() {
		t.Fatal("failed reply left the window open")
	}
}

func TestVoiceDurationFlagBounds(t *testing.T) {
	for _, args := range [][]string{{"--max-utterance", "1s"}, {"--max-utterance", "61s"}, {"--follow-up", "-1s"}, {"--follow-up", "121s"}} {
		if err := voiceCommand(args); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

func TestVoiceFollowUpToneAndOneWord(t *testing.T) {
	cfg := voiceConfig{wakeWord: "hey agent", followUp: 20 * time.Second}
	h := newVoiceFollowUpHarness()
	h.say(t, cfg, "hey agent first question")
	if h.tones != 0 {
		t.Fatal("tone played for a wake-word start")
	}
	h.now = h.now.Add(time.Second)
	h.say(t, cfg, "Yes.")
	if len(h.prompts) != 2 || h.prompts[1] != "Yes." || h.tones != 1 {
		t.Fatalf("one-word follow-up: %v tones=%d", h.prompts, h.tones)
	}
	if got := strings.Join(h.spoken, "|"); got != "I'm listening.|answer|answer" {
		t.Fatal("follow-up acknowledged with speech")
	}
	h.d.tone = func(context.Context) error { return errors.New("no paplay") }
	h.say(t, cfg, "go on")
	if len(h.prompts) != 3 {
		t.Fatal("tone failure aborted the turn")
	}
	logs := strings.Join(h.logs, "|")
	if !strings.Contains(logs, "voice follow-up tone playback|") || !strings.Contains(logs, "voice follow-up tone playback failed") {
		t.Fatal(logs)
	}
}

func TestVoiceFollowUpNoise(t *testing.T) {
	for _, noise := range []string{"you", "You.", "Thank you!", "thanks for watching", "Bye.", "uh", "Um...", "Hmm?", "blank audio", "[BLANK_AUDIO]", "(music)", " [Music] (applause) ", "...", ""} {
		if !voiceFollowUpNoise(noise) {
			t.Errorf("%q not treated as noise", noise)
		}
	}
	for _, real := range []string{"yes", "No.", "thank you very much", "you there?", "stop", "[pause] continue"} {
		if voiceFollowUpNoise(real) {
			t.Errorf("%q treated as noise", real)
		}
	}
	cfg := voiceConfig{wakeWord: "hey agent", followUp: 20 * time.Second}
	h := newVoiceFollowUpHarness()
	h.say(t, cfg, "hey agent hello")
	h.say(t, cfg, "[BLANK_AUDIO]")
	h.say(t, cfg, "Thank you.")
	if len(h.prompts) != 1 || h.state.until.IsZero() || h.tones != 0 {
		t.Fatalf("noise consumed the window: %v", h.prompts)
	}
	if !strings.Contains(strings.Join(h.logs, "|"), "follow-up noise ignored") {
		t.Fatal(h.logs)
	}
}

func TestVoiceToneWAVAndPlayerFile(t *testing.T) {
	wav := voiceToneWAV()
	if string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" || binary.LittleEndian.Uint32(wav[24:]) != voiceToneRate {
		t.Fatal("bad WAV header")
	}
	samples := (len(wav) - 44) / 2
	if samples != voiceToneRate*150/1000 {
		t.Fatalf("samples=%d", samples)
	}
	peak := 0
	for i := 0; i < samples; i++ {
		v := int(int16(binary.LittleEndian.Uint16(wav[44+2*i:])))
		if v < 0 {
			v = -v
		}
		peak = max(peak, v)
	}
	first := int16(binary.LittleEndian.Uint16(wav[44:]))
	last := int16(binary.LittleEndian.Uint16(wav[len(wav)-2:]))
	if peak < 6000 || peak > 9000 || first != 0 || last > 50 || last < -50 {
		t.Fatalf("peak=%d first=%d last=%d", peak, first, last)
	}
	t.Setenv("TMPDIR", t.TempDir())
	p := &voiceTonePlayer{}
	path, err := p.file()
	if err != nil {
		t.Fatal(err)
	}
	again, _ := p.file()
	st, err := os.Stat(path)
	if err != nil || again != path || st.Mode().Perm() != 0600 || st.Size() != int64(len(wav)) {
		t.Fatalf("tone file %v %v", st, err)
	}
	if err = p.close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("tone directory not removed")
	}
}
