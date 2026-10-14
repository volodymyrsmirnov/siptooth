package bluez

import (
	"testing"
	"time"
)

func set(a ...string) map[string]bool {
	m := map[string]bool{}
	for _, s := range a {
		m[s] = true
	}
	return m
}

func TestSelectPairedDevice(t *testing.T) {
	cases := []struct {
		name                 string
		before, now, handled map[string]bool
		want                 string
		ok                   bool
	}{
		{"nothing new", set("A"), set("A"), nil, "", false},
		{"newly paired", set("A"), set("A", "B"), nil, "B", true},
		{"already bonded and handled", set("A"), set("A"), set("A"), "A", true},
		{"handled but not yet paired", set(), set(), set("A"), "", false},
		{"handled wins over unrelated new", set("A"), set("A", "B"), set("A"), "A", true},
		{"handled other device wins", set(), set("B", "C"), set("C"), "C", true},
	}
	for _, c := range cases {
		got, ok := selectPairedDevice(c.before, c.now, c.handled)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: got %q,%v want %q,%v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestAddrFromDevicePath(t *testing.T) {
	if a := addrFromDevicePath("/org/bluez/hci0/dev_aa_bb_cc_dd_ee_ff"); a != "AA:BB:CC:DD:EE:FF" {
		t.Fatal(a)
	}
	if addrFromDevicePath("/org/bluez/hci0") != "" {
		t.Fatal("expected empty")
	}
}

// Hardware finding 2026-10-05: an already-bonded phone shows Paired=true while the passkey prompt
// is still open; it must count only after the user approved and the bond had time to settle.
func TestApprovalsSettle(t *testing.T) {
	var a approvals
	t0 := time.Unix(1000, 0)
	before := map[string]bool{"0C:7D:B0:76:0E:F1": true}
	now := map[string]bool{"0C:7D:B0:76:0E:F1": true}
	if _, ok := selectPairedDevice(before, now, a.settled(t0, bondSettle)); ok {
		t.Fatal("selected an already-bonded phone before any approval")
	}
	a.note("0C:7D:B0:76:0E:F1", t0)
	if _, ok := selectPairedDevice(before, now, a.settled(t0.Add(time.Second), bondSettle)); ok {
		t.Fatal("selected before the bond settled")
	}
	addr, ok := selectPairedDevice(before, now, a.settled(t0.Add(bondSettle), bondSettle))
	if !ok || addr != "0C:7D:B0:76:0E:F1" {
		t.Fatalf("got %q %v", addr, ok)
	}
	a.note("", t0) // ignored
}
