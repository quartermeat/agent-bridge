# Voice daemon handoff

As of 2026-10-25, `agent-bridge voice` uses the user's Starship analog microphone, local Voxtype transcription, a persistent Codex app-server thread, and local Flite speech. The packaged user service is enabled for the graphical session and listens for **hey agent**. It does not attach to the desktop conversation. The Codex thread runs read-only and declines permission grants; writing through voice requires a separate approval design.

The thread ID is private at `~/.local/state/agent-bridge/voice/thread-id`. Codex stores its own thread history. Temporary WAV files are private and removed. The journal records microphone RMS/trigger values and classified errors, without transcripts.

Verified: Go tests and vet pass, `systemd-analyze verify` passes, and a one-shot WAV test transcribed, resumed the persistent Codex thread, and played its reply. The Starship source is unmuted and a three-second FFmpeg sample registered audio (overall RMS about -23 dB). Ambient RMS around 0.030–0.046 repeatedly triggered the original 0.018 threshold, so the packaged service sets `--vad-threshold 0.08`. Live wake phrase detection still needs confirmation; compare journal readings with speech levels before further tuning.

Commands:

```bash
systemctl --user status agent-bridge-voice.service
journalctl --user -u agent-bridge-voice.service -f
systemctl --user disable --now agent-bridge-voice.service
systemctl --user enable --now agent-bridge-voice.service
```

If the source is not capturing speech, inspect `pactl list sources` and set `--source` in the installed user unit, then `systemctl --user daemon-reload` and restart the service. The selected Pulse source is `alsa_input.pci-0000_28_00.4.analog-stereo`, as confirmed by the user. The Brio source produced near-zero levels in the earlier test.
