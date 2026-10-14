# Providers

Recipes for the SIP targets siptooth is built for, a generic PBX section, and the qualification checklist (milestone M1) to fill in on the real Pi.

## ElevenLabs (direct to an agent)

1. In ElevenLabs import the phone number as a SIP trunk number, with exactly the string siptooth puts in the Request-URI user part (including the leading `+`), and assign the agent.
2. Inbound transport: TLS (TCP is acceptable; UDP is experimental on their side).
3. Inbound authentication: digest username and password. Their IP allowlist does not suit a dynamic home IP.
4. Media encryption `Disabled` with `sip.srtp: false`, or `Allowed` / `Required` with `sip.srtp: true`.

```yaml
phone:
  number: "+380441234567"                  # the number imported in ElevenLabs
sip:
  target: "sip:{number}@sip.rtc.elevenlabs.io;transport=tls"
  transport: tls
  username: "your-trunk-username"          # password: SIPTOOTH_SIP_PASSWORD in ~/.siptooth/env
  headers:
    X-CALLER-ID: "{caller}"
    X-CALL-ID: "{call_id}"
  srtp: false
media:
  codecs: [PCMU, PCMA]
```

`sip.register` stays `false`. `X-CALLER-ID` carries the cellular caller's number to the agent; `X-CALL-ID` is siptooth's call ID, also in the hook payloads.

## Zadarma (cloud PBX)

Create a PBX extension for siptooth (login `<account>-<ext>`, for example `1234-100`, password from the PBX settings) and route what it dials in the PBX: another extension, a voice menu, voicemail, or the extension's "Forward to: External server (SIP URI)" (for example an ElevenLabs agent number; Zadarma does not support REFER-based transfer, forwarding it does).

```yaml
phone:
  number: "+380441234567"
sip:
  target: "sip:200@pbx.zadarma.com"        # what the siptooth extension dials; TLS uses port 5061
  transport: tls                           # UDP 5060 also works; TLS preferred
  username: "1234-100"                     # password: SIPTOOTH_SIP_PASSWORD in ~/.siptooth/env
  register: true
  from: "sip:1234-100@pbx.zadarma.com"     # Zadarma identifies the extension by its login
  display_name: "{caller}"                 # the caller's number, shown where Zadarma passes it through
  headers:
    X-CALLER-ID: "{caller}"
    X-CALL-ID: "{call_id}"
  srtp: true                               # required over TLS: plain RTP gets 488 SRTP required
media:
  codecs: [PCMA, PCMU]                     # PCMA / PCMU
```

Verified on the Pi on 2026-10-14 (server `pbx.zadarma.com`, a CNAME of `pbxma1.zadarma.com`):

- UDP 5060 works. TLS 5061 works with a valid `*.zadarma.com` Sectigo certificate (verification on, nothing disabled).
- Over TLS Zadarma rejects plain RTP with `488 SRTP required`: set `srtp: true`. SDES `AES_CM_128_HMAC_SHA1_80` works.
- REGISTER is required and works. The registrar reports `received`/`rport`; siptooth re-registers with the public flow and dials only after that, so the INVITE Contact is the public address.
- Zadarma pings the registered contact with OPTIONS about every 30 s through the NAT, so far-end requests reach siptooth.
- The INVITE is challenged with 407, then answered 200. Codecs PCMA / PCMU.
- Caller ID: Zadarma replaces the caller with the extension name and number (for example "Inkhorn Office (100)") in its call history. The original number only reaches hooks and `X-CALLER-ID`; whether custom headers arrive at the destination could not be verified.
- Hang-up from the phone side sends BYE, answered 200 OK.

From Zadarma's documentation: `P-Asserted-Identity` only for numbers attached to the account (so it cannot carry an arbitrary caller's number). Still open: INVITE without REGISTER, TCP, display-name presentation on forwarding, and whether custom `X-` headers survive a forward to an external SIP URI.

## Generic PBX (best effort)

Any SIP server that accepts G.711 over UDP/TCP/TLS works. Asterisk / PJSIP hints:

- an endpoint with `identify_by=auth_username` (or `ip`) so a From user carrying the caller number still matches;
- `rewrite_contact=yes`, `force_rport=yes`, `rtp_symmetric=yes`, `direct_media=no`;
- `allow=ulaw,alaw`.

On the LAN, `nat.mode: auto` resolves to `local`. Typical config:

```yaml
sip:
  target: "sip:100@pbx.lan"
  transport: udp
  username: "siptooth"
  register: false
nat:
  mode: auto
```

Use `register: true` when the PBX should know where to reach siptooth (and to receive the far end's BYE on a registered flow).

## M1 qualification checklist

Run these from the Pi against each provider and record the outcome. `siptooth sip test --tone` places a real call to the configured target without Bluetooth (it registers its own Contact first when `sip.register` is on, and uses its own SIP and RTP ports, so it can run while the daemon runs; but with `sip.register: true` that registration replaces the daemon's binding on a registrar that keeps a single binding per account, so stop the daemon during M1 or expect a registration gap of up to the refresh interval afterwards). It prints the NAT decision, the Contact, every SIP response (`<- 180 Ringing`), the negotiated codec and SRTP, a progress line per second, and a final report (packets sent/received/lost/late, max inter-packet gap, latched source, received level, how the call ended). Use `--wait-remote-hangup` (max 300 s) to leave the call up and see whether the far end's BYE arrives (`remote BYE`) or only RTP stops (`media stopped`). Add `siptooth doctor` (`dns`, `sip_connect`, `sip_options`, `local_address`, `nat_mapping`) and `SIPTOOTH_LOG_LEVEL=debug` for detail. Exit status is 0 only when the call was answered and audio arrived.

| Check | What to look at | Result |
| --- | --- | --- |
| DNS / TLS | `doctor` `dns`, `sip_connect`: name resolves, TLS handshake and certificate verify | Zadarma: resolves, TLS valid (`*.zadarma.com`, Sectigo) |
| Auth | INVITE (and REGISTER) answered after the digest challenge; no 401/403 loop | Zadarma: 407 then 200 |
| REGISTER required? | INVITE accepted without `sip.register`? with it? | Zadarma: required, works; re-registers with the public flow |
| NAT mode / latching | `sip test`: `nat:` line and `latched source` in the report; `doctor` `nat_mapping`; audio both ways; `remote_media` in `call.connected` is the latched source | Zadarma: OPTIONS pings every ~30 s reach siptooth through the NAT |
| Codec | `sip test`: `codec:` line (PCMU / PCMA) | Zadarma: PCMA / PCMU |
| SRTP | does the trunk accept `sip.srtp: true`; `srtp_fail` stays 0 | Zadarma: UDP plain works; TLS needs `srtp: true` (488 otherwise), SDES AES_CM_128_HMAC_SHA1_80 |
| Caller ID at destination | the number appears where the destination shows it (display name, `X-CALLER-ID`) | Zadarma: replaced by the extension name/number in call history; original only in hooks and `X-CALLER-ID` |
| X- headers through forward | custom headers reach the final destination after a PBX forward | Zadarma: not verifiable at the destination |
| Far-end BYE reaches siptooth | `sip test --wait-remote-hangup`, end the call from the far side: `ended by: remote BYE`; in the daemon `hangup_by=far_end`, phone hung up within about 3 s, not `media_timeout` | Zadarma: phone-side hang-up gives BYE 200 OK |
| Max inter-packet gap | `sip test --tone` report: `max gap`, `lost`, `late` (is RTP continuous in silence?) | |
