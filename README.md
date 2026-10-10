# agent-bridge

Launch observable Codex CLI jobs from Claude Code or any shell. Jobs run in separate processes and write private JSONL event logs and status records under `~/.local/state/agent-bridge/jobs/`.

Build with `go build -o ~/.local/bin/agent-bridge .`.

```bash
printf 'Inspect this worktree and suggest the next task.' | agent-bridge start --cwd /path/to/worktree --name inspect
agent-bridge list
agent-bridge show JOB_ID
agent-bridge stop JOB_ID
```

`start` prints the job ID and returns while Codex runs in a transient user systemd service. The default sandbox is read-only. Add `--write` for workspace-write jobs. Each job's `events_path`, `stderr_path`, and `result_path` are returned by `show`; the JSONL events provide detailed progress. Keep concurrent writing jobs in separate worktrees.

Claude Code can invoke these commands through its shell tool. A Codex session outside this supervisor will not appear in `agent-bridge list`. This tool does not attach a spawned job to an existing Codex app conversation; it exposes its status and logs to any local agent that calls it.
