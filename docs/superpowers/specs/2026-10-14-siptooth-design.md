# Siptooth — Design Specification

**Date:** 2026-10-14
**Status:** Approved; implemented (revision 4, 2026-10-14: implementation decisions folded in, see Appendix A). Revision 5 (2026-10-14): SMS forwarding to hooks over Bluetooth MAP, section 17.
**Predecessor:** allochka (`~/Projects/Personal/allochka`), spec `docs/superpowers/specs/2026-10-04-allochka-design.md`. Allochka is retired once siptooth is proven on real calls.

Siptooth turns an Android phone that stays at home into a SIP endpoint. A Raspberry Pi 5 acts as a Bluetooth hands-free unit for the phone, exactly as allochka does. When the phone rings, siptooth places a SIP call to one configured target, answers the phone only once the far end answers, and bridges the audio both ways until either side hangs up. Siptooth has no AI, no conversation logic, no web UI and no call store.

## 1. Goals, non-goals, success criteria

**Goals**

- Keep allochka's proven Bluetooth layer: BlueZ over D-Bus, HFP hands-free AT protocol, SCO audio, in-app pairing, adapter power cycling, `doctor`, `install`.
- Forward every incoming call on the phone to one configured SIP target, with caller ID passed through.
- Work from a home network behind NAT, toward targets on the internet (STUN) and on the LAN.
- Generic SIP forwarding: the target is whatever the config names. Two **qualified** v1 targets, both first-class and used independently: **ElevenLabs Agents** SIP trunking (called directly) and the owner's **Zadarma** cloud PBX (an extension account; the PBX then rings people, plays a menu, takes voicemail, or forwards to an external SIP URI such as an ElevenLabs agent). Other standards-based trunks and LAN PBXs are best effort; siptooth is a deliberately small UAC, not a general-purpose softphone.
- Incoming SMS forwarded to shell hooks (sender and text) over Bluetooth MAP, section 17.
- Shell hooks on call lifecycle events, so later work (for example a separate service that accepts OpenAI SIP calls) can attach without changing siptooth.

**Non-goals (v1)**

- OpenAI Realtime SIP (needs a public webhook and an `accept` call per call; a later, separate component).
- Calls from SIP to the phone, answering unsolicited INVITEs, accepting inbound TCP/TLS connections. (REGISTER is supported only to authorize and keep a flow for outgoing calls, section 5.7.)
- Call waiting, hold, transfer (REFER), conferencing, multiple phones, multiple targets or routing rules.
- Recording, call store, transcripts, web UI, metrics endpoint.
- Wideband (mSBC / G.722), Opus, ICE, TURN, DTLS-SRTP, RTCP, RFC 4733 DTMF events, SIP UPDATE, codec or key renegotiation mid-call.
- Live config reload (restart to apply).
- Sending SMS, MMS/RCS, catch-up of messages received while disconnected (section 17.1).

**Success criteria**

1. **Zadarma:** a call to the phone reaches the configured Zadarma destination with two-way audio and hang-up propagating in both directions within 2 s. The original caller's number reaches the hooks and the SIP request (display name, `X-CALLER-ID`); whether Zadarma shows it to the destination is a qualification item (section 15.1).
2. **ElevenLabs direct:** the agent greets the caller, the caller hears the whole greeting, the agent receives the caller's number (`X-CALLER-ID`), and an agent-side hang-up reaches the caller within 2 s (or within `calls.media_timeout_seconds` if M1 shows ElevenLabs cannot reach siptooth's connection; section 5.5).
3. When the SIP target is unreachable, rejects, or does not answer, the phone keeps ringing as if siptooth were not there (the caller never hears silence or an error from siptooth).
4. Runs unattended for a week behind a home NAT without a restart.

## 2. Decisions (fixed)

| Topic | Decision |
| --- | --- |
| Language / build | Go, single binary `siptooth`, `CGO_ENABLED=0`, module `github.com/volodymyrsmirnov/siptooth`, `go 1.24` directive (Raspberry Pi OS trixie ships Go 1.24), built on the Pi. |
| Code origin | New repository. Allochka's hardware packages are copied in (not shared as a module) and trimmed; allochka is retired afterwards, so the copies never need to stay in sync. |
| Bluetooth | Unchanged from allochka: BlueZ 5.82 via D-Bus, app-owned HFP AT protocol over RFCOMM, CVSD narrowband SCO (16-bit PCM, 8 kHz), Broadcom routing check, periodic adapter power cycle. |
| SIP signaling | `github.com/emiago/sipgo` **v1.6.0** (pure Go; UDP, TCP, TLS; digest auth), wrapped by `internal/sip`, which owns the INVITE transaction outcome (section 5.3). UAC only: one INVITE per incoming phone call; optional REGISTER for providers that require it (section 5.7). |
| Media | Own small RTP layer on one UDP socket per call: G.711 PCMU/PCMA, 20 ms transmit packets, packet structs from `github.com/pion/rtp` **v1.10.5**. Optional SDES-SRTP (`AES_CM_128_HMAC_SHA1_80`) via `github.com/pion/srtp/v3` **v3.1.3**. |
| STUN | Hand-rolled Binding Request/Response (RFC 5389, XOR-MAPPED-ADDRESS only, about 100 lines in `internal/stun`) on the RTP socket. No STUN module. |
| NAT | Per call, the advertised address depends on the destination: LAN targets get the local interface address, internet targets the STUN-mapped (or configured public) address. Symmetric RTP with latching. TCP/TLS signaling connection kept open with keepalives for the whole dialog. |
| Answer policy | SIP first: on ring, dial immediately; answer the phone (`ATA`) only after the far end's `2xx`. Any SIP failure leaves the phone ringing. No answer delay. |
| Audio path | No resampling: SCO PCM16 8 kHz ↔ G.711 8 kHz. No echo gate, no processing. Startup audio buffered losslessly; latency recovered by dropping silence only. |
| Observability | `log/slog` to the journal. Shell hooks on `call.ringing`, `call.connected`, `call.ended`, `sms.received` (section 17), `system.error`. No call store. |
| Config | YAML at `~/.siptooth/config.yaml`, validated at startup; secrets in `~/.siptooth/env` (0600). Restart to reload. |
| Service | systemd **user** unit, runs as the current user (in group `bluetooth`), everything under `~/.siptooth/`. |
| Dependencies | Direct: `godbus/dbus/v5`, `golang.org/x/sys`, `gopkg.in/yaml.v3` (from allochka), `emiago/sipgo` v1.6.0, `pion/rtp` v1.10.5, `pion/srtp/v3` v3.1.3, pinned. Their transitive modules are accepted. No other direct dependency without the owner's approval. `coder/websocket` is dropped. The resolved graph must build with Go 1.24 (`make check`). |
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
 │                    ├─▶ hooks (sh -c)     stun (mapping, keepalive) │
 │                    └─▶ control (Unix socket, status for the CLI)   │
 └────────────────────────────────────────────────────────────────────┘
```

One daemon owns the HFP profile, the SCO socket and the SIP user agent. CLI subcommands talk to it over the control socket (`status`) or run standalone (`pair`, `doctor`, `install`, `sip test`, `hooks test`, `config check`).

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
  hooks/      # copied + trimmed: single-worker dispatcher, runner (sh, timeout, process group); new payloads
  control/    # copied + trimmed: HTTP over Unix socket, status only
  cli/        # copied + trimmed: run, pair, doctor, install, status, config, hooks, sip test, version
  config/     # new schema, same loading/validation style
  g711/       # μ-law (copied from allochka audio) + A-law (new)
  sdp/        # new: build offer, parse/validate answer and re-offers (one audio stream)
  rtp/        # new: socket, latching, packetize/depacketize, SRTP, startup + jitter buffer
  stun/       # new: Binding Request encode, response decode, address selection
  sip/        # new: sipgo wrapper: Dial (INVITE ownership), Hangup, in-dialog server, keepalive
  bridge/     # new: call coordinator state machine + media pump
  obex/       # new (rev 5): OBEX subset for MAP (section 17)
  sms/        # new (rev 5): MAP client, MNS server, bMessage, SMS service (section 17)
  defaults/   # embedded example config.yaml
  buildinfo/  # copied
config/hooks/ # example hook scripts
docs/         # setup.md, operations.md, hooks.md, providers.md (ElevenLabs, Zadarma, generic PBX recipes)
```

Dropped from allochka: `realtime`, `prompt`, `library`, `summary`, `web`, `store`, `export`, `probe`, `audio` (except μ-law), `call` (replaced by `bridge`).

Linux-only code stays behind `//go:build linux` with `_other.go` stubs, so `go test ./...` runs on macOS and `GOOS=linux GOARCH=arm64 go build ./...` checks the real code.

### 3.2 Interfaces between units

```go
// hfp (unchanged from allochka)
type Client interface {
    Events() <-chan Event   // Connected, Ring, CallerID, CallActive, CallEnded, CallSetupCleared, Disconnected
    Answer(ctx) error       // ATA, 3 s deadline; ErrNotSent / ErrTimeout / ERROR distinguished (section 4.4)
    Hangup(ctx) error       // AT+CHUP, 3 s deadline
    Close() error
}

// sco (unchanged)
type Link interface { io.ReadWriteCloser; MTU() int; Remote() string }
type Listener interface { Accept(ctx) (Link, error); Close() error }
type Dialer interface { Dial(ctx, adapter, remote string) (Link, error) }

// sip
type Dialer interface {
    // Start sends the INVITE and returns immediately. Results arrive on Attempt.Result.
    Start(req DialRequest) (Attempt, error)
}
type Attempt interface {
    Result() <-chan DialResult      // exactly one value: Answered(Call) or Failed(cause, status, reason)
    Abandon()                       // CANCEL now or after the first 1xx; any 2xx that still arrives is ACKed and BYEd
}
type DialRequest struct { Target, From, DisplayName, Contact string; Headers map[string]string; Offer sdp.Session }
type Call interface {
    Remote() sdp.Media                          // negotiated remote media
    MediaUpdates() <-chan sdp.Media             // accepted re-INVITE address changes (section 5.6)
    Ended() <-chan EndCause                     // RemoteBye | TransportLost
    Hangup(ctx context.Context) error           // BYE, authenticated if challenged; bounded by ctx
    SIPCallID() string
}

// rtp
type Session interface {
    PublicAddr() netip.AddrPort                 // advertised address (section 7)
    Start(remote sdp.Media, keys *SRTPKeys) error   // enables send + latching; early packets before Start are dropped
    Update(remote sdp.Media)                    // re-INVITE address change
    WriteFrame(pcm []int16) error               // exactly 160 samples (20 ms)
    Playout() *Playout                          // startup buffer + jitter buffer (section 6.3)
    Stats() Stats                               // sent, received, lost, late, duplicate, discarded_silence, srtp_fail, last_rx
    Err() <-chan error                          // fatal socket errors
    Close() error
}

// hooks
type Dispatcher interface { Emit(event string, payload any) }   // asynchronous, never blocks the caller
```

