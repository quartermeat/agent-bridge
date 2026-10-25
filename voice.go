package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	voiceRate           = 16000
	voiceFrameSamples   = 320
	voiceMaxDuration    = 12 * time.Second
	voiceMaxTranscript  = 2048
	voiceMaxReply       = 700
	voiceMaxSpokenReply = 4200
)

// voiceFailure contains only fixed labels and process status, never subprocess output.
type voiceFailure struct {
	code   string
	status string
}

func (e voiceFailure) Error() string {
	if e.status != "" {
		return e.code + " (" + e.status + ")"
	}
	return e.code
}
func voiceProcessFailure(code string, err error) error {
	var failure voiceFailure
	if errors.As(err, &failure) {
		return failure
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return voiceFailure{code, "exit " + strconv.Itoa(exit.ExitCode())}
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return voiceFailure{code, "executable missing"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return voiceFailure{code, "timeout"}
	}
	return voiceFailure{code, "start or I/O error"}
}

type voiceConfig struct {
	source, wakeWord, file string
	threshold              float64
}
type voiceDeps struct {
	capture    func(context.Context, string) (io.ReadCloser, func() error, error)
	transcribe func(context.Context, string) (string, error)
	reply      func(context.Context, string) (string, error)
	close      func() error
	speak      func(context.Context, string) error
	logf       func(string, ...any)
	pause      func(context.Context, time.Duration) error
	observe    func(float64, bool)
}

func voiceCommand(args []string) error {
	f := flag.NewFlagSet("voice", flag.ContinueOnError)
	source := f.String("source", "default", "PulseAudio source name")
	wake := f.String("wake-word", "hey agent", "Wake phrase; empty accepts every utterance")
	threshold := f.Float64("vad-threshold", 0.018, "RMS speech threshold from 0 to 1")
	file := f.String("file", "", "Transcribe one existing WAV file and exit")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || *source == "" || math.IsNaN(*threshold) || *threshold <= 0 || *threshold >= 1 {
		return errors.New("usage: agent-bridge voice [--source NAME] [--wake-word PHRASE] [--vad-threshold 0.018] [--file WAV]")
	}
	cfg := voiceConfig{source: *source, wakeWord: *wake, file: *file, threshold: *threshold}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	d := realVoiceDeps()
	defer d.close()
	if cfg.file != "" {
		return runVoiceFile(ctx, cfg, d)
	}
	return runVoice(ctx, cfg, d)
}

func realVoiceDeps() voiceDeps {
	agent := newVoiceAgent()
	lastLevel := time.Time{}
	return voiceDeps{capture: startVoiceCapture, transcribe: transcribeVoice, reply: agent.Reply, close: agent.Close, speak: speakVoice, logf: log.Printf, pause: voicePause,
		observe: func(level float64, trigger bool) {
			if trigger || time.Since(lastLevel) >= 5*time.Second {
				log.Printf("voice microphone rms=%.3f trigger=%t", level, trigger)
				lastLevel = time.Now()
			}
		}}
}
func voicePause(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func startVoiceCapture(ctx context.Context, source string) (io.ReadCloser, func() error, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, nil, voiceFailure{code: "capture ffmpeg missing"}
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-f", "pulse", "-i", source, "-ac", "1", "-ar", "16000", "-f", "s16le", "pipe:1")
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, voiceFailure{code: "capture pipe failed"}
	}
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		return nil, nil, voiceProcessFailure("capture start failed", err)
	}
	return pipe, cmd.Wait, nil
}

