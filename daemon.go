package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func latestPath() (string, error) {
	r, err := root()
	if err != nil {
		return "", err
	}
	return filepath.Join(r, "latest.json"), nil
}

func publishSnapshot(s Snapshot) error {
	path, err := latestPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".latest-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func readLatest(maxAge time.Duration) (Snapshot, error) {
	path, err := latestPath()
	if err != nil {
		return Snapshot{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, err
	}
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return Snapshot{}, err
	}
	if s.Version != 1 || s.SampledAt.IsZero() || time.Since(s.SampledAt) > maxAge || time.Until(s.SampledAt) > 5*time.Second {
		return Snapshot{}, errors.New("monitor snapshot is stale or invalid")
	}
	return s, nil
}

func latestCommand(args []string) error {
	if len(args) != 0 {
		return errors.New("latest takes no arguments")
	}
	s, err := readLatest(10 * time.Second)
	if err != nil {
		return fmt.Errorf("latest snapshot unavailable: %w", err)
	}
	return json.NewEncoder(os.Stdout).Encode(s)
}

func daemonCommand(args []string) error {
	f := flag.NewFlagSet("daemon", flag.ContinueOnError)
	interval := f.Duration("interval", 2*time.Second, "Sample interval (at least 500ms)")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || *interval < 500*time.Millisecond {
		return errors.New("usage: agent-bridge daemon [--interval 2s]")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runDaemon(ctx, *interval)
}

func runDaemon(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		s, err := collectSnapshot()
		if err != nil {
			return err
		}
		if err := publishSnapshot(s); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
