package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type modelLease struct {
	PID       int       `json:"pid"`
	Start     string    `json:"start"`
	Reason    string    `json:"reason,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type modelStatus struct {
	Model     string       `json:"model"`
	Pinned    bool         `json:"pinned"`
	Loaded    bool         `json:"loaded"`
	Yielded   bool         `json:"yielded"`
	Leases    []modelLease `json:"leases"`
	VRAMBytes int64        `json:"vram_bytes"`
}

func launcherModel() string {
	if v := strings.TrimSpace(os.Getenv("AGENT_BRIDGE_LAUNCHER_MODEL")); v != "" {
		return v
	}
	return "qwen3-coder:30b"
}
func modelStateDir() (string, error) {
	r, err := root()
	if err != nil {
		return "", err
	}
	p := filepath.Join(r, "local-model")
	err = os.MkdirAll(filepath.Join(p, "leases"), 0700)
	return p, err
}
func withModelLock(dir string, fn func() error) error {
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}
func liveModelLeases(dir string) ([]modelLease, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "leases"))
	if err != nil {
		return nil, err
	}
	leases := make([]modelLease, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, "leases", entry.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var lease modelLease
		if json.Unmarshal(b, &lease) != nil || !alive(lease.PID, lease.Start) {
			_ = os.Remove(path)
			continue
		}
		leases = append(leases, lease)
	}
	return leases, nil
}
func modelLoaded(ctx context.Context, base, model string) (bool, int64, error) {
	resp, err := ollamaRequest(ctx, http.MethodGet, "/api/ps", base, nil)
	if err != nil {
		return false, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, 0, fmt.Errorf("Ollama /api/ps: HTTP %d", resp.StatusCode)
	}
	var data struct {
		Models []struct {
			Name     string `json:"name"`
			SizeVRAM int64  `json:"size_vram"`
		} `json:"models"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&data); err != nil {
		return false, 0, err
	}
	for _, m := range data.Models {
		if m.Name == model {
			return true, m.SizeVRAM, nil
		}
	}
	return false, 0, nil
}
func setModelKeepAlive(ctx context.Context, base, model string, keep int) error {
	body, err := json.Marshal(map[string]any{"model": model, "prompt": "", "keep_alive": keep})
	if err != nil {
		return err
	}
	resp, err := ollamaRequest(ctx, http.MethodPost, "/api/generate", base, bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Ollama /api/generate: HTTP %d", resp.StatusCode)
	}
	_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<20))
	return err
}
func writeModelActivity(model, detail string) error {
	home, err := monitorHome()
	if err != nil {
		return err
	}
	dir := activityDir(home)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	id := "local-epm-model"
	return writeJSONPrivate(filepath.Join(dir, id+".json"), activityFile{ID: id, Name: "EPM model", Phase: "idle", Detail: detail, Model: model, SampledAt: time.Now().UTC()})
}
func keepModelOnce(ctx context.Context, dir, base, model string) error {
	return withModelLock(dir, func() error {
		leases, err := liveModelLeases(dir)
		if err != nil {
			return err
		}
		if len(leases) > 0 {
			return writeModelActivity(model, "yielded")
		}
		loaded, _, err := modelLoaded(ctx, base, model)
		if err != nil {
			return err
		}
		if !loaded {
			if err = setModelKeepAlive(ctx, base, model, -1); err != nil {
				return err
			}
		}
		return writeModelActivity(model, "pinned")
	})
}
func pinModelAtStart(ctx context.Context, dir, base, model string) error {
	return withModelLock(dir, func() error {
		leases, err := liveModelLeases(dir)
		if err != nil {
			return err
		}
		if len(leases) != 0 {
			return writeModelActivity(model, "yielded")
		}
		if err := setModelKeepAlive(ctx, base, model, -1); err != nil {
			return err
		}
		return writeModelActivity(model, "pinned")
	})
}

func keepModel(ctx context.Context, interval time.Duration, model string) error {
	dir, err := modelStateDir()
	if err != nil {
		return err
	}
	base, err := ollamaBase()
	if err != nil {
		return err
	}
	initial := true
	return keepModelLoop(ctx, interval, func(ctx context.Context) error {
		tickCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if initial {
			err = pinModelAtStart(tickCtx, dir, base, model)
		} else {
			err = keepModelOnce(tickCtx, dir, base, model)
		}
		if err == nil {
			initial = false
		}
		return err
	}, func(message string) { fmt.Fprintln(os.Stderr, "local-model keep:", message) })
}

