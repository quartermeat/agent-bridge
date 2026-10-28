# agent-bridge reference

Launch observable Codex CLI jobs from Claude Code or any shell. Jobs run in separate processes and write private JSONL event logs and status records under `~/.local/state/agent-bridge/jobs/`.

## Live awareness and usage

```bash
agent-bridge snapshot                 # one machine-readable JSON sample
agent-bridge latest                   # most recent daemon sample; fails if stale
agent-bridge watch --interval 2s      # full-screen viewer; q or Ctrl-C to exit
agent-bridge watch --plain            # append plain frames (also automatic when piped)
agent-bridge watch --once             # print one plain frame
agent-bridge speak --text 'Agent bridge is ready.'
```

## Always-up apps

Install the binary and the four user units. The viewer belongs to the graphical session; the others run under the user manager.

```bash
go build -o ~/.local/bin/agent-bridge .
for unit in monitor launcher viewer local-model; do
  install -Dm644 "packaging/agent-bridge-${unit}.service" "$HOME/.config/systemd/user/agent-bridge-${unit}.service"
done
install -Dm644 packaging/agent-bridge-viewer.desktop "$HOME/.local/share/applications/agent-bridge-viewer.desktop"
systemctl --user daemon-reload
systemctl --user enable --now agent-bridge-monitor.service agent-bridge-launcher.service agent-bridge-local-model.service agent-bridge-viewer.service
```

`agent-bridge-monitor.service` samples activity; `agent-bridge-launcher.service` handles launch requests; `agent-bridge-viewer.service` runs `watch` in an XFCE terminal; `agent-bridge-local-model.service` keeps the launcher's EPM model loaded in Ollama. The model defaults to `AGENT_BRIDGE_LAUNCHER_MODEL` or `qwen3-coder:30b`; configure Ollama and pull the model first. The local-model unit retries with backoff until Ollama is available, regardless of whether Ollama starts before or after the user unit. `systemctl --user stop agent-bridge-viewer.service` closes the viewer window. The Agent Viewer app-menu entry restarts it. Disable a unit with `systemctl --user disable --now UNIT.service` and enable it again with `systemctl --user enable --now UNIT.service`.

Wrap VRAM-heavy work with `agent-bridge local-model yield --reason 'FLUX render' -- render-command arg1 arg2`. This creates a private lease, unloads the model, waits up to 30 seconds for Ollama to report it gone, runs the command with inherited stdio, then releases the lease. Several renders can hold leases concurrently; stale leases from dead processes are ignored. The last lease re-pins when its command ends; the keep service also checks every 60 seconds by default. `agent-bridge local-model status --json` reports model, loaded/pinned/yielded state, leases, and VRAM bytes. During a yield the launcher waits up to its 30-second model verdict deadline, then escalates a request if the model remains unavailable.

## Background monitor

Install and enable the monitor with the other user units as shown above.

The daemon starts under the user systemd manager and refreshes `~/.local/state/agent-bridge/latest.json` every two seconds. It writes the file atomically with user-only permissions. Other monitors can read that file or call `agent-bridge latest`; `latest` exits nonzero if the sample is older than ten seconds. `snapshot` always collects a fresh sample. `watch` uses the daemon's sample when available and falls back to direct sampling otherwise. Closing the viewer window does not stop the daemon. In a terminal, `watch` uses the alternate screen and restores the prior display on exit, so redraws do not fill scrollback. `j`/`k` and arrow keys scroll one line; `PgUp`/`PgDn` and space scroll one page; `g`/`G` jump to top/bottom. `c` toggles compact active agents/jobs; `f` cycles all, active, jobs, and local filters; `q` or Ctrl-C quits. The last row shows time, daemon/direct origin, visible line range, filter, and key hints. Resize redraws the viewport. `--interval` defaults to 2 s and must be at least 500 ms. `--plain` appends frames without escape codes, and `--once` prints one plain frame; non-terminal output automatically uses plain mode.

`speak` provides local one-way audio output. It reads text from `--text` or stdin,
uses FFmpeg's Flite filter, and plays through `paplay` on the current default
PulseAudio/PipeWire sink. Installations need `ffmpeg` with Flite support and
`paplay` available on `PATH`. The default voice is `slt`; `--voice` also accepts
`awb`, `kal`, `kal16`, and `rms`. Input is limited to 4096 bytes. Text and WAV
files are private temporary files deleted when playback finishes or fails. The
command does not send speech text to a network service or read replies aloud
automatically.

