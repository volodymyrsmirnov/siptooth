# Siptooth SMS forwarding — Implementation Plan

**Spec:** `docs/superpowers/specs/2026-10-14-siptooth-design.md` section 17 (revision 5). The spec is binding; where this plan and the spec disagree, the spec wins and the plan is wrong.

**Goal:** incoming SMS on the phone reach `sms.received` hooks (sender, name, text) over Bluetooth MAP, with no effect on calls.

**Rules for every task** (from `CLAUDE.md`): TDD (failing test first), standard `testing` only, no new modules, Linux-only code behind `//go:build linux` with `_other.go` stubs, `make check` green before each commit, conventional commit messages ending with the session line, commit on `main`, never push. Tests never touch real Bluetooth, D-Bus, the network beyond loopback, or files outside `t.TempDir()`. Message text never appears in logs (tests assert it).

## Task graph

```
T1 obex codec+conn ─▶ T2 obex client/server ─▶ T4 sms MAS client + MNS ─▶ T5 sms.Service ─┐
T3 sms parsers ─────────────────────────────────▶ T4                                     │
T11 btsdp codec ─────────────────────────────────▶ T4                                     │
T11 ─▶ T12 btsdp linux dial ─────────────────────────────────────────────────────────────┤
T6 config + hooks ─▶ T9 doctor ──────────────────────────────────────────────────────────┼─▶ T8 daemon/control/status ─▶ T10 docs
T7 bluez MNS profile ────────────────────────────────────────────────────────────────────┘
```

T1, T3, T6, T7, T11 can start in parallel; T4 needs T2, T3 and T11; T10 needs T8 and T9. Each task's commit(s) must leave the tree green: an API change and every caller and test it breaks go in the same commit.

## Shared interfaces (fixed here so parallel tasks agree)

