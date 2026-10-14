# Siptooth

Raspberry Pi 5 Bluetooth hands-free (HFP HF) unit that forwards an Android phone's incoming calls to SIP. When the phone rings, siptooth dials the SIP target; only when the SIP side answers does it answer the cellular call (AT+ATA) and bridge CVSD 8 kHz SCO audio to G.711 RTP. One static Go binary, run as the normal user `pi`, no sudo.

## Sources of truth

- **Design spec (binding):** `docs/superpowers/specs/2026-10-14-siptooth-design.md`. Section numbers are referenced from package docs ("spec 5.3"). Behaviour or config changes update the spec in the same commit series, with a dated note explaining what changed and why.
- **Implementation plan (historical):** `docs/superpowers/plans/2026-10-14-siptooth.md`. Do not update it.
- **User docs:** `docs/setup.md`, `docs/operations.md`, `docs/hooks.md`, `docs/providers.md`. Keep them in sync with the CLI and config.
- **Predecessor:** `~/Projects/Personal/allochka` (read-only; the hardware packages were copied from it).

## Commands

```sh
make check          # gofmt check, go vet, go test, linux/arm64 cross-build: must pass before every commit
make race           # go test -race
make build          # ./siptooth for the host
make dist           # release binaries for linux arm64/amd64 + SHA256SUMS
make deploy         # build linux/arm64 and copy to pi@rpi-pt:~/.local/bin/siptooth (PI_HOST=... to override)
go test ./internal/bridge/ -run TestName -v
GOOS=linux GOARCH=arm64 go vet ./...   # vet the linux-only files from macOS
```

Development happens on macOS; Linux-only code (`*_linux.go`) is compiled by `make check` but its tests only run on Linux. To run them locally: `docker run --rm --init -v "$PWD":/src -w /src golang:1.24 go test ./...`.

## Layout

| Path | What |
|---|---|
| `cmd/siptooth` | `main`, delegates to `internal/cli` |
| `internal/cli` | subcommands (`cli.go` dispatcher with `register(command{...})`; `run`, `pair`, `install`, `status`, `config`, `hooks`, `doctor`, `sip test`, `version`) |
| `internal/buildinfo` | version injected by ldflags |
| `internal/host` | systemd **user** unit + linger (unit runs `siptooth run`) |
| `internal/hfp` | HFP HF AT-command state machine over RFCOMM |
| `internal/sco` | SCO SEQPACKET socket, Broadcom routing VSC |
| `internal/bluez` | BlueZ over system D-Bus: adapter, Profile1 (HFP), Agent1 pairing |
| `internal/g711` | mu-law, A-law, `Codec` type |
| `internal/placeholder` | template expansion with component-aware escaping |
| `internal/config` | config, validation, env file, embedded default `config.yaml` |
| `internal/stun` | STUN Binding request encode/decode, query on a PacketConn |
| `internal/nat` | local address probe, mode selection, STUN keepalive |
| `internal/dnsv4` | IPv4-only DNS resolver (never sends AAAA upstream; installed as `net.DefaultResolver` in `main`) |
| `internal/sipaddr` | next-hop rule shared by the daemon and doctor |
| `internal/sdp` | offer/answer/re-offer, SDES crypto, redaction |
| `internal/echo` | echo gate for the uplink (copied from allochka `audio.EchoGate`), echo delay estimate |
| `internal/rtp` | RTP session (socket, latching, send), SRTP, playout, stats |
| `internal/sip` | sipgo wrapper: UA, dial, call, server, register, contact, keepalive |
| `internal/hooks` | owner shell hooks on lifecycle events |
| `internal/bridge` | call coordinator (single goroutine state machine, injected clock), media pump, status |
| `internal/control` | HTTP over the Unix socket (`GET /status`), client |
| `internal/daemon` | composition root for `siptooth run` |
| `internal/doctor` | installation checks/fixes, plus SIP/STUN/TLS checks |
| `config/hooks/` | example hook scripts referenced by docs |

## Runtime layout (on the Pi)

`~/.siptooth/`: `config.yaml`, `env` (0600, `SIPTOOTH_SIP_PASSWORD=`), `siptooth.sock`. Service: `~/.config/systemd/user/siptooth.service` (written by `siptooth install`), logs `journalctl --user -u siptooth -f`, restart `systemctl --user restart siptooth`.

## Hard rules

- Module `github.com/volodymyrsmirnov/siptooth`; Go 1.24 (`go 1.24.0` in go.mod), `CGO_ENABLED=0` everywhere.
- Direct dependencies only: `godbus/dbus/v5`, `golang.org/x/sys`, `gopkg.in/yaml.v3`, `emiago/sipgo v1.6.0`, `pion/rtp v1.10.5`, `pion/srtp/v3 v3.1.3`. No others; testify is NOT allowed (standard `testing` only). No new modules without the owner's approval.
- Linux-only code behind `//go:build linux` with `_other.go` stubs; `go test ./...` must pass on macOS; `GOOS=linux GOARCH=arm64 go build ./...` must pass.
- TDD: failing test first, then code. Tests never touch real Bluetooth, real D-Bus, systemctl, the network beyond loopback, or files outside `t.TempDir()`.
- Never require or run sudo. Doctor may *print* a sudo command for the owner; it never runs one.
- Never log passwords, `Authorization` headers or SRTP keys (`inline:<redacted>`).
- `log/slog`; call-scoped log lines carry `call_id`.
- Copied files keep allochka's behaviour and comments, with names changed (`allochka` -> `siptooth`, `/com/allochka/` -> `/com/siptooth/`, alias `siptooth`, profile name `Siptooth`) and references to removed packages deleted.
- Commits: conventional style (`feat(sdp): ...`, `fix(bridge): ...`, `docs: ...`, `chore: ...`), small and logical, each passing `make check`, ending with the session attribution line when one is provided. Commit on `main`, never push, never disable signing.

## Domain knowledge (hard-won, from hardware testing)

- Phone: Motorola on Android; adapter: Pi 5 onboard (manufacturer 305 / Cypress, btbcm driver). Only CVSD (narrowband, 8 kHz) is negotiated (`AT+BRSF` without codec negotiation); wideband mSBC sounded clearer in an experiment but is not implemented yet.
- SCO: MTU 64, the controller delivers 60-byte packets; pacing is read-driven (write one frame per frame read). In-band ringing leaves a ~222-frame SCO backlog that is drained before the greeting. Broadcom SCO routing defaults to HCI already; the routing check is skipped for manufacturer 305.
- The controller can get stuck after hours connected: SCO comes up normally (eSCO, CVSD) but carries constant noise both ways (caller channel hiss at ~-17 dBFS from the first packet, the caller hears nothing). An adapter power cycle clears it; the daemon power-cycles every `bluetooth.power_cycle_minutes` (60) while idle.
- SIP: sipgo v1.6.0 `WaitAnswer` does not ACK/BYE a 2xx that crosses CANCEL; `internal/sip` owns that (spec 5.3).
- Debugging a call: `journalctl --user -u siptooth` lines with the `call_id`; one `call ended` summary line per call.

## Working with the owner

- The owner tests on real hardware. When hardware steps are needed, give **one command at a time**, wait for the output, then give the next. No long command lists. See the `pi-hardware-test` skill.
- For non-trivial features: short design in chat, owner says go, then implement (subagents for larger items; briefs and reports go in `.superpowers/sdd/`, which is git-ignored).
- Never paste or ask for the SIP password in chat.
