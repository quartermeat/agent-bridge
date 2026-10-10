# agent-bridge

Launch observable Codex CLI jobs from Claude Code or any shell. Jobs run in separate processes and write private JSONL event logs and status records under `~/.local/state/agent-bridge/jobs/`.

## Live awareness and usage

```bash
agent-bridge snapshot                 # one machine-readable JSON sample
agent-bridge watch --interval 2s      # live terminal view; Ctrl-C to exit
```

The snapshot combines Claude Code and Codex activity from the existing Digital Terrarium lifecycle hooks with token counters from their local session logs. It reports phase, safe activity detail, model, project path, process ID, and session input/output tokens when available. A session that has not written usage yet has `usage_known: false`. `watch` shows the change in input and output tokens between refreshes. `jobs` lists recent Codex jobs launched through this bridge, including exit status and completed-turn usage.

Every job has a `spawned_by` field. When known, it contains `agent_id` (matching `agents[].id`), `provider`, and the parent's `session_id` when available. It is `null` when the parent could not be identified, including older jobs. A bridge job also exposes its Codex `session_id` from the event stream; while that session is live, its `agents[]` entry carries the same `spawned_by` object. Viewers can join a job to its worker by `session_id` and the worker to its parent by `spawned_by.agent_id`.

Input tokens include cached reads and cache writes; those components are also shown separately. The counters are session totals, not a bill, and update when the clients write usage events. The snapshot reads usage fields only; it does not return prompts, responses, or tool output. The `version: 1` snapshot has provider-labeled agents and source status so a local-model/GPU sampler can be added later.

The activity hooks must be running to report an agent's current phase. If they are absent, `sources` indicates that limitation and bridge jobs remain visible. Reading `snapshot` does not require the Digital Terrarium app or HTTP service to be running.

## Launch Codex jobs

Build with `go build -o ~/.local/bin/agent-bridge .`.

```bash
printf 'Inspect this worktree and suggest the next task.' | agent-bridge start --cwd /path/to/worktree --name inspect
agent-bridge start --cwd /path/to/worktree --name task --spawned-by CLAUDE_AGENT_ID --prompt-file task.txt --write
agent-bridge list
agent-bridge show JOB_ID
agent-bridge wait JOB_ID [JOB_ID ...]
agent-bridge stop JOB_ID
```

`start` prints the job ID and returns while Codex runs in a transient user systemd service. The default sandbox is read-only. Add `--write` for workspace-write jobs. Each job's `events_path`, `stderr_path`, and `result_path` are returned by `show`; the JSONL events provide detailed progress. Keep concurrent writing jobs in separate worktrees.

At launch, the bridge detects the nearest live Claude or Codex process in its parent process chain and records it as `spawned_by`. Pass `--spawned-by` with an active `agents[].id` from `snapshot` when launching through a wrapper that breaks that process chain. The explicit ID is validated against the live snapshot.

`wait` blocks until every named job exits, then prints their final JSON records. It exits nonzero if any job failed, stopped, or was lost. Interrupting `wait` leaves the workers running. A coordinator should wait for all jobs it started, inspect their results and diffs, run the relevant checks, and only then finish its turn.

Claude Code can invoke these commands through its shell tool. A Codex session outside this supervisor will not appear in `agent-bridge list`, though `snapshot` can show its live activity when the Codex hook is present. This tool does not attach a spawned job to an existing Codex app conversation; it exposes status and usage to any local agent that calls it.
