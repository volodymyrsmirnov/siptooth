# siptooth

A Linux machine with Bluetooth (developed and tested on a Raspberry Pi 5) acts as a Bluetooth hands-free unit for an Android phone. When the phone gets a call, siptooth dials a SIP target first and answers the cellular call only once the SIP side picks up, then bridges the call audio (CVSD 8 kHz over SCO) to G.711 RTP. Optionally it also forwards incoming text messages (SMS) to hooks over Bluetooth MAP (`sms.enabled`). One static Go binary, no sudo.

## Hardware

siptooth is not tied to the Raspberry Pi: it uses no Pi hardware (no GPIO, no Pi libraries), only the Linux Bluetooth stack. `make dist` builds `linux-arm64` and `linux-amd64` binaries, so a mini PC or an old laptop works too. macOS and Windows are not supported (everything goes through BlueZ).

What the machine needs:

- **Linux with BlueZ on the system D-Bus** (tested with BlueZ 5.82), and nothing else handling hands-free calls: PipeWire, PulseAudio or oFono must not register the HFP role, or they compete with siptooth for the phone. `siptooth doctor` checks for competing handlers.
- **A Bluetooth adapter that carries call audio over HCI.** Some built-in controllers (laptops especially) route call audio over a separate PCM wire straight to a sound chip, where siptooth cannot read it; calls then connect but are silent. USB dongles usually use HCI.
- **CVSD support** (narrowband call audio; every hands-free-capable controller has it).
- **Bluetooth socket access without root**: the user in the `bluetooth` group and BlueZ's D-Bus policy. `siptooth doctor` checks this.

Pi-specific parts, harmless elsewhere:

- **Broadcom SCO routing check.** On adapters that report Broadcom as manufacturer, siptooth verifies (and fixes) that call audio is routed over HCI with a vendor command. Other manufacturers skip it; the Pi 5's onboard adapter reports Cypress (305) and skips it too.
- **Hourly adapter power cycle** (`bluetooth.power_cycle_minutes`, default 60). It works around a Pi 5 controller that, after hours connected, carried only noise on calls. Set it to 0 on an adapter that does not have that problem.
- **Adapter `hci0`** is only the config default (`bluetooth.adapter`).

SMS forwarding is the most portable part: it uses plain L2CAP/RFCOMM connections, with no audio and no vendor commands. It only needs BlueZ's own `obexd` not to hold the MAP notification channel (`siptooth doctor` warns `sms_obexd`).

On new hardware, run `siptooth doctor` and one test call before relying on it.

## Build

```sh
make check    # gofmt, vet, tests, linux/arm64 cross-build
make build    # ./siptooth for this machine
make dist     # linux arm64/amd64 release binaries
make deploy   # build linux/arm64 and copy it to pi@rpi-pt:~/.local/bin/siptooth (PI_HOST=... to override)
```

## Commands

| Command | Purpose |
| --- | --- |
| `siptooth run` | run the daemon in the foreground (systemd runs this) |
| `siptooth pair` | pair the phone interactively and record its address (daemon must be stopped) |
| `siptooth doctor [--fix] [--json]` | check the setup for the current user; `--fix` creates `~/.siptooth/`, a default config and the env file |
| `siptooth install [--dry-run]` | write and enable the systemd user service (no sudo) |
| `siptooth status [--json]` | daemon state via the control socket (including SMS forwarding) |
| `siptooth config check` | validate and print the effective configuration (password redacted) |
| `siptooth sip test [--caller N] [--seconds 20] [--tone] [--wait-remote-hangup]` | place a real SIP call to the target without Bluetooth and report responses, codec, NAT, packet loss, gaps and how the call ended (exit 0 only if answered with audio received) |
| `siptooth hooks test --event EVENT` | run the matching hooks with a sample payload |
| `siptooth version` | print the version |

All commands that read the configuration accept `--config PATH` (default `~/.siptooth/config.yaml`, or `$SIPTOOTH_CONFIG`).

## Docs

- Setup: `docs/setup.md`
- Operations and troubleshooting: `docs/operations.md`
- Hooks: `docs/hooks.md`
- Providers (ElevenLabs, Zadarma, generic PBX) and the M1 checklist: `docs/providers.md`
- Design: `docs/superpowers/specs/2026-10-14-siptooth-design.md`
- Plans (historical): `docs/superpowers/plans/2026-10-14-siptooth.md`, `docs/superpowers/plans/2026-10-14-siptooth-sms.md`

## License

MIT, see `LICENSE`.
