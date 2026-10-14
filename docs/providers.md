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
  target: "sip:200@sip.zadarma.com"        # what the siptooth extension dials; TLS uses port 5061
  transport: tls                           # UDP is also documented by Zadarma; TLS preferred
  username: "1234-100"                     # password: SIPTOOTH_SIP_PASSWORD in ~/.siptooth/env
  register: true
  from: "sip:1234-100@sip.zadarma.com"     # Zadarma identifies the extension by its login
  display_name: "{caller}"                 # the caller's number, shown where Zadarma passes it through
  headers:
    X-CALLER-ID: "{caller}"
    X-CALL-ID: "{call_id}"
  srtp: false                              # SRTP is not documented by Zadarma; test (M1) before enabling
media:
  codecs: [PCMA, PCMU]                     # Zadarma documents alaw/ulaw only
```

Known from Zadarma's documentation: `sip.zadarma.com`, UDP 5060 and TLS 5061, PCMA / PCMU, and `P-Asserted-Identity` only for numbers attached to the account (so it cannot carry an arbitrary caller's number). Not documented, to be verified in M1: INVITE without REGISTER, TCP, SRTP, TLS certificate validity (Zadarma's own Asterisk guide disables verification; siptooth never does: if the certificate fails, use UDP), display-name presentation on internal calls and on forwarding, whether custom `X-` headers survive a forward to an external SIP URI, NAT latching, and in-dialog request routing to a registered flow.

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

Run these from the Pi against each provider and record the outcome. The plan's `siptooth sip test` command is not in the current CLI; until it exists, observe with `siptooth doctor` (`dns`, `sip_connect`, `sip_options`, `local_address`, `nat_mapping`), `SIPTOOTH_LOG_LEVEL=debug` and a real test call.

| Check | What to look at | Result |
| --- | --- | --- |
| DNS / TLS | `doctor` `dns`, `sip_connect`: name resolves, TLS handshake and certificate verify | |
| Auth | INVITE (and REGISTER) answered after the digest challenge; no 401/403 loop | |
| REGISTER required? | INVITE accepted without `sip.register`? with it? | |
| NAT mode / latching | `nat_mode` in the summary line; `doctor` `nat_mapping`; audio both ways; `remote_media` in `call.connected` is the latched source | |
| Codec | `codec` in the summary line (PCMU / PCMA) | |
| SRTP | does the trunk accept `sip.srtp: true`; `srtp_fail` stays 0 | |
| Caller ID at destination | the number appears where the destination shows it (display name, `X-CALLER-ID`) | |
| X- headers through forward | custom headers reach the final destination after a PBX forward | |
| Far-end BYE reaches siptooth | end the call from the far side: `hangup_by=far_end`, phone hung up within about 3 s, not `media_timeout` | |
| Max inter-packet gap | longest gap in received RTP (watch `late` / `lost`; is RTP continuous in silence?) | |
