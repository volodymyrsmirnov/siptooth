---
name: new-feature
description: Use when adding or changing Siptooth behaviour, config keys, SIP or media handling, call logic or CLI commands - the checklist for touching every place a change must reach.
---

# Changing Siptooth

Design first: a few sentences in chat (what, files, tests), owner says go. Then TDD and small conventional commits, each passing `make check`.

## Checklist by kind of change

**New or changed config key**
- `internal/config/config.go` (struct + defaults), `validate.go` (enum/range + error text), tests in `internal/config`.
- Embedded default `config.yaml` with a short trailing comment; the defaults test keeps the embedded config valid.
- Spec section 9 (dated note) and `docs/operations.md` if it is a tuning knob.
- Removed keys: old config files must still load; `doctor` warns that the key is unused.

**SIP behaviour** (`internal/sip`)
- Tests against a loopback UAS (no network beyond loopback); cover CANCEL crossing a 2xx and forked 2xx.
- Never log passwords or `Authorization` headers.
- Spec section 5.

**Media** (`internal/rtp`, `internal/sdp`, `internal/g711`)
- Test with synthetic packets; SRTP keys never logged (`inline:<redacted>`).
- Spec section 6.

**Call behaviour** (`internal/bridge`)
- Single coordinator goroutine; everything arrives as a message; timers via the injected clock (fake clock in tests). No blocking calls on that goroutine.
- Assert the log/status timeline the owner sees; call-scoped lines carry `call_id`.
- Spec section 4.

**CLI**
- `internal/cli/*_cmd.go`, registered via `register(command{...})`; exit codes from `cli.go` (`ExitOK`, `ExitError`, `ExitUsage`); README/docs command lists.

## Done means

`make check` and `make race` pass, docs/spec updated, commits on `main` (never pushed), and a one-command-at-a-time hardware test plan for the owner (skill `pi-hardware-test`) when the change affects calls, audio or Bluetooth.
