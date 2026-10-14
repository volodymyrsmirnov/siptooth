# Siptooth — Design Specification

**Date:** 2026-10-14
**Status:** Draft for review
**Predecessor:** allochka (`~/Projects/Personal/allochka`), spec `docs/superpowers/specs/2026-10-04-allochka-design.md`. Allochka is retired once siptooth is proven on real calls.

Siptooth turns an Android phone that stays at home into a SIP trunk endpoint. A Raspberry Pi 5 acts as a Bluetooth hands-free unit for the phone, exactly as allochka does. When the phone rings, siptooth places a SIP call to a configured target (a PBX, a SIP trunk, or an AI voice platform such as ElevenLabs Agents), answers the phone only once the far end answers, and bridges the audio both ways until either side hangs up. Siptooth has no AI, no conversation logic, no web UI and no call store.

## 1. Goals, non-goals, success criteria

**Goals**

- Keep allochka's proven Bluetooth layer: BlueZ over D-Bus, HFP hands-free AT protocol, SCO audio, in-app pairing, adapter power cycling, `doctor`, `install`.
- Forward every incoming call on the phone to one configured SIP target, with caller ID passed through.
- Work from a home network behind NAT (STUN required).
- Work with any standards-based SIP trunk or PBX; ElevenLabs Agents SIP trunking and the owner's existing PBX are the two targets tested for v1.
- Shell hooks on call lifecycle events, so later work (for example a script that accepts OpenAI SIP calls) can attach without changing siptooth.

**Non-goals (v1)**

- OpenAI Realtime SIP (needs a public webhook and an `accept` API call per call; a later, separate component).
- Calls from SIP to the phone (outbound calls through the phone), SIP registration, receiving unsolicited INVITEs.
- Call waiting, hold, transfer (REFER), conferencing, multiple phones, multiple targets or routing rules.
- Recording, call store, transcripts, web UI, metrics endpoint.
- Wideband (mSBC / G.722), Opus, ICE, TURN, DTLS-SRTP, RTCP, RFC 4733 DTMF events.
- Live config reload (restart to apply).

**Success criteria**

1. A call to the phone reaches the configured PBX extension with two-way audio, caller ID visible on the PBX, and hang-up propagating in both directions.
2. The same works against an ElevenLabs agent: the agent greets the caller and the caller hears the whole greeting.
3. When the SIP target is unreachable, rejects, or does not answer, the phone keeps ringing as if siptooth were not there (the caller never hears silence or an error from siptooth).
4. Runs unattended for a week behind a home NAT without a restart.

## 2. Decisions (fixed)

