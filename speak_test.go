package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSpeakPreservesTextAndCleansPrivateFiles(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	want := "It's safe: $HOME; $(touch /tmp/nope) [a], 'b'!\nSecond line."
	for _, tc := range []struct {
		name  string
		args  []string
		input string
	}{
		{"flag", []string{"--voice", "kal16", "--gain", "-2.5", "--text", want}, "ignored"},
		{"stdin", nil, want},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			var tempDir string
			lookup := func(name string) (string, error) { return "/fake/" + name, nil }
			runner := func(name string, args []string, dir string) error {
				calls = append(calls, name)
				tempDir = dir
				st, err := os.Stat(dir)
				if err != nil || st.Mode().Perm() != 0700 {
					t.Fatalf("private directory: %v, %v", st, err)
				}
				for _, file := range []string{"speech.txt", "speech.wav"} {
					st, err := os.Stat(filepath.Join(dir, file))
					if err != nil || st.Mode().Perm() != 0600 {
						t.Fatalf("private %s: %v, %v", file, st, err)
					}
				}
				data, err := os.ReadFile(filepath.Join(dir, "speech.txt"))
				if err != nil || string(data) != want {
					t.Fatalf("speech text = %q, err = %v", data, err)
				}
				if name == "/fake/ffmpeg" {
					voice, gain := "slt", "6"
					if tc.name == "flag" {
						voice, gain = "kal16", "-2.5"
					}
					wantArgs := []string{"-y", "-nostdin", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "flite=textfile=speech.txt:voice=" + voice, "-af", "loudnorm=I=-14:TP=-1.5:LRA=11,volume=" + gain + "dB,alimiter=limit=0.95", "-ac", "2", "-ar", "48000", "-f", "wav", "speech.wav"}
					if !reflect.DeepEqual(args, wantArgs) {
						t.Fatalf("ffmpeg args = %q", args)
					}
				} else if name != "/fake/paplay" || !reflect.DeepEqual(args, []string{"speech.wav"}) {
					t.Fatalf("playback command = %q %q", name, args)
				}
				return nil
			}
			if err := speakWith(tc.args, strings.NewReader(tc.input), lookup, runner); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(calls, []string{"/fake/ffmpeg", "/fake/paplay"}) {
				t.Fatalf("commands = %q", calls)
			}
			if _, err := os.Stat(tempDir); !os.IsNotExist(err) {
				t.Fatalf("temporary directory remains: %v", err)
			}
		})
	}
}

func TestSpeakRejectsInvalidInputBeforeCommands(t *testing.T) {
	lookup := func(string) (string, error) { t.Fatal("unexpected dependency lookup"); return "", nil }
	runner := func(string, []string, string) error { t.Fatal("unexpected command"); return nil }
	for _, tc := range []struct {
		name string
		args []string
		in   string
		want string
	}{
		{"empty flag", []string{"--text="}, "ignored", "empty"},
		{"empty stdin", nil, " \n\t", "empty"},
		{"oversize flag", []string{"--text", strings.Repeat("x", maxSpeechBytes+1)}, "", "exceeds"},
		{"oversize stdin", nil, strings.Repeat("x", maxSpeechBytes+1), "exceeds"},
		{"bad voice", []string{"--voice", "slt;echo bad", "--text", "hello"}, "", "unsupported voice"},
		{"NUL", []string{"--text", "hello\x00world"}, "", "NUL"},
		{"gain too high", []string{"--gain", "20.5", "--text", "hello"}, "", "gain"},
		{"gain too low", []string{"--gain", "-21", "--text", "hello"}, "", "gain"},
		{"gain NaN", []string{"--gain", "NaN", "--text", "hello"}, "", "gain"},
		{"gain not number", []string{"--gain", "loud", "--text", "hello"}, "", "invalid value"},
		{"extra argument", []string{"--text", "hello", "extra"}, "", "usage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := speakWith(tc.args, strings.NewReader(tc.in), lookup, runner)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSpeakReportsDependenciesAndCommandFailures(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	for _, missing := range []string{"ffmpeg", "paplay"} {
		lookup := func(name string) (string, error) {
			if name == missing {
				return "", errors.New("missing")
			}
			return "/fake/" + name, nil
		}
		err := speakWith([]string{"--text", "hello"}, bytes.NewReader(nil), lookup, nil)
		if err == nil || !strings.Contains(err.Error(), missing) {
			t.Fatalf("missing %s: %v", missing, err)
		}
	}
	for _, failing := range []string{"ffmpeg", "paplay"} {
		var tempDir string
		var calls []string
		lookup := func(name string) (string, error) { return name, nil }
		runner := func(name string, _ []string, dir string) error {
			calls = append(calls, name)
			tempDir = dir
			if name == failing {
				return errors.New("test failure")
			}
			return nil
		}
		err := speakWith([]string{"--text", "hello"}, bytes.NewReader(nil), lookup, runner)
		if err == nil || !strings.Contains(err.Error(), failing) {
			t.Fatalf("failed %s: %v", failing, err)
		}
		if _, err := os.Stat(tempDir); !os.IsNotExist(err) {
			t.Fatalf("temporary directory remains after %s failure: %v", failing, err)
		}
		if failing == "ffmpeg" && len(calls) != 1 {
			t.Fatalf("playback ran after synthesis failure: %q", calls)
		}
	}
}

func TestSpeechFilterAndVoiceGain(t *testing.T) {
	if got := speechFilter(6); got != "loudnorm=I=-14:TP=-1.5:LRA=11,volume=6dB,alimiter=limit=0.95" {
		t.Fatalf("filter = %q", got)
	}
	if got := speakVoiceArgs(-3.5); !reflect.DeepEqual(got, []string{"speak", "--gain", "-3.5"}) {
		t.Fatalf("voice speak args = %q", got)
	}
	for _, gain := range []string{"21", "-20.01", "NaN", "loud"} {
		if err := voiceCommand([]string{"--speech-gain", gain}); err == nil {
			t.Fatalf("voice accepted --speech-gain %s", gain)
		}
	}
}
