package bluez

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

func TestDevicePath(t *testing.T) {
	if p := DevicePath("hci0", "aa:bb:cc:dd:ee:ff"); p != "/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF" {
		t.Fatal(p)
	}
}

func TestAdapterIndexFromName(t *testing.T) {
	if i, err := AdapterIndexFromName("hci0"); err != nil || i != 0 {
		t.Fatal(i, err)
	}
	if i, _ := AdapterIndexFromName("hci12"); i != 12 {
		t.Fatal(i)
	}
	if _, err := AdapterIndexFromName("eth0"); err == nil {
		t.Fatal("expected error")
	}
}

// K: only a successful property read may grant the non-Broadcom readiness exemption.
func TestPlanRouting(t *testing.T) {
	cases := []struct {
		manu uint16
		err  error
		want RoutingPlan
	}{
		{ManufacturerBroadcom, nil, RoutingBroadcom},
		{2, nil, RoutingNotNeeded}, // Intel
		{0, nil, RoutingNotNeeded}, // Ericsson: a real company identifier, not "unknown"
		{0, errors.New("no such property"), RoutingUnknown},
		{ManufacturerBroadcom, errors.New("stale read"), RoutingUnknown},
	}
	for _, c := range cases {
		if got := PlanRouting(c.manu, c.err); got != c.want {
			t.Errorf("PlanRouting(%d, %v) = %v, want %v", c.manu, c.err, got, c.want)
		}
	}
}

func TestJudgeRouting(t *testing.T) {
	eperm := fmt.Errorf("hci write: %w", unix.EPERM)
	cases := []struct {
		name  string
		after uint8
		err   error
		want  RoutingVerdict
		ready bool
	}{
		{"verified", 1, nil, RoutingVerified, true},
		{"still not HCI", 0, nil, RoutingFailed, false},
		// No CAP_NET_RAW: the routing cannot even be read, so it is assumed.
		{"read denied", 0, &RoutingReadError{Err: eperm}, RoutingAssumed, true},
		// The read worked (non-HCI) but the write was denied: never assume.
		{"write denied", 0, eperm, RoutingNeedsCapability, false},
		{"read failed otherwise", 0, &RoutingReadError{Err: errors.New("hci: timeout")}, RoutingFailed, false},
		{"write failed otherwise", 0, errors.New("hci: write pcm params status 0x1"), RoutingFailed, false},
	}
	for _, c := range cases {
		got := JudgeRouting(c.after, c.err)
		if got != c.want || got.Ready() != c.ready {
			t.Errorf("%s: %v (ready %v), want %v (ready %v)", c.name, got, got.Ready(), c.want, c.ready)
		}
	}
	if !errors.Is(&RoutingReadError{Err: eperm}, os.ErrPermission) {
		t.Fatal("RoutingReadError must unwrap")
	}
}

// A registration retried after a timed-out attempt that bluetoothd did complete must count as
// registered, or the daemon would retry forever.
func TestAlreadyRegistered(t *testing.T) {
	for _, already := range []error{
		fmt.Errorf("RegisterProfile: %w", dbus.NewError("org.bluez.Error.AlreadyExists", []any{"Already Exists"})), // as godbus returns it
		dbus.Error{Name: "org.bluez.Error.AlreadyExists"},
	} {
		if !AlreadyRegistered(already) {
			t.Fatalf("%v not recognised", already)
		}
	}
	for _, err := range []error{nil, errors.New("timeout"), dbus.NewError("org.bluez.Error.InvalidArguments", nil)} {
		if AlreadyRegistered(err) {
			t.Fatalf("%v taken as already registered", err)
		}
	}
}

func TestProfilePathsAreSiptooth(t *testing.T) {
	for name, v := range map[string]string{
		"profilePath": profilePath, "mnsProfilePath": mnsProfilePath, "agentPath": agentPath,
		"profileName": profileName, "pairAlias": pairAlias,
	} {
		l := strings.ToLower(v)
		if !strings.Contains(l, "siptooth") || strings.Contains(l, "allochka") {
			t.Errorf("%s = %q", name, v)
		}
	}
}

func TestMNSProfileOptions(t *testing.T) {
	o := MNSProfileOptions()
	if len(o) != 5 {
		t.Fatalf("unexpected option set: %v", o)
	}
	want := map[string]any{"Role": "server", "Channel": uint16(16), "RequireAuthentication": true, "RequireAuthorization": false}
	for k, v := range want {
		if o[k] != v {
			t.Errorf("%s = %#v, want %#v", k, o[k], v)
		}
	}
	if MNSUUID != "00001133-0000-1000-8000-00805f9b34fb" {
		t.Fatal(MNSUUID)
	}
	rec, ok := o["ServiceRecord"].(string)
	if !ok {
		t.Fatalf("ServiceRecord: %T", o["ServiceRecord"])
	}
	// The XML must parse and name the same RFCOMM channel as the Channel option.
	type attr struct {
		ID   string `xml:"id,attr"`
		Body string `xml:",innerxml"`
	}
	var doc struct {
		Attrs []attr `xml:"attribute"`
	}
	if err := xml.Unmarshal([]byte(rec), &doc); err != nil {
		t.Fatal(err)
	}
	byID := map[string]string{}
	for _, a := range doc.Attrs {
		byID[a.ID] = a.Body
	}
	for _, id := range []string{"0x0001", "0x0004", "0x0005", "0x0009", "0x0100"} {
		if byID[id] == "" {
			t.Errorf("attribute %s missing", id)
		}
	}
	if !strings.Contains(byID["0x0001"], `value="0x1133"`) || !strings.Contains(byID["0x0009"], `value="0x1134"`) ||
		!strings.Contains(byID["0x0009"], `value="0x0101"`) || !strings.Contains(byID["0x0100"], "Siptooth MNS") {
		t.Errorf("record content: %v", byID)
	}
	m := regexp.MustCompile(`<uuid value="0x0003"/><uint8 value="(0x[0-9a-f]+)"/>`).FindStringSubmatch(byID["0x0004"])
	if m == nil {
		t.Fatalf("no RFCOMM channel in %s", byID["0x0004"])
	}
	ch, err := strconv.ParseUint(m[1], 0, 8)
	if err != nil || uint16(ch) != o["Channel"] {
		t.Fatalf("record channel %v vs Channel option %v", m[1], o["Channel"])
	}
}

func TestDBusErrorNamed(t *testing.T) {
	const n = "org.bluez.Error.DoesNotExist"
	e := dbus.NewError(n, nil) // *dbus.Error, as godbus returns it
	if !dbusErrorNamed(e, n) || !dbusErrorNamed(*e, n) || !dbusErrorNamed(fmt.Errorf("wrapped: %w", e), n) {
		t.Fatal("error not recognised")
	}
	if dbusErrorNamed(errors.New("x"), n) || dbusErrorNamed(nil, n) || dbusErrorNamed(dbus.NewError("other", nil), n) {
		t.Fatal("false positive")
	}
}
