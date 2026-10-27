# Voice daemon handoff

Update 2026-10-25 (v0.18.0): the packaged service runs `--agent claude --access work`. Claude has its full tool set; Read, Grep and Glob run freely and every other tool call is relayed by the hidden `voice-approval-mcp` permission-prompt server to the daemon's private socket `~/.local/state/agent-bridge/voice/approval.sock`, which speaks a summary and approves only on the exact phrases approve, approved, yes approve or confirm (otherwise deny, 20 s window). Conversation text goes to the private `~/.local/state/agent-bridge/voice/transcript.jsonl`; watch it with `agent-bridge voice-log --follow`. Wake phrases now also match mid-utterance (text after the last occurrence; a bare "agent" stays leading-only). Real `claude -p` smoke runs confirmed deny, allow and no-socket paths; a live microphone approval still needs a user check. The older notes below describe the earlier read-only design.

As of 2026-10-25, `agent-bridge voice` uses the user's Starship analog microphone, local Voxtype transcription, a persistent Codex app-server thread, and local Flite speech. The packaged user service is enabled for the graphical session and listens for **hey agent**. It does not attach to the desktop conversation. The Codex thread runs read-only and declines permission grants; writing through voice requires a separate approval design.

The thread ID is private at `~/.local/state/agent-bridge/voice/thread-id`. Codex stores its own thread history. Temporary WAV files are private and removed. The journal records microphone RMS, trigger and processing stages, and classified errors, without transcripts. A recognized wake phrase plays a short acknowledgement before the Codex turn. Speech output is stereo for the front channels of the default 7.1 sink.

Verified: Go tests and vet pass, `systemd-analyze verify` passes, and a one-shot WAV test transcribed, resumed the persistent Codex thread, and played its reply. The Starship source is unmuted. A live mixer audit found its active capture mux on `Line` despite PipeWire advertising the rear-mic port. Switching `Input Source,0` to `Rear Mic` raised a five-second sample's RMS by 11.1 dB and peak by 17.3 dB. The service now selects that mux at startup. Its `--vad-threshold 0.03` is above observed background readings around 0.007–0.013 and below a clear 0.167 event. A live Codex thread produced a final answer after a trigger, but spoken playback was interrupted during debugging. The new acknowledgement and stereo output still need a user hearing check.

Commands:

```bash
systemctl --user status agent-bridge-voice.service
journalctl --user -u agent-bridge-voice.service -f
systemctl --user disable --now agent-bridge-voice.service
systemctl --user enable --now agent-bridge-voice.service
```

If the source is not capturing speech, inspect `pactl list sources` and set `--source` in the installed user unit, then `systemctl --user daemon-reload` and restart the service. The selected Pulse source is `alsa_input.pci-0000_28_00.4.analog-stereo`, as confirmed by the user. The Brio source produced near-zero levels in the earlier test.

Troubleshooting 2026-10-25: silent replies with "speech playback complete" in the journal meant PipeWire stream-restore had saved `paplay` at 0%; set to 100% while a phrase plays (`pactl set-sink-input-volume <id> 100%`). Whisper "Thanks for watching!" transcripts meant clipping: Starship capture was 100% (+30 dB) plus +20 dB mic boost; now 61% (+12 dB), ambient RMS ~0.002.

Launcher 2026-10-26 (v0.19.0): spool launcher, Claude jobs, roles, profiles and local agents landed (design: docs/LAUNCHER.md, docs/LAUNCHER-PROFILES.md). Verified live: a claude-quick job, and a coordinator→EPM request that the local qwen3-coder dispatcher approved, which created a worktree and ran a Claude EPM job. Not yet done: installing the new binary and enabling `packaging/agent-bridge-launcher.service`; L2 tool-approval socket; local token grains in token-sand; watch scrollback fix (alt screen + scrolling).

2026-10-26 late (Claude EPM, Codex at its 5-hour limit): v0.21.0 Claude coder fallback (`--fallback-for` a usage-halted Codex coder; rule in ~/AGENTS.md), v0.22.0 local-model queue (LOCAL-FEEDBACK milestone A), v0.23.0 proactive backpressure (Codex `provider_limits` in snapshot/watch; coder launches deferred at ≥90% until reset). Verified: full suite twice, -race on the queue, live `provider_limits` 98% → resets 00:46 EDT. Claude coders need single-command Bash (AB-016). Still open: Codex worktrees epm-default, epm-docs, claude-default-code, codex-only, routing (left for Codex to prune); docs/QUEUE-2026-10-27.md items 2–3 (GUP paused, ChatGPT theme needs approval).

## v0.24.0 (2026-10-27, Claude EPM; Codex coders m1c, m1c-baseline, watchfb)
- M1c/G3: Claude EPM write jobs get `Edit(<doc pattern>)` permissions only, `--restricted`, and no Bash. `owns` must be document paths. After the job, a git audit (with a pre-job baseline) marks any non-document change as `violation`.
- AB-016: the bridge prepends Tool rules to Claude coder prompts.
- Watch shows `fallback for <id> (<name>)`, a `↺` marker, and stale provider limits.
- The repo has no Makefile; the gate is `gofmt -l . && go vet ./... && go test ./...`.
- Next: docs/AUTOMATION.md (scripts/timers replacing manual rituals; Q1 auto-prune and Q2 hooks approved).
