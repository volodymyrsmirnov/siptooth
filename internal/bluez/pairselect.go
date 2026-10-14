package bluez

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// bondSettle is how long after an approved request an already-bonded phone counts as re-paired,
// giving bluetoothd time to finish the bond before the pairing agent is unregistered.
const bondSettle = 3 * time.Second

// approvals records the devices whose pairing request the user approved, and when.
type approvals struct {
	mu sync.Mutex
	at map[string]time.Time
}

func (a *approvals) note(addr string, t time.Time) {
	if addr == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.at == nil {
		a.at = map[string]time.Time{}
	}
	a.at[addr] = t
}

// settled returns the devices approved at least settle before now.
func (a *approvals) settled(now time.Time, settle time.Duration) map[string]bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]bool, len(a.at))
	for k, t := range a.at {
		if now.Sub(t) >= settle {
			out[k] = true
		}
	}
	return out
}

// selectPairedDevice picks the phone among the currently paired devices (upper-case addresses).
// A device the agent handled a pairing request for wins even if it was already paired before
// this run (one-sided bond: the phone forgot us and paired again); otherwise any device that
// was not paired at start is accepted. Ties resolve to the lexically smallest address.
func selectPairedDevice(before, now, handled map[string]bool) (string, bool) {
	var fresh, seen []string
	for addr := range now {
		if handled[addr] {
			seen = append(seen, addr)
		} else if !before[addr] {
			fresh = append(fresh, addr)
		}
	}
	for _, c := range [][]string{seen, fresh} {
		if len(c) > 0 {
			sort.Strings(c)
			return c[0], true
		}
	}
	return "", false
}

// addrFromDevicePath extracts the upper-case address from ".../dev_AA_BB_CC_DD_EE_FF".
func addrFromDevicePath(path string) string {
	i := strings.LastIndex(path, "/dev_")
	if i < 0 {
		return ""
	}
	return strings.ToUpper(strings.ReplaceAll(path[i+len("/dev_"):], "_", ":"))
}