func keepModelLoop(ctx context.Context, interval time.Duration, attempt func(context.Context) error, log func(string)) error {
	retry := time.Second
	if retry > interval {
		retry = interval
	}
	if retry <= 0 {
		retry = time.Second
	}
	var failing bool
	for {
		err := attempt(ctx)
		delay := interval
		if err != nil {
			if !failing && ctx.Err() == nil {
				log(err.Error())
			}
			failing = true
			delay = retry
			if retry < interval {
				retry *= 2
				if retry > interval {
					retry = interval
				}
			}
		} else {
			if failing {
				log("Ollama available; model pinned")
			}
			failing = false
			retry = time.Second
			if retry > interval {
				retry = interval
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}
func createModelLease(dir, reason string) (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "leases", hex.EncodeToString(token[:])+".json")
	lease := modelLease{PID: os.Getpid(), Start: processStart(os.Getpid()), Reason: reason, CreatedAt: time.Now().UTC()}
	if lease.Start == "" {
		return "", errors.New("cannot identify lease process")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	err = json.NewEncoder(f).Encode(lease)
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}
func yieldModel(ctx context.Context, model, reason string, command []string, run func([]string) error) error {
	dir, err := modelStateDir()
	if err != nil {
		return err
	}
	base, err := ollamaBase()
	if err != nil {
		return err
	}
	var leasePath string
	err = withModelLock(dir, func() error {
		leasePath, err = createModelLease(dir, reason)
		if err != nil {
			return err
		}
		unloadCtx, unloadCancel := context.WithTimeout(ctx, 30*time.Second)
		defer unloadCancel()
		if err = setModelKeepAlive(unloadCtx, base, model, 0); err != nil {
			return err
		}
		waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		for {
			loaded, _, err := modelLoaded(waitCtx, base, model)
			if err != nil {
				return err
			}
			if !loaded {
				return writeModelActivity(model, "yielded")
			}
			select {
			case <-waitCtx.Done():
				return fmt.Errorf("model unload timed out: %w", waitCtx.Err())
			case <-time.After(200 * time.Millisecond):
			}
		}
	})
	if leasePath != "" {
		defer func() {
			repinCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if releaseErr := releaseModelLease(repinCtx, dir, base, model, leasePath); releaseErr != nil {
				fmt.Fprintln(os.Stderr, "local-model yield: repin:", releaseErr)
			}
		}()
	}
	if err != nil {
		return err
	}
	return run(command)
}
func releaseModelLease(ctx context.Context, dir, base, model, path string) error {
	return withModelLock(dir, func() error {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		leases, err := liveModelLeases(dir)
		if err != nil {
			return err
		}
		if len(leases) != 0 {
			return writeModelActivity(model, "yielded")
		}
		if err := setModelKeepAlive(ctx, base, model, -1); err != nil {
			return err
		}
		return writeModelActivity(model, "pinned")
	})
}

func currentModelStatus(ctx context.Context, model string) (modelStatus, error) {
	s := modelStatus{Model: model, Leases: []modelLease{}}
	dir, err := modelStateDir()
	if err != nil {
		return s, err
	}
	err = withModelLock(dir, func() error { var e error; s.Leases, e = liveModelLeases(dir); return e })
	if err != nil {
		return s, err
	}
	s.Yielded = len(s.Leases) > 0
	base, err := ollamaBase()
	if err != nil {
		return s, err
	}
	s.Loaded, s.VRAMBytes, err = modelLoaded(ctx, base, model)
	if err != nil {
		return s, err
	}
	s.Pinned = s.Loaded && !s.Yielded
	return s, nil
}
func waitForModelLease(ctx context.Context) error {
	dir, err := modelStateDir()
	if err != nil {
		return err
	}
	logged := false
	for {
		var leases []modelLease
		err = withModelLock(dir, func() error { var e error; leases, e = liveModelLeases(dir); return e })
		if err != nil {
			return err
		}
		if len(leases) == 0 {
			return nil
		}
		if !logged {
			fmt.Fprintln(os.Stderr, "launcher: local model yielded to GPU job; waiting for lease release")
			logged = true
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("local model yielded: %w", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}
func localModelCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: agent-bridge local-model keep|yield|status")
	}
	model := launcherModel()
	switch args[0] {
	case "keep":
		f := flag.NewFlagSet("local-model keep", flag.ContinueOnError)
		m := f.String("model", model, "Ollama model")
		interval := f.Duration("interval", 60*time.Second, "Check interval")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if f.NArg() != 0 || *interval <= 0 || *m == "" {
			return errors.New("usage: local-model keep [--model M] [--interval 60s]")
		}
		return keepModel(context.Background(), *interval, *m)
	case "yield":
		f := flag.NewFlagSet("local-model yield", flag.ContinueOnError)
		reason := f.String("reason", "", "Reason for GPU lease")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		command := f.Args()
		if len(command) == 0 {
			return errors.New("usage: local-model yield [--reason R] -- CMD ARGS...")
		}
		return yieldModel(context.Background(), model, *reason, command, func(argv []string) error {
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := cmd.Run(); err != nil {
				return err
			}
			return nil
		})
	case "status":
		f := flag.NewFlagSet("local-model status", flag.ContinueOnError)
		jsonFlag := f.Bool("json", false, "JSON output")
		m := f.String("model", model, "Ollama model")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return errors.New("usage: local-model status [--json] [--model M]")
		}
		s, err := currentModelStatus(context.Background(), *m)
		if err != nil {
			return err
		}
		if *jsonFlag {
			return json.NewEncoder(os.Stdout).Encode(s)
		}
		fmt.Printf("%s: loaded=%t pinned=%t yielded=%t leases=%d vram_bytes=%d\n", s.Model, s.Loaded, s.Pinned, s.Yielded, len(s.Leases), s.VRAMBytes)
		return nil
	default:
		return fmt.Errorf("unknown local-model command %q", args[0])
	}
}
