# Voice daemon handoff

As of 2026-10-25, `agent-bridge voice` uses the Brio microphone, local Voxtype transcription, a persistent Codex app-server thread, and local Flite speech. The packaged user service is enabled for the graphical session and listens for **hey agent**. It does not attach to the desktop conversation. The Codex thread runs read-only and declines permission grants; writing through voice requires a separate approval design.

The thread ID is private at `~/.local/state/agent-bridge/voice/thread-id`. Codex stores its own thread history. Temporary WAV files are private and removed. The journal records microphone RMS/trigger values and classified errors, without transcripts.

Verified: Go tests and vet pass, `systemd-analyze verify` passes, and a one-shot WAV test transcribed, resumed the persistent Codex thread, and played its reply. The user service was enabled and observed active with FFmpeg attached to the Brio source. Live human speech detection has not been verified. Initial idle Brio RMS rounded to `0.000`; compare with live voice levels before changing `--vad-threshold` (default `0.018`).

Commands:

```bash
systemctl --user status agent-bridge-voice.service
journalctl --user -u agent-bridge-voice.service -f
systemctl --user disable --now agent-bridge-voice.service
systemctl --user enable --now agent-bridge-voice.service
```

If the source is not capturing speech, inspect `pactl list sources` and set `--source` in the installed user unit, then `systemctl --user daemon-reload` and restart the service. The currently selected Pulse source is `alsa_input.usb-046d_Brio_1080p_Webcam_2504CG900U08-02.mono-fallback`; the analog default source was muted during setup. Do not unmute another microphone without the user's choice.
