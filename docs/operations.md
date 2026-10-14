# Operations

## Logs

siptooth logs with `log/slog` text lines to stderr; under systemd they go to the journal:

```sh
journalctl --user -u siptooth -f
journalctl --user -u siptooth --since "1 hour ago" | grep 'call ended'
```

The level comes from `SIPTOOTH_LOG_LEVEL` (`debug`, `info` default, `warn`, `error`). `debug` adds each SIP message's start line and SDP bodies, with `a=crypto` keys and `Authorization` / `Proxy-Authorization` values redacted. Every call-scoped line carries `call_id`. Passwords and SRTP keys never appear in logs or hook payloads.

### The per-call summary line

Exactly one line per call at `info`:

```
msg="call ended" call_id=20261014T094331Z-b5fa caller=+380501234567 outcome=completed hangup_by=far_end sip_status=200 ringing_s=4.2 bridged_s=63.1 codec=PCMU nat_mode=stun lost=0 late=2 discarded_silence=12
```

| Field | Meaning |
| --- | --- |
| `call_id` | `YYYYMMDDTHHMMSSZ-xxxx`, the same ID as in hooks (`SIPTOOTH_CALL_ID`) |
| `caller` | cleaned caller number, or `withheld` |
| `outcome` | how the call ended, table below |
| `hangup_by` | `caller`, `far_end`, `siptooth`, or empty |
| `sip_status` | last final response to the INVITE; `0` when there was none |
| `ringing_s` | seconds from the ring until the phone call became active (or the call ended) |
| `bridged_s` | seconds of bridged audio; `0` if never bridged |
| `codec` | negotiated send codec (`PCMU` / `PCMA`); empty if never answered |
| `nat_mode` | the media address mode used: `local`, `stun`, `public_ip` |
| `lost` | RTP packets never received (gaps) |
| `late` | packets that arrived too late for the jitter buffer |
| `discarded_silence` | silence frames skipped by catch-up |

## Outcomes

Every call ends with exactly one `call.ended` (and one summary line). When Bluetooth is not ready at the ring, siptooth does nothing and the phone rings normally: one log line, no hook.

`hangup_by` is who ended the call: `caller` = the cellular caller hung up, `far_end` = the SIP side hung up (BYE), `siptooth` = siptooth ended it for the reason in the outcome. Empty (null in the hook payload) means no call was ever established, so nobody hung up.

