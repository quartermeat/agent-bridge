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
agent-bridge start --cwd DIR --name NAME --prompt-file task.yaml [--agent codex|claude] [--role coder|epm] [--profile NAME] [--write] [--spawned-by AGENT_ID] [--context PATH[,PATH...]] [--context-work]
agent-bridge ask [--provider grok|ollama] --model MODEL [--prompt-file F] [--name NAME]
agent-bridge ask --provider ollama --list-models
agent-bridge request --file launch.yaml | request-status REQUEST_ID
agent-bridge launcher [--once] | approvals | approve [--profile NAME] REQUEST_ID | deny REQUEST_ID
agent-bridge list | show JOB_ID | wait JOB_ID... | stop JOB_ID | ack JOB_ID
agent-bridge snapshot | latest | watch [--interval 2s] [--plain] [--once]
agent-bridge speak --text 'Agent bridge is ready.'
agent-bridge voice --agent claude [--access read|work] --source PULSE_SOURCE --wake-word 'hey agent' [--max-utterance 30s] [--follow-up 20s]
agent-bridge voice-log [--follow] [--lines 50] [--json]
```

- `start` launches a Codex or Claude job in a user systemd unit. The default is `--agent codex --role coder`; Codex cannot use `epm`. Read-only is the default. Claude read-only jobs allow Read, Grep, and Glob. Claude write jobs also allow Edit, Write, and the limited Go, Make, formatting, and Git build/review commands described in [the launcher design](docs/LAUNCHER.md). L1 has no per-tool approval prompt. Jobs expose `agent`, `role`, and `depth` in JSON and the live view.
- `request` queues a `launch: v1` YAML file in a private spool and prints its ID. `request-status` returns queued, pending, approved (with job ID), rejected, denied, or failed. The default spool is `/tmp/agent-bridge-$UID/spool`; override with `AGENT_BRIDGE_SPOOL_DIR`. The `launcher` user service applies hard rules, asks Ollama for a launch verdict, and creates the worktree before starting the job. `approvals` lists escalations; `approve` and `deny` resolve one. The default model is `qwen3-coder:30b` (`AGENT_BRIDGE_LAUNCHER_MODEL`). Install the optional unit from `packaging/agent-bridge-launcher.service` when ready.
- Named profiles select model, effort, and wall clock limit. `packaging/profiles.yaml` is the default; `~/.config/agent-bridge/profiles.yaml` overrides it (`AGENT_BRIDGE_PROFILES` for tests). See [job profiles](docs/LAUNCHER-PROFILES.md). The viewer includes local Ollama completions and loaded models.
- `--context` passes selected files from the context root to a `start` job under the sharing policy; repeat it or separate paths with commas. Work-sensitive files require `--context-work`.
- `ask` sends one prompt to Grok (xAI API key required) or a local Ollama model and prints the reply. It cannot run tools.
- `wait` blocks until jobs finish. Coordinators wait on every job they start.
- `ack` clears a halted job's alert after it has been resolved.
- `speak` reads `--text` or stdin, synthesizes stereo speech locally with FFmpeg's Flite filter, and plays it through the default PulseAudio/PipeWire sink with `paplay`. It sends no text to a remote service. Use `--voice awb|kal|kal16|rms|slt` to choose a voice (default `slt`). Flite output is quiet, so the render loudness-normalizes it (FFmpeg `loudnorm` to -14 LUFS), applies `--gain DB` (default `6`, range -20 to 20), and hard-limits peaks with `alimiter` before writing 48 kHz stereo; lower the gain if speech is too loud. Temporary text and audio files are private and removed after playback. This command speaks when invoked; it does not automatically read agent replies or monitor events.
- `voice` continuously listens to a selected PulseAudio/PipeWire source, waits for the wake phrase, transcribes an utterance locally with Voxtype, acknowledges it aloud, sends its text to an agent, and speaks the reply with `speak`. `--agent claude` (the default) runs one `claude -p --output-format json` per turn with the transcript on stdin; `--access` picks what it may do (see **Voice access and spoken approval** below). It resumes the same Claude Code session each turn and starts a fresh one if resume fails. Set `AGENT_BRIDGE_CLAUDE_BIN` to use another `claude` binary. `--agent codex` uses a persistent Codex app-server thread with a read-only sandbox that declines tool approvals. Either conversation is separate from the current desktop chat; the agent retains its text history and agent-bridge stores only the session or thread ID under `~/.local/state/agent-bridge/voice/` (`claude-session-id` or `thread-id`). With `--access read` (and always with Codex) the voice agent can converse and inspect accessible information, but cannot make changes or grant permissions from speech. A usage-limit failure is logged as `agent usage limit reached`; other failures use fixed labels and never include agent or server text. `--speech-gain DB` (default `6`, range -20 to 20) sets the `speak --gain` used for acknowledgements and replies. `--max-utterance DURATION` (default `30s`, range `2s` to `60s`) caps one captured utterance, including its 600 ms pre-roll (so a soft leading "hey" is not clipped); speech that reaches the cap is cut there, logged as `voice utterance hit length cap`, and still transcribed. `--follow-up DURATION` (default `20s`, max `120s`, `0` disables) opens a conversation window when a reply finishes playing: an utterance that begins inside it is sent without the wake phrase (which is still stripped if spoken), skips the "I'm listening." acknowledgement in favour of a soft 150 ms, 880 Hz tone, and reopens the window after its reply. The tone is synthesized once per process into a private `0600` temp WAV, played with `paplay`, and removed on exit; a playback failure is logged as `voice follow-up tone playback failed` and the turn continues. One-word follow-ups such as "yes" are accepted, but common Whisper noise transcripts (after lowercasing and stripping punctuation: "you", "thank you", "thanks for watching", "bye", "uh", "um", "hmm", "blank audio", or only bracketed text such as `[BLANK_AUDIO]` or `(music)`) are logged as `follow-up noise ignored` without closing the window; a failed turn closes it. Saying the wake phrase alone (or followed only by noise such as "um") plays "I'm listening.", logs `voice wake only; listening`, and opens the same window for the next utterance, which then gets the tone instead of a second acknowledgement; with `--follow-up 0` that bare-wake window lasts 10 s. Wake matching ignores case and punctuation ("Hey, agent."), and the phrase may come mid-utterance: the prompt is the text after its last occurrence ("Hello, hello, hey agent, are you listening?" sends "are you listening?"). The default phrase **hey agent** also accepts the Whisper variants "hi agent", "hey agents", and a bare leading "agent" (for a clipped "hey": "Agent, testing" matches, "the agent is" does not; "Agent." alone is a bare wake). The journal records only `wake_match` and `follow_up` booleans, never transcript text. Run `agent-bridge voice --file PATH.wav` for a one-turn check without opening a microphone.

Voice access and spoken approval:

- `--access read` keeps the read-only Claude run: only the Read, Grep and Glob tools, `--permission-mode dontAsk`, no MCP servers, and a 3-minute turn limit.
- `--access work` (the default for Claude; `--agent codex` is always read-only and rejects an explicit `--access work`) gives Claude its full tool set with `--permission-mode manual`. Read, Grep and Glob run without asking; every other call (Bash, Edit, Write, NotebookEdit, WebFetch, WebSearch, any MCP tool) goes to `--permission-prompt-tool mcp__voice_approval__approve`, served by the hidden `agent-bridge voice-approval-mcp` stdio MCP server (`--strict-mcp-config` loads only that server; Claude itself cannot call the tool). The server relays each request over the daemon's private socket `~/.local/state/agent-bridge/voice/approval.sock` (`0600`, peers must have the same uid). If the daemon or socket is unavailable the call is denied with `voice approval unavailable`.
- For each request the daemon writes the full detail (tool and command, file path or URL) to the transcript and speaks only a plain-language summary, never raw code: "Approval needed: DESCRIPTION. Say yes or no." For Bash, DESCRIPTION is the tool call's `description` field (punctuation and code characters stripped, at most 20 words); without one it is "run a shell command, PROGRAM" with only the first program's name (for example `agent-bridge`). Edit and Write say "edit NAME" / "write NAME" (file basename), WebFetch says "fetch HOST", and other tools say "use" plus the humanized tool name ("use todo write", "use do thing from srv"). Listening starts only after the prompt and the follow-up tone have finished playing, with a fresh microphone capture, so speech during playback is never taken as a reply. The whole normalized reply (case, punctuation and apostrophes ignored; a leading wake phrase allowed) must be one of the approve phrases **yes, yeah, yep, yes please, sure, ok, okay, approve, approved, confirm, confirmed, do it, go ahead, go for it, yes approve, yes do it** or deny phrases **no, nope, deny, denied, stop, cancel, don't, do not**. Anything else, including noise such as `[BLANK_AUDIO]`, silence, or a sentence that merely contains "yes", is unclear: the daemon says "Sorry, yes or no?" once, and a second unclear reply denies. Nothing heard within the 25 s window denies. It then says "Approved." or "Denied." Approvals are serialized, and the journal records only the tool name and the boolean result.
- Work turns may run up to 30 minutes; after 20 s the daemon says "Still working." at most once a minute (skipped while an approval is speaking or listening). The appended system prompt asks for one-to-three-sentence spoken replies and a description of each action before it is taken. The session runs in the service's working directory (your home).
- Risk: background audio (a video, a call, another speaker) could produce an approval phrase while a request is pending. Approvals require the whole reply to be one of the listed phrases (short words such as "yes" or "okay" are easier for background audio to produce), default to deny, and are visible in the transcript, but do not run `work` access where untrusted audio can reach the microphone. Claude Code user or project settings that allow tools (`permissions.allow`) would skip the spoken prompt for those tools; keep such rules out of `~/.claude/settings*.json` while the voice daemon runs.

Voice transcript:

- The daemon appends accepted prompts (after the wake phrase is removed), agent replies, approval requests and results, wake/listening and turn-start status lines, and fixed-label errors to `~/.local/state/agent-bridge/voice/transcript.jsonl` (directory `0700`, file `0600`), one JSON object per line: `{"ts","role":"user|agent|system","kind":"prompt|reply|ignored|approval_request|approval_heard|approval_verdict|approval_result|status|error","text"}`. Every utterance heard during an approval is recorded as `approval_heard` (role `user`; `(silence)` for an empty transcription) followed by its `approval_verdict` (`approve`, `deny`, `unclear`, or `deny (nothing heard in time)`). Utterances outside approvals that are dropped (no wake phrase, follow-up noise) are recorded as `ignored` (role `user`) so you can see what the microphone heard. At 5 MiB the file rotates to `transcript.jsonl.1` (one backup). The journal stays text-free.
- `agent-bridge voice-log` prints the last 50 entries (`--lines N`) as chat lines (`HH:MM you: …`, `HH:MM agent: …`, `HH:MM ⚠ approval: …`, `HH:MM you (approval): …`, and dropped speech as `HH:MM (ignored) you: …`), dropping terminal control characters; `--json` prints raw lines; `--follow` keeps tailing across rotation. Open a live window with `xfce4-terminal --title 'Voice transcript' -x agent-bridge voice-log --follow`.

The optional user daemon is `packaging/agent-bridge-voice.service`. Install it under `~/.config/systemd/user/`, then run `systemctl --user daemon-reload` and `systemctl --user enable --now agent-bridge-voice.service` to start listening and keep it available after login. Run `systemctl --user disable --now agent-bridge-voice.service` to stop listening and disable startup. Its packaged source is the Starship analog microphone and its wake phrase is **hey agent**; edit the unit's `ExecStart` for another source or phrase. Check `systemctl --user status agent-bridge-voice.service` and `journalctl --user -u agent-bridge-voice.service` when diagnosing it.

## Monitoring

The `agent-bridge-monitor` user service samples every two seconds to `~/.local/state/agent-bridge/latest.json`:

- agents and jobs, with `activity_state` and token usage;
- halts (`usage_limit`, `rate_limit`, `context_limit`) with scope;
- GPU VRAM, top VRAM processes, and loaded Ollama models.
- Steinberger timer health and recent commits from repo-attention's small feed.

`watch` opens a full-screen viewer in a terminal and restores the previous screen when it exits, leaving scrollback clean. Use `j`/`k` or arrow keys to scroll by line, `PgUp`/`PgDn` or space to move by page, `g`/`G` for top/bottom, `c` for a compact view of active agents and jobs, `f` to cycle all/active/jobs/local filters, and `q` or Ctrl-C to quit. The bottom bar shows sample origin, visible line range, filter, and key hints. `--interval DURATION` sets refresh time (minimum 500 ms; default 2 s). `--plain` appends frames without terminal controls; `--once` prints one plain frame and exits. Piped output automatically uses plain mode. The viewer turns red at 90% VRAM. The token feed is at `~/.local/state/agent-bridge/token-feed.tsv`.

## Build and check

```bash
gofmt -l . && go vet ./... && go test ./...
go build -o ~/.local/bin/agent-bridge .
systemctl --user restart agent-bridge-monitor.service
```

Every commit bumps `VERSION` and gets a matching annotated tag.
