# Siptooth SMS forwarding — Implementation Plan

**Spec:** `docs/superpowers/specs/2026-10-14-siptooth-design.md` section 17 (revision 5). The spec is binding; where this plan and the spec disagree, the spec wins and the plan is wrong.

**Goal:** incoming SMS on the phone reach `sms.received` hooks (sender, name, text) over Bluetooth MAP, with no effect on calls.

**Rules for every task** (from `CLAUDE.md`): TDD (failing test first), standard `testing` only, no new modules, Linux-only code behind `//go:build linux` with `_other.go` stubs, `make check` green before each commit, conventional commit messages ending with the session line, commit on `main`, never push. Tests never touch real Bluetooth, D-Bus, the network beyond loopback, or files outside `t.TempDir()`. Message text never appears in logs (tests assert it).

## Task graph

```
T1 obex codec+conn ─▶ T2 obex client/server ─▶ T4 sms MAS client + MNS ─▶ T5 sms.Service ─┐
T3 sms parsers ─────────────────────────────────▶ T4                                     │
T11 btsdp codec ─▶ T12 btsdp linux dial ─────────────────────────────────────────────────┤
T6 config + hooks ───────────────────────────────────────────────────────────────────────┼─▶ T8 daemon/control/status ─▶ T10 docs
T7 bluez MNS profile ────────────────────────────────────────────────────────────────────┘
T9 doctor (after T6)
```

T1, T3, T6, T7, T11 can start in parallel. Each task's commit(s) must leave the tree green.

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
func (c *Client) Connect(target []byte, appParams []byte, timeout time.Duration) error   // appParams nil: no header
func (c *Client) Get(hs ...Header) ([]byte, error)
func (c *Client) Put(hs Headers, body []byte) error
func (c *Client) Disconnect() error
func (c *Client) Done() <-chan struct{}          // closed when the connection is gone (idle EOF included)
func (c *Client) Close() error

type PutHandler func(hs Headers, body []byte) byte
func Serve(rwc io.ReadWriteCloser, target []byte, h PutHandler) error   // nil after Disconnect; otherwise the reason

// internal/btsdp  (spec 17.4)
type MASRecord struct { Channel uint8; InstanceID uint8; MessageTypes uint8; Features uint32; HasFeatures bool; Name string }
func EncodeSearchAttrRequest(tid uint16, uuid16 uint16, cont []byte) []byte
func ParseSearchAttrResponse(b []byte) (tid uint16, attrBytes, cont []byte, err error)
func ParseMASRecords(attrLists []byte) ([]MASRecord, error)
func ChooseSMS(recs []MASRecord) (MASRecord, error)          // first with Channel != 0 and SMS_GSM|SMS_CDMA
func Lookup(ctx context.Context, t Transport) ([]byte, error) // continuation loop over a request/response transport
type Transport interface { Exchange(ctx context.Context, req []byte) ([]byte, error) }
// linux only (stubs return ErrUnsupported):
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
func ConnectMAS(rwc io.ReadWriteCloser, rec btsdp.MASRecord) (*MAS, error)   // OBEX Connect, 40 s, features param iff rec.HasFeatures
func (m *MAS) RegisterNotifications() error
func (m *MAS) GetMessage(handle string) (BMessage, error)
func (m *MAS) Done() <-chan struct{}
func (m *MAS) Close() error
// ServeMNS answers one MNS connection; onEvents gets forwarded-or-not events with the instance ID
// (-1 when absent); onConnected is called once after a successful OBEX Connect.
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
type Status struct {
    Enabled bool `json:"enabled"`; State string `json:"state"`; Notifications bool `json:"notifications"`
    Received int `json:"received"`; LastError string `json:"last_error,omitempty"`
}
type Deps struct {
    Enabled bool; Sink Sink; Clock Clock; Logger *slog.Logger
    NewID func(time.Time) string; PhoneConnected func() bool
}
func NewService(d Deps) *Service
func (s *Service) Run(ctx context.Context)
func (s *Service) PhoneUp(d Dialer)
func (s *Service) PhoneDown()
func (s *Service) MNSConnection(rwc io.ReadWriteCloser)    // never blocks; closes rwc itself when not wanted
func (s *Service) MNSDisconnected()
func (s *Service) SetRegistrationError(err error)          // MNS profile registration state for status (nil clears)
func (s *Service) Status() Status

// internal/hooks
type SMSInfo struct { ID, Handle, Type, Text string; From, FromName *string }
func (d *Dispatcher) EmitSMS(m SMSInfo) bool
func BuildSMSPayload(m SMSInfo, test bool) ([]byte, error)
func Env(event, callID, caller, smsFrom string) []string