| Outcome | `hangup_by` | Meaning | Phone |
| --- | --- | --- | --- |
| `completed` | `caller` or `far_end` | A bridged call ended normally. On a far-end BYE, buffered downlink audio plays out (at most 3 s) and then `AT+CHUP` hangs up the phone. | hung up by siptooth when the far end ended |
| `sip_rejected` | empty | The target answered the INVITE with a final 3xx-6xx (`sip_status` records it; 3xx is not followed). | keeps ringing |
| `sip_timeout` | empty | No answer within `sip.dial_timeout_seconds`. | keeps ringing |
| `sip_error` | empty | DNS, connect, TLS, authentication or local setup failed (`error` has the detail). | keeps ringing |
| `sip_media_failed` | empty | The SDP answer was unusable (codec, SRTP mismatch, port 0, IPv6, ...). The dialog is ACKed and BYEd. | keeps ringing |
| `caller_abandoned` | `caller` (empty while dialing) | The caller gave up, or the owner rejected the call, before or right after answering. The SIP side gets a BYE or CANCEL. | already ended |
| `owner_answered` | empty | The owner answered on the handset (or the phone answered without siptooth's ATA). siptooth never touches that call. | owner's call, untouched |
| `answer_failed` | `siptooth` | `ATA` was refused, or no `CallActive` within 5 s, or no SCO link within 5 s of `CallActive`. BYE is sent; `AT+CHUP` only if siptooth had answered. | left alone unless siptooth answered |
| `far_end_ended` | `far_end` | The far end hung up after the SIP answer but before the phone call was answered. | keeps ringing (or hung up if already answered) |
| `signaling_lost` | `siptooth` | The SIP connection was lost mid-call. | `AT+CHUP` |
| `media_timeout` | `siptooth` | No RTP received for `calls.media_timeout_seconds`. BYE and `AT+CHUP`. | hung up |
| `audio_lost` | `siptooth` | SCO closed or stalled and the phone did not report the call ended within 1.5 s. BYE only: the owner may have moved the call to the handset. | left alone (no `AT+CHUP`) |
| `media_error` | `siptooth` | A fatal RTP socket error or a media pump failure. | hung up |
| `max_duration` | `siptooth` | `calls.max_duration_seconds` reached. | hung up |
| `bluetooth_lost` | `siptooth` | The Bluetooth link dropped. BYE sent. | n/a |
| `shutdown` | `siptooth` | The daemon was stopped (SIGTERM). | hung up if siptooth had answered |

## Tuning knobs

All in `~/.siptooth/config.yaml`; restart after changing (`systemctl --user restart siptooth`), and run `siptooth config check` first.

| Key | Default | When to change |
| --- | --- | --- |
| `media.jitter_ms` (20..500) | 60 | Playout buffer depth for audio from the far end. Raise it when the far end sounds choppy and `late` or `lost` in the summary line are high; lower it to reduce delay on a clean path. |
| `media.silence_rms` (0..5000) | 200 | Catch-up threshold: after a burst, frames quieter than this RMS may be skipped to bring the buffer back to `jitter_ms`. Lower it, or set `0` to disable catch-up, if you hear clipped quiet words (watch `discarded_silence`); raise it for more aggressive catch-up on a noisy line. |
| `calls.media_timeout_seconds` (5..300) | 10 | How long without RTP before siptooth hangs up. See "Far-end hang-up delayed". Raise it for a trunk that stops sending during silence. |
| `calls.sco_settle_ms` (0..5000) | 800 | Pause after the SCO link comes up before audio is bridged. Raise it if the first words of calls are lost or garbled. |
| `sip.dial_timeout_seconds` (5..120) | 30 | How long the SIP side may ring before siptooth gives up and lets the phone ring. Keep it shorter than the carrier's voicemail timeout. |
| `calls.caller_id_wait_ms` (0..3000) | 2500 | How long to wait for the caller number after the first ring. Raise it if `caller=withheld` appears for calls that should show a number; `0` dials at once. |
| `bluetooth.power_cycle_minutes` | 60 | Idle power cycle of the controller (see below). |

## NAT modes

`nat.mode` decides which media address (IP and RTP port) siptooth advertises in the SDP.

| Mode | Use when |
| --- | --- |
| `auto` (default) | Almost always. A private, loopback, link-local or CGNAT next hop resolves to `local`; anything else to `stun`. |
| `local` | The PBX is on your LAN (no NAT between siptooth and the far end). Advertises the Pi's address. |
| `stun` | The target is on the internet and the Pi is behind a home router. A Binding Request from the RTP socket to `nat.stun_servers` gives the public mapping, refreshed every 10 s while dialing. If every server fails, the call continues with `nat.public_ip` if set, else the local address, and a `stun_failed` `system.error` is emitted at most once per hour. |
| `public_ip` | You forwarded the RTP range (`nat.rtp_port_min`..`nat.rtp_port_max`) on the router. Advertises `nat.public_ip` with the local port, no STUN. |

siptooth relies on the far end latching onto the source address of the first RTP packets it receives (LiveKit and Asterisk with `rtp_symmetric=yes` do). CGNAT with a far end that does not latch is unsupported.

## Registration

With `sip.register: true` (needs `sip.username`; used for Zadarma) siptooth REGISTERs, refreshes at half of `sip.register_expires_seconds`, keeps the flow open with CRLF keepalives, and unregisters on shutdown. The Contact used in INVITEs is the flow address seen by the registrar. `siptooth status` shows `registration: registered=yes contact=... expires=...` or the last error; a failure emits `system.error` code `sip_registration` once per outage. Without registration, siptooth learns its signalling address with an OPTIONS on the INVITE's connection before each call.

## Power cycle

Every `bluetooth.power_cycle_minutes` (default 60; `0` = off, otherwise 10..1440) while no call is in progress, siptooth powers the adapter off and on and the phone reconnects by itself. This works around a controller that gets stuck after a long time connected.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Nothing happens when the phone rings | `siptooth status`: `ready` and `phone connected` must be yes. `siptooth doctor` for `bluetooth_group`, `phone_bonded`, `competing_handlers`. If Bluetooth is not ready the phone rings normally and the journal has one line. |
| Competing handlers | `doctor` reports `competing_handlers` when PipeWire, PulseAudio, bluealsa or ofono is running. Stop and disable them (for user services `systemctl --user disable --now pipewire pipewire-pulse wireplumber pulseaudio`; system services need an administrator), then reboot. They steal the hands-free profile and the SCO link. |
| allochka still running | `doctor` warns `allochka_service`. `systemctl --user disable --now allochka`. |
| Answers, the caller hears nothing and the audio is constant hiss from the first second | The Bluetooth controller got stuck (seen after a night connected). `bluetoothctl power off`, wait 2 s, `bluetoothctl power on`; the phone reconnects by itself. The daemon does the same every `bluetooth.power_cycle_minutes` (default 60) while no call is in progress, so lower it if this recurs within the hour. |
| Calls are answered but both sides are silent | SCO may not be routed over HCI. Raspberry Pi OS does this at boot via `bthelper`; check `systemctl status bthelper` and `hciconfig`/`btmgmt` if available, and reboot. `doctor` `sco_routing` cannot verify this without CAP_NET_RAW. |
| Calls end with `answer_failed` | The phone never opened SCO or refused `ATA`. Check the phone's "Phone calls" audio setting for the Pi and unlock the phone once if it rebooted. |
| Phone keeps ringing, outcome `sip_error` | Run `siptooth doctor`: `dns`, `sip_connect` (TCP/TLS connect and certificate check), `sip_options`, `sip_password`. A TLS certificate failure is never bypassed; try `sip.transport: udp` if the provider's certificate is invalid. Debug logging shows the SIP messages. |
| `sip_rejected` | `sip_status` and the reason are in the summary line and hook payload: 401/403 wrong credentials or From; 404 wrong number or route; 480/486/503 target busy or down. Compare `sip.target`, `sip.username`, `sip.from` with `docs/providers.md`. |
| `sip_timeout` | The far end never answered within `sip.dial_timeout_seconds`. Check the route on the provider side; with `register: true`, `siptooth status` must show `registered=yes`. |
| `sip_media_failed` | The SDP answer was refused. Check `media.codecs` against what the provider supports, and `sip.srtp` against its media-encryption setting. |
| One-way audio, or none | NAT checklist below. |
| Far-end hang-up delayed | The far end's BYE did not reach siptooth; see below. |
| Hooks fail | `siptooth hooks test --event call.ended` shows exit code, stdout and stderr; hooks run as you with a minimal environment. See `docs/hooks.md`. |
| `registration: registered=no` | Check `sip.username`, the password in `~/.siptooth/env`, `sip.registrar`, and that the transport matches the provider; the error is in `siptooth status` and as a `sip_registration` journal line. |

### One-way audio: NAT checklist

1. `nat_mode` in the summary line: on the internet it should be `stun`; on a LAN `local`.
2. `siptooth doctor` `nat_mapping`: "endpoint-independent mapping" is fine; "address-dependent mapping" needs a far end that latches (most do).
3. Caller hears nothing: the far end cannot send to the advertised address. Try `nat.mode: public_ip` with a router port-forward of the RTP range, or check that no VPN or double NAT hides the Pi.
4. siptooth hears nothing: `lost`/`late` zero and `bridged_s` small, then `media_timeout` after `calls.media_timeout_seconds`. The far end is sending to a different address than the one it latched, or the provider's media server is firewalled.
5. SRTP: if `sip.srtp` is on, `srtp_fail` in the hook payload's `media` counts packets that failed decryption (key mismatch).
6. With `debug` logging, compare `advertised_media` and `remote_media` in the `call.connected` payload.

### Far-end hang-up delayed

When the SIP side hangs up, its BYE should reach siptooth over the connection (or registered flow) siptooth opened, and the phone is then hung up within about 3 s. If a far end sends the BYE to a new connection towards the Contact address instead (this can happen behind NAT with some providers), siptooth never sees it. The call then ends only when RTP stops: after `calls.media_timeout_seconds` (default 10) of silence from the far end, siptooth hangs up (`media_timeout`), so the caller hears up to that many seconds of silence. Fixes: lower `calls.media_timeout_seconds` (not below the longest gap the far end leaves between packets, 5..300), enable `sip.register` where the provider supports it so the BYE returns on the registered flow, or give the Pi a reachable address. Far ends that stop sending RTP during silence need the opposite: a larger value.