`bridge` depends only on these interfaces. Tests use allochka's fake AG (scripted AT traces), an in-memory SCO link pair, a real sipgo UAS on loopback as the fake trunk, and a fake clock.

## 4. Call flow

### 4.1 States

```
Idle ──Ring──▶ Dialing ──2xx──▶ Answering ──SCO up──▶ Bridged ──end──▶ Ending ──▶ Idle
                  │                 │                                    ▲
                  └──── failure ────┴────────────────────────────────────┘
```

Every call, whatever path it takes, passes through Ending, which emits exactly one `call.ended`. Each call carries a generation number; late results (an AT response, a SIP result, a timer) that belong to an earlier generation or arrive after Ending started are logged and discarded.

**Ownership rule (from allochka):** siptooth sends `AT+CHUP` only for a phone call it answered itself (`ATA` sent and `CallActive` observed after it). A call the owner answered on the handset is never touched.

### 4.2 Idle → Dialing

On `hfp.Ring` while Bluetooth is ready and no call is in progress:

1. Create a call ID (`20261014T094331Z-b5fa`, as in allochka), log `call ringing`.
2. Wait up to `calls.caller_id_wait_ms` (default 2500; hardware: the phone sends `+CLIP` with the first `RING`, about 1.05 s after `callsetup`) for `hfp.CallerID` if it has not arrived. None by then: the caller is withheld (section 4.6).
3. Emit hook `call.ringing`.
4. Open the RTP socket and pick the advertised addresses (section 7). STUN failure is not fatal (section 7.2).
5. Build the SDP offer and the INVITE (section 5) and `Start` the attempt. A dial timer of `sip.dial_timeout_seconds` (default 30) starts.

If Bluetooth is not ready when the phone rings, siptooth does nothing: the phone rings normally. One log line, no hook, no `call.ended`.

### 4.3 Dialing

While dialing, a STUN Binding Request is re-sent from the RTP socket every 10 s to keep the NAT mapping alive through the ring (internet targets only; section 7.2).

| Event | Action | Outcome |
| --- | --- | --- |
| 1xx | log status; any early-media RTP is dropped | — |
| `Answered` with acceptable SDP (validated in `internal/sip` before reporting, section 6.4) | go to Answering | — |
| `Failed`: SDP answer unacceptable (the wrapper ACKs and BYEs) | phone keeps ringing | `sip_media_failed` |
| `Failed`: final 3xx–6xx | phone keeps ringing | `sip_rejected` (status recorded; 3xx not followed) |
| `Failed`: DNS, connect, TLS, auth | phone keeps ringing | `sip_error` (detail recorded) |
| dial timer fires | `Abandon` | `sip_timeout` |
| `hfp.CallSetupCleared` (caller gave up, or owner rejected) | `Abandon` | `caller_abandoned` |
| `hfp.CallActive` (siptooth has not sent `ATA`: the owner answered on the handset) | `Abandon`, never touch the phone call | `owner_answered` |
| `hfp.Disconnected` | `Abandon` | `bluetooth_lost` |
| SIGTERM | `Abandon` | `shutdown` |

`Abandon` returns at once; the bridge goes to Ending without waiting for the INVITE transaction, which `internal/sip` finishes in the background (section 5.3).

### 4.4 Answering

Right after `Answered`:

1. `rtp.Start`: siptooth sends 20 ms silence packets every 20 ms from now on (PCMU `0xFF`, PCMA `0xD5` payloads), which keeps the NAT mapping open and lets the far end latch. Received audio goes into the startup buffer (section 6.3).
2. Send `ATA` asynchronously. Its result, as in allochka (`internal/call/coordinator.go` around line 853):
   - `OK` → wait for `CallActive` up to 5 s.
   - `ErrTimeout` after the command was sent → ambiguous; wait for `CallActive` up to 5 s and let the indicator decide.
   - `ERROR`, `ErrNotSent`, `ErrClosed` → definite failure: BYE, outcome `answer_failed`, the phone is left alone (still ringing, or ending).
3. `CallActive` (after `ATA` was sent) → the call is siptooth's. Accept the SCO link (the in-band ringing backlog is drained first, as in allochka; up to `maxDrainFrames` 8192, enough for a 30 s ring). No SCO within 2 s → dial SCO. No SCO within 5 s of `CallActive` → `AT+CHUP` + BYE, outcome `answer_failed`.
4. SCO up → Bridged. Emit hook `call.connected`.

Termination events during Answering:

| Event | Before `CallActive` | After `CallActive` (SCO not yet up) |
| --- | --- | --- |
| far-end BYE / transport lost | leave the phone (it rings on, `ATA` result is ignored if late); outcome `far_end_ended` | `AT+CHUP`; outcome `far_end_ended` |
| `hfp.CallSetupCleared` with `call` = 0 | ATA definitively failed or not sent: BYE; `caller_abandoned`. ATA pending/ok/ambiguous: recorded only — some phones report `callsetup=0` before `call=1` after `ATA`; a `CallActive` within the 5 s window proceeds, otherwise BYE, `caller_abandoned` (the far end may hear up to ~8 s of silence in that case). A new Ring during this wait ends the old call at once. | — (not emitted while active) |
| `hfp.CallEnded` | — | BYE; `caller_abandoned` |
| `hfp.Disconnected` | BYE; `bluetooth_lost` | BYE; `bluetooth_lost` |
| no `CallActive` within 5 s | BYE; `answer_failed`; phone left alone | — |
| SIGTERM | BYE; `shutdown` | BYE + `AT+CHUP`; `shutdown` |

If `ATA` was sent and the call then ended on the SIP side before `CallActive`, a `CallActive` that still arrives within 5 s means siptooth's `ATA` answered the phone call: siptooth sends `AT+CHUP` so the caller is not left in silence (the call is siptooth's by the ownership rule).

### 4.5 Bridged

The media pump (section 6.2) runs. The call ends on the first of:

| Trigger | Action | `hangup_by` | Outcome |
| --- | --- | --- | --- |
| `hfp.CallEnded` | BYE | `caller` | `completed` |
| far-end BYE (`Ended` = `RemoteBye`) | play out buffered downlink audio (at most 3 s), then `AT+CHUP` | `far_end` | `completed` |
| SIP transport lost (`Ended` = `TransportLost`) | `AT+CHUP` | `siptooth` | `signaling_lost` |
| no RTP received for `calls.media_timeout_seconds` (default 10) | BYE, `AT+CHUP` | `siptooth` | `media_timeout` |
| SCO EOF, or no SCO read for 5 s, and no `CallEnded` within 1.5 s | BYE only, **no `AT+CHUP`** (the owner may have moved audio to the handset; the cellular call continues there) | `siptooth` | `audio_lost` |
| `calls.max_duration_seconds` reached (default 0 = off) | BYE, `AT+CHUP` | `siptooth` | `max_duration` |
| `hfp.Disconnected` | BYE | `siptooth` | `bluetooth_lost` |
| fatal RTP socket error | BYE, `AT+CHUP` | `siptooth` | `media_error` |
| SIGTERM | BYE, `AT+CHUP` | `siptooth` | `shutdown` |

The SCO-loss grace (1.5 s) and the no-CHUP rule are allochka's (`scoLossGrace`, coordinator.go:71 and :1096).

### 4.6 Ending

Close RTP and SCO; BYE and `AT+CHUP` each run with their own 3 s bound (BYE in the background via `internal/sip`, never blocking Idle). Log the summary line, emit `call.ended`, return to Idle. A ring that arrives while the previous call's background SIP cleanup is still running starts a new call normally.

### 4.7 Caller ID handling

- `+CLIP` number, trimmed. Type 145 (international) without a leading `+` gets one. Characters other than `+0123456789*#` are removed.
- Empty after cleaning, or no `+CLIP` within the wait: withheld. `{caller}` then expands to `sip.anonymous_user` (default `anonymous`); the hook payload has `"caller": null, "caller_withheld": true`.

## 5. SIP signaling

### 5.1 Placeholders

Template strings (`sip.target`, `sip.from`, `sip.display_name`, values of `sip.headers`) may contain:

- `{caller}`: cleaned caller number (section 4.7), or `sip.anonymous_user`.
- `{number}`: the phone's own number from `phone.number` (static config; HFP `AT+CNUM` is unreliable on Android and not used).
- `{call_id}`: siptooth's call ID (for correlation, e.g. `X-CALL-ID`).

Unknown placeholders fail config validation. Expansion is component-aware: in a URI user part, characters outside RFC 3261 `user` unreserved/escaped set are percent-encoded (`#` → `%23`); in a display name the value is quoted; in header values CR, LF and NUL are rejected at validation and cannot appear after expansion.

### 5.2 The INVITE