```go
// internal/obex  (spec 17.5)
const (
    OpConnect = 0x80; OpDisconnect = 0x81; OpPut = 0x02; OpPutFinal = 0x82; OpGetFinal = 0x83; OpAbort = 0xFF
    RespContinue = 0x90; RespSuccess = 0xA0; RespBadRequest = 0xC0; RespUnauthorized = 0xC1; RespForbidden = 0xC3
    RespNotFound = 0xC4; RespNotAcceptable = 0xC6; RespEntityTooLarge = 0xCD; RespInternalError = 0xD0
    RespNotImplemented = 0xD1; RespServiceUnavailable = 0xD3
    HName = 0x01; HType = 0x42; HTarget = 0x46; HWho = 0x4A; HBody = 0x48; HEndOfBody = 0x49
    HAppParams = 0x4C; HConnectionID = 0xCB
    MaxPacket = 8192; MinPeerPacket = 255
    MaxBody = 1 << 20; MaxHeaderBytes = 64 << 10
    PacketTimeout = 10 * time.Second; OperationTimeout = 30 * time.Second; AbortWait = 2 * time.Second
)
type Header struct { ID byte; Value []byte }   // byte-seq: raw; unicode: UTF-16BE incl. trailing 0x0000; 1-byte/4-byte: big-endian value
type Headers []Header
func (hs Headers) Get(id byte) ([]byte, bool)
func Unicode(id byte, s string) Header
func ASCII(id byte, s string) Header             // bytes + 0x00 (Type)
func Bytes(id byte, b []byte) Header
func U32(id byte, v uint32) Header
func DecodeUnicode(v []byte) (string, error)
func DecodeASCII(v []byte) string               // strips one trailing NUL
type AppParam struct { Tag byte; Value []byte }
func EncodeAppParams(ps ...AppParam) []byte
func DecodeAppParams(b []byte) ([]AppParam, error)
type ConnectFields struct { Version, Flags byte; MaxPacket uint16 }
type Packet struct { Code byte; Connect *ConnectFields; Headers Headers }
func (p Packet) Marshal() ([]byte, error)
func ParseRequest(b []byte) (Packet, error)                  // Connect fields when Code == OpConnect
func ParseResponse(b []byte, toConnect bool) (Packet, error)
type ResponseError struct { Code byte }
var ErrProtocol = errors.New("obex: protocol error")         // wrapped with detail, never with payload bytes

// Conn frames packets on a byte stream. NewConn starts one reader goroutine; Packets() yields
// complete packets, Done() is closed when the reader stops, Err() then reports why (io.EOF, protocol, ...).
type Conn struct{ ... }
func NewConn(rwc io.ReadWriteCloser, maxIn int) *Conn
func (c *Conn) Next(timeout time.Duration) ([]byte, error)   // next packet; timeout<=0: wait forever; timeout → error, caller closes
func (c *Conn) Write(b []byte) error                          // loops until written; uses SetWriteDeadline(PacketTimeout) when available
func (c *Conn) Done() <-chan struct{}
func (c *Conn) Err() error
func (c *Conn) Close() error

type Client struct{ ... }
func NewClient(rwc io.ReadWriteCloser) *Client
// Connect: ctx cancel closes the transport at once (watcher goroutine), returning ctx.Err(). appParams nil: no header.
func (c *Client) Connect(ctx context.Context, target []byte, appParams []byte, timeout time.Duration) error
// Get/Put: not safe for overlapping calls; the owner serialises (sms op worker). A non-final/invalid
// response code where a final one is due is ErrProtocol (the connection is closed).
func (c *Client) Get(hs ...Header) ([]byte, error)
func (c *Client) Put(hs Headers, body []byte) error
func (c *Client) Disconnect() error
func (c *Client) Done() <-chan struct{}          // closed when the connection is gone (idle EOF included)
func (c *Client) Close() error

type PutHandler func(hs Headers, body []byte) byte
// Serve: onConnect (may be nil) is called once, after the Connect Success response is written.
func Serve(rwc io.ReadWriteCloser, target []byte, h PutHandler, onConnect func()) error   // nil after Disconnect; otherwise the reason

// internal/btsdp  (spec 17.4)
type MASRecord struct { Channel uint8; InstanceID uint8; MessageTypes uint8; Features uint32; HasFeatures bool; Name string }
func EncodeSearchAttrRequest(tid uint16, uuid16 uint16, cont []byte) []byte
func ParseSearchAttrResponse(b []byte, wantTID uint16, maxCount uint16) (attrBytes, cont []byte, err error) // validates PDU 0x07, TID, param length, count, cont <= 16
// L2Addr returns display-order bytes for unix.SockaddrL2.Addr; RFCOMMAddr reversed bytes for unix.SockaddrRFCOMM.Addr (x/sys v0.41.0). Untagged, golden-tested.
func L2Addr(mac string) ([6]byte, error)
func RFCOMMAddr(mac string) ([6]byte, error)
func ParseMASRecords(attrLists []byte) ([]MASRecord, error)
func ChooseSMS(recs []MASRecord) (MASRecord, error)          // first with Channel != 0 and SMS_GSM|SMS_CDMA
func Lookup(ctx context.Context, t Transport) ([]byte, error) // continuation loop; concatenates raw attribute bytes, decodes nothing
type Transport interface { Exchange(ctx context.Context, req []byte) ([]byte, error) }
// linux only (stubs return ErrUnsupported):
// All three honour ctx during connect (ctx cancel closes the socket at once). FindSMSMAS bounds connect+lookup by 15 s.
func DialSDP(ctx context.Context, adapter, phone string) (Transport, io.Closer, error)
func DialRFCOMM(ctx context.Context, adapter, phone string, channel uint8) (*os.File, error)
func FindSMSMAS(ctx context.Context, adapter, phone string) (MASRecord, error)   // DialSDP + Lookup + Parse + Choose

// internal/sms  (spec 17.6, 17.7)
var MASTarget, MNSTarget []byte
const OwnFeatures uint32 = 0x00000003
type Event struct { Type, Handle, Folder, MsgType string }
func (e Event) Forward() bool
func (e Event) IsMMS() bool
func ParseEventReport(b []byte) ([]Event, error)
type BMessage struct { Type, From, FromName, Text string }
func ParseBMessage(b []byte) (BMessage, error)
var ErrUnsupported error

type MAS struct{ ... }
// OBEX Connect, 40 s; ctx cancel closes rwc at once. Features param iff rec.HasFeatures && rec.Features&(1<<19) != 0.
func ConnectMAS(ctx context.Context, rwc io.ReadWriteCloser, rec btsdp.MASRecord) (*MAS, error)
// Called only from the service's single MAS operation worker (never concurrently).
func (m *MAS) RegisterNotifications() error
func (m *MAS) GetMessage(handle string) (BMessage, error)
type GetAction int  // GetDeliver, GetDrop, GetRetry, GetEndSession (spec 17.7 step 5; ResponseError default = GetDrop; 0xC1 = GetEndSession; 0xD0/0xD3 = GetRetry; connection/protocol errors = GetEndSession; parse errors = GetDrop)
func ClassifyGetError(err error) GetAction
func (m *MAS) Done() <-chan struct{}
func (m *MAS) Close() error
// ServeMNS answers one MNS connection; onEvents gets forwarded-or-not events with the instance ID
// (-1 when absent); onConnected is called once after a successful OBEX Connect (obex.Serve's onConnect).
func ServeMNS(rwc io.ReadWriteCloser, onConnected func(), onEvents func(instance int, evs []Event, err error)) error

type Message struct { ID, Handle, Type, From, FromName, Text string }
type Sink interface {
    Deliver(Message) bool            // true when the hook dispatcher accepted it
    Unavailable(detail string)
}
// Dialer is the per-Bluetooth-session way to reach the phone's MAS (btsdp on Linux, fakes in tests).
type Dialer interface {
    FindMAS(ctx context.Context) (btsdp.MASRecord, error)
    Dial(ctx context.Context, channel uint8) (io.ReadWriteCloser, error)
}
type Clock interface { Now() time.Time; NewTimer(d time.Duration) Timer }
type Timer interface { C() <-chan time.Time; Stop() bool }
// Disabled: exactly {"enabled":false,"state":"disabled"} (nil pointers omitted).
type Status struct {
    Enabled bool `json:"enabled"`; State string `json:"state"`; Notifications *bool `json:"notifications,omitempty"`
    Received *int `json:"received,omitempty"`; LastError string `json:"last_error,omitempty"`
}
type Deps struct {
    Enabled bool; Sink Sink; Clock Clock; Logger *slog.Logger
    NewID func(time.Time) string
    MNSStuck func()   // spec 17.7 "MNS listener recovery"; may be nil; called from the loop, must not block
}
func NewService(d Deps) *Service
func (s *Service) Run(ctx context.Context)
// PhoneLink: up with this link's dialer, or down (d nil). Never blocks (non-blocking post, inbox 64).
// A repeated up replaces the previous link. Called from the coordinator loop via the daemon.
func (s *Service) PhoneLink(up bool, d Dialer)
func (s *Service) MNSConnection(rwc io.ReadWriteCloser)    // non-blocking admission: full inbox or stopped → closes rwc and returns
func (s *Service) MNSDisconnected()                         // closes the current MNS rwc (under a mutex) BEFORE returning, then posts
func (s *Service) SetRegistrationError(err error)          // MNS profile registration state for status (nil clears)
func (s *Service) Status() Status

// internal/hooks
type SMSInfo struct { ID, Handle, Type, Text string; From, FromName *string }
func (d *Dispatcher) EmitSMS(m SMSInfo) bool
func BuildSMSPayload(m SMSInfo, test bool) ([]byte, error)
func Env(event, callID, caller, smsFrom string) []string
// Result gains StdoutBytes, StderrBytes int64: totals written, including bytes beyond the 8 KiB capture.

// internal/bridge
// Deps gains PhoneLink func(up bool, adapterAddr string): optional; called from the coordinator loop on
// mAttach (up) and in dropPhone when a phone was attached (down: Detach or HFP EOF). Must not block.

// internal/bluez (linux; stubs return ErrUnsupported)
const MNSUUID = "00001133-0000-1000-8000-00805f9b34fb"
func MNSProfileOptions() map[string]any            // untagged, tested: the spec 17.8 options incl. ServiceRecord XML
func (m *Manager) RegisterMNSProfile(ctx context.Context, h ProfileHandler) error
func (m *Manager) UnregisterMNSProfile(ctx context.Context) error   // UnregisterProfile + unexport; DoesNotExist = success
func (m *Manager) AdapterAddress() string          // exists
```

