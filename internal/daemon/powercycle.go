package daemon

import (
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/bridge"
	"github.com/volodymyrsmirnov/siptooth/internal/config"
)

// The periodic adapter power cycle (spec section 3.1, 2026-10-06 iteration 7): after a night
// connected, the controller kept the SCO link up but delivered noise both ways until the
// adapter was powered off and on again.
const (
	powerCycleOffTime   = 2 * time.Second // the adapter stays off this long (the manual fix used 2 s)
	powerCycleBusyRetry = time.Minute     // a cycle that falls due during a call is retried this often
)

// powerCycleInterval returns how often the adapter is power-cycled; 0 when it is off.
func powerCycleInterval(bt config.Bluetooth) time.Duration {
	return time.Duration(bt.PowerCycleMinutes) * time.Minute
}

// powerCycleAllowed reports whether a due power cycle may run now: only while no call is in
// progress, since powering off drops the call's audio and HFP link.
func powerCycleAllowed(st bridge.Status) bool { return st.State == "idle" }
