---
name: pi-hardware-test
description: Use when a change needs verification on the real Raspberry Pi + phone + SIP provider (deploying a build, test calls, pairing, SIP registration, audio problems), or when the owner reports a problem from a real call.
---

# Testing on the Pi with the owner

The owner runs the hardware; you guide. Host `pi@rpi-pt`, binary `~/.local/bin/siptooth`, config `~/.siptooth/`, service `siptooth` (systemd user unit).

## Protocol

1. Give exactly **one** command per message, say in one line what to look for, and wait for the pasted output before the next step. Never send a list of commands.
2. Read-only inspection you may run yourself over ssh (`ssh -o BatchMode=yes pi@rpi-pt 'journalctl --user-unit siptooth --since -10min'`, `... '~/.local/bin/siptooth status'`, reading files under `~/.siptooth/` except `env`). Anything that changes the Pi (deploy, restart, config edits, pairing) is the owner's step unless they ask you to do it.
3. Never ask for or print the SIP password.

## Typical sequence after a code change

1. Owner, on the Mac: `make deploy` (builds linux/arm64, copies the binary, prints `siptooth version`).
2. Owner, on the Pi: `systemctl --user restart siptooth`, or `SIPTOOTH_LOG_LEVEL=debug siptooth run` in the foreground for a first look.
3. If config keys changed: give the full new `config.yaml` to paste (keep the owner's values), then `siptooth config check`.
4. `siptooth doctor` - everything ok or explained.
5. `siptooth sip test --tone` - SIP leg works without a phone call (registration, INVITE, tone audible at the far end).
6. Owner places a test call to the phone from a second phone.
7. You read `ssh -o BatchMode=yes pi@rpi-pt 'journalctl --user-unit siptooth --since -10min'` yourself and analyse the call.

## Reading a call

Lines carry `call_id`. A good call reads: `call ringing` -> SIP responses (180/183, 200) -> `answered` -> `sco up` -> ... -> one `call ended` summary line (duration, who hung up, RTP/SCO stats, reason).

- Ringing but no SIP responses: provider unreachable or registration lost; check `siptooth status` and `siptooth doctor` (STUN/TLS).
- SIP answered but no `sco up`: the phone did not bring up audio; look at HFP/SCO lines and the Bluetooth link.
- `sco up` but one-way or silent audio: check RTP stats in the `call ended` line (packets sent/received, latching, SRTP errors) and NAT mode.
- A hangup the owner did not expect: the `call ended` reason says whether SIP, the phone, or an SCO loss ended it.
