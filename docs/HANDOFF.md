# Voice daemon handoff

As of 2026-10-25, `agent-bridge voice` uses the user's Starship analog microphone, local Voxtype transcription, a persistent Codex app-server thread, and local Flite speech. The packaged user service is enabled for the graphical session and listens for **hey agent**. It does not attach to the desktop conversation. The Codex thread runs read-only and declines permission grants; writing through voice requires a separate approval design.

The thread ID is private at `~/.local/state/agent-bridge/voice/thread-id`. Codex stores its own thread history. Temporary WAV files are private and removed. The journal records microphone RMS/trigger values and classified errors, without transcripts.

Verified: Go tests and vet pass, `systemd-analyze verify` passes, and a one-shot WAV test transcribed, resumed the persistent Codex thread, and played its reply. The Starship source is unmuted. A live mixer audit found its active capture mux on `Line` despite PipeWire advertising the rear-mic port. Switching `Input Source,0` to `Rear Mic` raised a five-second sample's RMS by 11.1 dB and peak by 17.3 dB. The service now selects that mux at startup. Its `--vad-threshold 0.03` is above observed background readings around 0.007–0.013 and below a clear 0.167 event. Live spoken wake phrase recognition still needs confirmation.

Commands:

```bash
systemctl --user status agent-bridge-voice.service
journalctl --user -u agent-bridge-voice.service -f
systemctl --user disable --now agent-bridge-voice.service
systemctl --user enable --now agent-bridge-voice.service
```

If the source is not capturing speech, inspect `pactl list sources` and set `--source` in the installed user unit, then `systemctl --user daemon-reload` and restart the service. The selected Pulse source is `alsa_input.pci-0000_28_00.4.analog-stereo`, as confirmed by the user. The Brio source produced near-zero levels in the earlier test.
