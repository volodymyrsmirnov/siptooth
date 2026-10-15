# Hooks

Hooks run owner-configured shell commands on call lifecycle events and, when `sms.enabled` is true, on incoming text messages (`sms.received`). They never affect calls: a hook that fails, hangs or is dropped is only logged.

```yaml
hooks:
  - id: log
    events: [call.ringing, call.connected, call.ended, system.error]
    command: "/home/pi/.siptooth/hooks/write-file.sh"
    timeout_seconds: 10      # 1..300, default 10
```

Test without the daemon: `siptooth hooks test --event call.ended` runs the matching hooks with a sample payload (`"test": true`) and prints the payload, exit code, stdout and stderr.

## Delivery

- One worker runs all hooks sequentially, in emission order, so the order of events within a call is preserved (`call.ringing`, `call.connected`, `call.ended`). A slow hook delays the ones behind it.
- The command runs as `/bin/sh -c COMMAND` as the daemon's user, with the JSON payload on stdin (one line, newline terminated).
- On `timeout_seconds` the process group gets SIGTERM and then SIGKILL. stdout and stderr are captured up to 8 KiB each and logged.
- stdout and stderr of `sms.received` hooks are never logged (they may echo the message); the log line has the exit code, the timeout flag and the byte counts of both streams instead.
- One attempt, no retries. The queue holds 100 events; when it is full the event is dropped with a warning (`hook_queue_full`), which is never dispatched as a hook.

## Environment

The daemon's environment minus `SIPTOOTH_SIP_PASSWORD`, plus:

| Variable | Value |
| --- | --- |
| `SIPTOOTH_EVENT` | event name, for example `call.ended` |
| `SIPTOOTH_CALL_ID` | call ID, for example `20261014T094331Z-b5fa` (empty for `system.error` and `sms.received`) |
| `SIPTOOTH_CALLER` | caller number; empty when withheld (empty for `sms.received`) |
| `SIPTOOTH_SMS_FROM` | sender of an `sms.received` message; empty when unknown or for other events |

## Events

Every payload has `schema_version` (1), `event`, `occurred_at` (UTC, second precision) and `test`. Credentials and SRTP keys never appear.

### call.ringing

Emitted before the INVITE, so side processes can prepare for the call (correlate by `X-CALL-ID`).

```json
{"schema_version":1,"event":"call.ringing","occurred_at":"2026-10-14T09:43:31Z","test":false,
 "call":{"id":"20261014T094331Z-b5fa","caller":"+380501234567","caller_withheld":false,
         "number":"+380441234567","target":"sip:+380441234567@sip.rtc.elevenlabs.io;transport=tls"}}
```

`caller` is `null` and `caller_withheld` true when the number is withheld. `target` is the expanded URI.

### call.connected

Audio is bridged. Adds to `call`:

```json
{"schema_version":1,"event":"call.connected","occurred_at":"2026-10-14T09:43:36Z","test":false,
 "call":{"id":"20261014T094331Z-b5fa","caller":"+380501234567","caller_withheld":false,
         "number":"+380441234567","target":"sip:+380441234567@sip.rtc.elevenlabs.io;transport=tls",
         "sip_call_id":"3f9c1a@10.0.0.5","codec":"PCMU","srtp":false,
         "remote_media":"34.12.5.6:20118","advertised_media":"203.0.113.7:14002",
         "nat_mode":"stun","connected_at":"2026-10-14T09:43:36Z"}}
```

`remote_media` is the latched RTP source, or the SDP address before latching.

### call.ended

Exactly once per call, whatever the outcome. A call that ended before `call.ringing` went out still gets a `call.ringing` first. Adds to the `call.connected` fields (those are empty strings / `false` / `null` when the call never connected):

```json
{"schema_version":1,"event":"call.ended","occurred_at":"2026-10-14T09:44:39Z","test":false,
 "call":{"id":"20261014T094331Z-b5fa","caller":"+380501234567","caller_withheld":false,
         "number":"+380441234567","target":"sip:+380441234567@sip.rtc.elevenlabs.io;transport=tls",
         "sip_call_id":"3f9c1a@10.0.0.5","codec":"PCMU","srtp":false,
         "remote_media":"34.12.5.6:20118","advertised_media":"203.0.113.7:14002",
         "nat_mode":"stun","connected_at":"2026-10-14T09:43:36Z",
         "outcome":"completed","hangup_by":"far_end","sip_status":200,"sip_reason":"OK","error":null,
         "ringing_seconds":4.2,"bridged_seconds":63.1,
         "media":{"sent":3155,"received":3150,"lost":0,"late":2,"duplicate":0,"overflow":0,
                  "discarded_silence":12,"srtp_fail":0}}}
```

