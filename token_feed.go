package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The first four lines are the public feed. Session lines are a durable cursor
// so a daemon restart cannot count the same session tokens twice.
type tokenFeed struct {
	Claude   int64
	Codex    int64
	Sessions map[string]int64
}

func tokenFeedPath() (string, error) {
	r, err := root()
	if err != nil {
		return "", err
	}
	return filepath.Join(r, "token-feed.tsv"), nil
}

func loadTokenFeed() (tokenFeed, error) {
	f := tokenFeed{Sessions: map[string]int64{}}
	path, err := tokenFeedPath()
	if err != nil {
		return f, err
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	scan := bufio.NewScanner(bytes.NewReader(b))
	if !scan.Scan() || scan.Text() != "agent-bridge-token-feed\t1" {
		return f, errors.New("invalid token feed header")
	}
	var sampled, claude, codex bool
	for scan.Scan() {
		parts := strings.Split(scan.Text(), "\t")
		switch {
		case len(parts) == 2 && parts[0] == "sampled_at":
			if _, err := time.Parse(time.RFC3339Nano, parts[1]); err != nil {
				return f, err
			}
			sampled = true
		case len(parts) == 2 && (parts[0] == "claude" || parts[0] == "codex"):
			n, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil || n < 0 {
				return f, errors.New("invalid token total")
			}
			if parts[0] == "claude" {
				f.Claude = n
				claude = true
			} else {
				f.Codex = n
				codex = true
			}
		case len(parts) == 4 && parts[0] == "session" && (parts[1] == "claude" || parts[1] == "codex") && parts[2] != "":
			n, err := strconv.ParseInt(parts[3], 10, 64)
			if err != nil || n < 0 {
				return f, errors.New("invalid session total")
			}
			f.Sessions[parts[1]+"\t"+parts[2]] = n
		default:
			return f, errors.New("invalid token feed row")
		}
	}
	if err := scan.Err(); err != nil {
		return f, err
	}
	if !sampled || !claude || !codex {
		return f, errors.New("incomplete token feed")
	}
	return f, nil
}

func (f *tokenFeed) addSnapshot(s Snapshot) {
	for _, a := range s.Agents {
		if !a.UsageKnown || a.SessionID == "" || (a.Provider != "claude" && a.Provider != "codex") || strings.ContainsAny(a.SessionID, "\t\n\r") {
			continue
		}
		current := a.Usage.Input + a.Usage.Output
		if current < 0 {
			continue
		}
		key := a.Provider + "\t" + a.SessionID
		prior := f.Sessions[key]
		if current <= prior {
			continue
		}
		delta := current - prior
		f.Sessions[key] = current
		if a.Provider == "claude" {
			f.Claude += delta
		} else {
			f.Codex += delta
		}
	}
}

func (f tokenFeed) publish(sampledAt time.Time) error {
	path, err := tokenFeedPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "agent-bridge-token-feed\t1\nsampled_at\t%s\nclaude\t%d\ncodex\t%d\n", sampledAt.Format(time.RFC3339Nano), f.Claude, f.Codex)
	keys := make([]string, 0, len(f.Sessions))
	for key := range f.Sessions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&b, "session\t%s\t%d\n", key, f.Sessions[key])
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".token-feed-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
