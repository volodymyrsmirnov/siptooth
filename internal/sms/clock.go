package sms

import "time"

// Clock is the service's time source; tests inject a fake so no timer needs a real wait.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

// Timer is a one-shot timer.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

// SystemClock is the real clock.
type SystemClock struct{}

// Now returns the current time.
func (SystemClock) Now() time.Time { return time.Now() }

// NewTimer starts a real timer.
func (SystemClock) NewTimer(d time.Duration) Timer { return sysTimer{time.NewTimer(d)} }

type sysTimer struct{ t *time.Timer }

func (t sysTimer) C() <-chan time.Time { return t.t.C }
func (t sysTimer) Stop() bool          { return t.t.Stop() }

// slot holds at most one armed timer of the loop; its channel is nil while unarmed, so a
// stopped timer's stale tick is never read.
type slot struct{ t Timer }

func (s *slot) C() <-chan time.Time {
	if s.t == nil {
		return nil
	}
	return s.t.C()
}

func (s *slot) set(c Clock, d time.Duration) {
	s.stop()
	s.t = c.NewTimer(d)
}

func (s *slot) stop() {
	if s.t != nil {
		s.t.Stop()
		s.t = nil
	}
}

func (s *slot) armed() bool { return s.t != nil }