The same daemon publishes `~/.local/state/agent-bridge/token-feed.tsv` for the Orbital Drift sand table. Its first lines are a version marker, `sampled_at`, and cumulative `claude` and `codex` token totals. Session cursor rows follow so restarting the daemon does not count a session twice. Totals include input (including cached reads) and output tokens; cache components are not added again. The file is atomically replaced and readable only by this user. A sand table starting mid-session treats the first values as its baseline and adds grains only for later increases.

Check its state with `systemctl --user status agent-bridge-monitor.service` and recent errors with `journalctl --user -u agent-bridge-monitor.service -n 50`. To stop it for this session, run `systemctl --user stop agent-bridge-monitor.service`. To keep it off across boots, run `systemctl --user disable --now agent-bridge-monitor.service`; resume with `systemctl --user enable --now agent-bridge-monitor.service`. Boot startup also requires user lingering (`loginctl show-user "$USER" -p Linger`), which is already enabled on this workstation.

The snapshot combines Claude Code and Codex activity from the existing Digital Terrarium lifecycle hooks with token counters from their local session logs. It reports phase, safe activity detail, model, project path, process ID, and session input/output tokens when available. A session that has not written usage yet has `usage_known: false`. `watch` shows the change in input and output tokens between refreshes. `jobs` lists recent Codex jobs launched through this bridge, including exit status and completed-turn usage.

Every job has a `spawned_by` field. When known, it contains `agent_id` (matching `agents[].id`), `provider`, and the parent's `session_id` when available. It is `null` when the parent could not be identified, including older jobs. A bridge job also exposes its Codex `session_id` from the event stream; while that session is live, its `agents[]` entry carries the same `spawned_by` object. Viewers can join a job to its worker by `session_id` and the worker to its parent by `spawned_by.agent_id`.

Input tokens include cached reads and cache writes; those components are also shown separately. The counters are session totals, not a bill, and update when the clients write usage events. The snapshot reads usage fields only; it does not return prompts, responses, or tool output. The `version: 1` snapshot has provider-labeled agents and source status so a local-model/GPU sampler can be added later.

The activity hooks must be running to report an agent's current phase. If they are absent, `sources` indicates that limitation and bridge jobs remain visible. At-rest hook reports older than one hour are omitted, since a shared host process can outlive the session that emitted them. Reading `snapshot` does not require the Digital Terrarium app or HTTP service to be running.

The hook's `phase` and `detail` are raw reports. In particular, its “ready for direction” detail can mean either a completed turn or a watchdog fallback after activity events went quiet. Current hooks label these reports with `report_origin: event` or `report_origin: watchdog`. Consumers should use `activity_state`, `state_detail`, and `active_job_ids` for the job-aware view: `running_job`, `has_running_children`, `active`, `approval_required`, `halted_usage`, `error`, `ready`, or `unverified`. `ready` requires an explicit turn-completion or session-start event; watchdog reports and legacy reports without an origin remain `unverified`. The terminal `watch` command displays this state and keeps the raw hook phase in parentheses.

An explicit context, session budget, credit, usage, or token rate error produces a machine-readable `halt` object with `kind`, `scope`, `code`, `source`, and `detected_at`. The kinds are `context_limit`, `usage_limit`, and `rate_limit`; `scope` is `account`, `session`, `temporary`, or `unknown`. A generic usage-limit message has unknown scope until the coordinator verifies whether a fresh Codex session can proceed. Bridge jobs end as `halted` when their failed Codex turn or fatal stderr provides that evidence. A live Claude session can expose the same field after a matching API error. Token totals alone never trigger a halt: they are not a remaining allowance. The monitor does not inspect prompts or ordinary tool output to infer one.

Unacknowledged halted jobs remain in `snapshot` and appear in a prominent `watch` alert. The viewer is read-only. A coordinating agent decides whether to resume, take over under its project rules, or defer the task. Leave a deferred task unacknowledged so the alert stays visible; run `agent-bridge ack JOB_ID` after the task is completed or a replacement worker has taken ownership. The job's `halt` and `acknowledged_at` remain in `list` and `show` for history. A completed job is never marked halted solely because it used many tokens.
If a provider stops without an explicit limit error, the bridge cannot verify why; inspect that job's final result before accepting it.

## Schedules

