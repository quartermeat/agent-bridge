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
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	voiceRate           = 16000
	voiceFrameSamples   = 320
	voiceFrameDuration  = 20 * time.Millisecond
	voicePrerollFrames  = 30 // 600 ms keeps a soft leading "hey" before the 3-frame trigger
	voiceMaxDuration    = 30 * time.Second
	voiceMinUtterance   = 2 * time.Second
	voiceMaxUtterance   = 60 * time.Second
	voiceFollowUp       = 20 * time.Second
	voiceMaxFollowUp    = 120 * time.Second
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
	// maxUtterance caps captured speech including pre-roll; zero means voiceMaxDuration.
	maxUtterance time.Duration
	// followUp accepts utterances without the wake phrase for this long after a reply; zero disables.
	followUp time.Duration
	// access is "read" or "work" (Claude only); work enables progress updates.
	access string
	// stillWorkingAfter and stillWorkingEvery pace "Still working." during long
	// work turns; zero stillWorkingAfter disables them.
	stillWorkingAfter, stillWorkingEvery time.Duration
}

// voiceFollowUpState tracks when the wake-word-free follow-up window closes.
type voiceFollowUpState struct{ until time.Time }

// voiceMaxFrames is the frame budget for an utterance, pre-roll included.
func voiceMaxFrames(limit time.Duration) int {
	if limit <= 0 {
		limit = voiceMaxDuration
	}
	return int(limit / voiceFrameDuration)
}

// voiceMaxBytes is the PCM byte limit shared by capture and the WAV checks.
func voiceMaxBytes(limit time.Duration) int {
	return voiceMaxFrames(limit) * voiceFrameSamples * 2
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
	now        func() time.Time
	// tone plays the short follow-up acknowledgement; nil skips it.
	tone func(context.Context) error
	// record appends to the private transcript; nil disables it.
	record func(role, kind, text string) error
	// audio serializes speech between spoken approvals and "Still working." updates.
	audio *sync.Mutex
}

func voiceNow(d voiceDeps) time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

