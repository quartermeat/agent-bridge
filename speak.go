package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	maxSpeechBytes    = 4096
	defaultSpeechGain = 6.0
	minSpeechGain     = -20.0
	maxSpeechGain     = 20.0
)

// speechFilter loudness-normalizes Flite output, applies gainDB, and hard-limits peaks.
func speechFilter(gainDB float64) string {
	return "loudnorm=I=-14:TP=-1.5:LRA=11,volume=" + strconv.FormatFloat(gainDB, 'f', -1, 64) + "dB,alimiter=limit=0.95"
}

func validSpeechGain(gainDB float64) bool {
	return !math.IsNaN(gainDB) && gainDB >= minSpeechGain && gainDB <= maxSpeechGain
}

type speechRunner func(name string, args []string, dir string) error

func speak(args []string) error {
	return speakWith(args, os.Stdin, exec.LookPath, runSpeechCommand)
}

func speakWith(args []string, input io.Reader, lookPath func(string) (string, error), run speechRunner) (retErr error) {
	f := flag.NewFlagSet("speak", flag.ContinueOnError)
	voice := f.String("voice", "slt", "Flite voice: awb, kal, kal16, rms, slt")
	textArg := f.String("text", "", "Text to speak; default: stdin")
	gain := f.Float64("gain", defaultSpeechGain, "Gain in dB after loudness normalization, -20 to 20")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("usage: agent-bridge speak [--voice slt] [--gain 6] [--text TEXT]")
	}
	textProvided := false
	f.Visit(func(opt *flag.Flag) {
		if opt.Name == "text" {
			textProvided = true
		}
	})
	switch *voice {
	case "awb", "kal", "kal16", "rms", "slt":
	default:
		return fmt.Errorf("unsupported voice %q (choose awb, kal, kal16, rms, or slt)", *voice)
	}
	if !validSpeechGain(*gain) {
		return fmt.Errorf("gain %v dB is outside %v to %v", *gain, minSpeechGain, maxSpeechGain)
	}

	var speech []byte
	if textProvided {
		speech = []byte(*textArg)
	} else {
		var err error
		speech, err = io.ReadAll(io.LimitReader(input, maxSpeechBytes+1))
		if err != nil {
			return fmt.Errorf("read speech text: %w", err)
		}
	}
	if len(speech) > maxSpeechBytes {
		return fmt.Errorf("speech text exceeds %d bytes", maxSpeechBytes)
	}
	if len(strings.TrimSpace(string(speech))) == 0 {
		return errors.New("speech text is empty")
	}
	if strings.IndexByte(string(speech), 0) >= 0 {
		return errors.New("speech text contains a NUL byte")
	}

	ffmpeg, err := lookPath("ffmpeg")
	if err != nil {
		return fmt.Errorf("ffmpeg is required for speech: %w", err)
	}
	paplay, err := lookPath("paplay")
	if err != nil {
		return fmt.Errorf("paplay is required for speech: %w", err)
	}

	dir, err := os.MkdirTemp("", "agent-bridge-speak-")
	if err != nil {
		return fmt.Errorf("create private speech directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("remove private speech files: %w", err))
		}
	}()
	if err := os.WriteFile(filepath.Join(dir, "speech.txt"), speech, 0600); err != nil {
		return fmt.Errorf("write speech text: %w", err)
	}
	wav, err := os.OpenFile(filepath.Join(dir, "speech.wav"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("create private audio file: %w", err)
	}
	if err := wav.Close(); err != nil {
		return fmt.Errorf("close audio file: %w", err)
	}
	if err := run(ffmpeg, []string{"-y", "-nostdin", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "flite=textfile=speech.txt:voice=" + *voice, "-af", speechFilter(*gain), "-ac", "2", "-ar", "48000", "-f", "wav", "speech.wav"}, dir); err != nil {
		return fmt.Errorf("ffmpeg speech synthesis failed: %w", err)
	}
	if err := run(paplay, []string{"speech.wav"}, dir); err != nil {
		return fmt.Errorf("paplay audio playback failed: %w", err)
	}
	return nil
}

func runSpeechCommand(name string, args []string, dir string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