A `schedule: v1` YAML file describes one fixed-time job. The ID uses lowercase kebab case; `command` is an argv list whose executable has an absolute path. `cwd` is absolute, `on_calendar` is a systemd `OnCalendar` expression, and `timeout` is a Go duration. Scripts run without a model call.

```yaml
schedule: v1
id: daily-check
kind: script
command:
  - /home/me/bin/daily-check
cwd: /home/me
on_calendar: daily
timeout: 5m
description: Daily check
```

Run `agent-bridge schedule add FILE`, then inspect `agent-bridge approvals` and `agent-bridge approve REQUEST_ID` or `deny REQUEST_ID`. Approval creates `agent-bridge-schedule-ID.service` and `.timer` under the systemd **user** unit directory, reloads the user manager, and enables the timer. `schedule remove ID` also requires approval; approval disables and removes the user units. `schedule list [--json]`, `schedule show ID`, and `schedule run ID` inspect or run approved entries. Each run creates a private job whose exit code and log paths are available through `agent-bridge list` and `show JOB_ID`. Registry files are private under `~/.local/state/agent-bridge/schedules/`. `kind: local-epm` is reserved and returns `local_epm_not_implemented` when added or run. Event triggers are not implemented.

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

### Need-to-know context

Use `--context PATH[,PATH...]` on `start` to pass only selected background files to the Codex job. The flag can be repeated. Paths are relative to `AGENT_BRIDGE_CONTEXT_DIR` (default `~/private/chatgpt-handoff/context`); a directory selects its immediate `*.md` files. For example:

```bash
agent-bridge start --cwd /path/to/worktree --prompt-file task.txt --context profile/about.md,project --context-work
```

The context root must contain `sharing-policy.yaml`. Its `never` list forbids matching files; `work_sensitive` matches require `--context-work`. An entry ending in `/` covers a directory's contents, while other entries match one exact path. The policy file itself is never shared. Missing policy, unsafe paths, and forbidden selections fail before a job is created.

Approved files are copied into the job's private `context/` directory, retaining paths relative to the context root. `show` reports those paths in `context_files`. The job prompt starts with a YAML `context` block listing the absolute paths of the copies and stating that they are historical background, not instructions. Without `--context`, `start` behaves as before.

`wait` blocks until every named job exits, then prints their final JSON records. It exits nonzero if any job failed, stopped, or was lost. Interrupting `wait` leaves the workers running. A coordinator should wait for all jobs it started, inspect their results and diffs, run the relevant checks, and only then finish its turn.

Claude Code can invoke these commands through its shell tool. A Codex session outside this supervisor will not appear in `agent-bridge list`, though `snapshot` can show its live activity when the Codex hook is present. This tool does not attach a spawned job to an existing Codex app conversation; it exposes status and usage to any local agent that calls it.

## Ask a model

`agent-bridge ask` sends one text prompt to Grok or a local Ollama model and prints the assistant's reply. It runs synchronously and records a job under the same jobs directory used by `list`, `show`, `wait`, and `snapshot`. It does not run tools. Grok is the default provider, so existing commands work unchanged.

### Grok