func voiceCommand(args []string) error {
	f := flag.NewFlagSet("voice", flag.ContinueOnError)
	source := f.String("source", "default", "PulseAudio source name")
	wake := f.String("wake-word", "hey agent", "Wake phrase; empty accepts every utterance")
	threshold := f.Float64("vad-threshold", 0.018, "RMS speech threshold from 0 to 1")
	file := f.String("file", "", "Transcribe one existing WAV file and exit")
	agentName := f.String("agent", "claude", "Reply backend: claude or codex")
	speechGain := f.Float64("speech-gain", defaultSpeechGain, "Speech gain in dB passed to speak, -20 to 20")
	maxUtterance := f.Duration("max-utterance", voiceMaxDuration, "Longest captured utterance, 2s to 60s")
	followUp := f.Duration("follow-up", voiceFollowUp, "Accept replies without the wake phrase for this long after speaking; 0 disables, max 120s")
	access := f.String("access", voiceAccessWork, "Claude access: read (read-only tools) or work (all tools, spoken approval)")
	if err := f.Parse(args); err != nil {
		return err
	}
	accessSet := false
	f.Visit(func(fl *flag.Flag) { accessSet = accessSet || fl.Name == "access" })
	if *agentName == "codex" {
		// Codex stays read-only; an explicit work request is a usage error.
		if accessSet && *access != voiceAccessRead {
			return errors.New("voice: --access work requires --agent claude")
		}
		*access = voiceAccessRead
	}
	if f.NArg() != 0 || *source == "" || math.IsNaN(*threshold) || *threshold <= 0 || *threshold >= 1 || (*agentName != "claude" && *agentName != "codex") || !validSpeechGain(*speechGain) ||
		*maxUtterance < voiceMinUtterance || *maxUtterance > voiceMaxUtterance || *followUp < 0 || *followUp > voiceMaxFollowUp || (*access != voiceAccessRead && *access != voiceAccessWork) {
		return errors.New("usage: agent-bridge voice [--agent claude|codex] [--access read|work] [--source NAME] [--wake-word PHRASE] [--vad-threshold 0.018] [--speech-gain 6] [--max-utterance 30s] [--follow-up 20s] [--file WAV]")
	}
	cfg := voiceConfig{source: *source, wakeWord: *wake, file: *file, threshold: *threshold, maxUtterance: *maxUtterance, followUp: *followUp, access: *access}
	if cfg.access == voiceAccessWork {
		cfg.stillWorkingAfter, cfg.stillWorkingEvery = 20*time.Second, 60*time.Second
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	socket, err := voiceApprovalSocketPath()
	if err != nil {
		return err
	}
	d := realVoiceDeps(*agentName, *speechGain, cfg.access, socket)
	defer d.close()
	if path, err := voiceTranscriptPath(); err == nil {
		d.record = newVoiceTranscript(path).Append
	}
	if cfg.access == voiceAccessWork {
		approver := &voiceApprover{cfg: cfg, d: d}
		stopApprovals, err := serveVoiceApprovals(socket, approver.decide)
		if err != nil {
			// Without the socket every approval denies; reading still works.
			log.Printf("voice approval socket unavailable; actions will be denied")
		} else {
			defer stopApprovals()
		}
	}
	if cfg.file != "" {
		return runVoiceFile(ctx, cfg, d)
	}
	return runVoice(ctx, cfg, d)
}

type voiceReplier interface {
	Reply(context.Context, string) (string, error)
	Close() error
}

func realVoiceDeps(agentName string, speechGain float64, access, socket string) voiceDeps {
	var agent voiceReplier = newVoiceClaudeAccess(access, socket)
	if agentName == "codex" {
		agent = newVoiceAgent()
	}
	lastLevel := time.Time{}
	tone := &voiceTonePlayer{}
	closeAll := func() error { return errors.Join(agent.Close(), tone.close()) }
	return voiceDeps{capture: startVoiceCapture, transcribe: transcribeVoice, reply: agent.Reply, close: closeAll, tone: tone.play, audio: &sync.Mutex{}, speak: func(ctx context.Context, text string) error { return speakVoice(ctx, text, speechGain) }, logf: log.Printf, pause: voicePause,
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
	pcm, _, err := readVoiceUtteranceObserved(ctx, input, threshold, 0, nil)
	return pcm, err
}

// readVoiceUtteranceObserved returns one utterance of at most voiceMaxBytes(limit) bytes,
// pre-roll included, and reports whether it was cut at that cap.
func readVoiceUtteranceObserved(ctx context.Context, input io.Reader, threshold float64, limit time.Duration, observe func(float64, bool)) ([]byte, bool, error) {
	frame := make([]byte, voiceFrameSamples*2)
	// preroll is a fixed ring; reslicing it would shrink capacity and let it grow without bound in silence.
	preroll := make([][]byte, 0, voicePrerollFrames)
	var utterance []byte
	speechFrames, quietFrames := 0, 0
	maxBytes := voiceMaxBytes(limit)
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		_, err := io.ReadFull(input, frame)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, false, io.EOF
			}
			return nil, false, err
		}
		level := voiceEnergy(frame)
		active := level >= threshold
		if len(utterance) == 0 {
			if len(preroll) == voicePrerollFrames {
				copy(preroll, preroll[1:])
				preroll[len(preroll)-1] = bytes.Clone(frame)
			} else {
				preroll = append(preroll, bytes.Clone(frame))
			}
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
				if len(utterance) > maxBytes {
					utterance = utterance[len(utterance)-maxBytes:]
				}
				if len(utterance) >= maxBytes {
					return utterance, true, nil
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
		if len(utterance) >= maxBytes {
			return utterance[:maxBytes], true, nil
		}
		if quietFrames >= 35 {
			return utterance, false, nil
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

func captureVoiceUtterance(ctx context.Context, d voiceDeps, source string, threshold float64, limit time.Duration) ([]byte, error) {
	captureCtx, cancel := context.WithCancel(ctx)
	input, wait, err := d.capture(captureCtx, source)
	if err != nil {
		cancel()
		return nil, err
	}
	pcm, capped, readErr := readVoiceUtteranceObserved(captureCtx, input, threshold, limit, d.observe)
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
	if capped {
		voiceLog(d, "voice utterance hit length cap")
	}
	return pcm, nil
}

func runVoice(ctx context.Context, cfg voiceConfig, d voiceDeps) error {
	failures := 0
	lastFeedback := time.Time{}
	followUp := &voiceFollowUpState{}
	for ctx.Err() == nil {
		pcm, err := captureVoiceUtterance(ctx, d, cfg.source, cfg.threshold, cfg.maxUtterance)
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
		// The follow-up window is judged by when speech began, not when transcription ends.
		startedAt := voiceNow(d).Add(-time.Duration(len(pcm)/2) * time.Second / voiceRate)
		if err = handleVoicePCMTurn(ctx, cfg, d, followUp, startedAt, pcm); err != nil && ctx.Err() == nil {
			d.logf("voice %s", voiceDiagnostic(err))
			voiceRecord(d, "system", "error", voiceDiagnostic(err))
			message := "Voice request failed. Check the service journal."
			var failure voiceFailure
			if errors.As(err, &failure) && strings.HasPrefix(failure.code, "agent ") {
				message = "I could not complete that request. Voice access is read only."
				if cfg.access == voiceAccessWork {
					message = "I could not complete that request."
				}
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
	return handleVoicePCMTurn(ctx, cfg, d, nil, time.Time{}, pcm)
}
func handleVoicePCMTurn(ctx context.Context, cfg voiceConfig, d voiceDeps, state *voiceFollowUpState, startedAt time.Time, pcm []byte) error {
	dir, err := os.MkdirTemp("", "agent-bridge-voice-")
	if err != nil {
		return voiceFailure{code: "private audio directory failed"}
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "utterance.wav")
	if err = writeVoiceWAVLimit(path, pcm, cfg.maxUtterance); err != nil {
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
	return handleVoiceTurn(ctx, cfg, d, state, startedAt, transcript)
}
func writeVoiceWAV(path string, pcm []byte) error { return writeVoiceWAVLimit(path, pcm, 0) }
func writeVoiceWAVLimit(path string, pcm []byte, limit time.Duration) error {
	if len(pcm) > voiceMaxBytes(limit) {
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
	if err != nil || !st.Mode().IsRegular() || st.Size() < 44 || st.Size() > int64(voiceMaxBytes(cfg.maxUtterance)+4096) {
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
	return handleVoiceTurn(ctx, cfg, d, nil, time.Time{}, transcript)
}

// handleVoiceTurn gates a transcript on the wake phrase or an open follow-up window.
// A nil state disables follow-ups. Logs carry only booleans, never transcript text.
func handleVoiceTurn(ctx context.Context, cfg voiceConfig, d voiceDeps, state *voiceFollowUpState, startedAt time.Time, transcript string) error {
	if len(transcript) > voiceMaxTranscript || !utf8.ValidString(transcript) {
		return voiceFailure{code: "transcript invalid or too long"}
	}
	if strings.TrimSpace(transcript) == "" {
		voiceLog(d, "voice transcription empty; ignored")
		return nil
	}
	prompt, wake := stripWakeWord(transcript, cfg.wakeWord)
	if wake && strings.TrimSpace(cfg.wakeWord) != "" && voiceFollowUpNoise(prompt) {
		return handleVoiceBareWake(ctx, cfg, d, state)
	}
	inWindow := state != nil && !state.until.IsZero() && !startedAt.After(state.until)
	followUp := inWindow && (!wake || prompt != "")
	if followUp && !wake {
		prompt = strings.TrimSpace(transcript)
		if voiceFollowUpNoise(prompt) {
			// Likely a Whisper hallucination or echo; leave the window open.
			voiceLog(d, "voice transcription complete wake_match=false follow_up=false; follow-up noise ignored")
			voiceRecord(d, "user", "ignored", transcript)
			return nil
		}
	}
	voiceLog(d, "voice transcription complete wake_match=%t follow_up=%t", wake, followUp)
	if (!wake && !followUp) || prompt == "" {
		// Dropped speech is still shown in the transcript so the user can see what the mic heard.
		voiceRecord(d, "user", "ignored", transcript)
		return nil
	}
	if state != nil {
		// Accepted turns consume the window; only a completed reply reopens it.
		state.until = time.Time{}
	}
	voiceRecord(d, "user", "prompt", prompt)
	if !followUp {
		voiceRecord(d, "system", "status", "Wake phrase heard; listening.")
		voiceLog(d, "voice acknowledgement playback start")
		if err := d.speak(ctx, "I'm listening."); err != nil {
			return voiceProcessFailure("acknowledgement playback failed", err)
		}
		voiceLog(d, "voice acknowledgement playback complete")
	} else if d.tone != nil {
		voiceLog(d, "voice follow-up tone playback")
		if err := d.tone(ctx); err != nil {
			voiceLog(d, "voice follow-up tone playback failed")
		}
	}
	voiceLog(d, "voice agent turn start")
	voiceRecord(d, "system", "status", "Agent turn started.")
	stopProgress := startVoiceProgress(ctx, cfg, d)
	reply, err := d.reply(ctx, prompt)
	stopProgress()
	if err != nil {
		return voiceFailure{code: "agent reply failed", status: voiceDiagnostic(err)}
	}
	voiceLog(d, "voice agent turn complete")
	reply = strings.TrimSpace(reply)
	if reply == "" || !utf8.ValidString(reply) {
		return voiceFailure{code: "agent reply invalid"}
	}
	voiceRecord(d, "agent", "reply", reply)
	voiceLog(d, "voice speech playback start")
	for _, chunk := range voiceReplyChunks(reply) {
		if err = d.speak(ctx, chunk); err != nil {
			return voiceProcessFailure("speech playback failed", err)
		}
	}
	voiceLog(d, "voice speech playback complete")
	if state != nil && cfg.followUp > 0 {
		state.until = voiceNow(d).Add(cfg.followUp)
		voiceLog(d, "voice follow-up window open")
	}
	return nil
}

// startVoiceProgress speaks "Still working." while a long turn runs, at most once
// per stillWorkingEvery. It skips an update while an approval holds the audio.
// The returned function stops it and waits for any update in progress.
func startVoiceProgress(ctx context.Context, cfg voiceConfig, d voiceDeps) func() {
	if cfg.stillWorkingAfter <= 0 || d.speak == nil {
		return func() {}
	}
	every := cfg.stillWorkingEvery
	if every <= 0 {
		every = time.Minute
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		wait := cfg.stillWorkingAfter
		for {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if d.audio == nil || d.audio.TryLock() {
				_ = d.speak(ctx, "Still working.")
				if d.audio != nil {
					d.audio.Unlock()
				}
				wait = every
			} else {
				wait = 5 * time.Second
			}
		}
	}()
	return func() { cancel(); <-done }
}

// voiceBareWakeWindow is the listening window after a bare wake phrase when follow-ups are disabled.
const voiceBareWakeWindow = 10 * time.Second

// handleVoiceBareWake acknowledges a wake phrase spoken alone and opens a listening
// window so the request can arrive as the next utterance without the wake phrase.
func handleVoiceBareWake(ctx context.Context, cfg voiceConfig, d voiceDeps, state *voiceFollowUpState) error {
	voiceLog(d, "voice transcription complete wake_match=true follow_up=false")
	voiceLog(d, "voice wake only; listening")
	voiceRecord(d, "system", "status", "Wake phrase heard; listening.")
	if err := d.speak(ctx, "I'm listening."); err != nil {
		return voiceProcessFailure("acknowledgement playback failed", err)
	}
	if state != nil {
		window := cfg.followUp
		if window <= 0 {
			window = voiceBareWakeWindow
		}
		state.until = voiceNow(d).Add(window)
		voiceLog(d, "voice follow-up window open")
	}
	return nil
}

// voiceNoisePhrases are common Whisper hallucinations on silence or echo.
var voiceNoisePhrases = map[string]bool{"you": true, "thank you": true, "thanks for watching": true, "bye": true, "uh": true, "um": true, "hmm": true, "blank audio": true}

// voiceFollowUpNoise reports whether a wake-free follow-up is likely noise rather than a request.
func voiceFollowUpNoise(text string) bool {
	text = strings.TrimSpace(text)
	var outside strings.Builder
	depth := 0
	for _, r := range text {
		switch r {
		case '[', '(':
			depth++
		case ']', ')':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				outside.WriteRune(r)
			}
		}
	}
	normalized := strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return ' '
	}, outside.String())), " ")
	// Bracket-only transcripts such as "[BLANK_AUDIO]" or "(music)" leave nothing outside.
	return normalized == "" || voiceNoisePhrases[normalized]
}

const (
	voiceToneRate      = 48000
	voiceToneHz        = 880.0
	voiceToneDuration  = 150 * time.Millisecond
	voiceToneFade      = 25 * time.Millisecond
	voiceToneAmplitude = 0.25
)

// voiceToneWAV returns a mono 16-bit WAV of a soft sine beep with raised-cosine fades.
func voiceToneWAV() []byte {
	n := int(voiceToneDuration * voiceToneRate / time.Second)
	fade := int(voiceToneFade * voiceToneRate / time.Second)
	b := make([]byte, 44+2*n)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+2*n))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], voiceToneRate)
	binary.LittleEndian.PutUint32(b[28:], voiceToneRate*2)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(2*n))
	for i := 0; i < n; i++ {
		env := 1.0
		if i < fade {
			env = 0.5 - 0.5*math.Cos(math.Pi*float64(i)/float64(fade))
		} else if n-1-i < fade {
			env = 0.5 - 0.5*math.Cos(math.Pi*float64(n-1-i)/float64(fade))
		}
		v := voiceToneAmplitude * env * math.Sin(2*math.Pi*voiceToneHz*float64(i)/voiceToneRate)
		binary.LittleEndian.PutUint16(b[44+2*i:], uint16(int16(math.Round(v*32767))))
	}
	return b
}