- **Request-URI and To:** `sip.target`, a `sip:` URI (not `sips:`; use `;transport=tls`), e.g. `sip:{number}@sip.rtc.elevenlabs.io;transport=tls` or `sip:600@192.168.1.10`.
- **Transport:** `sip.transport` (`udp` | `tcp` | `tls`, default `tls`); must agree with a `;transport=` parameter in the target. Next hop: `sip.proxy` (`host:port`) when set, otherwise the target host and its explicit port, otherwise the transport default (5060 / 5061). DNS: A/AAAA via sipgo (IPv4 preferred); no NAPTR/SRV (use `sip.proxy` for SRV-only domains).
- **From:** `sip.from` (default `sip:{caller}@<target host>`) with `sip.display_name` (default `{caller}`), new tag per call.
- **Contact:** `sip:siptooth@<signaling address>;transport=<t>`, signaling address per section 7.3.
- **Allow:** `INVITE, ACK, CANCEL, BYE, OPTIONS`. No `Supported: timer`, no `100rel`.
- **Extra headers:** `sip.headers` map with expanded values. `Call-ID`, `CSeq`, `Via`, `Max-Forwards`, `Contact`, `From`, `To`, `Route`, `Record-Route`, `Allow`, `Supported`, `Require`, `Authorization`, `Proxy-Authorization`, `Content-*` cannot be set (validation; names compared case-insensitively). `X-CALL-ID` is not SIP `Call-ID` and is allowed.
- **User-Agent:** `siptooth/<version>`.
- **Body:** SDP offer (section 6.4).
- **TLS:** system roots, TLS 1.2 minimum, verification always on. The TLS `ServerName` is **not** fixed in config: sipgo derives it from the host of each connection's destination URI (`sip/transport_tls.go`), so a follow-up connection to ElevenLabs' node Contact `<ip>.hosts.rtc.elevenlabs.io` verifies against `*.hosts.rtc.elevenlabs.io`. sipgo's `DialogUA.RewriteContact` stays off.
- **Write timeout:** every TCP/TLS write is bounded by 5 s (connection write deadline set by the wrapper), so teardown is bounded even on a stuck connection.

### 5.3 INVITE transaction ownership (`internal/sip`)

sipgo v1.6.0's `DialogClientSession.WaitAnswer` does not guarantee the cleanup siptooth needs (verified in `dialog_client.go`): on context cancel it CANCELs and may return `ctx.Err()` or `"non provisional response received during CANCEL"` while a crossing `2xx` sits in `InviteResponse` un-ACKed; cancelling waits for a 1xx (up to Timer B) and then up to 64·T1 for the 487. The wrapper therefore:

- runs the whole INVITE lifecycle in its own goroutine per attempt, using a context that `Abandon` does not cancel directly;
- on `Abandon` before any 1xx: records the intent and sends CANCEL as soon as the first 1xx arrives (RFC 3261 §9.1); if a final response arrives first, it handles that instead;
- whenever a `2xx` is received for an attempt that is abandoned, already answered by another fork, or whose SDP fails validation: ACKs that dialog (per To-tag) and then BYEs it;
- after `WaitAnswer`/`inviteCancel` return for any reason, checks `InviteResponse` and applies the rule above if it is a `2xx`;
- reports exactly one `DialResult`; for an abandoned attempt the result is discarded by the bridge (generation check).

Accepted `2xx`: ACK via `Ack()` immediately; ACK retransmission on `2xx` retransmits (UDP) is handled per dialog.

### 5.4 Authentication

`sip.username` set: digest credentials from `SIPTOOTH_SIP_PASSWORD` are passed to sipgo (`AnswerOptions{Username, Password}`), which answers up to 2 challenges (`maxAuthAttempts = 2`, covering a stale nonce or a proxy plus UAS challenge); a third challenge is `sip_error` / `auth_failed`. MD5 and SHA-256 with `qop=auth` (via `icholy/digest`). Without `sip.username`, a 401/407 is `sip_error` / `auth_required`. A BYE that is challenged is re-sent once with credentials. Credentials and `Authorization` headers never appear in logs.

### 5.5 Connection lifetime and far-end requests

The same sipgo `UserAgent` hosts the client and a `sipgo.Server` (handlers `OnBye`, `OnAck`, `OnInvite`, `OnOptions`, `OnNoRoute`), so requests arriving on a connection siptooth opened are dispatched. Siptooth never listens for new inbound TCP/TLS connections.

- **TCP/TLS:** the connection used for the INVITE stays open until Ending, with an RFC 5626 CRLF keepalive (`\r\n\r\n`) every 25 s to hold the NAT mapping. Siptooth's own BYE/ACK go to the dialog's remote target (the `2xx` Contact, honoring Record-Route), opening a new connection if needed (sipgo default; ElevenLabs requires the node Contact, not the shared host, or the BYE gets 481).
- **UDP:** sipgo `WithClientNAT()` (adds `rport`), local SIP port `sip.local_port` (default 5060 for UDP, ephemeral for TCP/TLS), CRLF keepalive every 25 s during a dialog.
- **Far-end requests are only reachable if the far end reuses siptooth's connection or flow.** Siptooth helps by advertising the address the far end actually sees (section 7.3). With `sip.register: true` (Zadarma) the registered Contact is the flow siptooth keeps open, and a registrar-aware far end sends in-dialog requests through it (section 5.7). For a self-hosted PBX the providers recipe configures connection reuse (`rewrite_contact=yes`, `force_rport=yes`, `rtp_symmetric=yes`, `direct_media=no` on an Asterisk PJSIP endpoint). For ElevenLabs, whether its BYE reuses the open connection is a qualification item of milestone M1 (`siptooth sip test --wait-remote-hangup`, section 11). If it does not, an agent-side hang-up is detected by RTP loss: ElevenLabs (LiveKit) sends a packet every 20 ms including silence, so `calls.media_timeout_seconds` (default 10) bounds the delay.

### 5.6 In-dialog requests from the far end

- `BYE` → `200 OK` (via `DialogClientCache.ReadBye`), ends the call.
- `ACK` → absorbed (no response).
- `OPTIONS` → `200 OK` with `Allow`.
- `re-INVITE` (tracked against the dialog: remote CSeq must increase, otherwise `500`):
  - with SDP whose audio stream offers at least one of our payload types, `sendrecv`, and the same profile and crypto as established: `200 OK` with our SDP (same codecs, o= version incremented). A changed connection address or port is applied through `rtp.Update` after the ACK. A different codec choice than the one in use is not applied; our answer lists only the codec in use, which must be in the re-offer.
  - offerless re-INVITE: `200 OK` carrying our current SDP as an offer; the SDP in the ACK is validated as above and an address change is applied.
  - anything else (`sendonly`/`recvonly`/`inactive` hold, codec set without ours, profile or key change): `488 Not Acceptable Here`; existing media continues.
  - on every transport the wrapper retransmits the `2xx` (T1 doubling to T2) until the ACK arrives or 32 s pass (then BYE, `signaling_lost`), per RFC 3261 §13.3.1.4 (a proxy may have a UDP leg).
- `UPDATE`, `INFO`, `NOTIFY`, `REFER`, `MESSAGE`, `PRACK` → `501 Not Implemented`.
- Requests that match no dialog (including new INVITEs) → `481` (in-dialog) or `403` (dialog-creating). Siptooth never answers a new incoming SIP call.

### 5.7 Registration (optional)

`sip.register: true` (needed for Zadarma extensions, whose docs only describe registered extensions or a static-IP trunk; harmless elsewhere):

