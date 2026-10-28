package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type scheduleEntry struct {
	Schedule    string   `json:"schedule"`
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	Command     []string `json:"command"`
	Cwd         string   `json:"cwd"`
	OnCalendar  string   `json:"on_calendar"`
	Timeout     string   `json:"timeout"`
	Description string   `json:"description"`
}
type schedulePending struct {
	ID     string        `json:"id"`
	Status string        `json:"status"`
	Action string        `json:"action"`
	Entry  scheduleEntry `json:"entry"`
	Source string        `json:"source,omitempty"`
}

var scheduleSystemctl = func(args ...string) error {
	return exec.Command("systemctl", append([]string{"--user"}, args...)...).Run()
}

func scheduleDir() (string, error) {
	r, e := root()
	if e != nil {
		return "", e
	}
	p := filepath.Join(r, "schedules")
	if e = os.MkdirAll(p, 0700); e != nil {
		return "", e
	}
	return p, nil
}
func scheduleUnitDir() (string, error) {
	if p := os.Getenv("AGENT_BRIDGE_SCHEDULE_UNIT_DIR"); p != "" {
		if e := os.MkdirAll(p, 0700); e != nil {
			return "", e
		}
		return p, nil
	}
	h, e := os.UserHomeDir()
	if e != nil {
		return "", e
	}
	p := filepath.Join(h, ".config", "systemd", "user")
	if e = os.MkdirAll(p, 0700); e != nil {
		return "", e
	}
	return p, nil
}
func scheduleValidID(id string) bool { return taskName.MatchString(id) && len(id) <= 64 }
func scheduleScalar(v string) (string, error) {
	if strings.HasPrefix(v, "\"") {
		return strconv.Unquote(v)
	}
	if strings.HasPrefix(v, "'") && strings.HasSuffix(v, "'") {
		return strings.ReplaceAll(v[1:len(v)-1], "''", "'"), nil
	}
	if strings.ContainsAny(v, "\n\r") {
		return "", errors.New("invalid scalar")
	}
	return v, nil
}
func scheduleList(v string) ([]string, error) {
	if !strings.HasPrefix(v, "[") || !strings.HasSuffix(v, "]") {
		return nil, errors.New("command must be an argv list")
	}
	body := strings.TrimSpace(v[1 : len(v)-1])
	if body == "" {
		return []string{}, nil
	}
	parts := []string{}
	start := 0
	quote := byte(0)
	escaped := false
	for i := 0; i < len(body); i++ {
		c := body[i]
		if escaped {
			escaped = false
			continue
		}
		if quote == '"' && c == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			quote = c
			continue
		}
		if c == ',' {
			parts = append(parts, strings.TrimSpace(body[start:i]))
			start = i + 1
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated command quote")
	}
	parts = append(parts, strings.TrimSpace(body[start:]))
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		value, e := scheduleScalar(part)
		if e != nil {
			return nil, e
		}
		if value == "" {
			return nil, errors.New("empty command argument")
		}
		out = append(out, value)
	}
	return out, nil
}
func parseScheduleYAML(data []byte) (scheduleEntry, error) {
	var s scheduleEntry
	seen := map[string]bool{}
	lines := strings.Split(string(data), "\n")
	for i := 0; i < len(lines); i++ {
		raw := lines[i]
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if raw != strings.TrimLeft(raw, " \t") {
			return s, fmt.Errorf("invalid indentation on line %d", i+1)
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return s, fmt.Errorf("invalid line %d", i+1)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if seen[k] {
			return s, fmt.Errorf("duplicate key %q", k)
		}
		seen[k] = true
		if k == "command" {
			if v == "" {
				for i+1 < len(lines) && strings.HasPrefix(lines[i+1], "  - ") {
					i++
					a, e := scheduleScalar(strings.TrimSpace(strings.TrimPrefix(lines[i], "  - ")))
					if e != nil {
						return s, e
					}
					s.Command = append(s.Command, a)
				}
			} else if strings.HasPrefix(v, "[") {
				parsed, e := scheduleList(v)
				if e != nil {
					return s, e
				}
				s.Command = parsed
			} else {
				return s, errors.New("command must be an argv list")
			}
			continue
		}
		value, e := scheduleScalar(v)
		if e != nil {
			return s, e
		}
		switch k {
		case "schedule":
			s.Schedule = value
		case "id":
			s.ID = value
		case "kind":
			s.Kind = value
		case "cwd":
			s.Cwd = value
		case "on_calendar":
			s.OnCalendar = value
		case "timeout":
			s.Timeout = value
		case "description":
			s.Description = value
		default:
			return s, fmt.Errorf("unknown key %q", k)
		}
	}
	if s.Schedule != "v1" || !scheduleValidID(s.ID) || (s.Kind != "script" && s.Kind != "local-epm") || len(s.Command) == 0 || !filepath.IsAbs(s.Command[0]) || !filepath.IsAbs(s.Cwd) || s.OnCalendar == "" || s.Timeout == "" || s.Description == "" {
		return s, errors.New("missing or invalid schedule fields")
	}
	d, e := time.ParseDuration(s.Timeout)
	if e != nil || d <= 0 {
		return s, errors.New("invalid timeout")
	}
	for _, v := range append(append([]string{}, s.Command...), s.Cwd, s.OnCalendar, s.Description) {
		if v == "" || strings.ContainsAny(v, "\x00\r\n") {
			return s, errors.New("invalid schedule value")
		}
	}
	return s, nil
}
func scheduleEntryPath(dir, id string) string   { return filepath.Join(dir, id+".yaml") }
func schedulePendingPath(dir, id string) string { return filepath.Join(dir, "pending", id+".json") }
func scheduleRead(dir, id string) (scheduleEntry, error) {
	if !scheduleValidID(id) {
		return scheduleEntry{}, errors.New("invalid schedule ID")
	}
	b, e := os.ReadFile(scheduleEntryPath(dir, id))
	if e != nil {
		return scheduleEntry{}, e
	}
	return parseScheduleYAML(b)
}
func schedulePendingList() ([]schedulePending, error) {
	d, e := scheduleDir()
	if e != nil {
		return nil, e
	}
	p := filepath.Join(d, "pending")
	entries, e := os.ReadDir(p)
	if errors.Is(e, os.ErrNotExist) {
		return []schedulePending{}, nil
	}
	if e != nil {
		return nil, e
	}
	out := []schedulePending{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		b, e := os.ReadFile(filepath.Join(p, entry.Name()))
		if e != nil {
			return nil, e
		}
		var item schedulePending
		if e = json.Unmarshal(b, &item); e != nil {
			return nil, e
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func scheduleQuote(s string) string {
	return "\"" + strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(s, "\\", "\\\\"), "\"", "\\\""), "%", "%%") + "\""
}
func scheduleUnits(s scheduleEntry) (string, string, error) {
	bin, e := os.Executable()
	if e != nil {
		return "", "", e
	}
	name := "agent-bridge-schedule-" + s.ID
	service := fmt.Sprintf("[Unit]\nDescription=Agent Bridge schedule %s\n[Service]\nType=oneshot\nExecStart=%s schedule run %s\n", s.ID, scheduleQuote(bin), scheduleQuote(s.ID))
	timer := fmt.Sprintf("[Unit]\nDescription=Timer for Agent Bridge schedule %s\n[Timer]\nOnCalendar=%s\nPersistent=true\nUnit=%s.service\n[Install]\nWantedBy=timers.target\n", s.ID, s.OnCalendar, name)
	return service, timer, nil
}
func scheduleResolve(action, id string) error {
	d, e := scheduleDir()
	if e != nil {
		return e
	}
	p := schedulePendingPath(d, id)
	b, e := os.ReadFile(p)
	if e != nil {
		return e
	}
	var pending schedulePending
	if e = json.Unmarshal(b, &pending); e != nil {
		return e
	}
	if pending.ID != id {
		return errors.New("pending schedule ID mismatch")
	}
	if action == "deny" {
		return os.Remove(p)
	}
	if pending.Entry.Kind == "local-epm" {
		return errors.New("local_epm_not_implemented")
	}
	unitDir, e := scheduleUnitDir()
	if e != nil {
		return e
	}
	name := "agent-bridge-schedule-" + pending.Entry.ID
	servicePath := filepath.Join(unitDir, name+".service")
	timerPath := filepath.Join(unitDir, name+".timer")
	switch pending.Action {
	case "add":
		if _, e = os.Stat(scheduleEntryPath(d, pending.Entry.ID)); e == nil {
			return errors.New("duplicate schedule ID")
		}
		if !errors.Is(e, os.ErrNotExist) {
			return e
		}
		service, timer, e := scheduleUnits(pending.Entry)
		if e != nil {
			return e
		}
		if e = os.WriteFile(scheduleEntryPath(d, pending.Entry.ID), []byte(pending.Source), 0600); e != nil {
			return e
		}
		if e = os.WriteFile(servicePath, []byte(service), 0600); e != nil {
			return e
		}
		if e = os.WriteFile(timerPath, []byte(timer), 0600); e != nil {
			return e
		}
		if e = scheduleSystemctl("daemon-reload"); e != nil {
			return e
		}
		if e = scheduleSystemctl("enable", "--now", name+".timer"); e != nil {
			return e
		}
	case "remove":
		if _, e = scheduleRead(d, pending.Entry.ID); e != nil {
			return e
		}
		if e = scheduleSystemctl("disable", "--now", name+".timer"); e != nil {
			return e
		}
		if e = os.Remove(timerPath); e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		if e = os.Remove(servicePath); e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		if e = scheduleSystemctl("daemon-reload"); e != nil {
			return e
		}
		if e = os.Remove(scheduleEntryPath(d, pending.Entry.ID)); e != nil {
			return e
		}
	default:
		return errors.New("unknown schedule action")
	}
	return os.Remove(p)
}
func scheduleRun(id string) error {
	d, e := scheduleDir()
	if e != nil {
		return e
	}
	s, e := scheduleRead(d, id)
	if e != nil {
		return e
	}
	if s.Kind == "local-epm" {
		return errors.New("local_epm_not_implemented")
	}
	runID, e := newRequestID()
	if e != nil {
		return e
	}
	runID = "schedule-" + runID
	dir, e := jobDir(runID)
	if e != nil {
		return e
	}
	if e = os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	now := time.Now().UTC()
	j := Job{ID: runID, Name: "schedule-" + id, Agent: "script", Role: "schedule", Provider: "script", Cwd: s.Cwd, Sandbox: "none", Launcher: "schedule", Status: "running", CreatedAt: now, UpdatedAt: now, EventsPath: filepath.Join(dir, "events.jsonl"), StderrPath: filepath.Join(dir, "stderr.log"), ResultPath: filepath.Join(dir, "result.txt")}
	if e = writeJob(j); e != nil {
		return e
	}
	log, e := os.OpenFile(j.StderrPath, os.O_CREATE|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer log.Close()
	timeout, _ := time.ParseDuration(s.Timeout)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.Command[0], s.Command[1:]...)
	cmd.Dir = s.Cwd
	cmd.Stdout = log
	cmd.Stderr = log
	e = cmd.Run()
	code := 0
	if e != nil {
		code = 1
		var ex *exec.ExitError
		if errors.As(e, &ex) {
			code = ex.ExitCode()
		}
		j.Status = "failed"
		j.Error = e.Error()
	} else {
		j.Status = "completed"
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		code = 124
		j.Status = "failed"
		j.Error = "timeout"
	}
	j.ExitCode = &code
	j.UpdatedAt = time.Now().UTC()
	_ = os.WriteFile(j.ResultPath, []byte(fmt.Sprintf("exit_code: %d\n", code)), 0600)
	if w := writeJob(j); w != nil {
		return w
	}
	fmt.Println(runID)
	return e
}
func scheduleCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: agent-bridge schedule add|list|show|remove|run")
	}
	d, e := scheduleDir()
	if e != nil {
		return e
	}
	switch args[0] {
	case "add":
		if len(args) != 2 {
			return errors.New("usage: agent-bridge schedule add FILE")
		}
		b, e := os.ReadFile(args[1])
		if e != nil {
			return e
		}
		s, e := parseScheduleYAML(b)
		if e != nil {
			return e
		}
		if s.Kind == "local-epm" {
			return errors.New("local_epm_not_implemented")
		}
		if _, e = os.Stat(scheduleEntryPath(d, s.ID)); e == nil {
			return errors.New("duplicate schedule ID")
		}
		if !errors.Is(e, os.ErrNotExist) {
			return e
		}
		pending, e := schedulePendingList()
		if e != nil {
			return e
		}
		for _, p := range pending {
			if p.Entry.ID == s.ID {
				return errors.New("schedule already pending")
			}
		}
		id, e := newRequestID()
		if e != nil {
			return e
		}
		id = "schedule-" + id
		p := schedulePendingPath(d, id)
		if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
			return e
		}
		if e = writeJSONPrivate(p, schedulePending{ID: id, Status: "pending", Action: "add", Entry: s, Source: string(b)}); e != nil {
			return e
		}
		fmt.Println(id)
		return nil
	case "remove":
		if len(args) != 2 {
			return errors.New("usage: agent-bridge schedule remove ID")
		}
		s, e := scheduleRead(d, args[1])
		if e != nil {
			return e
		}
		id, e := newRequestID()
		if e != nil {
			return e
		}
		id = "schedule-" + id
		p := schedulePendingPath(d, id)
		if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
			return e
		}
		if e = writeJSONPrivate(p, schedulePending{ID: id, Status: "pending", Action: "remove", Entry: s}); e != nil {
			return e
		}
		fmt.Println(id)
		return nil
	case "list":
		f := flag.NewFlagSet("schedule list", flag.ContinueOnError)
		asJSON := f.Bool("json", false, "")
		if e = f.Parse(args[1:]); e != nil {
			return e
		}
		if f.NArg() != 0 {
			return errors.New("usage: agent-bridge schedule list [--json]")
		}
		entries, e := os.ReadDir(d)
		if e != nil {
			return e
		}
		out := []scheduleEntry{}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".yaml") {
				s, e := scheduleRead(d, strings.TrimSuffix(entry.Name(), ".yaml"))
				if e != nil {
					return e
				}
				out = append(out, s)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(out)
		}
		for _, s := range out {
			fmt.Printf("%s\t%s\t%s\n", s.ID, s.Kind, s.OnCalendar)
		}
		return nil
	case "show":
		if len(args) != 2 {
			return errors.New("usage: agent-bridge schedule show ID")
		}
		s, e := scheduleRead(d, args[1])
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(s)
	case "run":
		if len(args) != 2 {
			return errors.New("usage: agent-bridge schedule run ID")
		}
		return scheduleRun(args[1])
	default:
		return errors.New("usage: agent-bridge schedule add|list|show|remove|run")
	}
}
func scheduleApprovalsCommand() error {
	pending, e := schedulePendingList()
	if e != nil {
		return e
	}
	return json.NewEncoder(os.Stdout).Encode(pending)
}

func combinedApprovalsCommand(args []string) error {
	if len(args) != 0 {
		return errors.New("approvals takes no arguments")
	}
	p, e := schedulePendingList()
	if e != nil {
		return e
	}
	out := make([]any, 0, len(p))
	for _, item := range p {
		out = append(out, item)
	}
	dir, e := pendingDir()
	if e != nil {
		return e
	}
	entries, e := os.ReadDir(dir)
	if e != nil {
		return e
	}
	spool, e := spoolDir()
	if e != nil {
		return e
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			id := strings.TrimSuffix(entry.Name(), ".json")
			if r, e := readResult(spool, id); e == nil && r.Status == "pending" {
				out = append(out, r)
			}
		}
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}