// voiceTonePlayer synthesizes the tone once into a private temp file and replays it with paplay.
type voiceTonePlayer struct {
	mu   sync.Mutex
	dir  string
	path string
}

func (t *voiceTonePlayer) file() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.path != "" {
		return t.path, nil
	}
	dir, err := os.MkdirTemp("", "agent-bridge-voice-tone-")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "tone.wav")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		_, err = f.Write(voiceToneWAV())
		err = errors.Join(err, f.Close())
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	t.dir, t.path = dir, path
	return path, nil
}

func (t *voiceTonePlayer) play(ctx context.Context) error {
	path, err := t.file()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "paplay", path)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	return cmd.Run()
}

func (t *voiceTonePlayer) close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dir == "" {
		return nil
	}
	err := os.RemoveAll(t.dir)
	t.dir, t.path = "", ""
	return err
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

// voiceWakeVariants are conservative Whisper mishearings accepted for the default wake phrase.
// Multi-word variants match anywhere like the phrase; a single-word variant matches
// only at the start of the transcript, so a bare "agent" accepts
// "Agent, testing" (a clipped "hey") but never "the agent is".
var voiceWakeVariants = map[string][]string{"hey agent": {"hi agent", "hey agents", "agent"}}

// stripWakeWord finds the wake phrase (or a multi-word variant) anywhere in the
// transcript and returns the text after its last occurrence, so lead-in chatter
// such as "Hello, hello, hey agent, are you listening?" still matches. Single-word
// variants (a bare "agent") match only at the start. Case and punctuation are ignored.
func stripWakeWord(transcript, wake string) (string, bool) {
	transcript = strings.TrimSpace(transcript)
	wake = strings.TrimSpace(wake)
	if wake == "" {
		return transcript, true
	}
	phrases := []string{wake}
	var leadingOnly []string
	for _, variant := range voiceWakeVariants[strings.ToLower(strings.Join(strings.Fields(wake), " "))] {
		if len(strings.Fields(variant)) > 1 {
			phrases = append(phrases, variant)
		} else {
			leadingOnly = append(leadingOnly, variant)
		}
	}
	best, prompt := -1, ""
	for i := 0; i < len(transcript); i++ {
		if i > 0 {
			prev, _ := utf8.DecodeLastRuneInString(transcript[:i])
			if !utf8.RuneStart(transcript[i]) || unicode.IsLetter(prev) || unicode.IsDigit(prev) || prev == '_' {
				continue
			}
		}
		for _, phrase := range phrases {
			if rest, ok := stripWakePhrase(transcript[i:], phrase); ok && i > best {
				best, prompt = i, rest
			}
		}
	}
	if best >= 0 {
		return prompt, true
	}
	for _, variant := range leadingOnly {
		if rest, ok := stripWakePhrase(transcript, variant); ok {
			return rest, true
		}
	}
	return "", false
}