// internal/bluez (linux; stubs return ErrUnsupported)
const MNSUUID = "00001133-0000-1000-8000-00805f9b34fb"
func MNSProfileOptions() map[string]any            // untagged, tested: the spec 17.8 options incl. ServiceRecord XML
func (m *Manager) RegisterMNSProfile(ctx context.Context, h ProfileHandler) error
func (m *Manager) AdapterAddress() string          // exists
```

## T1 — `internal/obex` codec and stream framing

Files: `internal/obex/obex.go`, `internal/obex/conn.go`, tests.

Tests first: `Marshal` golden bytes (Connect request with Target; Connect response with Connection-ID and Who; final Get with Connection-ID, Name, Type, App-Params; final Put with End-of-Body); header kinds round trip (unicode incl. Cyrillic and empty name `01 00 03`); parse errors (length field mismatch, header length < 3, header past end, odd unicode length, short Connect fields); unknown headers kept; AppParams round trip and truncated TLV. `Conn`: 1-byte chunks; two packets in one Read; length < 3 or > maxIn → `ErrProtocol` and Done closed; `Next` timeout; idle EOF closes Done with `io.EOF`; `Write` with a writer that writes 1 byte per call.

Commit: `feat(obex): packet and header codec with stream framing`.

## T2 — `internal/obex` client and server

Files: `internal/obex/client.go`, `internal/obex/server.go`, tests over `net.Pipe` with a scripted peer.

Every client and server bullet of spec 17.5 and the obex list of 17.12 (Connect headers, wrong Target, request before Connect, Connection-ID mismatch, multipart Put with header-only first packet, size caps, Abort, idle without deadline, operation deadline, Abort-then-close on caps, Disconnect). Use a short fake timeout via package-level variables that tests override (`var packetTimeout = PacketTimeout` etc.) — no real 30 s waits in tests.

Commit: `feat(obex): client and put server`.

## T3 — `internal/sms` parsers

Files: `internal/sms/event.go`, `internal/sms/bmessage.go`, `internal/sms/vcard.go`, tests (table driven, raw strings with explicit `\r\n`).

Every parser case in spec 17.12. Quoted-printable via `mime/quotedprintable`. Error values never contain input text (assert with a sentinel word in the input).

Commit: `feat(sms): MAP event report, bMessage and vCard parsers`.

## T4 — `internal/sms` MAS client and MNS server

Files: `internal/sms/mas.go`, `internal/sms/mns.go`, tests with a scripted fake MSE / fake phone on `net.Pipe`, exact request bytes asserted.

Commit: `feat(sms): MAP access client and notification server`.

## T11 — `internal/btsdp` codec

Files: `internal/btsdp/sdp.go`, `internal/btsdp/element.go`, `internal/btsdp/mas.go`, tests (every btsdp case of spec 17.12; `Lookup` with a fake Transport returning three continuation fragments; round and byte limits).

Commit: `feat(btsdp): SDP service search attribute codec and MAS record choice`.

## T12 — `internal/btsdp` Linux sockets

Files: `internal/btsdp/socket_linux.go`, `internal/btsdp/socket_other.go`. L2CAP SEQPACKET PSM 1 (`unix.SockaddrL2{PSM: 1, Addr: ...}`; check `x/sys/unix` address byte order, `internal/sco` packs addresses itself — follow its approach), 5 s per exchange via poll/deadline on `os.File`; RFCOMM `SOCK_STREAM` with `unix.SockaddrRFCOMM`, bind to adapter address channel 0, `setsockopt(SOL_BLUETOOTH=274, BT_SECURITY=4, struct{level=2 (medium), key_size=0})`, non-blocking connect + 15 s wait for writability + `SO_ERROR`, then `os.NewFile`. Only a compile check (`GOOS=linux GOARCH=arm64 go vet ./...`) plus a test of the address packing helper (untagged).

Commit: `feat(btsdp): L2CAP SDP and RFCOMM sockets (linux)`.

## T5 — `internal/sms` Service

Files: `internal/sms/service.go`, `internal/sms/clock.go`, `internal/sms/service_test.go` (fake clock modelled on `internal/bridge/fakeclock_test.go`, fake Dialer returning `net.Pipe` ends served by the fake MSE from T4 tests, fake Sink, fake MNS phone).

Implement spec 17.7 exactly: loop goroutine owns state; inputs via a buffered channel (`select` on ctx so callers never block after `Run` returned); `Status()` from an atomically stored snapshot; workers tagged with generations; constants as package variables (tests shorten nothing real: the fake clock drives all timers; the OBEX 40 s/30 s/10 s deadlines are inside obex and covered there, so service tests drive failures through the fakes). Every Service bullet in spec 17.12.

Commits: `feat(sms): service lifecycle and retries`, `feat(sms): fetch queue and delivery`.

## T6 — config and hooks

- `internal/config`: `type SMS struct { Enabled bool \`yaml:"enabled"\` }`, `Config.SMS SMS \`yaml:"sms"\``, default false; `KnownEvents` adds `sms.received`; embedded `config.yaml` gets the `sms:` block (spec 17.11) before `hooks:`; tests.
- `internal/hooks`: `SMSInfo`, `BuildSMSPayload` (golden JSON of spec 17.9; null from/from_name), `EmitSMS` returning acceptance (job carries the SMS; payload built in `process`), `Env(event, callID, caller, smsFrom)` (update callers), the dispatcher's finish log for `sms.received` has `sms_id`, exit, timed_out, `stdout_bytes`, `stderr_bytes` and never the streams (test with a hook `cat; cat >&2` style command echoing the payload, log captured).
- `internal/cli/hooks_cmd.go`: sample for `--event sms.received`.

Commits: `feat(config): sms.enabled`, `feat(hooks): sms.received event`.

## T7 — BlueZ MNS profile

`internal/bluez/bluez.go` (untagged): `MNSUUID`, `mnsProfilePath = "/com/siptooth/mns"`, `MNSProfileOptions()` with the exact options and XML of spec 17.8 (test asserts every key and that the XML parses with `encoding/xml` and names channel 16 consistently with `Channel`). `manager_linux.go`: generalise `profileObject` with a label for logs (HFP behaviour unchanged), `RegisterMNSProfile` (export at the MNS path, RegisterProfile, `AlreadyExists` = success). `manager_other.go` stub.

Commit: `feat(bluez): MNS profile with a MAP 1.1 RFCOMM record`.

## T8 — daemon, control, status

- `internal/daemon`: build `sms.Service` (Enabled from config, Sink adapter: `hooks.EmitSMS` + the `sms received` info line of spec 17.10 + `EmitError("sms_unavailable", …)`, `PhoneConnected` from `coord.Status().PhoneConnected`, NewID shared with calls). Run it with its own context; stop order: coordinator, Bluetooth, **sms**, registration, hooks, control, UA. `bluetoothRunner` gains an `smsLink` parameter (interface with `PhoneUp`, `PhoneDown`, `MNSConnection`, `MNSDisconnected`, `SetRegistrationError`); `daemon_test.go` fake updated. `btHandler`: after a successful `Attach`, `sms.PhoneUp(dialer)` where the dialer wraps `btsdp.FindSMSMAS`/`DialRFCOMM` with this session's adapter address and the configured phone; `Disconnected` → `sms.PhoneDown()`. Session loop: MNS registration with its own `mnsRetry` timer (spec 17.8), only when `cfg.SMS.Enabled`; also after `BluezRestarted`. `bluetooth_other.go` updated.
- ID generator: export `bridge.NewCallID` (or move into a tiny shared package) and use it for SMS ids.
- `internal/control`: `type Status struct { bridge.Status; SMS sms.Status \`json:"sms"\` }`, `NewServer(sp StatusProvider, smsStatus func() sms.Status)`, client returns `control.Status`; tests.
- `internal/cli/status_cmd.go`: the `sms:` line; test.

Commits: `feat(control): combined status with SMS`, `feat(daemon): wire SMS forwarding`.

## T9 — doctor

`sms_hooks` (warn when enabled and no hook has `sms.received`; skip when disabled), `sms_obexd` (warn when enabled and a process named `obexd` runs; use the existing `/proc` scan used for `competingHandlerNames`, injectable like the existing tests). Tests.

Commit: `feat(doctor): SMS checks`.

## T10 — docs

`docs/hooks.md` (`sms.received`, env, output not logged, sample, delivery semantics), `docs/operations.md` (enabling SMS, the phone's message-access prompt and setting, status line, troubleshooting: no notifications, RCS/MMS not forwarded, obexd), `docs/setup.md` (optional step), README if it lists events/features, `CLAUDE.md` layout table (`internal/btsdp`, `internal/obex`, `internal/sms`), `config/hooks/` example `sms-log.sh` if the docs reference example scripts.

Commit: `docs: SMS forwarding`.

## Final gate

`make check`, `make race`, `GOOS=linux GOARCH=arm64 go vet ./...`, Linux test run in Docker (`docker run --rm --init -v "$PWD":/src -w /src golang:1.24 go test ./...`) if Docker is available. Then the Fable 5.1 review of the whole change and fixes. Then the owner's hardware test (spec 15, M5) using the `pi-hardware-test` skill.