| Topic | Decision |
| --- | --- |
| Language / build | Go, single binary `siptooth`, `CGO_ENABLED=0`, module `github.com/volodymyrsmirnov/siptooth`, `go 1.24` directive (Raspberry Pi OS trixie ships Go 1.24), built on the Pi. |
| Code origin | New repository. Allochka's hardware packages are copied in (not shared as a module) and trimmed; allochka is retired afterwards, so the copies never need to stay in sync. |
| Bluetooth | Unchanged from allochka: BlueZ 5.82 via D-Bus, app-owned HFP AT protocol over RFCOMM, CVSD narrowband SCO (16-bit PCM, 8 kHz), Broadcom routing check, periodic adapter power cycle. |
| SIP signaling | `github.com/emiago/sipgo` (pure Go; UDP, TCP, TLS; digest auth). Siptooth is a user agent client only: one outgoing INVITE per incoming phone call. No REGISTER. |
| Media | Own small RTP layer: one UDP socket per call, G.711 PCMU or PCMA, 20 ms packets, packet structs from `github.com/pion/rtp`. Optional SRTP (SDES, `AES_CM_128_HMAC_SHA1_80`) via `github.com/pion/srtp/v3`. |
| NAT | STUN (`github.com/pion/stun`) on the RTP socket before every INVITE; the mapped address goes into the SDP. Static `public_ip` override and a configurable RTP port range for port forwarding. Symmetric RTP (send from the port we receive on). |
| Answer policy | SIP first: on ring, dial immediately; answer the phone (`ATA`) only after the far end sends `200 OK`. Any SIP failure leaves the phone ringing. No answer delay. |
| Audio path | No resampling: SCO PCM16 8 kHz ↔ G.711 8 kHz. No echo gate, no processing. |
| Observability | Structured logs to the journal (`log/slog`, text handler). Shell hooks on `call.ringing`, `call.connected`, `call.ended`, `system.error`. No call store. |
| Config | YAML at `~/.siptooth/config.yaml`, validated at startup; secrets in `~/.siptooth/env` (0600). Restart to reload. |
| Service | systemd **user** unit, runs as the current user (in group `bluetooth`), everything under `~/.siptooth/`. |
| Dependencies | `godbus/dbus/v5`, `golang.org/x/sys`, `gopkg.in/yaml.v3` (from allochka) plus `emiago/sipgo`, `pion/rtp`, `pion/srtp/v3`, `pion/stun`. No others without the owner's approval. `coder/websocket` is dropped. |
| Calls | One at a time. Call waiting is never answered (allochka's HFP latch). |

## 3. Architecture

```
Caller ──PSTN──▶ Android phone (HFP Audio Gateway)
                    │ RFCOMM (AT)            │ SCO (CVSD, PCM16 8 kHz)
                    ▼                        ▼
 ┌──────────────────────── siptooth run ─────────────────────────────┐
 │  bluez ─▶ hfp ─▶ bridge (call coordinator) ◀──▶ sip ──TLS/TCP/UDP──▶ PBX / trunk
 │                    │      ▲                                        │
 │  sco ─────────▶ media pump ◀── G.711 ──▶ rtp ────(S)RTP/UDP──────▶ │
 │                    │                      ▲                        │
 │                    ├─▶ hooks (sh -c)     stun (public mapping)     │
 │                    └─▶ control (Unix socket, status for the CLI)   │
 └────────────────────────────────────────────────────────────────────┘
```

One daemon owns the HFP profile, the SCO socket, and the SIP user agent. CLI subcommands talk to it over the control socket (`status`) or run standalone (`pair`, `doctor`, `install`, `sip test`, `hooks test`, `config check`).

### 3.1 Repository layout

```
go.mod, go.sum, Makefile, README.md, CLAUDE.md, AGENTS.md
cmd/siptooth/main.go          # dispatches to internal/cli
internal/
  bluez/      # copied: D-Bus adapter, device, Profile1, Agent1, Broadcom routing (linux tag)
  hfp/        # copied: AT framing, SLC, call indicators, call-waiting latch
  sco/        # copied: SCO listen/accept/dial, MTU, backlog drain (linux tag)
  daemon/     # copied + trimmed: composition root, Bluetooth session glue, power cycle
  doctor/     # copied + trimmed: setup checks; new SIP/STUN/TLS reachability checks
  host/       # copied: command runner, systemd user unit template
  hooks/      # copied + trimmed: runner (sh, timeout, process group, bounded output); new payloads
  control/    # copied + trimmed: HTTP over Unix socket, status only
  cli/        # copied + trimmed: run, pair, doctor, install, status, config, hooks, sip test, version
  config/     # new schema, same loading/validation style
  g711/       # μ-law (copied from allochka audio) + A-law (new)
  sdp/        # new: build offer, parse answer (one audio stream)
  rtp/        # new: socket, packetize/depacketize, SRTP wrap, jitter buffer
  stun/       # new: binding request on the RTP socket
  sip/        # new: sipgo wrapper: Dial, Cancel, Bye, in-dialog request handling
  bridge/     # new: call coordinator state machine + media pump
  defaults/   # embedded example config.yaml
  buildinfo/  # copied
config/hooks/ # example hook scripts
docs/         # setup.md, operations.md, hooks.md, providers.md
```

Dropped from allochka: `realtime`, `prompt`, `library`, `summary`, `web`, `store`, `export`, `probe`, `audio` (except μ-law), `call` (replaced by `bridge`).

Linux-only code stays behind `//go:build linux` with `_other.go` stubs, so `go test ./...` runs on macOS and `GOOS=linux GOARCH=arm64 go build ./...` checks the real code.

### 3.2 Interfaces between units

```go
// hfp (unchanged from allochka)
type Client interface {
    Events() <-chan Event   // Connected, Ring, CallerID, CallActive, CallEnded, CallSetupCleared, Disconnected
    Answer(ctx) error       // ATA, 3 s deadline
    Hangup(ctx) error       // AT+CHUP, 3 s deadline
    Close() error
}

// sco (unchanged)
type Link interface { io.ReadWriteCloser; MTU() int; Remote() string }
type Listener interface { Accept(ctx) (Link, error); Close() error }
type Dialer interface { Dial(ctx, adapter, remote string) (Link, error) }

// sip
type Dialer interface {
    // Dial sends the INVITE and returns once a final response arrives, ctx ends (CANCEL sent),
    // or the transport fails. On 2xx the ACK has been sent and the Call is established.
    Dial(ctx context.Context, req DialRequest) (Call, error)   // error carries *Rejected{Status, Reason} or a transport error
}
type DialRequest struct { Target, From, DisplayName string; Headers map[string]string; Offer sdp.Offer }
type Call interface {
    Answer() sdp.Answer             // the negotiated remote media
    Updates() <-chan sdp.Answer     // re-INVITE/UPDATE with changed remote media
    Done() <-chan struct{}          // far end sent BYE or the dialog died
    Hangup(ctx context.Context) error   // BYE
    SIPCallID() string
}

// rtp
type Session interface {
    LocalAddr() netip.AddrPort                 // bound address
    PublicAddr() netip.AddrPort                // STUN-mapped or configured public address
    Start(remote netip.AddrPort, codec g711.Codec, keys *srtp.Keys) error
    SetRemote(remote netip.AddrPort)           // re-INVITE
    WriteFrame(pcm []int16) error              // exactly 160 samples (20 ms)
    ReadInto(jb *JitterBuffer)                 // receive loop feeds the jitter buffer
    Stats() Stats                              // packets sent/received/lost/late, last packet time
    Close() error
}

// hooks
type Dispatcher interface { Emit(event string, payload any) }   // asynchronous, never blocks the caller
```

`bridge` depends only on these interfaces. Tests use allochka's fake AG (scripted AT traces), an in-memory SCO link pair, a real sipgo UAS on loopback as the fake trunk, and a fake clock.

## 4. Call flow

### 4.1 States

```
Idle ──Ring──▶ Dialing ──200 OK──▶ Answering ──SCO up──▶ Bridged ──hang-up──▶ Ending ──▶ Idle
                 │                    │                                   ▲
                 └─ failure ──▶ Idle   └─ failure ──▶ Ending ─────────────┘
```

**Idle → Dialing.** On `hfp.Ring` while Bluetooth is ready and no call is in progress:

1. Create a call ID (`20261014T094331Z-b5fa`, as in allochka), log `call.ringing`.
2. Wait up to `calls.caller_id_wait_ms` (default 1000) for `hfp.CallerID` if it has not arrived yet. No caller ID by then: the caller is withheld.
3. Emit hook `call.ringing`.
4. Open the RTP socket (section 6.1) and run STUN (section 7). A STUN failure is not fatal: siptooth uses `nat.public_ip` if set, otherwise the local address, and logs a warning.
5. Build the SDP offer and the INVITE (section 5) and dial with a context bounded by `sip.dial_timeout_seconds` (default 30).

If Bluetooth is not ready when the phone rings, siptooth does nothing: the phone rings normally. One log line, no hook.

**Dialing.** Events and outcomes:

| Event | Action | Outcome |
| --- | --- | --- |
| 1xx | log (status, reason) | — |
| 2xx with acceptable SDP | ACK (sipgo), go to Answering | — |
| 2xx with unacceptable SDP (no common codec, SRTP mismatch, no audio stream, port 0) | ACK, BYE, phone keeps ringing | `sip_media_failed` |
| 3xx–6xx final | phone keeps ringing | `sip_rejected` (status code recorded) |
| dial timeout | CANCEL, phone keeps ringing | `sip_timeout` |
| DNS / connect / TLS / auth failure | phone keeps ringing | `sip_error` |
| `hfp.CallSetupCleared` (caller gave up, or owner rejected on the handset) | CANCEL | `caller_abandoned` |
| `hfp.CallActive` (the owner answered on the handset; siptooth has not sent `ATA` yet) | CANCEL, never touch the call | `owner_answered` |
| `hfp.Disconnected` | CANCEL | `bluetooth_lost` |

A `2xx` that crosses an already sent CANCEL is ACKed and immediately BYEd (RFC 3261 §9.1); the outcome stays the one that caused the CANCEL. Redirects (3xx) are not followed.

**Answering.** Right after the ACK:

1. Start the RTP session toward the remote address from the SDP answer. Siptooth sends 20 ms silence packets every 20 ms from now on (keeps the NAT mapping open and satisfies endpoints that expect media). Received audio goes into the jitter buffer (section 6.3) and waits there.
2. Send `ATA`. `ATA` error or timeout: let the next indicator decide, as allochka does; no `CallActive` within 5 s → BYE the SIP call; outcome `answer_failed`; the phone is left ringing (or ends if the caller gave up).
3. On `CallActive`, accept the SCO link (the in-band ringing backlog is drained, as in allochka); no SCO within 2 s → dial SCO; no SCO within 5 s of `CallActive` → `AT+CHUP` and BYE; outcome `answer_failed`.
4. SCO up → Bridged. Emit hook `call.connected`.

**Bridged.** The media pump (section 6.2) runs. The call ends on the first of:

| Trigger | Action | `hangup_by` | Outcome |
| --- | --- | --- | --- |
| `hfp.CallEnded` or SCO EOF | BYE | `caller` | `completed` |
| SIP BYE from far end (`Call.Done`) | `AT+CHUP` | `far_end` | `completed` |
| No RTP received for `calls.media_timeout_seconds` (default 30) | BYE, `AT+CHUP` | `siptooth` | `media_timeout` |
| `calls.max_duration_seconds` reached (default 0 = no limit) | BYE, `AT+CHUP` | `siptooth` | `max_duration` |
| `hfp.Disconnected` | BYE | `siptooth` | `bluetooth_lost` |
| Daemon shutdown (SIGTERM) | BYE, `AT+CHUP` | `siptooth` | `shutdown` |

SCO EOF alone is not trusted as a hang-up for the first 2 s after it, matching allochka: handsets often drop SCO just before reporting the end of the call. If `CallEnded` does not follow within 2 s, siptooth hangs up both sides with outcome `completed`, `hangup_by: caller`.

**Ending.** Close RTP and SCO, wait at most 3 s for BYE/CHUP responses, log the call summary line, emit hook `call.ended`, return to Idle. Every call path ends with exactly one `call.ended` (including failures in Dialing), so hooks can count missed forwards.

### 4.2 Caller ID handling

- `+CLIP` number, trimmed. Type 145 (international) without a leading `+` gets one. Characters other than `+0123456789*#` are removed.
- Empty after cleaning, or no `+CLIP` within the wait: withheld. Placeholder `{caller}` then expands to `sip.anonymous_user` (default `anonymous`) and the hook payload has `"caller": null, "caller_withheld": true`.

## 5. SIP signaling

### 5.1 Placeholders

Template strings (`sip.target`, `sip.from`, `sip.display_name`, values of `sip.headers`) may contain:

- `{caller}`: cleaned caller number (section 4.2), or `sip.anonymous_user`.
- `{number}`: the phone's own number from `phone.number` (static config; HFP `AT+CNUM` is unreliable on Android and not used).

Unknown placeholders fail config validation. Expanded values contain only `+0-9*#` or the configured anonymous user (validated as a SIP user token), so no escaping is needed.

### 5.2 The INVITE

- **Request-URI and To:** `sip.target`, e.g. `sip:{number}@sip.rtc.elevenlabs.io;transport=tls` or `sip:600@pbx.home.arpa`.
- **Transport:** `sip.transport` (`udp` | `tcp` | `tls`, default `tls`); it must match `;transport=` in the target when that parameter is present (validation). Destination host and port come from `sip.proxy` (`host:port`) when set, otherwise from the target URI host with the transport default port (5060 UDP/TCP, 5061 TLS). DNS: A/AAAA only; NAPTR/SRV are not resolved (set `sip.proxy` for SRV-only domains).
- **From:** `sip.from` (default `sip:{caller}@<target host>`) with `sip.display_name` (default `{caller}`), new tag per call.
- **Contact:** `sip:siptooth@<public ip>:<local sip port>;transport=<t>`. Public IP from STUN (section 7) or `nat.public_ip`.
- **Extra headers:** `sip.headers` map, values expanded. Example for ElevenLabs: `X-Caller-ID: {caller}`. `Call-ID`, `CSeq`, `Via`, `Max-Forwards`, `Contact`, `From`, `To`, `Content-*` cannot be overridden (validation).
- **User-Agent:** `siptooth/<version>`.
- **Body:** SDP offer (section 6.4).
- **TLS:** system roots, server name = proxy host or target host, certificate verification always on, TLS 1.2 minimum.

### 5.3 Authentication

On `401`/`407`, sipgo resends the INVITE with digest credentials: username `sip.username`, password from `SIPTOOTH_SIP_PASSWORD` (env file or environment). No username configured: a 401/407 is a final failure (`sip_error`, `auth_required`). A second challenge after credentials were sent is `sip_error`, `auth_failed`.

### 5.4 In-dialog requests from the far end

The SIP user agent listens on the same transport it dialed with (for TCP/TLS: the outbound connection stays open for the whole dialog and in-dialog requests are accepted on it; for UDP: the local SIP port).

- `BYE` → `200 OK`, ends the call.
- `re-INVITE` / `UPDATE` with SDP → apply a changed remote RTP address; a changed codec within the offered set is applied; anything else (hold `a=sendonly`/`inactive`, unsupported codec) is answered with our current SDP and ignored. Answer `200 OK` with the current local SDP (version incremented).
- `OPTIONS` → `200 OK`.
- `INFO`, `NOTIFY`, `REFER`, `MESSAGE` → `501 Not Implemented`.
- Requests that match no dialog (unsolicited INVITEs included) → `481` / `403`. Siptooth never answers a new incoming SIP call.

Session timers are not advertised (no `Supported: timer`); refresh re-INVITEs from the far end are answered as above.

## 6. Media

### 6.1 RTP socket

- One UDP socket per call, bound to `nat.rtp_bind` (default `0.0.0.0`) on an even port chosen randomly in `nat.rtp_port_min`–`nat.rtp_port_max` (default 10000–20000; retries on `EADDRINUSE`). RTCP is neither sent nor received; port+1 is not bound.
- Outgoing packets: version 2, random SSRC and initial sequence number per call, timestamp advances 160 per packet, marker bit on the first packet, payload type 0 (PCMU) or 8 (PCMA), 160 payload bytes.
- Packets are sent from the bound socket to the remote address (symmetric RTP). Siptooth does not latch onto a different source address; NAT traversal relies on the far end latching onto siptooth's (common for trunks behind NAT-aware edges).
- Incoming packets: accepted only from the SDP remote IP (any port, to tolerate far-end NAT), version 2, negotiated payload type. Other payload types (comfort noise 13, telephone-event) are counted and dropped. SSRC changes reset the jitter buffer's sequence tracking.
- STUN responses arriving on the socket during the call (first byte `0x00`/`0x01`) are ignored by the RTP reader.

### 6.2 Media pump (SCO ↔ RTP)

Pacing is read-driven, as in allochka: the SCO controller is the clock.

- **Uplink (phone → SIP):** each SCO read (MTU bytes, typically 60–64 bytes = 30–32 samples) is appended to an accumulator; every time it holds 160 samples, one 20 ms frame is G.711-encoded and sent. RTP timestamps follow the SCO sample count, so the far end sees the phone's clock.
- **Downlink (SIP → phone):** after each SCO read, the pump writes the same number of bytes taken from the jitter buffer's PCM output (silence on underrun).
- **Stall fallback:** no SCO read for 100 ms → a ticker writes silence to SCO at the frame cadence and keeps sending RTP silence at 20 ms, so neither side times out (copied behaviour).
- **Before SCO is up** (Answering): a 20 ms ticker sends RTP silence; nothing is read from the jitter buffer.

### 6.3 Jitter buffer and catch-up

- Packets are ordered by sequence number (16-bit wrap handled). Target depth `media.jitter_ms` (default 60), hard cap 2 s (oldest frames dropped when exceeded, counted as `overflow`). Packets older than the playout point are dropped (`late`). Gaps play as silence (`lost`).
- **Catch-up.** Audio received while answering (the 0.5–2 s between `200 OK` and SCO up — the ElevenLabs agent starts its greeting immediately) and clock drift between the phone's SCO clock and the far end's RTP clock both grow the buffer. Whenever depth exceeds target + 40 ms, frames whose RMS is below `media.silence_rms` (default 200 on the 16-bit scale) are discarded instead of played until the depth is back at target. Speech is never discarded, so the greeting is heard in full and the added latency drains during the first natural pauses.

### 6.4 SDP

**Offer** (one audio stream):

```
v=0
o=siptooth <sess-id> <version> IN IP4 <public ip>
s=siptooth
c=IN IP4 <public ip>
t=0 0
m=audio <public port> RTP/AVP 0 8          (RTP/SAVP when sip.srtp is true; order from media.codecs)
a=rtpmap:0 PCMU/8000
a=rtpmap:8 PCMA/8000
a=ptime:20
a=sendrecv
a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:<base64 key||salt>    (only with srtp)
```

**Answer** parsing: the first `m=audio` with a non-zero port; connection address from media-level `c=` or session-level `c=`; IPv4 only (IPv6 answer → `sip_media_failed`); the first payload type in the answer's format list that is in our offer is the codec; with SRTP, the answer must use `RTP/SAVP` and carry an `a=crypto` line with suite `AES_CM_128_HMAC_SHA1_80` and a valid inline key, otherwise `sip_media_failed`. Without SRTP, an answer that uses `RTP/SAVP` is also `sip_media_failed`.

### 6.5 SRTP

`sip.srtp: true` is allowed only with `sip.transport: tls` (keys travel in the SDP). Siptooth generates a fresh 30-byte master key+salt per call, uses `pion/srtp/v3` contexts for both directions with `ProtectionProfileAes128CmHmacSha1_80`. Decryption failures are counted and the packet dropped.

## 7. NAT and STUN

- Before every INVITE, siptooth sends a STUN Binding Request from the RTP socket to each of `nat.stun_servers` in order (default `stun.l.google.com:19302`, `stun.cloudflare.com:3478`), 500 ms timeout, 2 attempts per server, first success wins. The XOR-MAPPED-ADDRESS gives the public IP and port advertised in SDP `c=`/`m=`.
- The same public IP is used in the SIP Contact (section 5.2). For UDP signaling siptooth assumes port-preserving NAT for the SIP port and relies on `rport` (RFC 3581, sipgo default) for responses; TCP/TLS is recommended and the default.
- `nat.public_ip` set: STUN is still run for the port when `nat.rtp_port_forwarded` is false; when true, siptooth advertises `public_ip` and the local port as is (for a router port-forward of the RTP range) and skips STUN.
- STUN failure on every server: warning log, `system.error` hook with code `stun_failed` (at most once per hour), and the call proceeds with the best address known (`public_ip` or local). On a NAT that STUN cannot traverse (symmetric NAT), the documented fix is the port-forward setup above.
- `siptooth doctor` runs a STUN probe from two sockets and reports whether the mapping looks port-preserving / endpoint-independent (warns on symmetric NAT).

## 8. Bluetooth layer (copied)

Copied from allochka with these changes only:

- Names: adapter alias `siptooth`, D-Bus object paths `/com/siptooth/hfp` and `/com/siptooth/agent`, profile name `Siptooth`.
- `bluetooth.answer_delay_seconds` removed; the coordinator no longer calls `Answer` on ring, `bridge` does after SIP answers.
- Probe mode (`internal/probe`) removed; `siptooth sip test` (section 11) covers the SIP side, and the Bluetooth side is already qualified by allochka on this hardware.
- Everything else is kept unchanged, including hard-won fixes: call-waiting latch, SLC retries, profile re-registration, routing retries, bus reconnect, idle-only power cycle (`bluetooth.power_cycle_minutes`, default 60), in-band ringing backlog drain, read-driven SCO pacing with the stall fallback.

Allochka and siptooth both register the HFP hands-free profile and cannot run at the same time. The existing BlueZ bond with the phone is reused: `bluetooth.phone_address` is copied from allochka's config, no re-pairing needed.

## 9. Configuration

`~/.siptooth/config.yaml` (relative paths resolve against its directory):

```yaml
bluetooth:
  adapter: hci0
  phone_address: ""            # required; `siptooth pair` fills it in
  power_cycle_minutes: 60      # 0 = off, otherwise 10..1440

phone:
  number: ""                   # the phone's own number, E.164 (e.g. +380441234567); required if {number} is used

sip:
  target: "sip:{number}@sip.rtc.elevenlabs.io;transport=tls"   # required
  transport: tls               # udp | tcp | tls
  proxy: ""                    # optional host:port; default: target host + transport default port
  from: ""                     # default "sip:{caller}@<target host>"
  display_name: "{caller}"
  anonymous_user: anonymous    # {caller} when the number is withheld
  username: ""                 # digest username; password: SIPTOOTH_SIP_PASSWORD in ~/.siptooth/env
  headers:                     # optional extra INVITE headers, values may use placeholders
    X-Caller-ID: "{caller}"
  srtp: false                  # SDES SRTP; requires transport tls
  dial_timeout_seconds: 30     # 5..120

media:
  codecs: [PCMU, PCMA]         # offer order; at least one
  jitter_ms: 60                # 20..500
  silence_rms: 200             # catch-up threshold, 0..5000 (0 disables catch-up)

nat:
  stun_servers: ["stun.l.google.com:19302", "stun.cloudflare.com:3478"]
  public_ip: ""                # optional static public IPv4
  rtp_port_forwarded: false    # true: advertise public_ip + local port, skip STUN
  rtp_bind: 0.0.0.0
  rtp_port_min: 10000
  rtp_port_max: 20000

calls:
  caller_id_wait_ms: 1000      # 0..3000
  media_timeout_seconds: 30    # 5..300
  max_duration_seconds: 0      # 0 = no limit, else 60..86400

control:
  socket: siptooth.sock

# hooks:
#   - id: notify
#     events: [call.ended]
#     command: "/home/pi/.siptooth/hooks/notify.sh"
#     timeout_seconds: 10
```

Validation (startup fails on any error, `siptooth config check` prints all of them): required fields present; MAC format; placeholders known; `{number}` used ⇒ `phone.number` set; target is a `sip:`/`sips:` URI; transport consistent with target; `srtp` ⇒ `tls`; `rtp_port_forwarded` ⇒ `public_ip`; port range even-aligned and min < max; `username` set ⇒ `SIPTOOTH_SIP_PASSWORD` present; forbidden header names; hook events known; numeric ranges as noted. Unknown keys are errors (`yaml.v3` `KnownFields`).

## 10. Hooks

Same runner as allochka: `/bin/sh -c COMMAND`, JSON payload on stdin, `timeout_seconds` (default 10, max 300) then SIGTERM/SIGKILL of the process group, stdout/stderr captured up to 8 KiB and logged, one attempt, no retries. Environment: the daemon's environment minus `SIPTOOTH_SIP_PASSWORD`, plus `SIPTOOTH_EVENT`, `SIPTOOTH_CALL_ID`, `SIPTOOTH_CALLER` (empty when withheld).

Hooks run asynchronously and never delay the call: `Emit` enqueues to a bounded queue (64 events) served by up to 4 concurrent runners; a full queue drops the event with a warning log. Each hook's events run in emission order for that call.

**Events and payloads** (`schema_version: 1`):

```json
{ "schema_version": 1, "event": "call.ringing", "occurred_at": "2026-10-14T09:43:31Z",
  "call": { "id": "20261014T094331Z-b5fa", "caller": "+380501234567", "caller_withheld": false,
            "number": "+380441234567", "target": "sip:+380441234567@sip.rtc.elevenlabs.io;transport=tls" } }
```

- `call.ringing`: before the INVITE (fields above). Intended for side processes that must prepare for the call (for example a future OpenAI-accept service correlating by caller).
- `call.connected`: audio bridged; adds `sip_call_id`, `codec`, `srtp`, `remote_media` (ip:port), `public_media` (ip:port), `connected_at`.
- `call.ended`: always exactly once per call; adds `outcome` (`completed`, `sip_rejected`, `sip_timeout`, `sip_error`, `sip_media_failed`, `caller_abandoned`, `owner_answered`, `answer_failed`, `media_timeout`, `max_duration`, `bluetooth_lost`, `shutdown`), `hangup_by` (`caller`, `far_end`, `siptooth`, or null), `sip_status` and `sip_reason` (last final response, null if none), `error` (string or null), `ringing_seconds`, `bridged_seconds` (null if never bridged), `media` (`{"sent","received","lost","late","overflow","discarded_silence"}`).
- `system.error`: `{"error":{"code","detail"}}`, codes `bluetooth_unavailable`, `profile_registration`, `routing`, `stun_failed`, `hook_queue_full`. Same once-per-outage rules as allochka.

The target in payloads is the expanded URI; credentials never appear in payloads or logs.

`siptooth hooks test --event <name>` runs matching hooks with a sample payload (`"test": true`) without the daemon.

## 11. CLI and control socket

| Command | Purpose |
| --- | --- |
| `siptooth run` | the daemon (systemd runs this) |
| `siptooth pair` | interactive pairing (copied; daemon must be stopped) |
| `siptooth doctor [--fix]` | setup checks: config valid, env file mode, group `bluetooth`, BlueZ, adapter, conflicting HFP handlers (PipeWire, bluealsa, **allochka service**), linger, DNS of the SIP host, TCP/TLS connect + certificate check to the proxy, STUN probe and NAT type. `--fix` creates `~/.siptooth/`, default config, env file. Never runs sudo. |
| `siptooth install` | writes and enables the systemd user unit (copied) |
| `siptooth status` | daemon state via control socket: Bluetooth ready, phone connected, current call (state, caller, duration), last call outcome |
| `siptooth config check` | validate and print effective config (password redacted) |
| `siptooth hooks test --event E` | run hooks with a sample payload |
| `siptooth sip test [--caller N] [--seconds 20] [--tone]` | places a real SIP call to the target without Bluetooth (placeholders expanded with `--caller`, default `+10000000000`), runs STUN, prints the mapped address, every SIP response, the negotiated codec/SRTP, then for `--seconds` sends silence (or a 1 kHz tone at -20 dBFS with `--tone`) and reports packets received, loss, max gap, and received audio level; hangs up. Exit 0 only when the call was answered and audio packets were received. Works while the daemon runs (separate SIP and RTP ports). |
| `siptooth version` | build info |

Control socket: HTTP over `~/.siptooth/siptooth.sock`, one endpoint `GET /status` (JSON). No admin actions.

## 12. Logging

`log/slog` text handler to stderr (journal). Level from `SIPTOOTH_LOG_LEVEL` (`info` default, `debug` logs every SIP message's first line and SDP bodies, never `Authorization` headers). Every call-scoped line carries `call_id`. One summary line per call at `info`: `msg="call ended" call_id=… caller=… outcome=… hangup_by=… sip_status=… ringing_s=… bridged_s=… codec=… lost=… late=… discarded_silence=…`.

## 13. Error handling summary

| Failure | Behaviour |
| --- | --- |
| SIP target unreachable, rejects, times out, auth fails | phone keeps ringing; `call.ended` with the reason |
| SDP answer unusable | ACK + BYE, phone keeps ringing |
| `ATA` fails / no `CallActive` / no SCO | BYE (and `AT+CHUP` once active); `answer_failed` |
| STUN fails | proceed with fallback address, warning, `system.error` (rate-limited) |
| Far end stops sending RTP | `media_timeout` after 30 s, hang up both |
| Bluetooth link lost mid-call | BYE, `bluetooth_lost` |
| Hook fails / times out | logged, never affects calls |
| Panic in a call goroutine | recovered, both sides hung up, `call.ended` with `error`, daemon keeps running |
| Invalid config | startup fails with all errors listed; systemd restarts with backoff |

## 14. Testing

TDD throughout; tests never touch real Bluetooth, real networks beyond loopback, or real system services.

- **Copied packages** keep their existing tests (hfp AT traces, bluez helpers, sco framing, daemon, doctor, hooks runner).
- **g711:** μ-law tests copied; A-law against ITU G.711 vectors; round-trip error bounds.
- **sdp:** offer golden files; answer parsing table (codec choice, session vs media `c=`, port 0, SAVP/crypto combinations, IPv6 rejection, malformed input).
- **rtp:** packetization, sequence wrap, jitter buffer ordering/late/loss/overflow, catch-up discards only silence, SRTP round trip with pion contexts, symmetric send/receive on loopback.
- **stun:** binding against a loopback `pion/stun` server; timeout and fallback ordering; demux with RTP on one socket.
- **sip:** a real sipgo UAS on loopback (UDP and TCP; TLS with a test CA) acting as the trunk: 200, 486, 503, 407 challenge then 200, wrong password, no answer (CANCEL), 200 after CANCEL (ACK+BYE), far-end BYE, re-INVITE with new address, OPTIONS, unsolicited INVITE rejected; asserts on headers (placeholders, From, Contact, extra headers, no forbidden overrides).
- **bridge:** state machine with fake HFP client, fake SIP dialer, in-memory SCO pair, fake clock: every row of the tables in section 4.1, caller ID wait, withheld caller, owner answering on the handset, call waiting ignored, exactly one `call.ended` per call, audio frames flow both ways with correct pacing.
- **End-to-end (loopback):** fake AG + in-memory SCO + sipgo UAS that echoes RTP: a tone written on the SCO side comes back on the SCO side; hang-up from either end.
- **CI:** `make check` = `go vet ./...`, `go test -race ./...`, `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...`.

## 15. Rollout

1. **M1 — SIP from the Pi without Bluetooth:** `siptooth sip test --tone` against the owner's PBX (call an echo or record extension), then against ElevenLabs. Confirms DNS, TLS, auth, STUN, NAT, codec, SRTP.
2. **M2 — real call to the PBX:** stop allochka (`systemctl --user disable --now allochka`), install siptooth with allochka's `phone_address`, call the phone, verify the success criteria 1 and 3.
3. **M3 — ElevenLabs:** point `sip.target` at the agent number, verify criterion 2 (full greeting heard).
4. **M4 — soak:** a week of normal use (criterion 4). Then allochka is archived.

Each hardware step is run by the owner, one command at a time.

## 16. Risks and open questions

- **Line echo.** The phone network returns an echo of the far end's own voice (allochka needed an echo gate against OpenAI). ElevenLabs and most PBX endpoints have their own echo handling, so siptooth ships without one; if M3 shows the agent interrupting itself, allochka's echo gate can be ported as an option.
- **NAT variants.** Symmetric NAT or a far end that does not latch onto siptooth's source address breaks audio; mitigated by the port-forward mode and the doctor NAT check.
- **No RTCP.** Some media servers time out calls without RTCP; none of the v1 targets is known to. Adding RTCP sender reports later is contained in `rtp`.
- **TLS in-dialog requests.** Some trunks open a new connection to the Contact for BYE instead of reusing the existing one; behind NAT that BYE is lost. The `media_timeout` ends such calls within 30 s; `hangup_by` will show `siptooth`.
- **Answer gap.** The far end is connected 0.5–2 s before the caller hears anything; the catch-up buffer keeps the greeting intact, but the far end's VAD may hear silence first. Acceptable for v1.
- **Narrowband only.** CVSD 8 kHz is the audio ceiling; mSBC/G.722 wideband is a later iteration.
- **OpenAI path (later).** OpenAI's SIP endpoint needs TLS + SRTP (supported here) plus a public webhook and an `accept` call per call. That belongs in a separate service; siptooth's `call.ringing` hook can notify it.