// stripLeadingWakeWord removes a wake phrase or variant only at the start of the
// transcript. Spoken approvals use it so earlier speech cannot be skipped over.
func stripLeadingWakeWord(transcript, wake string) (string, bool) {
	transcript = strings.TrimSpace(transcript)
	wake = strings.TrimSpace(wake)
	if wake == "" {
		return transcript, true
	}
	if prompt, ok := stripWakePhrase(transcript, wake); ok {
		return prompt, true
	}
	for _, variant := range voiceWakeVariants[strings.ToLower(strings.Join(strings.Fields(wake), " "))] {
		if prompt, ok := stripWakePhrase(transcript, variant); ok {
			return prompt, true
		}
	}
	return "", false
}

func stripWakePhrase(transcript, wake string) (string, bool) {
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
	// Output is returned with a non-zero exit so callers can read a structured error.
	return b, cmd.Wait()
}
func speakVoice(ctx context.Context, text string, gainDB float64) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "agent-bridge-voice-playback-")
	if err != nil {
		return errors.New("private playback directory failed")
	}
	defer os.RemoveAll(dir)
	cmd := exec.Command(self, speakVoiceArgs(gainDB)...)
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

func speakVoiceArgs(gainDB float64) []string {
	return []string{"speak", "--gain", strconv.FormatFloat(gainDB, 'f', -1, 64)}
}