// readVoiceUtterance keeps a short pre-roll and closes an utterance after quiet frames.
func readVoiceUtterance(ctx context.Context, input io.Reader, threshold float64) ([]byte, error) {
	return readVoiceUtteranceObserved(ctx, input, threshold, nil)
}
func readVoiceUtteranceObserved(ctx context.Context, input io.Reader, threshold float64, observe func(float64, bool)) ([]byte, error) {
	frame := make([]byte, voiceFrameSamples*2)
	preroll := make([][]byte, 0, 10)
	var utterance []byte
	speechFrames, quietFrames := 0, 0
	maxFrames := int(voiceMaxDuration / (20 * time.Millisecond))
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		_, err := io.ReadFull(input, frame)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, io.EOF
			}
			return nil, err
		}
		level := voiceEnergy(frame)
		active := level >= threshold
		if len(utterance) == 0 {
			if len(preroll) == cap(preroll) {
				preroll = preroll[1:]
			}
			preroll = append(preroll, bytes.Clone(frame))
			if active {
				speechFrames++
			} else {
				speechFrames = 0
			}
			if speechFrames >= 3 {
				if observe != nil {
					observe(level, true)
				}
				for _, p := range preroll {
					utterance = append(utterance, p...)
				}
				quietFrames = 0
			}
			if observe != nil && speechFrames < 3 {
				observe(level, false)
			}
			continue
		}
		if observe != nil {
			observe(level, false)
		}
		utterance = append(utterance, frame...)
		if active {
			quietFrames = 0
		} else {
			quietFrames++
		}
		if quietFrames >= 35 || len(utterance) >= maxFrames*len(frame) {
			return utterance, nil
		}
	}
}
func voiceEnergy(frame []byte) float64 {
	var sum float64
	for i := 0; i+1 < len(frame); i += 2 {
		n := float64(int16(binary.LittleEndian.Uint16(frame[i:]))) / 32768
		sum += n * n
	}
	return math.Sqrt(sum / float64(len(frame)/2))
}

func captureVoiceUtterance(ctx context.Context, d voiceDeps, source string, threshold float64) ([]byte, error) {
	captureCtx, cancel := context.WithCancel(ctx)
	input, wait, err := d.capture(captureCtx, source)
	if err != nil {
		cancel()
		return nil, err
	}
	pcm, readErr := readVoiceUtteranceObserved(captureCtx, input, threshold, d.observe)
	cancel()
	_ = input.Close()
	waitErr := wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if readErr != nil {
		if waitErr != nil {
			return nil, voiceProcessFailure("capture process stopped", waitErr)
		}
		if errors.Is(readErr, io.EOF) {
			return nil, voiceFailure{code: "capture ended"}
		}
		return nil, voiceFailure{code: "capture read failed"}
	}
	return pcm, nil
}