`outcome` is one of `completed`, `sip_rejected`, `sip_timeout`, `sip_error`, `sip_media_failed`, `caller_abandoned`, `owner_answered`, `answer_failed`, `far_end_ended`, `signaling_lost`, `media_timeout`, `audio_lost`, `media_error`, `max_duration`, `bluetooth_lost`, `shutdown` (meanings in `docs/operations.md`). `hangup_by` is `caller`, `far_end`, `siptooth` or `null`. `sip_status` / `sip_reason` are the last final response to the INVITE, `null` if none. `bridged_seconds` is `null` if the call was never bridged. A call that failed to dial looks like:

```json
{"schema_version":1,"event":"call.ended","occurred_at":"2026-10-14T09:50:02Z","test":false,
 "call":{"id":"20261014T095000Z-17ac","caller":null,"caller_withheld":true,"number":"+380441234567",
         "target":"sip:+380441234567@sip.rtc.elevenlabs.io;transport=tls",
         "sip_call_id":"","codec":"","srtp":false,"remote_media":"","advertised_media":"203.0.113.7:14002",
         "nat_mode":"stun","connected_at":null,
         "outcome":"sip_rejected","hangup_by":null,"sip_status":486,"sip_reason":"Busy Here","error":null,
         "ringing_seconds":2.1,"bridged_seconds":null,
         "media":{"sent":0,"received":0,"lost":0,"late":0,"duplicate":0,"overflow":0,
                  "discarded_silence":0,"srtp_fail":0}}}
```

### sms.received

An incoming SMS was fetched from the phone (needs `sms.enabled: true`, see `docs/operations.md`). There is no `call` object; `sms.id` has the same format as a call ID.

```json
{"schema_version":1,"event":"sms.received","occurred_at":"2026-10-14T09:43:31Z","test":false,
 "sms":{"id":"20261014T094331Z-7c1e","from":"+380501234567","from_name":"Jane Doe",
        "text":"Hello","type":"SMS_GSM","handle":"0400000000000012"}}
```

`from` and `from_name` are `null` when unknown (`from_name` is the contact name the phone knows; `from` may be alphanumeric for service senders). `text` is the whole message in UTF-8 (concatenated SMS arrive joined). `type` is `SMS_GSM` or `SMS_CDMA`. `occurred_at` is when siptooth fetched the message. The environment has `SIPTOOTH_EVENT=sms.received` and `SIPTOOTH_SMS_FROM`.

Delivery is best effort with one hook attempt per message, through the same queue and worker as call hooks (a slow SMS hook delays call hooks behind it). A message that arrives while siptooth has no message session with the phone is not delivered and is not replayed later. MMS, RCS chats (Google Messages "chat features") and Android group messages never appear. The logs hold the message id, sender number, type and length only, never the text or the sender's name.

`siptooth hooks test --event sms.received` sends a sample (`from` `+380501234567`, `from_name` `Test Sender`, `text` `Test message from siptooth`) to the matching hooks and prints the hook's output.

### system.error

Operational problems, emitted once per outage or rate-limited. There is no `call` object.

```json
{"schema_version":1,"event":"system.error","occurred_at":"2026-10-14T03:00:12Z","test":false,
 "error":{"code":"sip_registration","detail":"401 Unauthorized"}}
```

Codes: `bluetooth_unavailable`, `profile_registration`, `routing`, `stun_failed` (at most once per hour), `sip_registration`, `sms_unavailable` (SMS forwarding enabled, the phone is connected and the message channel has not been up for 5 minutes; the detail is the last error; emitted again only after the channel was up in between).

## Examples

Ready-to-copy scripts are in `config/hooks/`:

| Script | What it does |
| --- | --- |
| `write-file.sh` | appends every payload to `~/.siptooth/data/hook-events.jsonl` (`SIPTOOTH_HOOK_LOG` overrides the path) |
| `sms-log.sh` | appends `<time> [<name> ]<<number>>: <text>` of every `sms.received` to `~/.siptooth/data/sms.log` (`SIPTOOTH_SMS_LOG` overrides the path; mode 0600). Uses `jq` when installed, else appends the raw JSON payload |
| `http-post.sh` | POSTs the payload as JSON to the URL in `~/.siptooth/hooks/http-post.env` (`SIPTOOTH_WEBHOOK_URL`), with an `X-Siptooth-Event` header |

Install them:

```sh
mkdir -p ~/.siptooth/hooks ~/.siptooth/data
cp config/hooks/write-file.sh config/hooks/http-post.sh config/hooks/sms-log.sh ~/.siptooth/hooks/
cp config/hooks/http-post.env.example ~/.siptooth/hooks/http-post.env
chmod 0755 ~/.siptooth/hooks/*.sh
chmod 0600 ~/.siptooth/hooks/http-post.env
```

Then reference them in `~/.siptooth/config.yaml` and run `siptooth hooks test --event call.ended` (or `--event sms.received`).

A one-liner for a missed-call style notification, using `jq` on the payload:

```yaml
hooks:
  - id: failed_to_forward
    events: [call.ended]
    command: "jq -r 'select(.call.bridged_seconds == null) | \"unforwarded call from \\(.call.caller // \"withheld\"): \\(.call.outcome)\"' >> $HOME/.siptooth/data/unforwarded.log"
```
