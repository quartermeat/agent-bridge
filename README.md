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
agent-bridge voice --agent claude --source PULSE_SOURCE --wake-word 'hey agent' [--max-utterance 30s] [--follow-up 20s]
```

- `start` launches a Codex job in a user systemd unit. Read-only by default; `--write` allows edits in that worktree only.
- `--context` passes selected files from the context root to a `start` job under the sharing policy; repeat it or separate paths with commas. Work-sensitive files require `--context-work`.
- `ask` sends one prompt to Grok (xAI API key required) or a local Ollama model and prints the reply. It cannot run tools.
- `wait` blocks until jobs finish. Coordinators wait on every job they start.
- `ack` clears a halted job's alert after it has been resolved.
- `speak` reads `--text` or stdin, synthesizes stereo speech locally with FFmpeg's Flite filter, and plays it through the default PulseAudio/PipeWire sink with `paplay`. It sends no text to a remote service. Use `--voice awb|kal|kal16|rms|slt` to choose a voice (default `slt`). Flite output is quiet, so the render loudness-normalizes it (FFmpeg `loudnorm` to -14 LUFS), applies `--gain DB` (default `6`, range -20 to 20), and hard-limits peaks with `alimiter` before writing 48 kHz stereo; lower the gain if speech is too loud. Temporary text and audio files are private and removed after playback. This command speaks when invoked; it does not automatically read agent replies or monitor events.
- `voice` continuously listens to a selected PulseAudio/PipeWire source, waits for the wake phrase, transcribes an utterance locally with Voxtype, acknowledges it aloud, sends its text to an agent, and speaks the reply with `speak`. `--agent claude` (the default) runs one `claude -p --output-format json` per turn with the transcript on stdin, only the Read, Grep and Glob tools, `--permission-mode dontAsk`, and no MCP servers; it resumes the same Claude Code session each turn and starts a fresh one if resume fails. Set `AGENT_BRIDGE_CLAUDE_BIN` to use another `claude` binary. `--agent codex` uses a persistent Codex app-server thread with a read-only sandbox that declines tool approvals. Either conversation is separate from the current desktop chat; the agent retains its text history and agent-bridge stores only the session or thread ID under `~/.local/state/agent-bridge/voice/` (`claude-session-id` or `thread-id`). The voice agent can converse and inspect accessible information, but cannot make changes or grant permissions from speech. A usage-limit failure is logged as `agent usage limit reached`; other failures use fixed labels and never include agent or server text. `--speech-gain DB` (default `6`, range -20 to 20) sets the `speak --gain` used for acknowledgements and replies. `--max-utterance DURATION` (default `30s`, range `2s` to `60s`) caps one captured utterance, including its 600 ms pre-roll (so a soft leading "hey" is not clipped); speech that reaches the cap is cut there, logged as `voice utterance hit length cap`, and still transcribed. `--follow-up DURATION` (default `20s`, max `120s`, `0` disables) opens a conversation window when a reply finishes playing: an utterance that begins inside it is sent without the wake phrase (which is still stripped if spoken), skips the "I'm listening." acknowledgement in favour of a soft 150 ms, 880 Hz tone, and reopens the window after its reply. The tone is synthesized once per process into a private `0600` temp WAV, played with `paplay`, and removed on exit; a playback failure is logged as `voice follow-up tone playback failed` and the turn continues. One-word follow-ups such as "yes" are accepted, but common Whisper noise transcripts (after lowercasing and stripping punctuation: "you", "thank you", "thanks for watching", "bye", "uh", "um", "hmm", "blank audio", or only bracketed text such as `[BLANK_AUDIO]` or `(music)`) are logged as `follow-up noise ignored` without closing the window; a failed turn closes it. Saying the wake phrase alone (or followed only by noise such as "um") plays "I'm listening.", logs `voice wake only; listening`, and opens the same window for the next utterance, which then gets the tone instead of a second acknowledgement; with `--follow-up 0` that bare-wake window lasts 10 s. Wake matching ignores case and punctuation ("Hey, agent."), and the default phrase **hey agent** also accepts the Whisper variants "hi agent", "hey agents", and a bare leading "agent" (for a clipped "hey": "Agent, testing" matches, "the agent is" does not; "Agent." alone is a bare wake). The journal records only `wake_match` and `follow_up` booleans, never transcript text. Run `agent-bridge voice --file PATH.wav` for a one-turn check without opening a microphone.

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
