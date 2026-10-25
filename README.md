# agent-bridge

One local interface for coordinating AI agents: Codex jobs, Grok and Ollama prompts, live Claude/Codex activity, token usage, and GPU VRAM. Full details: [docs/REFERENCE.md](docs/REFERENCE.md).

## Communication rule

Every message sent through agent-bridge is a YAML document of **50 words or fewer**, with no prose around it. Anything longer goes in a file, linked by `detail:`. The specs live in `~/AGENTS.md` ("Response length"); change them only there, and bump the version key.

Task message (coordinator → worker, the job's prompt):

```yaml
task: v1
goal: one sentence
owns: [path, ...]
avoid: [path, ...]
checks: [command, ...]
detail: /absolute/path/to/task.md
```

Report message (worker → coordinator, its final message):

```yaml
report: v2
status: done | partial | blocked | failed
summary: one sentence
checks: pass | fail | partial | none
detail: /absolute/path/to/report.md
```

The coordinator rejects messages that do not parse or exceed 50 words.

## Commands

```bash
agent-bridge start --cwd DIR --name NAME --prompt-file task.yaml [--write] [--spawned-by AGENT_ID] [--context PATH[,PATH...]] [--context-work]
agent-bridge ask [--provider grok|ollama] --model MODEL [--prompt-file F] [--name NAME]
agent-bridge ask --provider ollama --list-models
agent-bridge list | show JOB_ID | wait JOB_ID... | stop JOB_ID | ack JOB_ID
agent-bridge snapshot | latest | watch
agent-bridge speak --text 'Agent bridge is ready.'
agent-bridge voice --source PULSE_SOURCE --wake-word 'hey agent'
```

- `start` launches a Codex job in a user systemd unit. Read-only by default; `--write` allows edits in that worktree only.
- `--context` passes selected files from the context root to a `start` job under the sharing policy; repeat it or separate paths with commas. Work-sensitive files require `--context-work`.
- `ask` sends one prompt to Grok (xAI API key required) or a local Ollama model and prints the reply. It cannot run tools.
- `wait` blocks until jobs finish. Coordinators wait on every job they start.
- `ack` clears a halted job's alert after it has been resolved.
- `speak` reads `--text` or stdin, synthesizes stereo speech locally with FFmpeg's Flite filter, and plays it through the default PulseAudio/PipeWire sink with `paplay`. It sends no text to a remote service. Use `--voice awb|kal|kal16|rms|slt` to choose a voice (default `slt`). Temporary text and audio files are private and removed after playback. This command speaks when invoked; it does not automatically read agent replies or monitor events.
- `voice` continuously listens to a selected PulseAudio/PipeWire source, waits for the wake phrase, transcribes an utterance locally with Voxtype, acknowledges it aloud, sends its text to a persistent Codex app-server thread, and speaks the reply with `speak`. The thread is separate from the current desktop chat. Codex retains that thread's text history; agent-bridge stores only its thread ID under `~/.local/state/agent-bridge/voice/`. The voice agent currently uses a read-only sandbox and declines tool approvals. It can converse and inspect accessible information, but cannot make changes or grant permissions from speech. Run `agent-bridge voice --file PATH.wav` for a one-turn check without opening a microphone.

The optional user daemon is `packaging/agent-bridge-voice.service`. Install it under `~/.config/systemd/user/`, then run `systemctl --user daemon-reload` and `systemctl --user enable --now agent-bridge-voice.service` to start listening and keep it available after login. Run `systemctl --user disable --now agent-bridge-voice.service` to stop listening and disable startup. Its packaged source is the Starship analog microphone and its wake phrase is **hey agent**; edit the unit's `ExecStart` for another source or phrase. Check `systemctl --user status agent-bridge-voice.service` and `journalctl --user -u agent-bridge-voice.service` when diagnosing it.

## Monitoring

The `agent-bridge-monitor` user service samples every two seconds to `~/.local/state/agent-bridge/latest.json`:

- agents and jobs, with `activity_state` and token usage;
- halts (`usage_limit`, `rate_limit`, `context_limit`) with scope;
- GPU VRAM, top VRAM processes, and loaded Ollama models.
- Steinberger timer health and recent commits from repo-attention's small feed.

`watch` shows the same view live and turns red at 90% VRAM. The token feed is at `~/.local/state/agent-bridge/token-feed.tsv`.

## Build and check

```bash
gofmt -l . && go vet ./... && go test ./...
go build -o ~/.local/bin/agent-bridge .
systemctl --user restart agent-bridge-monitor.service
```

Every commit bumps `VERSION` and gets a matching annotated tag.