## T1 — `internal/obex` codec and stream framing

Files: `internal/obex/obex.go`, `internal/obex/conn.go`, tests.

Tests first: `Marshal` golden bytes (Connect request with Target; Connect response with Connection-ID and Who; final Get with Connection-ID, Name, Type, App-Params; final Put with End-of-Body); header kinds round trip (unicode incl. Cyrillic and empty name `01 00 03`); parse errors (length field mismatch, header length < 3, header past end, odd unicode length, short Connect fields); unknown headers kept; AppParams round trip and truncated TLV. `Conn`: 1-byte chunks; two packets in one Read; length < 3 or > maxIn → `ErrProtocol` and Done closed; `Next` timeout; idle EOF closes Done with `io.EOF`; `Write` with a writer that writes 1 byte per call.

Commit: `feat(obex): packet and header codec with stream framing`.

## T2 — `internal/obex` client and server

Files: `internal/obex/client.go`, `internal/obex/server.go`, tests over `net.Pipe` with a scripted peer.

Every client and server bullet of spec 17.5 and the obex list of 17.12 (Connect headers, `onConnect` called once after the response is written, `Connect` ctx cancel returns at once and closes the transport, wrong Target, request before Connect, Connection-ID mismatch, multipart Put with header-only first packet, size caps, Abort, idle without deadline, operation deadline, Abort-then-close on caps, Disconnect). Use a short fake timeout via package-level variables that tests override (`var packetTimeout = PacketTimeout` etc.) — no real 30 s waits in tests.