func runVoice(ctx context.Context, cfg voiceConfig, d voiceDeps) error {
	failures := 0
	lastFeedback := time.Time{}
	for ctx.Err() == nil {
		pcm, err := captureVoiceUtterance(ctx, d, cfg.source, cfg.threshold)
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			failures++
			d.logf("voice %s; retrying", voiceDiagnostic(err))
			voiceFailureFeedback(ctx, d, &lastFeedback, "Microphone capture failed. Check the audio source.")
			if failures >= 3 {
				return err
			}
			delay := time.Second * time.Duration(1<<min(failures-1, 5))
			if d.pause(ctx, delay) != nil {
				break
			}
			continue
		}
		failures = 0
		if err = handleVoicePCM(ctx, cfg, d, pcm); err != nil && ctx.Err() == nil {
			d.logf("voice %s", voiceDiagnostic(err))
			message := "Voice request failed. Check the service journal."
			var failure voiceFailure
			if errors.As(err, &failure) && strings.HasPrefix(failure.code, "agent ") {
				message = "I could not complete that request. Voice access is read only."
			}
			voiceFailureFeedback(ctx, d, &lastFeedback, message)
		}
		if d.pause(ctx, 800*time.Millisecond) != nil {
			break
		}
	}
	return nil
}
func voiceDiagnostic(err error) string {
	var failure voiceFailure
	if errors.As(err, &failure) {
		return failure.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return "operation failed"
}
func voiceLog(d voiceDeps, format string, args ...any) {
	if d.logf != nil {
		d.logf(format, args...)
	}
}
func voiceFailureFeedback(ctx context.Context, d voiceDeps, last *time.Time, message string) {
	if d.speak == nil || (!last.IsZero() && time.Since(*last) < time.Minute) {
		return
	}
	*last = time.Now()
	if err := d.speak(ctx, message); err != nil && d.logf != nil {
		d.logf("voice %s", voiceDiagnostic(voiceProcessFailure("feedback playback failed", err)))
	}
}
func handleVoicePCM(ctx context.Context, cfg voiceConfig, d voiceDeps, pcm []byte) error {
	dir, err := os.MkdirTemp("", "agent-bridge-voice-")
	if err != nil {
		return voiceFailure{code: "private audio directory failed"}
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "utterance.wav")
	if err = writeVoiceWAV(path, pcm); err != nil {
		return err
	}
	transcript, err := d.transcribe(ctx, path)
	_ = os.Remove(path)
	if err != nil {
		var failure voiceFailure
		if errors.As(err, &failure) && failure.code == "empty transcription" {
			voiceLog(d, "voice transcription empty; ignored")
			return nil
		}
		return voiceProcessFailure("transcription failed", err)
	}
	return handleVoiceText(ctx, cfg, d, transcript)
}
func writeVoiceWAV(path string, pcm []byte) error {
	if len(pcm) > voiceRate*2*int(voiceMaxDuration/time.Second) {
		return voiceFailure{code: "audio too long"}
	}
	if len(pcm) == 0 || len(pcm)%2 != 0 {
		return voiceFailure{code: "invalid audio"}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return voiceFailure{code: "private audio file failed"}
	}
	defer f.Close()
	header := make([]byte, 44)
	copy(header, "RIFF")
	binary.LittleEndian.PutUint32(header[4:], uint32(36+len(pcm)))
	copy(header[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(header[16:], 16)
	binary.LittleEndian.PutUint16(header[20:], 1)
	binary.LittleEndian.PutUint16(header[22:], 1)
	binary.LittleEndian.PutUint32(header[24:], voiceRate)
	binary.LittleEndian.PutUint32(header[28:], voiceRate*2)
	binary.LittleEndian.PutUint16(header[32:], 2)
	binary.LittleEndian.PutUint16(header[34:], 16)
	copy(header[36:], "data")
	binary.LittleEndian.PutUint32(header[40:], uint32(len(pcm)))
	if _, err = f.Write(header); err != nil {
		return voiceFailure{code: "write audio failed"}
	}
	if _, err = f.Write(pcm); err != nil {
		return voiceFailure{code: "write audio failed"}
	}
	return nil
}

func runVoiceFile(ctx context.Context, cfg voiceConfig, d voiceDeps) error {
	f, err := os.Open(cfg.file)
	if err != nil {
		return errors.New("cannot open WAV file")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() < 44 || st.Size() > int64(voiceRate*2*int(voiceMaxDuration/time.Second)+4096) {
		return voiceFailure{code: "invalid or oversized WAV file"}
	}
	header := make([]byte, 12)
	if _, err = io.ReadFull(f, header); err != nil || string(header[:4]) != "RIFF" || string(header[8:]) != "WAVE" {
		return voiceFailure{code: "invalid WAV file"}
	}
	transcript, err := d.transcribe(ctx, cfg.file)
	if err != nil {
		var failure voiceFailure
		if errors.As(err, &failure) && failure.code == "empty transcription" {
			voiceLog(d, "voice transcription empty; ignored")
			return nil
		}
		return voiceProcessFailure("transcription failed", err)
	}
	return handleVoiceText(ctx, cfg, d, transcript)
}
func handleVoiceText(ctx context.Context, cfg voiceConfig, d voiceDeps, transcript string) error {
	if len(transcript) > voiceMaxTranscript || !utf8.ValidString(transcript) {
		return voiceFailure{code: "transcript invalid or too long"}
	}
	if strings.TrimSpace(transcript) == "" {
		voiceLog(d, "voice transcription empty; ignored")
		return nil
	}
	prompt, ok := stripWakeWord(transcript, cfg.wakeWord)
	voiceLog(d, "voice transcription complete wake_match=%t", ok)
	if !ok || prompt == "" {
		return nil
	}
	voiceLog(d, "voice acknowledgement playback start")
	if err := d.speak(ctx, "I'm listening."); err != nil {
		return voiceProcessFailure("acknowledgement playback failed", err)
	}
	voiceLog(d, "voice acknowledgement playback complete")
	voiceLog(d, "voice agent turn start")
	reply, err := d.reply(ctx, prompt)
	if err != nil {
		return voiceFailure{code: "agent reply failed", status: voiceDiagnostic(err)}
	}
	voiceLog(d, "voice agent turn complete")
	reply = strings.TrimSpace(reply)
	if reply == "" || !utf8.ValidString(reply) {
		return voiceFailure{code: "agent reply invalid"}
	}
	voiceLog(d, "voice speech playback start")
	for _, chunk := range voiceReplyChunks(reply) {
		if err = d.speak(ctx, chunk); err != nil {
			return voiceProcessFailure("speech playback failed", err)
		}
	}
	voiceLog(d, "voice speech playback complete")
	return nil
}
func voiceReplyChunks(reply string) []string {
	truncated := len(reply) > voiceMaxSpokenReply
	if truncated {
		reply = reply[:voiceMaxSpokenReply]
		for !utf8.ValidString(reply) {
			reply = reply[:len(reply)-1]
		}
	}
	var chunks []string
	for len(reply) > 0 {
		if len(reply) <= voiceMaxReply {
			chunks = append(chunks, reply)
			break
		}
		cut := voiceMaxReply
		for !utf8.RuneStart(reply[cut]) {
			cut--
		}
		if space := strings.LastIndexAny(reply[:cut], " \n\t"); space > cut/2 {
			cut = space + 1
		}
		chunks = append(chunks, strings.TrimSpace(reply[:cut]))
		reply = strings.TrimSpace(reply[cut:])
	}
	if truncated {
		chunks = append(chunks, "The reply was longer than I can read aloud. Ask me to continue.")
	}
	return chunks
}
func stripWakeWord(transcript, wake string) (string, bool) {
	transcript = strings.TrimSpace(transcript)
	wake = strings.TrimSpace(wake)
	if wake == "" {
		return transcript, true
	}
	separator := func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSpace(r) }
	remainder := strings.TrimLeftFunc(transcript, separator)
	words := strings.Fields(wake)
	for i, word := range words {
		if len(remainder) < len(word) || !strings.EqualFold(remainder[:len(word)], word) {
			return "", false
		}
		remainder = remainder[len(word):]
		if remainder != "" {
			r, _ := utf8.DecodeRuneInString(remainder)
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
				return "", false
			}
		}
		clean := strings.TrimLeftFunc(remainder, separator)
		if i < len(words)-1 && clean == remainder {
			return "", false
		}
		remainder = clean
	}
	return remainder, true
}

func transcribeVoice(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/voxtype", "transcribe", path)
	cmd.Stderr = io.Discard
	output, err := limitedCommandOutput(cmd, 64<<10)
	if err != nil {
		return "", voiceProcessFailure("transcription process failed", err)
	}
	lines := strings.Split(string(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			if len(line) > voiceMaxTranscript {
				return "", voiceFailure{code: "transcript too long"}
			}
			return line, nil
		}
	}
	return "", voiceFailure{code: "empty transcription"}
}
func limitedCommandOutput(cmd *exec.Cmd, limit int64) ([]byte, error) {
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(pipe, limit+1))
	if int64(len(b)) > limit {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, voiceFailure{code: "transcription output too large"}
	}
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	if err = cmd.Wait(); err != nil {
		return nil, err
	}
	return b, nil
}
func speakVoice(ctx context.Context, text string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "agent-bridge-voice-playback-")
	if err != nil {
		return errors.New("private playback directory failed")
	}
	defer os.RemoveAll(dir)
	cmd := exec.Command(self, "speak")
	cmd.Env = append(os.Environ(), "TMPDIR="+dir)
	cmd.Stdin = strings.NewReader(text)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
		return err
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return ctx.Err()
	}
}