Get an API key from [console.x.ai](https://console.x.ai). Set `XAI_API_KEY`, or save the key in `~/.config/agent-bridge/xai-api-key` and restrict that file to your user:

```bash
install -d -m 700 ~/.config/agent-bridge
chmod 600 ~/.config/agent-bridge/xai-api-key
```

Check available models before choosing one. xAI trial or free credits are not guaranteed; check the current credit balance and terms at [console.x.ai](https://console.x.ai).

```bash
agent-bridge ask --list-models
printf 'Explain this error.' | agent-bridge ask --model MODEL_ID --name quick-question
agent-bridge ask --model MODEL_ID --system 'Be concise.' --prompt-file question.txt
AGENT_BRIDGE_GROK_MODEL=MODEL_ID agent-bridge ask --timeout 60s < question.txt
```

`--model` overrides `AGENT_BRIDGE_GROK_MODEL`. One of them is required for a prompt; there is no built-in model default. The default timeout is 120 seconds. Pass `--spawned-by AGENT_ID` to associate the job with a live parent agent when automatic detection cannot find it. For local testing, `AGENT_BRIDGE_XAI_BASE_URL` overrides the default `https://api.x.ai/v1` endpoint.

### Ollama

Start the local server with `ollama serve`, then list installed models or pull one with `ollama pull MODEL_ID`. Ollama needs no API key.

```bash
agent-bridge ask --provider ollama --list-models
printf 'Explain this error.' | agent-bridge ask --provider ollama --model MODEL_ID --name quick-question
agent-bridge ask --provider ollama --model MODEL_ID --system 'Be concise.' --prompt-file question.txt
AGENT_BRIDGE_OLLAMA_MODEL=MODEL_ID agent-bridge ask --provider ollama < question.txt
```

`--model` overrides `AGENT_BRIDGE_OLLAMA_MODEL`. One of them is required for a prompt; there is no built-in model default. `OLLAMA_HOST` accepts `host:port` or a full HTTP(S) URL and defaults to `http://127.0.0.1:11434`. The Ollama timeout is 600 seconds to allow models to load; `--timeout` overrides either provider's default. Ollama jobs keep the same private result file and metadata-only events, and support `--spawned-by` as Grok jobs do.

## Steinberger and repository activity

`snapshot`, `latest`, and `watch` report the Steinberger scan timer, watchdog
timer, last scan result, and age of the last successful scan. A scan older than
15 minutes or an inactive timer is shown as unhealthy. The daemon checks systemd
every 30 seconds; the viewer places an unhealthy alert above the agent list.

The viewer also reads repo-attention's private `activity-feed.json` after each
five-minute scan. It displays recent commits and exposes the feed, including
active repositories, in `repo_attention`. Missing or stale feeds are shown in
`sources`, independently from timer health. This avoids rereading repo-attention's
large line-hash state or running Git for every viewer refresh.

## GPU and local models

`snapshot` and the daemon's `latest` sample include NVIDIA GPU memory usage, total memory, utilization, compute-process VRAM, and loaded Ollama models when those sources are available. `watch` shows a VRAM section and highlights GPUs at 90% memory use. The `sources` entries named `nvidia-smi` and `ollama` explain unavailable data. GPU sampling requires `nvidia-smi`; Ollama sampling uses `OLLAMA_HOST` or `http://127.0.0.1:11434`. Neither service is required for the other snapshot data.

## Launcher L1

`agent-bridge start` accepts `--agent codex|claude` and `--role coder|epm`. The default agent is Codex. `--agent claude` defaults to role `epm` and profile `claude-epm`; Codex defaults to role `coder` and profile `codex-std`. Codex EPM and Claude coder are rejected. Jobs contain `agent`, `role`, and `depth`, including `list`, `show`, `snapshot`, and `watch`. Legacy jobs read as Codex/coder/depth 0.

`start --profile NAME` selects a profile from `packaging/profiles.yaml`, or the user override at `~/.config/agent-bridge/profiles.yaml` (`AGENT_BRIDGE_PROFILES` overrides the path). Jobs record `profile`, `model`, `effort`, `time_limit`, and `substitution`. The worker halts at its wall clock limit with `halt.code: time_limit`. A launch request may set `profile` as a ceiling, and `approve --profile NAME ID` can override it. The launcher keeps requests over its total, Claude, or Claude-deep cap queued in FIFO order. See [LAUNCHER-PROFILES.md](LAUNCHER-PROFILES.md).

Local Ollama completions appear as `local` agents with token usage. Ollama `/api/ps` models without an active local completion appear as passive `loaded` rows. The token feed includes a `local` total and per-session rows; older consumers can ignore these rows.

Queue a launch with `agent-bridge request --file launch.yaml`; poll `agent-bridge request-status ID`. The YAML schema and hard rules are in [LAUNCHER.md](LAUNCHER.md). The private spool defaults to `/tmp/agent-bridge-$UID/spool`, or `AGENT_BRIDGE_SPOOL_DIR`. Requests and results are user-owned 0600 files.

`agent-bridge launcher` polls the spool. `--once` processes one pass. It uses the Ollama model selected by `AGENT_BRIDGE_LAUNCHER_MODEL` (default `qwen3-coder:30b`). Failed or malformed model verdicts become escalations. `agent-bridge approvals` lists pending requests; `approve ID` or `deny ID` resolves them. The default timeout is 30 minutes. Decisions are logged to `~/.local/state/agent-bridge/launcher/decisions.jsonl`. The always-up unit is `packaging/agent-bridge-launcher.service`; configure Ollama and verify the worktree base paths before enabling it.

Claude jobs use `AGENT_BRIDGE_CLAUDE_BIN` (default `claude`) and write stream JSON events plus final text. Read-only jobs allow Read, Grep, and Glob. Write jobs allow Edit/Write and a limited build/test command list. L1 does not have the L2 tool approval socket.
