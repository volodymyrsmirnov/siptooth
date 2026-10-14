package bridge

import "time"

// Timer is a one-shot timer.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

// Ticker delivers ticks at a fixed period.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// Clock abstracts time so the pump's pacing is testable.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
	NewTicker(d time.Duration) Ticker
}

// RealClock is the wall clock.
type RealClock struct{}

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time { return r.t.C }
func (r realTimer) Stop() bool          { return r.t.Stop() }

type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time { return r.t.C }
func (r realTicker) Stop()               { r.t.Stop() }

func (RealClock) Now() time.Time                   { return time.Now() }
func (RealClock) NewTimer(d time.Duration) Timer   { return realTimer{time.NewTimer(d)} }
func (RealClock) NewTicker(d time.Duration) Ticker { return realTicker{time.NewTicker(d)} }
