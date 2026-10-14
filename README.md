# siptooth

A Raspberry Pi 5 acts as a Bluetooth hands-free unit for an Android phone. When the phone gets a call, siptooth dials a SIP target first and answers the cellular call only once the SIP side picks up, then bridges the call audio (CVSD 8 kHz over SCO) to G.711 RTP. One static Go binary, no sudo.

## Build

```sh
make check    # gofmt, vet, tests, linux/arm64 cross-build
make build    # ./siptooth for this machine
make dist     # linux arm64/amd64 release binaries
make deploy   # build for the Pi and copy it to pi@rpi-pt:~/.local/bin/siptooth
```

## Docs

- Design: `docs/superpowers/specs/2026-10-14-siptooth-design.md`
- Plan: `docs/superpowers/plans/2026-10-14-siptooth.md`
- Setup and operations guides (`docs/setup.md`, `docs/operations.md`) arrive with the CLI.
