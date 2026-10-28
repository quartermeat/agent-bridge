package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func scheduleFixture(t *testing.T, body string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGENT_BRIDGE_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("AGENT_BRIDGE_SCHEDULE_UNIT_DIR", filepath.Join(dir, "units"))
	file := filepath.Join(dir, "entry.yaml")
	if err := os.WriteFile(file, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return dir, file
}
func scheduleYAML(kind, script string) string {
	return "schedule: v1\nid: daily-check\nkind: " + kind + "\ncommand:\n  - " + script + "\ncwd: /tmp\non_calendar: daily\ntimeout: 5s\ndescription: Daily check\n"
}
func schedulePendingID(t *testing.T) string {
	t.Helper()
	items, e := schedulePendingList()
	if e != nil || len(items) != 1 {
		t.Fatalf("pending: %+v %v", items, e)
	}
	return items[0].ID
}
func TestScheduleRegistry(t *testing.T) {
	_, file := scheduleFixture(t, scheduleYAML("script", "/bin/true"))
	calls := [][]string{}
	old := scheduleSystemctl
	scheduleSystemctl = func(a ...string) error { calls = append(calls, a); return nil }
	t.Cleanup(func() { scheduleSystemctl = old })
	if e := scheduleCommand([]string{"add", file}); e != nil {
		t.Fatal(e)
	}
	id := schedulePendingID(t)
	if e := scheduleResolve("approve", id); e != nil {
		t.Fatal(e)
	}
	d, _ := scheduleDir()
	s, e := scheduleRead(d, "daily-check")
	if e != nil || s.ID != "daily-check" {
		t.Fatalf("read: %+v %v", s, e)
	}
	if e := scheduleCommand([]string{"add", file}); e == nil {
		t.Fatal("duplicate accepted")
	}
	units, _ := scheduleUnitDir()
	for _, suffix := range []string{"service", "timer"} {
		p := filepath.Join(units, "agent-bridge-schedule-daily-check."+suffix)
		b, e := os.ReadFile(p)
		if e != nil {
			t.Fatal(e)
		}
		if suffix == "service" && !strings.Contains(string(b), "schedule run") {
			t.Fatal(string(b))
		}
		if suffix == "timer" && !strings.Contains(string(b), "OnCalendar=daily") {
			t.Fatal(string(b))
		}
	}
	if len(calls) != 2 || strings.Join(calls[1], " ") != "enable --now agent-bridge-schedule-daily-check.timer" {
		t.Fatalf("calls: %v", calls)
	}
	if e := scheduleCommand([]string{"remove", "daily-check"}); e != nil {
		t.Fatal(e)
	}
	id = schedulePendingID(t)
	if e := scheduleResolve("approve", id); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(scheduleEntryPath(d, "daily-check")); !os.IsNotExist(e) {
		t.Fatalf("entry remains: %v", e)
	}
	if len(calls) != 4 || strings.Join(calls[2], " ") != "disable --now agent-bridge-schedule-daily-check.timer" {
		t.Fatalf("calls: %v", calls)
	}
}
func TestScheduleApprovalGate(t *testing.T) {
	_, file := scheduleFixture(t, scheduleYAML("script", "/bin/true"))
	old := scheduleSystemctl
	scheduleSystemctl = func(...string) error { t.Fatal("systemctl before approval"); return nil }
	t.Cleanup(func() { scheduleSystemctl = old })
	if e := scheduleCommand([]string{"add", file}); e != nil {
		t.Fatal(e)
	}
	d, _ := scheduleDir()
	if _, e := os.Stat(scheduleEntryPath(d, "daily-check")); !os.IsNotExist(e) {
		t.Fatalf("installed early: %v", e)
	}
	units, _ := scheduleUnitDir()
	if entries, e := os.ReadDir(units); e != nil || len(entries) != 0 {
		t.Fatalf("units: %v %v", entries, e)
	}
	pending, e := schedulePendingList()
	if e != nil || len(pending) != 1 || pending[0].Status != "pending" {
		t.Fatalf("approval missing: %v %v", pending, e)
	}
	if e := scheduleResolve("deny", pending[0].ID); e != nil {
		t.Fatal(e)
	}
	pending, _ = schedulePendingList()
	if len(pending) != 0 {
		t.Fatal("denied item remains")
	}
}
func TestScheduleRunRecordsJob(t *testing.T) {
	dir, file := scheduleFixture(t, scheduleYAML("script", filepath.Join(t.TempDir(), "fail.sh")))
	_ = dir
	b, e := os.ReadFile(file)
	if e != nil {
		t.Fatal(e)
	}
	script := strings.TrimSpace(strings.Split(strings.Split(string(b), "  - ")[1], "\n")[0])
	if e = os.WriteFile(script, []byte("#!/bin/sh\necho failure-detail >&2\nexit 7\n"), 0700); e != nil {
		t.Fatal(e)
	}
	old := scheduleSystemctl
	scheduleSystemctl = func(...string) error { return nil }
	t.Cleanup(func() { scheduleSystemctl = old })
	if e := scheduleCommand([]string{"add", file}); e != nil {
		t.Fatal(e)
	}
	if e := scheduleResolve("approve", schedulePendingID(t)); e != nil {
		t.Fatal(e)
	}
	if e := scheduleRun("daily-check"); e == nil {
		t.Fatal("failing script reported success")
	}
	r, _ := root()
	entries, e := os.ReadDir(filepath.Join(r, "jobs"))
	if e != nil || len(entries) != 1 {
		t.Fatalf("jobs: %v %v", entries, e)
	}
	j, e := readJob(entries[0].Name())
	if e != nil {
		t.Fatal(e)
	}
	if j.Status != "failed" || j.ExitCode == nil || *j.ExitCode != 7 {
		t.Fatalf("job: %+v", j)
	}
	if j.UpdatedAt.IsZero() || j.UpdatedAt.Before(j.CreatedAt) {
		t.Fatalf("invalid job timestamps: created=%v updated=%v", j.CreatedAt, j.UpdatedAt)
	}
	log, e := os.ReadFile(j.StderrPath)
	if e != nil || !strings.Contains(string(log), "failure-detail") {
		t.Fatalf("log: %s %v", log, e)
	}
	if _, e = json.Marshal(current(j)); e != nil {
		t.Fatal(e)
	}
}
func TestScheduleRunStatusMatchesWait(t *testing.T) {
	_, file := scheduleFixture(t, scheduleYAML("script", "/bin/true"))
	old := scheduleSystemctl
	scheduleSystemctl = func(...string) error { return nil }
	t.Cleanup(func() { scheduleSystemctl = old })
	if e := scheduleCommand([]string{"add", file}); e != nil {
		t.Fatal(e)
	}
	if e := scheduleResolve("approve", schedulePendingID(t)); e != nil {
		t.Fatal(e)
	}
	if e := scheduleRun("daily-check"); e != nil {
		t.Fatal(e)
	}
	r, e := root()
	if e != nil {
		t.Fatal(e)
	}
	entries, e := os.ReadDir(filepath.Join(r, "jobs"))
	if e != nil || len(entries) != 1 {
		t.Fatalf("jobs: %v %v", entries, e)
	}
	j, e := readJob(entries[0].Name())
	if e != nil {
		t.Fatal(e)
	}
	if j.Status != "completed" || j.ExitCode == nil || *j.ExitCode != 0 {
		t.Fatalf("job: %+v", j)
	}
	if j.CreatedAt.IsZero() || j.UpdatedAt.IsZero() || j.UpdatedAt.Before(j.CreatedAt) {
		t.Fatalf("invalid job timestamps: created=%v updated=%v", j.CreatedAt, j.UpdatedAt)
	}
	if e := wait([]string{j.ID}); e != nil {
		t.Fatalf("wait rejected completed schedule job: %v", e)
	}
}
func TestScheduleLocalEPMRefused(t *testing.T) {
	_, file := scheduleFixture(t, scheduleYAML("local-epm", "/bin/true"))
	b, _ := os.ReadFile(file)
	s, e := parseScheduleYAML(b)
	if e != nil || s.Kind != "local-epm" {
		t.Fatalf("schema: %+v %v", s, e)
	}
	if e = scheduleCommand([]string{"add", file}); e == nil || !strings.Contains(e.Error(), "local_epm_not_implemented") {
		t.Fatalf("add: %v", e)
	}
	d, _ := scheduleDir()
	if e = os.WriteFile(scheduleEntryPath(d, s.ID), b, 0600); e != nil {
		t.Fatal(e)
	}
	if e = scheduleRun(s.ID); e == nil || !strings.Contains(e.Error(), "local_epm_not_implemented") {
		t.Fatalf("run: %v", e)
	}
}
func TestScheduleStrictYAML(t *testing.T) {
	for _, bad := range []string{"unknown: key\n", "id: duplicate\n", "command: /bin/true\n"} {
		_, e := parseScheduleYAML([]byte(scheduleYAML("script", "/bin/true") + bad))
		if e == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestScheduleFlowArgv(t *testing.T) {
	body := strings.Replace(scheduleYAML("script", "/bin/true"), "command:\n  - /bin/true", `command: [/bin/echo, "hello, world", 'two words']`, 1)
	s, err := parseScheduleYAML([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Command) != 3 || s.Command[1] != "hello, world" || s.Command[2] != "two words" {
		t.Fatalf("argv: %q", s.Command)
	}
}
