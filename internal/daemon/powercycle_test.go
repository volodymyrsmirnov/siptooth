package daemon

import (
	"testing"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/bridge"
	"github.com/volodymyrsmirnov/siptooth/internal/config"
)

func TestPowerCycleInterval(t *testing.T) {
	if d := powerCycleInterval(config.Bluetooth{PowerCycleMinutes: 60}); d != time.Hour {
		t.Fatalf("60 minutes: %v", d)
	}
	if d := powerCycleInterval(config.Bluetooth{}); d != 0 {
		t.Fatalf("off: %v", d)
	}
}

func TestPowerCycleOnlyWhileIdle(t *testing.T) {
	if !powerCycleAllowed(bridge.Status{State: "idle", PhoneConnected: true}) {
		t.Fatal("idle with phone: want a cycle")
	}
	if !powerCycleAllowed(bridge.Status{State: "idle"}) {
		t.Fatal("idle without phone: want a cycle")
	}
	for _, s := range []string{"dialing", "answering", "bridged", "ending"} {
		if powerCycleAllowed(bridge.Status{State: s, PhoneConnected: true}) {
			t.Fatalf("%s: a cycle would drop the call", s)
		}
	}
}
