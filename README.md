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

The hook's `phase` and `detail` are raw reports. In particular, its “ready for direction” detail can mean either a completed turn or a watchdog fallback after activity events went quiet. Consumers should use `activity_state`, `state_detail`, and `active_job_ids` for the job-aware view: `running_job`, `has_running_children`, `active`, `approval_required`, `halted_usage`, `error`, or `unverified`. `unverified` deliberately makes no claim that the agent is available. The terminal `watch` command displays this state and keeps the raw hook phase in parentheses.

An explicit context, session budget, credit, usage, or token rate error produces a machine-readable `halt` object with `kind`, `code`, `source`, and `detected_at`. The kinds are `context_limit`, `usage_limit`, and `rate_limit`; they are distinct because a full context window needs a new or compacted session, while account limits and temporary rate limits have different remedies. Bridge jobs end as `halted` when their failed Codex turn or fatal stderr provides that evidence. A live Claude session can expose the same field after a matching API error. Token totals alone never trigger a halt: they are not a remaining allowance. The monitor does not inspect prompts or ordinary tool output to infer one.

Unacknowledged halted jobs remain in `snapshot` and appear in a prominent `watch` alert until handled. After recording how the work will continue, run `agent-bridge ack JOB_ID` to clear that alert; the job's `halt` and `acknowledged_at` remain in `list` and `show` for history. A completed job is never marked halted solely because it used many tokens.
If a provider stops without an explicit limit error, the bridge cannot verify why; inspect that job's final result before accepting it.

## Launch Codex jobs

Build with `go build -o ~/.local/bin/agent-bridge .`.

```bash
printf 'Inspect this worktree and suggest the next task.' | agent-bridge start --cwd /path/to/worktree --name inspect
agent-bridge start --cwd /path/to/worktree --name task --spawned-by CLAUDE_AGENT_ID --prompt-file task.txt --write
agent-bridge list
agent-bridge show JOB_ID
agent-bridge wait JOB_ID [JOB_ID ...]
agent-bridge ack JOB_ID
agent-bridge stop JOB_ID
```

`start` prints the job ID and returns while Codex runs in a transient user systemd service. The default sandbox is read-only. Add `--write` for workspace-write jobs. Each job's `events_path`, `stderr_path`, and `result_path` are returned by `show`; the JSONL events provide detailed progress. Keep concurrent writing jobs in separate worktrees.

At launch, the bridge detects the nearest live Claude or Codex process in its parent process chain and records it as `spawned_by`. Pass `--spawned-by` with an active `agents[].id` from `snapshot` when launching through a wrapper that breaks that process chain. The explicit ID is validated against the live snapshot.

`wait` blocks until every named job exits, then prints their final JSON records. It exits nonzero if any job failed, stopped, or was lost. Interrupting `wait` leaves the workers running. A coordinator should wait for all jobs it started, inspect their results and diffs, run the relevant checks, and only then finish its turn.

Claude Code can invoke these commands through its shell tool. A Codex session outside this supervisor will not appear in `agent-bridge list`, though `snapshot` can show its live activity when the Codex hook is present. This tool does not attach a spawned job to an existing Codex app conversation; it exposes status and usage to any local agent that calls it.
