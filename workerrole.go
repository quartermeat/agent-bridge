package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const maxJobDepth = 3

// callerJob identifies a bridge job the current process runs inside. Process
// ancestry is checked first; the environment is a second signal.
func callerJob() (id, role string, ok bool) {
	if j := ancestorJob(os.Getppid()); j != nil {
		return j.ID, j.Role, true
	}
	if id := os.Getenv("AGENT_BRIDGE_JOB_ID"); id != "" {
		role := os.Getenv("AGENT_BRIDGE_JOB_ROLE")
		if role == "" {
			role = "coder"
		}
		return id, role, true
	}
	return "", "", false
}

// refuseWorkerLaunch rejects job-launching commands from inside a bridge
// worker. Coder jobs never launch. EPM jobs may launch only when allowEPM is
// set (the documented request path).
func refuseWorkerLaunch(allowEPM bool) error {
	id, role, ok := callerJob()
	if !ok || (allowEPM && role == "epm") {
		return nil
	}
	if role == "epm" {
		return fmt.Errorf("bridge EPM jobs cannot launch jobs directly (job %s, role epm); use agent-bridge request", id)
	}
	return fmt.Errorf("bridge workers cannot launch jobs (job %s, role %s); finish the task or report partial/blocked", id, role)
}

// workerCommandAllowed is the complete command policy inside a bridge job.
// Unknown commands are refused before dispatch. Keep subcommands explicit so
// adding a mutating operation to a command does not silently make it available.
func workerCommandAllowed(role string, args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "request":
		return role == "epm"
	case "request-status", "approvals", "list", "show", "wait", "snapshot", "latest", "watch", "voice-log":
		return true
	case "schedule":
		return len(args) > 1 && (args[1] == "list" || args[1] == "show")
	case "local-queue":
		return len(args) > 1 && (args[1] == "status" || args[1] == "report")
	case "local-model":
		// yield intentionally wraps an arbitrary command while releasing the GPU.
		return len(args) > 1 && (args[1] == "status" || args[1] == "yield")
	}
	return false
}

func refuseWorkerCommand(args []string) error {
	_, role, ok := callerJob()
	if !ok || workerCommandAllowed(role, args) {
		return nil
	}
	return refuseWorkerLaunch(false)
}

func bridgeChildEnv() []string {
	var env []string
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "AGENT_BRIDGE_LAUNCHER=") {
			env = append(env, item)
		}
	}
	return env
}

func jobEnv(j Job) []string {
	return append(bridgeChildEnv(),
		"AGENT_BRIDGE_JOB_ID="+j.ID,
		"AGENT_BRIDGE_JOB_ROLE="+j.Role,
		"AGENT_BRIDGE_JOB_DEPTH="+strconv.Itoa(j.Depth),
	)
}

func workerRoleBlock(id, role string) string {
	b := fmt.Sprintf("Bridge worker role: job %s, role %s.\nThis session is a bridge worker, not the interactive EPM. This role overrides any EPM instructions in AGENTS.md or CLAUDE.md. Do the task yourself. Your final message must be one bare `report: v2` YAML document of at most 50 words: no fences or prose, with keys status, summary, checks, and optional detail.\n", id, role)
	if role == "coder" {
		b += "Launch no jobs: do not run agent-bridge start, request, ask, local, or schedule.\n"
	} else {
		b += "Launch jobs only with agent-bridge request; never use start.\n"
	}
	return b + "\n"
}