Commit: `feat(obex): client and put server`.

## T3 — `internal/sms` parsers

Files: `internal/sms/event.go`, `internal/sms/bmessage.go`, `internal/sms/vcard.go`, tests (table driven; fixtures are interpreted strings with real `\r\n`, or raw strings passed through `strings.ReplaceAll(s, "\n", "\r\n")` — a raw string's `\r\n` is four literal characters).

Every parser case in spec 17.12. Quoted-printable via `mime/quotedprintable`. Error values never contain input text (assert with a sentinel word in the input).

Commit: `feat(sms): MAP event report, bMessage and vCard parsers`.

## T4 — `internal/sms` MAS client and MNS server

Files: `internal/sms/mas.go`, `internal/sms/mns.go`, tests with a scripted fake MSE / fake phone on `net.Pipe`, exact request bytes asserted. Features parameter cases: attribute absent, present without bit 19, present with bit 19. `ClassifyGetError` table (every code in spec 17.7 step 5, `0xCC`, a protocol error, EOF, a parse error). `ConnectMAS` ctx cancel during a stalled Connect returns at once. Needs T11's `btsdp.MASRecord`.

Commit: `feat(sms): MAP access client and notification server`.

## T11 — `internal/btsdp` codec

Files: `internal/btsdp/sdp.go`, `internal/btsdp/element.go`, `internal/btsdp/mas.go`, `internal/btsdp/addr.go`, tests (every btsdp case of spec 17.12; `Lookup` with a fake Transport returning three continuation fragments, one split inside a data-element header; round and byte limits; response validation cases; `L2Addr`/`RFCOMMAddr` golden on `AA:BB:CC:DD:EE:FF`).

Commit: `feat(btsdp): SDP service search attribute codec and MAS record choice`.

## T12 — `internal/btsdp` Linux sockets

Files: `internal/btsdp/socket_linux.go`, `internal/btsdp/socket_other.go`. Sockets created with `SOCK_NONBLOCK|SOCK_CLOEXEC`. L2CAP SEQPACKET PSM 1 (`unix.SockaddrL2{PSM: 1, Addr: L2Addr(...)}`, bind to the adapter with PSM 0), connect = non-blocking connect + wait for writability (poll in a loop of ≤200 ms slices checking ctx, or a ctx watcher that closes the fd) + `SO_ERROR`; receive buffer ≥ 4096 (or the socket's `L2CAP_OPTIONS` imtu); 5 s per exchange via deadline on `os.File`; the whole `FindSMSMAS` under one 15 s ctx including connect; RFCOMM `SOCK_STREAM` with `unix.SockaddrRFCOMM{Addr: RFCOMMAddr(...)}`, bind to adapter address channel 0, `setsockopt(SOL_BLUETOOTH=274, BT_SECURITY=4, []byte{2, 0})` (two-byte struct `{level, key_size}`, via `unix.SetsockoptString` or a raw setsockopt — not an int option), non-blocking connect + wait as above (≤15 s, ctx) + `SO_ERROR`, then `os.NewFile` (fd stays non-blocking so deadlines work). Only a compile check (`GOOS=linux GOARCH=arm64 go vet ./...`) plus a test of the address packing helper (untagged).

Commit: `feat(btsdp): L2CAP SDP and RFCOMM sockets (linux)`.

## T5 — `internal/sms` Service

Files: `internal/sms/service.go`, `internal/sms/clock.go`, `internal/sms/service_test.go` (own fake clock: `Advance` only after the test has observed the timer being armed — e.g. the fake clock exposes `WaitTimers(n)` that blocks until n timers are pending — never a sleep as synchronisation; fake Dialer returning `net.Pipe` ends served by the fake MSE from T4 tests; fake Sink; fake MNS phone). Temp paths only from `t.TempDir()`.

Implement spec 17.7 exactly: loop goroutine owns state; inputs via a buffered channel with **non-blocking** sends (`select { case inbox <- m: default: drop+log }`; MNS admission closes the rwc on drop or after stop — a `stopped` flag under the same mutex that guards the admission, so no fd is stranded); `Status()` from an atomically stored snapshot; workers tagged with generations; attempt context cancelled on phone down/replace/shutdown; one MAS operation worker per MAS session fed by a channel from the loop (registration and GetMessage strictly serial), results back to the loop, which validates the generation and only then calls `Sink.Deliver`; `MNSStuck` after two consecutive notification-channel timeouts, at most once per 10 min; constants as package variables (tests shorten nothing real: the fake clock drives all timers; the OBEX 40 s/30 s/10 s deadlines are inside obex and covered there, so service tests drive failures through the fakes). Every Service bullet in spec 17.12.

Commits: `feat(sms): service lifecycle and retries`, `feat(sms): fetch queue and delivery`.

## T6 — config and hooks

- `internal/config`: `type SMS struct { Enabled bool \`yaml:"enabled"\` }`, `Config.SMS SMS \`yaml:"sms"\``, default false; `KnownEvents` adds `sms.received`; embedded `config.yaml` gets the `sms:` block (spec 17.11) before `hooks:`; tests.
- `internal/hooks`: `SMSInfo`, `BuildSMSPayload` (golden JSON of spec 17.9; null from/from_name), `EmitSMS` returning acceptance (job carries the SMS; payload built in `process`), `Env(event, callID, caller, smsFrom)` (update callers), the dispatcher's finish log for `sms.received` has `sms_id`, exit, timed_out, `stdout_bytes`, `stderr_bytes` and never the streams. `cappedBuffer` counts total bytes written (it currently keeps no total), `Result` gains `StdoutBytes`/`StderrBytes`. Test with `tee /dev/stderr` (payload to both streams; plus a variant writing >8 KiB, e.g. `cat; head -c 10000 /dev/zero | tr '\0' x`) and a captured log: the payload's sentinel text never appears, both byte counts do.
- `internal/cli/hooks_cmd.go`: sample for `--event sms.received`.

Commits: `feat(config): sms.enabled`, `feat(hooks): sms.received event`.

## T7 — BlueZ MNS profile

`internal/bluez/bluez.go` (untagged): `MNSUUID`, `mnsProfilePath = "/com/siptooth/mns"`, `MNSProfileOptions()` with the exact options and XML of spec 17.8 (test asserts every key and that the XML parses with `encoding/xml` and names channel 16 consistently with `Channel`). `manager_linux.go`: generalise `profileObject` with a label for logs (HFP behaviour unchanged), `RegisterMNSProfile` (export at the MNS path, RegisterProfile, `AlreadyExists` = success), `UnregisterMNSProfile` (UnregisterProfile; `DoesNotExist` = success; unexport). The MNS profile object's `RequestDisconnection`/`Release` call the handler's `Disconnected` synchronously before the D-Bus reply (the sms side closes its fd inside that call). `manager_other.go` stub.

Commit: `feat(bluez): MNS profile with a MAP 1.1 RFCOMM record`.

## T8 — daemon, control, status

- `internal/bridge` (first commit, `feat(bridge): phone link callback`): `Deps.PhoneLink` called on `mAttach` (up, adapter address) and in `dropPhone` when a phone was attached (down); tests for attach, Detach, and an HFP `Disconnected` event (EOF path).
- `internal/daemon`: build `sms.Service` (Enabled from config, Sink adapter: `hooks.EmitSMS` + the `sms received` info line of spec 17.10 + `EmitError("sms_unavailable", …)`, NewID shared with calls, `MNSStuck` posting a re-register request to the Bluetooth session). Wire `bridge.Deps.PhoneLink` to `sms.PhoneLink(up, dialer)` where the dialer wraps `btsdp.FindSMSMAS`/`DialRFCOMM` with the given adapter address and the configured phone (the coordinator is built before the service: use a small forwarding closure set before `coord.Run`). Run the service with its own context; stop order: coordinator, Bluetooth, **sms**, registration, hooks, control, UA. `btHandler` is unchanged for SMS (phone up/down comes from the coordinator). `bluetoothRunner` gains an `smsLink` parameter (interface with `MNSConnection`, `MNSDisconnected`, `SetRegistrationError`, plus a `ReregisterMNS() <-chan struct{}`-style request channel or func registration); session loop: MNS registration on a **worker goroutine** tagged with the bus-session generation, its own `mnsRetry` timer (spec 17.8), only when `cfg.SMS.Enabled`; also after `BluezRestarted`; unregister+register on the stuck request. `bluetooth_other.go` updated.
- Same-commit migrations (each API change compiles and tests green in its own commit): `daemon_test.go` (`var st bridge.Status` → `control.Status`; the fake/hung `bluetoothRunner` lambdas get the new parameter; the shutdown-test `stops` literal gets a no-op `sms` stop), `internal/cli` status tests calling `printStatus`, every `control.NewServer` caller. In `daemon.go` initialise `stops.sms` to a no-op before anything can fail.
- ID generator: export `bridge.NewCallID` (or move into a tiny shared package) and use it for SMS ids.
- `internal/control`: `type Status struct { bridge.Status; SMS sms.Status \`json:"sms"\` }`, `NewServer(sp StatusProvider, smsStatus func() sms.Status)`, client returns `control.Status`; tests (exact disabled JSON).
- `internal/cli/status_cmd.go`: the `sms:` line and `sms error:` line, printed regardless of the SIP-registration early return (restructure it); tests with registration on and off.

Commits: `feat(control): combined status with SMS`, `feat(daemon): wire SMS forwarding`.

## T9 — doctor

`sms_hooks` (warn when enabled and no hook has `sms.received`; skip when disabled), `sms_obexd` (warn when enabled and a process named `obexd` runs; use the existing `/proc` scan used for `competingHandlerNames`, injectable like the existing tests). Tests.

Commit: `feat(doctor): SMS checks`.

## T10 — docs

`docs/hooks.md` (`sms.received`, env, output not logged, sample, delivery semantics), `docs/operations.md` (enabling SMS, the phone's message-access prompt and setting, status line, troubleshooting: no notifications, RCS/MMS not forwarded, obexd), `docs/setup.md` (optional step), README if it lists events/features, `CLAUDE.md` layout table (`internal/btsdp`, `internal/obex`, `internal/sms`), `config/hooks/` example `sms-log.sh` if the docs reference example scripts.

Commit: `docs: SMS forwarding`.

## Final gate

`make check`, `make race`, `GOOS=linux GOARCH=arm64 go vet ./...`, Linux test run in Docker (`docker run --rm --init -v "$PWD":/src -w /src golang:1.24 go test ./...`) if Docker is available. Then the Fable 5.1 review of the whole change and fixes. Then the owner's hardware test (spec 15, M5) using the `pi-hardware-test` skill.