- At daemon start, and again after any failure, siptooth sends `REGISTER` to the registrar (`sip.registrar`, default the target host) on the same transport and next hop as the INVITEs, with digest credentials, `Expires: sip.register_expires_seconds` (default 120). It refreshes at half the granted expiry (Zadarma's examples: 120 s expiry, 60 s refresh). Failures are retried every 30 s with one rate-limited `system.error` `sip_registration` per outage.
- AOR and From user: `sip.username`'s user part (e.g. `1234-100`) unless `sip.from` overrides; the Contact is the flow address learned from the REGISTER response's top `Via` `received`/`rport` (this replaces the pre-INVITE OPTIONS of section 7.3 while registered), re-registered when it changes.
- Over TCP/TLS the registration and every INVITE share one long-lived connection with the CRLF keepalive; over UDP the same local port, so the NAT binding stays open between calls and the far end's in-dialog requests can reach siptooth.
- Calls are attempted whether or not registration currently succeeds (the INVITE carries its own credentials); `siptooth status` shows the registration state.
- Incoming INVITEs that arrive on the registered Contact (someone calling the extension) are answered `603 Decline`; siptooth never answers SIP calls. Incoming `OPTIONS` (registrar qualify) → `200 OK`.
- Shutdown: `REGISTER` with `Expires: 0`, bounded to 2 s.

## 6. Media

### 6.1 RTP socket and latching

- One UDP socket per call, bound to the local address chosen in section 7.1, on an even port picked randomly in `nat.rtp_port_min`–`nat.rtp_port_max` (default 10000–20000; retry on `EADDRINUSE`). RTCP is neither sent nor received.
- Outgoing packets: version 2, random SSRC and initial sequence number, timestamp advances 160 per packet (startup ticker, pump and stall fallback share one counter, so sequence and timestamp stay monotonic), marker on the first packet, 160 payload bytes.
- **Early media:** packets that arrive before `Start` (183 early media, STUN noise) are dropped and not counted.
- **Latching:** after `Start`, packets are sent to the SDP address. The first packet from any source that passes validation (version 2, a negotiated payload type, and SRTP authentication when enabled) latches the remote address; from then on siptooth sends to the latched source and accepts packets only from it. A re-INVITE address change resets the latch to the new SDP address.
- **Payload types:** every G.711 type present in both offer and answer is decoded on receive (PCMU 0, PCMA 8), whatever the peer switches to. The transmit codec is the first type in the answer's list that we offered. Other types (comfort noise 13, telephone-event) are counted and dropped.
- **SSRC change:** resets playout sequence/timestamp tracking (the new stream continues at the current playout point).
- STUN responses (first byte `0x00`/`0x01`) are routed to the STUN code before `Start`, ignored after.

### 6.2 Media pump (SCO ↔ RTP)

Pacing is read-driven, as in allochka: the SCO controller is the clock.

- **Uplink (phone → SIP):** each SCO read (MTU bytes, typically 60–64 bytes = 30–32 samples) is appended to an accumulator; every 160 samples one frame is G.711-encoded and sent.
- **Downlink (SIP → phone):** after each SCO read the pump writes the same number of bytes taken from the playout (section 6.3); silence on underrun.
- **Stall fallback (allochka `bridge.go`):** no SCO read for 100 ms → a ticker writes playout audio (silence only when empty) to SCO at the frame cadence, and the uplink sends RTP silence every 20 ms. No SCO read for 5 s → `audio_lost` path (section 4.5).
- **Settle delay:** for `calls.sco_settle_ms` (default 800, allochka's hardware-derived `greeting_delay_ms`) after SCO is up, the pump writes silence to SCO and keeps buffering downlink audio, so the first word is not lost to a settling link.
- **Before SCO is up** (Answering): the 20 ms ticker sends RTP silence; downlink audio accumulates in the startup buffer.

### 6.3 Playout: startup buffer, jitter buffer, catch-up

- **Timestamp-based:** playout position is derived from RTP timestamps (32-bit wrap handled) and payload sample counts, not sequence numbers alone, so variable packet sizes (10–60 ms) and silence-suppression gaps play correctly. Duplicates are dropped; late packets (before the playout point) dropped and counted; gaps play as silence.
- **Startup buffer:** from `Start` until SCO is up plus the settle delay, every received sample is kept, with no cap below the worst case answering time (`ATA` 3 s + `CallActive` 5 s + SCO 5 s + settle; the buffer holds 20 s, 320 KB). Nothing in it is dropped.
- **Steady state:** target depth `media.jitter_ms` (default 60). Hard cap 20 s (oldest dropped and counted as `overflow` only beyond that; not reachable in a normal call).
- **Catch-up:** whenever depth exceeds target + 40 ms, 20 ms blocks whose RMS is below `media.silence_rms` (default 200 on the 16-bit scale) are skipped instead of played, until the depth is back at target. Speech is never skipped, and every pause keeps its first `media.min_pause_ms` (default 200; 0..2000) before any of it is skipped, so the gaps between words and sentences play at their natural length and the extra latency drains in the first long pause (typically while the agent waits for the caller after its greeting). `silence_rms: 0` disables catch-up.

### 6.4 SDP

**Offer** (one audio stream):

```
v=0
o=siptooth <sess-id> <version> IN IP4 <media address>
s=siptooth
c=IN IP4 <media address>
t=0 0
m=audio <media port> RTP/AVP 0 8          (RTP/SAVP when sip.srtp is true; order from media.codecs)
a=rtpmap:0 PCMU/8000
a=rtpmap:8 PCMA/8000
a=ptime:20
a=sendrecv
a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:<base64 30 bytes>    (only with srtp)
```

**Answer validation** (in `internal/sip`, before `Answered` is reported): the first `m=audio` line is the answer to our stream; port non-zero; profile exactly `RTP/AVP` (or `RTP/SAVP` with SRTP); direction `sendrecv` (or absent, which means `sendrecv`); at least one payload type we offered; IPv4 connection address from the media-level or session-level `c=`. With SRTP, exactly one `a=crypto` with tag `1`, suite `AES_CM_128_HMAC_SHA1_80`, 30-byte key‖salt after base64; a lifetime parameter is accepted and ignored, an MKI or session parameters are rejected. Any failure → `sip_media_failed`.

### 6.5 SRTP (SDES)

`sip.srtp: true` is allowed only with `sip.transport: tls`. Per call siptooth generates a fresh 30-byte master key‖salt from `crypto/rand`. The send context uses our offered key, the receive context the key from the answer (`srtp.CreateContext(key16, salt14, ProtectionProfileAes128CmHmacSha1_80, srtp.SRTPReplayProtection(64))`). Authentication or replay failures are counted and dropped. Key material is redacted (`inline:<redacted>`) in every log line, error and CLI output that includes SDP.

### 6.6 Echo gate

(2026-10-14, hardware test.) The phone call returns part of the far end's own voice on the line (allochka measured it at up to +2.4 dB over the played audio; a caller on a laptop adds acoustic echo), and siptooth, the hands-free side, has no echo canceller. An ElevenLabs agent took its own echo for the caller and interrupted itself. `internal/echo.Gate`, copied from allochka's `audio.EchoGate` (allochka spec section 4, 2026-10-05 iterations 4 and 5), sits in the media pump with `media.echo_suppression: true` (default):

- Reference: every block the pump writes to SCO (playout audio, silence included), as RMS per 20 ms block; the expected echo level is the loudest reference block of the last `media.echo_window_ms` (default 600, 100..2000).
- Each 160-sample uplink frame, before encoding: while the window holds far-end audio (reference RMS at least 50), a frame passes only when its RMS is at least 50 and above the reference level × 10^(`media.echo_threshold_db`/20) (default 6, -30..20); otherwise it is sent as silence. Two frames above open the gate, 15 frames (300 ms) below close it, so double-talk is not chopped. Without far-end audio in the window every frame passes.
- At call end an `echo gate` log line gives the gated/passed counts, caller-minus-played dB (median, p90, max) over gated frames with sound, and the lag (0..1 s, 20 ms steps) and Pearson correlation at which the caller level envelope best follows the played envelope over the first 3 minutes, to locate the echo source.

A gate is half-duplex while the agent talks: a caller must be `echo_threshold_db` louder than the played audio to interrupt. An adaptive canceller would allow full duplex but needs a gate as its last stage anyway on a cellular line; it is left until barge-in proves to matter.

## 7. NAT and addressing

### 7.1 Local address

The local address is the source address the kernel picks for the next hop (a connected-UDP-socket probe to the resolved SIP destination, no packet sent), never `0.0.0.0`. The RTP socket binds to it. `nat.rtp_bind` overrides.

### 7.2 Media address (`nat.mode`)

- `auto` (default): if the resolved SIP next hop is a private, loopback, link-local or CGNAT-shared (100.64/10) address, use `local`; otherwise `stun`.
- `local`: advertise the local address and port. For a PBX on the LAN (no hairpin through the router needed).
- `stun`: send a Binding Request from the RTP socket to each of `nat.stun_servers` in order (default `stun.l.google.com:19302`, `stun.cloudflare.com:3478`), 500 ms timeout, 2 attempts per server, last successful server tried first; advertise the XOR-MAPPED-ADDRESS. While Dialing, re-send a Binding Request every 10 s to the same server (keepalive; a changed mapping is logged as a warning). All servers failing: warning, rate-limited `system.error` `stun_failed` (once per hour), advertise `nat.public_ip` if set, else the local address.
- `public_ip`: advertise `nat.public_ip` with the local port (for a router port-forward of the RTP range), no STUN.

The mapping describes traffic to the STUN server; with an address-dependent NAT it may differ toward the trunk. Siptooth relies on the far end latching onto its source (LiveKit and Asterisk with `rtp_symmetric=yes` both do), which also covers that case. CGNAT without latching on the far end is unsupported.

### 7.3 Signaling address (Contact)

- `local` mode: local address and the local SIP port.
- registered (section 5.7): the flow address from the last REGISTER response.
- otherwise: before each INVITE over TCP/TLS (or UDP), siptooth sends an `OPTIONS` to the next hop on the connection the INVITE will use, and reads `received`/`rport` from the top `Via` of the response (any status code). That address — the far end's view of siptooth's flow — becomes the Contact host and port, which makes a far end that reuses connections by remote address find the open connection. No usable `received`/`rport`: the media address IP with the local SIP port. The OPTIONS costs one round trip (≤ 1 s budget; timeout → fallback).

### 7.4 Diagnostics

`siptooth doctor` reports: local address toward the target, chosen mode, the STUN mapping from one socket toward two different STUN servers (same mapped port → "endpoint-independent mapping observed", different → "address-dependent mapping: needs a latching far end"), and the OPTIONS `received`/`rport` toward the target. It reports observations, not a NAT type.

## 8. Bluetooth layer (copied)

Copied from allochka with these changes only:

- Names: adapter alias `siptooth`, D-Bus object paths `/com/siptooth/hfp` and `/com/siptooth/agent`, profile name `Siptooth`.
- `calls.answer_delay_seconds` removed; nothing calls `Answer` on ring any more: `bridge` does after the SIP `2xx`.
- Remote volume control (2026-10-14, hardware test): `AT+BRSF=52` adds bit 4 to allochka's 36, the SLC ends with `AT+VGS=15` and `AT+VGM=15` (a refusal only logs), and every `+VGS` below 15 from the phone is answered with `AT+VGS=15`. Android scales the call audio it sends over SCO by this speaker gain, and at the phone's own volume the caller reached ElevenLabs at about -24 dB below the agent and sounded distorted once ElevenLabs raised it.
- Probe mode (`internal/probe`) removed; `siptooth sip test` (section 11) covers the SIP side, and the Bluetooth side is already qualified by allochka on this hardware.
- Everything else unchanged, including the hard-won fixes: ring dedup and call-waiting latch, `CallActive` while ringing means the owner answered, `CallSetupCleared` only with `call` = 0, SLC retries, profile re-registration, routing retries, bus reconnect, idle-only power cycle (`bluetooth.power_cycle_minutes`, default 60), in-band ringing backlog drain, read-driven SCO pacing with the stall fallback, 3 s AT deadlines, 2 s SCO dial fallback.

Allochka and siptooth both register the HFP hands-free profile and cannot run at the same time (`doctor` reports a running allochka service as a conflict). The existing BlueZ bond with the phone is reused: `bluetooth.phone_address` is copied from allochka's config, no re-pairing needed.

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
  target: "sip:{number}@sip.rtc.elevenlabs.io;transport=tls"   # required, sip: URI
  transport: tls               # udp | tcp | tls
  proxy: ""                    # optional host:port next hop
  local_port: 0                # 0: 5060 for udp, ephemeral for tcp/tls
  from: ""                     # default "sip:{caller}@<target host>"
  display_name: "{caller}"
  anonymous_user: anonymous    # {caller} when the number is withheld
  username: ""                 # digest username; password: SIPTOOTH_SIP_PASSWORD in ~/.siptooth/env
  register: false              # REGISTER and keep the flow open (Zadarma); requires username
  registrar: ""                # default: target host (with sip.proxy as next hop)
  register_expires_seconds: 120   # 60..3600
  headers: {}                  # extra INVITE headers, values may use placeholders
                               # ElevenLabs: {X-CALLER-ID: "{caller}", X-CALL-ID: "{call_id}"}
  srtp: false                  # SDES SRTP; requires transport tls
  dial_timeout_seconds: 30     # 5..120

media:
  codecs: [PCMU, PCMA]         # offer order; at least one
  jitter_ms: 60                # 20..500
  silence_rms: 200             # catch-up threshold, 0..5000 (0 disables catch-up)
  min_pause_ms: 200            # catch-up keeps this much of every pause, 0..2000
  echo_suppression: true       # gate the line echo of the far end's own voice (spec 6.6)
  echo_threshold_db: 6         # caller audio must be this much louder than the played audio, -30..20
  echo_window_ms: 600          # how long after played audio its echo is expected, 100..2000

nat:
  mode: auto                   # auto | local | stun | public_ip
  stun_servers: ["stun.l.google.com:19302", "stun.cloudflare.com:3478"]
  public_ip: ""                # required for mode public_ip; fallback for stun
  rtp_bind: ""                 # default: local address toward the next hop
  rtp_port_min: 10000
  rtp_port_max: 20000

calls:
  caller_id_wait_ms: 2500      # 0..3000; the Motorola sends +CLIP ~1.05 s after callsetup
  sco_settle_ms: 800           # 0..5000
  media_timeout_seconds: 10    # 5..300
  max_duration_seconds: 0      # 0 = no limit, else 60..86400

control:
  socket: siptooth.sock

sms:
  enabled: false               # SMS to sms.received hooks over Bluetooth MAP (section 17)

hooks: []
#   - id: notify
#     events: [call.ended]
#     command: "/home/pi/.siptooth/hooks/notify.sh"
#     timeout_seconds: 10
```

Validation (startup fails on any error; `siptooth config check` prints all of them): required fields; MAC format; known placeholders; `{number}` ⇒ `phone.number`; target parses as a `sip:` URI after a trial expansion; transport agrees with `;transport=`; `srtp` ⇒ `tls`; `mode: public_ip` ⇒ `public_ip`; `public_ip` is an IPv4 address; RTP range even-aligned, min < max, within 1024–65534; `username` ⇒ `SIPTOOTH_SIP_PASSWORD` present; `register` ⇒ `username`; forbidden header names and CR/LF/NUL in header names or values; hook events known; numeric ranges as noted. Unknown keys are errors (`yaml.v3` `KnownFields`).

## 10. Hooks

Allochka's dispatcher and runner: one worker goroutine, bounded queue of 100 events, hooks run sequentially in emission order (so per-call order is preserved); `/bin/sh -c COMMAND`, JSON payload on stdin, `timeout_seconds` (default 10, max 300) then SIGTERM/SIGKILL of the process group, stdout/stderr captured up to 8 KiB and logged; one attempt, no retries. A full queue drops the event with a warning (and a rate-limited `system.error` `hook_queue_full` log line; not dispatched as a hook). Environment: the daemon's environment minus `SIPTOOTH_SIP_PASSWORD`, plus `SIPTOOTH_EVENT`, `SIPTOOTH_CALL_ID`, `SIPTOOTH_CALLER` (empty when withheld). `Emit` never blocks the call.

**Events and payloads** (`schema_version: 1`):

```json
{ "schema_version": 1, "event": "call.ringing", "occurred_at": "2026-10-14T09:43:31Z", "test": false,
  "call": { "id": "20261014T094331Z-b5fa", "caller": "+380501234567", "caller_withheld": false,
            "number": "+380441234567", "target": "sip:+380441234567@sip.rtc.elevenlabs.io;transport=tls" } }
```

- `call.ringing`: before the INVITE (fields above). For side processes that prepare for the call (for example a future OpenAI-accept service correlating by `X-CALL-ID`).
- `call.connected`: audio bridged; adds `sip_call_id`, `codec`, `srtp`, `remote_media` (latched ip:port, or SDP address before latching), `advertised_media` (ip:port), `nat_mode`, `connected_at`.
- `call.ended`: exactly once per call; adds `outcome` (`completed`, `sip_rejected`, `sip_timeout`, `sip_error`, `sip_media_failed`, `caller_abandoned`, `owner_answered`, `answer_failed`, `far_end_ended`, `signaling_lost`, `media_timeout`, `audio_lost`, `media_error`, `max_duration`, `bluetooth_lost`, `shutdown`), `hangup_by` (`caller`, `far_end`, `siptooth`, or null), `sip_status` and `sip_reason` (last final response to the INVITE, null if none), `error` (string or null), `ringing_seconds`, `bridged_seconds` (null if never bridged), `media` (`{"sent","received","lost","late","duplicate","overflow","discarded_silence","srtp_fail"}`).
- `system.error`: `{"error":{"code","detail"}}`, codes `bluetooth_unavailable`, `profile_registration`, `routing`, `stun_failed`, `sip_registration`, `sms_unavailable` (section 17.6).
- `sms.received`: an incoming SMS (section 17.8): `{"sms":{"id","from","from_name","text","type","handle"}}`. Same once-per-outage rules as allochka.

The `target` in payloads is the expanded URI; credentials and SRTP keys never appear in payloads or logs.

`siptooth hooks test --event <name>` runs matching hooks with a sample payload (`"test": true`) without the daemon.

## 11. CLI and control socket

| Command | Purpose |
| --- | --- |
| `siptooth run` | the daemon (systemd runs this) |
| `siptooth pair` | interactive pairing (copied; daemon must be stopped) |
| `siptooth doctor [--fix]` | setup checks: config valid, env file mode, group `bluetooth`, BlueZ, adapter, conflicting HFP handlers (PipeWire, bluealsa, a running allochka service), linger, DNS of the next hop, TCP/TLS connect and certificate check, NAT observations (section 7.4), SMS checks when enabled (section 17.9). `--fix` creates `~/.siptooth/`, default config, env file. Never runs sudo. |
| `siptooth install` | writes and enables the systemd user unit (copied) |
| `siptooth status` | daemon state via control socket: Bluetooth ready, phone connected, SIP registration (when enabled: state, Contact, expiry), current call (state, caller, duration), last call outcome, SMS service state (section 17.9) |
| `siptooth config check` | validate and print the effective config (password redacted) |
| `siptooth hooks test --event E` | run hooks with a sample payload |
| `siptooth sip test [--caller N] [--seconds 20] [--tone] [--wait-remote-hangup]` | a real SIP call to the target without Bluetooth (registers first when `sip.register` is on, unregisters at the end): prints the NAT decision and mapping, the OPTIONS `received`/`rport`, every SIP response, the negotiated codec and SRTP state; then for `--seconds` sends silence (or a 1 kHz tone at −20 dBFS with `--tone`) and reports packets received, loss, max inter-packet gap, latched source, received audio level. With `--wait-remote-hangup` it does not hang up and instead reports whether the far end's BYE arrived (and on which connection), or whether only RTP stopped. Exit 0 only when answered and audio packets were received. Runs while the daemon runs (separate SIP and RTP ports). |
| `siptooth version` | build info |

Control socket: HTTP over `~/.siptooth/siptooth.sock`, `GET /status` (JSON) only.

## 12. Logging

`log/slog` text handler to stderr (journal). Level from `SIPTOOTH_LOG_LEVEL` (`info` default). `debug` logs each SIP message's start line and SDP bodies with `a=crypto` keys and `Authorization`/`Proxy-Authorization` values redacted. Every call-scoped line carries `call_id`. One summary line per call at `info`: `msg="call ended" call_id=… caller=… outcome=… hangup_by=… sip_status=… ringing_s=… bridged_s=… codec=… nat_mode=… lost=… late=… discarded_silence=…`.

## 13. Error handling summary

| Failure | Behaviour |
| --- | --- |
| SIP target unreachable, rejects, times out, auth fails | phone keeps ringing; `call.ended` with the reason |
| SDP answer unusable | ACK + BYE by the wrapper, phone keeps ringing |
| `2xx` after CANCEL, or a second fork's `2xx` | ACK + BYE that dialog |
| `ATA` explicit error | BYE at once; phone left alone |
| `ATA` timeout, no `CallActive` in 5 s | BYE; phone left alone |
| No SCO after `CallActive` | `AT+CHUP` + BYE |
| SCO lost mid-call | BYE only; the owner's cellular call is left alone |
| STUN fails | fallback address, warning, rate-limited `system.error` |
| Far end stops sending RTP | `media_timeout` after 10 s, hang up both |
| Far end BYE unreachable through NAT | covered by `media_timeout` |
| Bluetooth link lost mid-call | BYE, `bluetooth_lost` |
| Hook fails / times out / queue full | logged, never affects calls |
| Panic in a call goroutine | recovered, both sides hung up per the ownership rule, `call.ended` with `error`, daemon keeps running |
| MAP (SMS) connection fails or drops | retried with backoff while the phone is connected; `sms_unavailable` after 5 min; calls unaffected (section 17.6) |
| Invalid config | startup fails listing all errors with exit code 2; the unit's `RestartPreventExitStatus=2` stops restart loops (also used when another daemon already owns the control socket) |

## 14. Testing

TDD throughout; tests never touch real Bluetooth, real networks beyond loopback, or real system services.

- **Copied packages** keep their tests (hfp AT traces, bluez helpers, sco framing incl. a 30 s-ring drain case, daemon, doctor, hooks dispatcher and runner).
- **g711:** μ-law tests copied; A-law against ITU G.711 vectors; round-trip error bounds; silence bytes.
- **sdp:** offer golden files (with and without SRTP); answer validation table (codec choice, both codecs kept for receive, session vs media `c=`, port 0, direction attributes, profile mismatch both ways, crypto tag/suite/length/MKI/lifetime, IPv6 rejection, malformed input); re-offer classification (refresh, address change, hold, codec change, key change); redaction.
- **stun:** encode/decode against RFC 5769 test vectors; loopback server; timeout, ordering, last-good-first, keepalive cadence; demux with RTP on one socket.
- **rtp:** packetization, shared seq/timestamp counter across startup/pump/stall, early-media drop, latching (first valid source, rejection of others after latch, reset on update), mixed PCMU/PCMA receive, timestamp-based playout with 10/20/30/60 ms packets, wrap of seq and timestamp, duplicates, gaps, startup buffer keeps everything for 13 s, catch-up skips only silence, SRTP round trip with replay rejection.
- **sip:** a real sipgo UAS on loopback (UDP, TCP, and TLS with a test CA) acting as the trunk: 200; 486; 503; 401 and 407 challenges; stale-nonce second challenge; three challenges → failure; no answer → CANCEL after 1xx; Abandon before any 1xx; 200 crossing CANCEL → ACK + BYE; two forked 200s → second ACKed + BYEd; unacceptable SDP → ACK + BYE; far-end BYE on the same connection; BYE challenged; re-INVITE refresh, address change, hold → 488, offerless; OPTIONS; unsolicited INVITE → 403; Contact from OPTIONS `received`/`rport`; REGISTER with challenge, refresh at half expiry, Contact update on a changed `received`/`rport`, retry after failure, unregister on shutdown, INVITE on the registered Contact → 603; CRLF keepalive; header expansion and escaping; forbidden headers; TLS ServerName per destination (two hostnames with distinct certs).
- **bridge:** state machine with fake HFP client, fake SIP dialer, in-memory SCO pair, fake clock: every row of the tables in sections 4.3–4.5, `ATA` error classes, owner answering during Dialing and during Answering, generation discards late results, exactly one `call.ended` per call, no `AT+CHUP` for unowned calls or on SCO loss, settle delay, drain before CHUP on far-end BYE.
- **End-to-end (loopback):** fake AG + in-memory SCO + sipgo UAS that echoes RTP: a tone written on the SCO side comes back on the SCO side; hang-up from either end.
- **CI:** `make check` = gofmt, `go vet ./...`, `go test ./...`, `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...`; `make race` runs `go test -race ./...` (cgo enabled for that target only); a Go 1.24 toolchain vet/test is part of the release gate.

## 15. Rollout

1. **M1 — SIP from the Pi without Bluetooth.** `siptooth sip test --tone --wait-remote-hangup` against Zadarma (a test extension or the echo test number, first without then with `register`, over UDP and TLS) and against ElevenLabs (end the conversation from the agent side). Confirms DNS, TLS, auth, whether Zadarma needs REGISTER, NAT mode, latching, codec, SRTP, caller-ID presentation at the destination, and whether the far end's BYE reaches siptooth. Record the results in `docs/providers.md`.
2. **M2 — real call through Zadarma.** Stop allochka (`systemctl --user disable --now allochka`), install siptooth with allochka's `phone_address`, call the phone; verify criteria 1 and 3.
3. **M3 — ElevenLabs direct.** Switch `sip.target` to the agent number; verify criterion 2 (full greeting heard, caller number received, hang-up propagation).
4. **M4 — soak.** A week of normal use (criterion 4). Then allochka is archived.
5. **M5 — SMS (revision 5).** Enable `sms.enabled`, restart, allow message access on the phone, send a text from another phone; verify `sms.received` reaches a hook with sender and text, the message stays unread on the phone, and calls still work. Then a power cycle and a reconnect.

Each hardware step is run by the owner, one command at a time.

### 15.1 Provider recipes (content of `docs/providers.md`)

**ElevenLabs.** Import the phone number as a SIP trunk number with exactly the string siptooth puts in the Request-URI user part (including the leading `+`); assign the agent; inbound transport TLS (TCP acceptable, UDP is experimental on their side); inbound authentication: digest username/password (their IP allowlist does not suit a dynamic home IP); media encryption `Disabled` with `srtp: false`, or `Allowed`/`Required` with `srtp: true`. Config: `target: "sip:{number}@sip.rtc.elevenlabs.io;transport=tls"`, `headers: {X-CALLER-ID: "{caller}", X-CALL-ID: "{call_id}"}`.

**Zadarma (cloud PBX).** Create a PBX extension for siptooth (login `<account>-<ext>`, e.g. `1234-100`, password from the PBX settings) and route what it dials in the PBX: another extension, a voice menu, voicemail, or the extension's "Forward to: External server (SIP URI)" (e.g. an ElevenLabs agent number; REFER-based transfer is not supported by Zadarma, forwarding is). Config:

```yaml
sip:
  target: "sip:200@sip.zadarma.com"        # what the siptooth extension dials; transport tls → port 5061
  transport: tls                           # udp also documented by Zadarma; TLS preferred
  username: "1234-100"
  register: true
  from: "sip:1234-100@sip.zadarma.com"     # Zadarma identifies the extension by its login
  display_name: "{caller}"                 # original caller's number, shown where Zadarma passes it through
  headers: {X-CALLER-ID: "{caller}", X-CALL-ID: "{call_id}"}
  srtp: false                              # SRTP is not documented by Zadarma; test in M1 before enabling
media: {codecs: [PCMA, PCMU]}              # Zadarma documents alaw/ulaw only
```

Known from Zadarma's docs: `sip.zadarma.com`, UDP 5060 and TLS 5061, PCMA/PCMU, `P-Asserted-Identity` only for numbers attached to the account (so it cannot carry an arbitrary caller's number). Not documented and verified in M1: INVITE without REGISTER, TCP, SRTP, TLS certificate validity (Zadarma's own Asterisk guide disables verification; siptooth never does — if the certificate fails, use UDP), display-name presentation on internal calls and on forwarding, whether custom `X-` headers survive a forward to an external SIP URI, NAT latching, and in-dialog request routing to a registered flow.

**Self-hosted PBX (best effort, e.g. Asterisk/PJSIP).** An endpoint with `identify_by=auth_username` (or `ip`) so a From user carrying the caller number still matches; `rewrite_contact=yes`, `force_rport=yes`, `rtp_symmetric=yes`, `direct_media=no`; `allow=ulaw,alaw`. On the LAN, `nat.mode: auto` resolves to `local`.

## 16. Risks and open questions

- **ElevenLabs far-end BYE.** If M1 shows ElevenLabs opens a new connection to the Contact or top Via for its BYE, the hang-up arrives only through `media_timeout` (10 s of silence for the caller). Mitigations if that is unacceptable: a router port-forward plus a TLS listener (out of v1 scope), or a shorter media timeout once the gap statistics from `sip test` are known.
- **Caller ID through Zadarma.** If Zadarma drops the display name and `X-` headers, the original caller's number is available only to siptooth's hooks (and on the ElevenLabs-direct path). A fallback, if needed, is a per-caller target such as `sip:{caller}@...` routed by a Zadarma scenario; decided after M1.
- **Line echo.** The phone network returns an echo of the far end's own voice (allochka needed an echo gate against OpenAI). ElevenLabs and PBX endpoints have their own echo handling, so siptooth ships without one; if M3 shows the agent interrupting itself, allochka's echo gate can be ported as an option.
- **NAT variants.** CGNAT or an address-dependent NAT toward a far end that does not latch breaks media; the doctor observations and `sip test` make this visible early.
- **No RTCP.** Some media servers time out calls without RTCP; neither v1 target does. RTCP sender reports later are contained in `rtp`.
- **Media timeout vs silence suppression.** A far end that stops RTP during silence would trip the 10 s timeout. Neither v1 target does (LiveKit sends every 20 ms; Asterisk without VAD likewise); other trunks may need a larger value.
- **Narrowband only.** CVSD 8 kHz is the audio ceiling; mSBC/G.722 is a later iteration.
- **MAP on Android / BlueZ (revision 5).** Unverified on hardware: that BlueZ's client-role external profile connects the phone's MAS (RFCOMM or L2CAP), that the phone shows the message-access prompt and then opens the MNS channel, and whether the MAS instance BlueZ picks is the SMS one when a phone has several. RCS chats and MMS are not forwarded by design. If BlueZ cannot connect the MAS through `ConnectProfile`, a fallback is siptooth's own SDP lookup and socket (larger change).
- **OpenAI path (later).** OpenAI's SIP endpoint needs TLS + SRTP (supported here) plus a public webhook and an `accept` call per call; a separate service that siptooth's `call.ringing` hook (with `X-CALL-ID` correlation) can feed.

## 17. SMS forwarding (Bluetooth MAP)

Added 2026-10-14 (revision 5, owner request). The owner wants text messages that reach the phone to reach siptooth's hooks as well, the same way calls do, with no third-party integration: siptooth only hands the sender and the text to `sms.received` hooks; what a hook does with them is up to the owner.

### 17.1 Scope

**In scope:** incoming SMS (`SMS_GSM`, `SMS_CDMA`) that land in the phone's inbox while siptooth's MAP session is up; sender number, sender name when the phone has one (contact name), full text (concatenated SMS are joined by the phone); `sms.received` hook event; status and doctor reporting.

**Out of scope (v1):** sending SMS; MMS (including Android group messages, which are MMS) and RCS chats (Google Messages "chat features" never appear in MAP); email and IM MAS instances; catching up on messages that arrived while the MAP session was down (no inbox listing at connect, so a reconnect never replays old messages); persistence across daemon restarts; any network integration.

Delivery is **at most once**: a message is delivered to the hooks once, and a message that arrives while the link is down is not delivered.

### 17.2 Why MAP

HFP carries call control only; Android's Audio Gateway does not implement the SMS AT commands (`AT+CNMI`, `AT+CMGR`). Android exposes messages over the Bluetooth **Message Access Profile** (MAP 1.4): the phone runs a Message Access Server (MSE, SDP UUID `0x1132`; the owner's phone advertises it), and a car kit-style client (MCE) connects to it over OBEX, turns on notifications, and receives `NewMessage` event reports on a **Message Notification Server** (MNS, UUID `0x1133`) that the phone connects back to. siptooth implements the MCE side in pure Go; BlueZ only provides SDP records and the sockets through `Profile1` (as it does for HFP). BlueZ's `obexd` is not used (not installed on the Pi, needs the session bus and a package install).

### 17.3 Components

```
internal/obex/   # OBEX 1.5 subset: packet + header codec, packet reader over stream or SEQPACKET,
                 # client (Connect, Get with continuation, Put, Disconnect) and a server loop
internal/sms/    # MAP over obex: MAS client ops, MNS server, event-report and bMessage parsers,
                 # Service (lifecycle, retries, dedup, fetch, status), Message type
internal/bluez/  # + MAP profile registration (MAS client, MNS server), ConnectMAS/DisconnectMAS
internal/daemon/ # + wires sms.Service to the Bluetooth session and hooks; status
internal/hooks/  # + sms.received payload and env
```

`obex` and `sms` are platform independent and fully testable over `net.Pipe`/in-memory connections; only the BlueZ glue is Linux-only.

**Transport.** BlueZ hands each MAP connection to siptooth as a socket fd through `Profile1.NewConnection`, either RFCOMM (stream) or L2CAP ERTM (SEQPACKET, GOEP 2.0) depending on the SDP records. The OBEX reader accumulates reads into a buffer and cuts packets by their 16-bit length field, so the same code serves both: a SEQPACKET read with a 64 KiB buffer returns one whole SDU, a stream read returns arbitrary chunks, and bytes beyond one packet are kept for the next. Writes are one `Write` per packet (siptooth's requests and responses are small, under 1 KiB). The fd is set non-blocking and wrapped in `os.File`, so read/write deadlines work (as for HFP).

### 17.4 OBEX subset (`internal/obex`)

- Packets: opcode (1 byte), length (2 bytes, big endian, whole packet), Connect/its response add version `0x10`, flags `0x00`, max packet length (2 bytes); SetPath is not used.
- Opcodes: Connect `0x80`, Disconnect `0x81`, Put `0x02`/final `0x82`, Get final `0x83`, Abort `0xFF`. Responses: Continue `0x90`, Success `0xA0`, and the error classes `0xC0`–`0xDF` reported as a typed error carrying the code.
- Headers by the two high bits of the ID: Unicode (UTF-16BE, null terminated, 2-byte length), byte sequence (2-byte length), 1-byte, 4-byte. Used: Name `0x01`, Type `0x42` (ASCII, null terminated), Target `0x46`, Who `0x4A`, Body `0x48`, End-of-Body `0x49`, App-Parameters `0x4C` (TLV: tag 1 byte, length 1 byte, value), Connection-ID `0xCB`. Unknown headers are skipped by their encoded length; a header that runs past the packet is a protocol error.
- siptooth advertises a maximum packet length of 8192 and never sends a packet larger than the peer's advertised maximum; SRM (single response mode) is not offered, so every exchange is strictly request/response.
- Client: `Connect(target UUID)` stores the Connection-ID from the response and puts it first in every later request; `Get` sends the request headers in one final Get and repeats an empty final Get (Connection-ID only) while the answer is Continue, collecting Body/End-of-Body; `Put` sends headers plus End-of-Body in one final Put. Each exchange has a 10 s deadline; a deadline or protocol error closes the connection (OBEX has no resynchronisation).
- Server loop (for MNS): answers Connect (checking the Target), collects a Put across Continue packets (answering `0x90` to each non-final packet, then the handler's result to the final one), answers Disconnect with Success and returns, Abort with Success (discarding the partial Put), anything else with `0xD1` Not Implemented. A request body larger than 1 MiB is answered `0xCD` Request Entity Too Large and discarded.
- A received Get answer is capped at 1 MiB (then Abort and an error).

### 17.5 MAP operations (`internal/sms`)

Constants: MAS target `BB582B40-420C-11DB-B0DE-0800200C9A66`, MNS target `BB582B41-420C-11DB-B0DE-0800200C9A66`.

| Operation | Request |
| --- | --- |
| Connect MAS | Connect, Target = MAS UUID; no `MapSupportedFeatures` app parameter, so the phone uses the MAP 1.0 default features and event reports version 1.0 |
| SetNotificationRegistration | final Put, Type `x-bt/MAP-NotificationRegistration`, App-Params `NotificationStatus (0x0E) = 1`, End-of-Body `0x30` |
| GetMessage | final Get, Name = handle, Type `x-bt/message`, App-Params `Attachment (0x0A) = 0`, `Charset (0x14) = 1` (UTF-8) |
| SetMessageStatus | final Put, Name = handle, Type `x-bt/messageStatus`, App-Params `StatusIndicator (0x17) = 0` (read), `StatusValue (0x18) = 0` (unread), End-of-Body `0x30` |
| MNS event | the phone's Put, Type `x-bt/MAP-event-report`, App-Params `MASInstanceID (0x0F)`, body = event report XML |

GetMessage and SetMessageStatus address a message by handle alone (handles are unique within a MAS instance), so no SetPath is needed.

**Event report.** `<MAP-event-report version="1.0"><event type="NewMessage" handle="…" folder="TELECOM/MSG/INBOX" msg_type="SMS_GSM"/>…</MAP-event-report>`, parsed with `encoding/xml`; extra attributes (version 1.1 `datetime`, `sender_name`, …) are ignored. One report may hold several events. An event is **forwarded** when `type` is `NewMessage`, `msg_type` is `SMS_GSM` or `SMS_CDMA`, and the folder's last path element is `inbox` (case-insensitive; `INBOX`, `telecom/msg/inbox` and `TELECOM/MSG/INBOX` all match). Every other event is logged at debug and ignored (MMS `NewMessage` events at info, so the owner sees why a message did not arrive). A malformed report is answered with Success anyway (the phone must not retry it) and logged as a warning.

**bMessage.** The GetMessage body:

```
BEGIN:BMSG
VERSION:1.0
STATUS:UNREAD
TYPE:SMS_GSM
FOLDER:TELECOM/MSG/INBOX
BEGIN:VCARD            <- originator (zero or one, before BENV)
VERSION:2.1
N:Doe;Jane
TEL:+380501234567
END:VCARD
BEGIN:BENV
BEGIN:VCARD ... END:VCARD   <- recipients, ignored
BEGIN:BBODY
CHARSET:UTF-8
LENGTH:28
BEGIN:MSG
Hello
END:MSG
END:BBODY
END:BENV
END:BMSG
```

Parser rules: lines end in CRLF or LF; property names are case-insensitive and may carry parameters (`TEL;TYPE=CELL:…`); vCard 2.1 and 3.0 are both accepted, including folded continuation lines in the vCard (a line starting with a space or tab). The originator is the first vCard after the BMSG properties and before `BEGIN:BENV`. `from` is its first `TEL` value, trimmed (it may be alphanumeric for service senders); `from_name` is `FN`, else `N` turned into "Given Family" (components 2 then 1, empty parts dropped), else none. A vCard name equal to the number is reported as no name. The text is everything between a `BEGIN:MSG` line and the matching `END:MSG` line inside `BBODY`, with the final line break before `END:MSG` removed and CRLF normalised to LF; several MSG blocks are concatenated. `LENGTH` is ignored (Android's value is not reliable). `STATUS` is read (`READ`/`UNREAD`). A `BBODY` with an `ENCODING` property or a `CHARSET` other than `UTF-8` is rejected as unsupported (siptooth asked for UTF-8). A missing BBODY or MSG block is a parse error.

### 17.6 Service lifecycle

`sms.Service` owns the MAP state. It runs one goroutine and gets everything as messages, with an injected clock (as `bridge`). Inputs come from the daemon:

- `PhoneUp()` and `PhoneDown()`: the HFP service-level connection came up or went away (`btHandler` calls them next to `Attach`/`Detach`). MAP follows the HFP link, so the hourly power cycle, bluetoothd restarts and phone reconnects are all handled by the HFP machinery that already handles them.
- `MASConnection(conn)` and `MNSConnection(conn)`: fds from `Profile1.NewConnection` on the MAS and MNS profile objects (returned to BlueZ at once; the work happens on the service's goroutines).
- `MASDisconnected()` / `MNSDisconnected()`: `RequestDisconnection` or `Release` on those profile objects.

States: `disabled` (config off), `waiting` (no HFP link), `connecting`, `ready` (MAS connected, notifications on), `retrying` (waiting for the next attempt).

Sequence once the phone is up:

1. Wait 3 s (HFP SLC and the phone's own setup settle first), then call `Device1.ConnectProfile(MAS UUID)` with a 15 s timeout through the `Connector` interface. BlueZ looks up the phone's MAS record, connects, and delivers the socket through `NewConnection`. A `ConnectProfile` error, or no MAS connection within 15 s, is a failed attempt.
2. OBEX Connect with the MAS target, then SetNotificationRegistration on. Both must succeed.
3. The service is `ready`. The phone then connects to siptooth's MNS record and sends event reports on it. If no MNS connection arrives within 30 s of registration, the attempt failed ("phone did not open the notification channel"): the MAS connection is closed (`Device1.DisconnectProfile(MAS UUID)` too) and retried.
4. For each forwarded event: if the handle was delivered or is already queued (dedup memory of the last 256 handles, in RAM only), skip it; otherwise queue it. The fetch queue holds at most 32 handles (oldest dropped with a warning). Fetches run one at a time on the MAS connection: GetMessage, parse, emit `sms.received`, then SetMessageStatus(unread) when the bMessage said `UNREAD`, so siptooth never changes what the owner sees on the phone (Android may mark a message read when it is downloaded). A failed SetMessageStatus is only logged.
5. A fetch that fails with a connection-level error (deadline, EOF, protocol error) closes the MAS connection; the handle stays queued and is fetched after the next successful connect. A handle is dropped after 3 failed attempts, or immediately on an OBEX error response for that message (for example Not Found because it was deleted) or a parse error (logged with the handle, never with the text).

**Retries.** Any failed attempt or lost MAS connection while the phone is up schedules the next attempt after a backoff of 10 s, 30 s, 60 s, 120 s, then 300 s, reset by a successful `ready`. An OBEX Connect refused with Unauthorized/Forbidden or a socket refusal also logs the hint "allow message access for siptooth on the phone (Bluetooth device settings, Text messages / Message access)". `PhoneDown` closes both connections, empties nothing from the fetch queue (handles stay for the next connection, subject to the 3-attempt rule), cancels timers, and goes to `waiting`.

**MNS without MAS.** An MNS connection is accepted whenever it arrives, even before or between MAS connections, and at most one is kept (a new one replaces the old). Events received while MAS is down are queued and fetched after the next connect.

**system.error.** `sms_unavailable` is emitted once when the service has not been `ready` for 5 minutes while the phone is up (detail: the last error); it is re-armed by the next `ready`. Short gaps (the hourly power cycle) therefore emit nothing.

**Isolation from calls.** The service shares nothing with the call coordinator except the Bluetooth link. It never blocks the coordinator or the Bluetooth session goroutine, and a MAP failure never affects calls. MAP traffic during a call is allowed (a few hundred bytes on the ACL link next to SCO).

### 17.7 BlueZ integration

When `sms.enabled`, the Bluetooth session's `register()` also registers two `Profile1` objects after the HFP one:

| Object | UUID | Options |
| --- | --- | --- |
| `/com/siptooth/mas` | `00001132-0000-1000-8000-00805f9b34fb` | `Role: client`, `AutoConnect: false`, `RequireAuthentication: true`, `RequireAuthorization: false` |
| `/com/siptooth/mns` | `00001133-0000-1000-8000-00805f9b34fb` | `Role: server`, `Name: Siptooth MNS`, `RequireAuthentication: true`, `RequireAuthorization: false` (BlueZ publishes its default MNS SDP record and listens on RFCOMM and L2CAP) |

Registering MAP is best effort: a failure is logged (`AlreadyExists` counts as success, as for HFP), retried with the HFP registration retry, and never blocks HFP registration or calls. `NewConnection` from a device other than the configured phone is rejected (as for HFP). `ConnectMAS`/`DisconnectMAS` call `Device1.ConnectProfile`/`DisconnectProfile` with the MAS UUID. With `sms.enabled: false` nothing MAP-related is registered and the service stays `disabled`.

### 17.8 Hook event `sms.received`

```json
{"schema_version":1,"event":"sms.received","occurred_at":"2026-10-14T09:43:31Z","test":false,
 "sms":{"id":"20261014T094331Z-7c1e","from":"+380501234567","from_name":"Jane Doe",
        "text":"Hello","type":"SMS_GSM","handle":"0400000000000012"}}
```

`id` uses the call ID format (UTC timestamp and 4 random hex digits). `from` and `from_name` are null when unknown. `occurred_at` is when siptooth fetched the message (version 1.0 event reports carry no time). Environment: `SIPTOOTH_EVENT=sms.received`, `SIPTOOTH_SMS_FROM` (empty when unknown); `SIPTOOTH_CALL_ID` and `SIPTOOTH_CALLER` are set empty. Delivery uses the same dispatcher, queue and ordering as call events. `siptooth hooks test --event sms.received` sends a sample (`from` `+380501234567`, `from_name` `Test Sender`, `text` `Test message from siptooth`).

### 17.9 Logging, status, doctor

- Logs never contain message text. One info line per delivered message: `msg="sms received" sms_id=… from=… type=… text_chars=…`. Service state changes are logged at info, failed attempts at warning with the error and the next retry delay.
- `GET /status` gains `"sms":{"enabled":…,"state":"…","notifications":…,"received":N,"last_error":"…"}` (`notifications`: an MNS connection is open; `received`: messages delivered since start; `last_error` omitted when empty). `siptooth status` prints `sms: <state>, notifications <yes|no>, <N> received` (or `sms: disabled`).
- `siptooth doctor` with `sms.enabled`: warns when no hook subscribes to `sms.received` (`sms_hooks`), and fails when an `obexd` process is running (`sms_obexd`, it may own the MNS profile; hint: stop it).

### 17.10 Configuration

```yaml
sms:
  enabled: false               # forward incoming SMS to sms.received hooks over Bluetooth MAP (spec 17)
```

Default off: turning it on makes the phone ask for message access. No other keys; timings are constants.

### 17.11 Testing

- **obex:** golden bytes for each request and header type; header decode table (truncated header, length past packet, unknown header skipped); packet reader fed 1-byte chunks, whole packets, and two packets in one read; client Get across several Continue responses; error response codes; deadline closes the connection; 1 MiB caps; server loop for Connect/Put (multi-packet)/Abort/Disconnect/unknown opcode, over `net.Pipe`.
- **sms parsers:** bMessage table (vCard 2.1 and 3.0, FN vs N vs none, name equal to number, alphanumeric sender, missing originator, TEL with parameters, folded lines, LF-only, multiple MSG blocks, empty text, unsupported charset/encoding, missing MSG); event report table (v1.0, v1.1 attributes, several events, MMS, sent folder, malformed XML).
- **sms client/MNS:** a scripted fake MSE on `net.Pipe` (exact request bytes asserted, scripted answers) for connect, registration, fetch, unread restore; a fake phone driving the MNS server.
- **Service:** fake clock, fake `Connector`, in-memory connections: phone up → connect → ready; MNS timeout → retry; backoff sequence and reset; refused connect hint; dedup; queue cap; fetch failure keeps the handle, third failure drops it; Not Found drops it; `PhoneDown` mid-fetch; MNS before MAS; `sms_unavailable` after 5 minutes only, once per outage; no hook text in logs (log capture asserts the text never appears).
- **hooks, config, control, cli, doctor:** payload golden, env, `hooks test` sample, `sms.enabled` default and unknown-key error, status JSON and text, doctor checks.
- BlueZ glue is vetted by the linux/arm64 build only; its behaviour is qualified on hardware (section 15, M5).

## Appendix A — Review log (revision 2)

Reviewed by Fable 5.1 (Claude subagent), gpt-6.1-sol and gpt-6-astra (Codex CLI). Changes:

- INVITE ownership moved into `internal/sip` with explicit CANCEL/2xx-crossing, fork and late-2xx handling, because sipgo v1.6.0 `WaitAnswer`/`inviteCancel` leave a crossing 2xx un-ACKed and can block up to 64·T1 (all three).
- Startup audio buffered losslessly for the whole answering window, allochka's 800 ms settle delay kept, drain before CHUP on far-end BYE (all three).
- Answering event table, ATA error classes and generation discards added; ownership rule stated (all three).
- SCO loss: BYE only, never CHUP, 1.5 s grace, outcome `audio_lost` (Fable, gpt-6.1-sol, gpt-6-astra).
- Stall fallback plays buffered audio, not silence (all three).
- NAT: per-destination `nat.mode` (LAN PBX gets local addresses; Fable), STUN keepalive while ringing (Fable), Contact from OPTIONS `received`/`rport`, CRLF keepalives, `WithClientNAT`, `sip.local_port`, honest diagnostics, no `0.0.0.0` fallback (all three).
- TLS ServerName derived per destination for ElevenLabs node Contacts; `RewriteContact` off (all three).
- Far-end BYE reachability made a qualification item; media timeout default 30 → 10 s (Fable).
- RTP latching instead of source filtering, early media dropped, all negotiated G.711 types decoded, timestamp-based playout (all three).
- Renegotiation narrowed: UPDATE dropped, re-INVITE limited to refresh/address change, 488 otherwise, remote CSeq tracked, UDP 2xx retransmission (all three).
- SDES recipe: directional keys, replay protection, tag/length/MKI rules, key redaction in logs (gpt-6.1-sol, gpt-6-astra).
- Auth: two challenges allowed (sipgo `maxAuthAttempts`), BYE re-sent on challenge (all three).
- `pion/stun` dropped for a hand-rolled Binding Request (the unsuffixed module is v0.6.1 with a parser panic; Fable, gpt-6.1-sol); versions pinned (all three).
- URIs: `sip:` only, explicit port honored, component-aware escaping, CR/LF rejected; `{call_id}` placeholder; provider recipes (all three).
- Hooks back to allochka's single worker (Fable, gpt-6.1-sol); claims narrowed to two qualified targets (gpt-6-astra).

2026-10-14 (hardware test, ElevenLabs direct, caller level): the caller was quiet and distorted at ElevenLabs; siptooth now holds the phone's HFP speaker gain at 15 (section 8) and logs the caller's speech level (`caller_db_p50`, `caller_db_p95`, `caller_speech_s`) in the call summary.

2026-10-14 (hardware test, ElevenLabs direct, later): the agent heard its own echo from the phone line and interrupted itself; the allochka echo gate is ported as section 6.6 with `media.echo_suppression`, `media.echo_threshold_db` and `media.echo_window_ms`.

2026-10-14 (hardware test, ElevenLabs direct): the agent's greeting sounded faster than the rest of its speech. ElevenLabs starts speaking on its 200 OK, so 1–2 s of greeting is buffered while the phone answers and SCO settles, and catch-up then removed the short silences between its words to drain that backlog. Catch-up now keeps the first `media.min_pause_ms` (200) of every pause (§6.3), so the backlog drains in the first long pause instead.

Revision 4 (implementation): the owner-answer ownership rule was tightened (a definite ATA failure means the call is never siptooth's even if CallActive arrives first; outgoing call setup invalidates any pending answer); CallSetupCleared after ATA waits for CallActive (above); SCO loss latches no-CHUP; the far-end BYE drain bypasses jitter priming; re-INVITE 2xx is retransmitted on every transport (§5.6); `sip.local_port` must be 0 for TCP/TLS and maps to 5060 for UDP; the UDP Contact advertises the real bound port; transient RTP send errors are tolerated for 5 s.

Revision 3 (owner input): the LAN PBX is replaced by the owner's Zadarma cloud PBX as a qualified target next to ElevenLabs direct (both first-class); optional REGISTER (section 5.7) because Zadarma documents only registered extensions or static-IP trunks, which also gives far-end BYEs a flow to arrive on; Zadarma recipe with its documented facts and the M1 qualification list.

2026-10-14 (revision 5, owner request): SMS forwarding to hooks over Bluetooth MAP (section 17): new `sms.enabled` key (default off), `sms.received` hook event, `sms_unavailable` system error, `internal/obex` and `internal/sms`, two more BlueZ profiles, status and doctor additions, rollout step M5.
