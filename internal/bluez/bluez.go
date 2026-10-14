// Package bluez talks to bluetoothd over the system D-Bus (spec sections 3.1, 3.2, 3.5).
package bluez

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/godbus/dbus/v5"
)

const (
	HFPHFUUID                   = "0000111e-0000-1000-8000-00805f9b34fb" // local HF profile (RegisterProfile)
	HFPAGUUID                   = "0000111f-0000-1000-8000-00805f9b34fb" // remote AG service (ConnectProfile)
	ManufacturerBroadcom uint16 = 15
	profilePath                 = "/com/siptooth/hfp"
	agentPath                   = "/com/siptooth/agent"
	profileName                 = "Siptooth" // HFP profile Name in RegisterProfile
	pairAlias                   = "siptooth" // adapter alias shown to the phone while pairing
)

var ErrUnsupported = errors.New("bluez: not supported on this platform")

// AlreadyRegistered reports a RegisterProfile error saying the profile is registered already,
// for example by an earlier attempt that timed out on our side but completed in bluetoothd.
func AlreadyRegistered(err error) bool {
	const name = "org.bluez.Error.AlreadyExists"
	var pe *dbus.Error // what godbus returns for an error reply
	if errors.As(err, &pe) {
		return pe != nil && pe.Name == name
	}
	var ve dbus.Error
	return errors.As(err, &ve) && ve.Name == name
}

// RoutingPlan is what an adapter's manufacturer means for the vendor SCO routing (spec 3.5).
type RoutingPlan int

const (
	// RoutingUnknown: the manufacturer could not be read, so audio readiness is unverified.
	RoutingUnknown RoutingPlan = iota
	// RoutingNotNeeded: a known non-Broadcom controller; no vendor command applies.
	RoutingNotNeeded
	// RoutingBroadcom: apply and verify the Broadcom SCO-over-HCI routing.
	RoutingBroadcom
)

func (p RoutingPlan) String() string {
	switch p {
	case RoutingNotNeeded:
		return "not_needed"
	case RoutingBroadcom:
		return "broadcom"
	}
	return "unknown"
}

// PlanRouting decides the routing plan from Adapter1.Manufacturer and the error of reading
// it. A failed read is never treated as "not Broadcom": company identifier 0 is a real
// company, and an unreadable property on a Broadcom controller must not skip verification.
func PlanRouting(manufacturer uint16, readErr error) RoutingPlan {
	switch {
	case readErr != nil:
		return RoutingUnknown
	case manufacturer == ManufacturerBroadcom:
		return RoutingBroadcom
	}
	return RoutingNotNeeded
}

// RoutingReadError is returned by EnsureSCORouting when the current routing could not be
// read at all (opening the raw HCI socket or the read command failed), as opposed to a
// failure to change it.
type RoutingReadError struct{ Err error }

func (e *RoutingReadError) Error() string { return "read SCO routing: " + e.Err.Error() }
func (e *RoutingReadError) Unwrap() error { return e.Err }

// RoutingVerdict is what an EnsureSCORouting result means for answering calls.
type RoutingVerdict int

const (
	// RoutingFailed: the routing is not HCI or could not be established; do not answer.
	RoutingFailed RoutingVerdict = iota
	// RoutingVerified: SCO is routed over HCI (read back after any write).
	RoutingVerified
	// RoutingAssumed: without CAP_NET_RAW the routing cannot even be read; it is assumed
	// to be HCI (the usual state once set) and the daemon answers.
	RoutingAssumed
	// RoutingNeedsCapability: the read showed a non-HCI routing and changing it was
	// denied; the adapter needs CAP_NET_RAW once to switch it.
	RoutingNeedsCapability
)

func (v RoutingVerdict) String() string {
	switch v {
	case RoutingVerified:
		return "verified"
	case RoutingAssumed:
		return "assumed"
	case RoutingNeedsCapability:
		return "needs_capability"
	}
	return "failed"
}

// Ready reports whether calls may be answered.
func (v RoutingVerdict) Ready() bool { return v == RoutingVerified || v == RoutingAssumed }

// JudgeRouting turns EnsureSCORouting's result (routing value after, error) into a verdict.
// Only a permission error on the read is assumed fine; a denied write after a successful
// read of a non-HCI value is never assumed.
func JudgeRouting(after uint8, err error) RoutingVerdict {
	var readErr *RoutingReadError
	switch {
	case err == nil && after == scoRoutingHCI:
		return RoutingVerified
	case err == nil:
		return RoutingFailed
	case errors.As(err, &readErr) && errors.Is(err, os.ErrPermission):
		return RoutingAssumed
	case errors.As(err, &readErr):
		return RoutingFailed
	case errors.Is(err, os.ErrPermission):
		return RoutingNeedsCapability
	}
	return RoutingFailed
}

type ChangeKind int

const (
	AdapterUp ChangeKind = iota + 1
	AdapterDown
	BluezRestarted
	BluezLost // bluetoothd left the bus; profile registration is gone until BluezRestarted
	PhoneConnected
	PhoneDisconnected
)

type Change struct{ Kind ChangeKind }

// ProfileHandler receives the RFCOMM connection for the configured phone.
type ProfileHandler interface {
	NewConnection(device string, fd *os.File) error
	Disconnected(device string)
}

func DevicePath(adapter, address string) string {
	return "/org/bluez/" + adapter + "/dev_" + strings.ReplaceAll(strings.ToUpper(address), ":", "_")
}

func AdapterIndexFromName(name string) (int, error) {
	if !strings.HasPrefix(name, "hci") {
		return 0, fmt.Errorf("adapter name %q must look like hci0", name)
	}
	return strconv.Atoi(strings.TrimPrefix(name, "hci"))
}
