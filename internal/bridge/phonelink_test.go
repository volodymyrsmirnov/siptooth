package bridge

import (
	"slices"
	"sync"
	"testing"

	"github.com/volodymyrsmirnov/siptooth/internal/hfp"
)

type linkRecorder struct {
	mu     sync.Mutex
	events []string
}

func (l *linkRecorder) record(up bool, addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if up {
		l.events = append(l.events, "up "+addr)
	} else {
		l.events = append(l.events, "down")
	}
}

func (l *linkRecorder) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

func newLinkRig(t *testing.T) (*rig, *linkRecorder) {
	r := newRig(t, nil)
	rec := &linkRecorder{}
	r.c.d.PhoneLink = rec.record
	return r.start(), rec
}

func TestPhoneLinkUpOnAttachDownOnDetach(t *testing.T) {
	r, rec := newLinkRig(t)
	r.waitFor("up", func() bool { return len(rec.get()) == 1 })
	r.c.Detach()
	r.waitFor("down", func() bool { return len(rec.get()) == 2 })
	if got, want := rec.get(), []string{"up AA:BB:CC:DD:EE:FF", "down"}; !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
	// A second Detach with no phone attached reports nothing.
	r.c.Detach()
	r.settle()
	if n := len(rec.get()); n != 2 {
		t.Fatalf("events after second detach = %q", rec.get())
	}
}

func TestPhoneLinkDownOnHFPDisconnect(t *testing.T) {
	r, rec := newLinkRig(t)
	r.waitFor("up", func() bool { return len(rec.get()) == 1 })
	// The HFP transport's EOF reaches only the coordinator, as an hfp Disconnected event.
	r.tel.send(hfp.Disconnected, "")
	r.waitFor("down", func() bool { return len(rec.get()) == 2 })
	if got := rec.get()[1]; got != "down" {
		t.Fatalf("second event = %q, want down", got)
	}
}

func TestPhoneLinkReattachReportsUpAgain(t *testing.T) {
	r, rec := newLinkRig(t)
	r.waitFor("up", func() bool { return len(rec.get()) == 1 })
	r.c.Detach()
	r.c.Attach(newFakeTel(), r.lis, "AA:BB:CC:DD:EE:FF")
	r.waitFor("up again", func() bool { return len(rec.get()) == 3 })
	if got, want := rec.get(), []string{"up AA:BB:CC:DD:EE:FF", "down", "up AA:BB:CC:DD:EE:FF"}; !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}
