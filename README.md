# siptooth

A Raspberry Pi 5 acts as a Bluetooth hands-free unit for an Android phone. When the phone gets a call, siptooth dials a SIP target first and answers the cellular call only once the SIP side picks up, then bridges the call audio (CVSD 8 kHz over SCO) to G.711 RTP. One static Go binary, no sudo.

## Build

```sh
make check    # gofmt, vet, tests, linux/arm64 cross-build
make build    # ./siptooth for this machine
make dist     # linux arm64/amd64 release binaries
make deploy   # build for the Pi and copy it to pi@rpi-pt:~/.local/bin/siptooth
```

## Commands

| Command | Purpose |
| --- | --- |
| `siptooth run` | run the daemon in the foreground (systemd runs this) |
| `siptooth pair` | pair the phone interactively and record its address (daemon must be stopped) |
| `siptooth doctor [--fix] [--json]` | check the setup for the current user; `--fix` creates `~/.siptooth/`, a default config and the env file |
| `siptooth install [--dry-run]` | write and enable the systemd user service (no sudo) |
| `siptooth status [--json]` | daemon state via the control socket |
| `siptooth config check` | validate and print the effective configuration (password redacted) |
| `siptooth hooks test --event EVENT` | run the matching hooks with a sample payload |
| `siptooth version` | print the version |

All commands that read the configuration accept `--config PATH` (default `~/.siptooth/config.yaml`, or `$SIPTOOTH_CONFIG`).

## Docs

- Setup: `docs/setup.md`
- Operations and troubleshooting: `docs/operations.md`
- Hooks: `docs/hooks.md`
- Providers (ElevenLabs, Zadarma, generic PBX) and the M1 checklist: `docs/providers.md`
- Design: `docs/superpowers/specs/2026-10-14-siptooth-design.md`
- Plan: `docs/superpowers/plans/2026-10-14-siptooth.md`
