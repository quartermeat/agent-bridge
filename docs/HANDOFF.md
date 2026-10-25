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
