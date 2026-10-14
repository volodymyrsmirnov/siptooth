package bridge

import (
	"sync"
	"time"
)

// fakeClock fires timers and tickers only when Advance moves time. Advance
// steps from event to event and yields after each so the code under test can
// react before the next one fires.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	items []*fakeItem
}

type fakeItem struct {
	ch      chan time.Time
	next    time.Time
	period  time.Duration // 0 for a one-shot timer
	stopped bool
	c       *fakeClock
}

func (i *fakeItem) C() <-chan time.Time { return i.ch }
func (i *fakeItem) Stop() bool {
	i.c.mu.Lock()
	defer i.c.mu.Unlock()
	was := !i.stopped
	i.stopped = true
	return was
}

type fakeTicker struct{ *fakeItem }

func (t fakeTicker) Stop() { t.fakeItem.Stop() }

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *fakeClock) add(d, period time.Duration) *fakeItem {
	c.mu.Lock()
	defer c.mu.Unlock()
	it := &fakeItem{ch: make(chan time.Time, 1), next: c.now.Add(d), period: period, c: c}
	c.items = append(c.items, it)
	return it
}

func (c *fakeClock) NewTimer(d time.Duration) Timer   { return c.add(d, 0) }
func (c *fakeClock) NewTicker(d time.Duration) Ticker { return fakeTicker{c.add(d, d)} }

// Advance moves time forward by d, firing everything due on the way in order.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var due *fakeItem
		for _, it := range c.items {
			if !it.stopped && !it.next.After(target) && (due == nil || it.next.Before(due.next)) {
				due = it
			}
		}
		if due == nil {
			c.now = target
			c.mu.Unlock()
			return
		}
		c.now = due.next
		at := due.next
		if due.period > 0 {
			due.next = due.next.Add(due.period)
		} else {
			due.stopped = true
		}
		c.mu.Unlock()
		select {
		case due.ch <- at:
		default: // like a real ticker, a tick is dropped if the receiver is behind
		}
		time.Sleep(150 * time.Microsecond)
	}
}

// armed is the number of timers and tickers created so far.
func (c *fakeClock) armed() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.items) }
