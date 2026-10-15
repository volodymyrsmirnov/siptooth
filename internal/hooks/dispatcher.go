package hooks

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/config"
)

const (
	queueSize       = 100
	dropLogInterval = 10 * time.Second
	maxHookTimeout  = 300 * time.Second
)

type job struct {
	event      string
	call       CallInfo
	sms        SMSInfo
	errCode    string
	errDetail  string
	occurredAt time.Time
}

// Dispatcher delivers hook events on a single worker goroutine (Run) fed by a bounded queue.
type Dispatcher struct {
	hooks []config.Hook
	log   *slog.Logger
	queue chan job
	// outstanding counts jobs queued or being delivered, so shutdown can wait
	// for the last events (Outstanding).
	outstanding atomic.Int64

	mu       sync.Mutex
	dropped  int
	lastDrop time.Time
}

// NewDispatcher returns a dispatcher; call Run to start delivery.
func NewDispatcher(hooks []config.Hook, logger *slog.Logger) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Dispatcher{hooks: hooks, log: logger, queue: make(chan job, queueSize)}
}

// Pending reports the number of queued, not yet started deliveries.
func (d *Dispatcher) Pending() int { return len(d.queue) }

// Outstanding reports the deliveries queued or in progress; the daemon polls
// it at shutdown to let the final events out.
func (d *Dispatcher) Outstanding() int { return int(d.outstanding.Load()) }

// enqueue never blocks. On overflow the event is dropped and a rate-limited warning
// (hook_queue_full) is logged; it never triggers system.error hooks.
func (d *Dispatcher) enqueue(j job) bool {
	j.occurredAt = time.Now()
	d.outstanding.Add(1)
	select {
	case d.queue <- j:
		return true
	default:
	}
	d.outstanding.Add(-1)
	d.mu.Lock()
	d.dropped++
	n := d.dropped
	log := d.lastDrop.IsZero() || time.Since(d.lastDrop) >= dropLogInterval
	if log {
		d.lastDrop, d.dropped = time.Now(), 0
	}
	d.mu.Unlock()
	if log {
		d.log.Warn("hooks: queue full, dropping events", "code", "hook_queue_full", "event", j.event, "call_id", j.call.ID, "dropped", n)
	}
	return false
}

// Emit queues delivery of a call.* event to every subscribed hook. It never blocks.
func (d *Dispatcher) Emit(event string, c CallInfo) {
	d.enqueue(job{event: event, call: c.clone()})
}

// EmitSMS queues delivery of an sms.received event and reports whether the queue accepted it.
// It never blocks.
func (d *Dispatcher) EmitSMS(m SMSInfo) bool {
	return d.enqueue(job{event: "sms.received", sms: m.clone()})
}

// EmitError queues a system.error delivery. It never blocks.
func (d *Dispatcher) EmitError(code, detail string) {
	d.enqueue(job{event: "system.error", errCode: code, errDetail: detail})
}

// Run processes jobs sequentially until ctx ends.
func (d *Dispatcher) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-d.queue:
			d.process(ctx, j)
			d.outstanding.Add(-1)
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func (d *Dispatcher) process(ctx context.Context, j job) {
	var payload []byte
	var err error
	switch j.event {
	case "system.error":
		payload, err = buildErrorPayload(j.errCode, j.errDetail, false, j.occurredAt)
	case "sms.received":
		payload, err = buildSMSPayload(j.sms, false, j.occurredAt)
	default:
		payload, err = buildPayload(j.event, j.call, false, j.occurredAt)
	}
	if err != nil {
		d.log.Error("hooks: payload", "event", j.event, "err", err)
		return
	}
	caller, smsFrom := "", ""
	if j.call.Caller != nil {
		caller = *j.call.Caller
	}
	if j.sms.From != nil {
		smsFrom = *j.sms.From
	}
	for _, h := range d.hooks {
		if !contains(h.Events, j.event) {
			continue
		}
		timeout := time.Duration(h.TimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = config.DefaultHookTimeoutSeconds * time.Second
		}
		if timeout > maxHookTimeout {
			timeout = maxHookTimeout
		}
		res := Run(ctx, h.Command, timeout, payload, Env(j.event, j.call.ID, caller, smsFrom))
		if j.event == "sms.received" {
			// The streams may echo the message: log only exit status and sizes (spec 17.9).
			d.log.Info("hooks: finished", "hook", h.ID, "event", j.event, "sms_id", j.sms.ID, "exit", res.ExitCode,
				"timed_out", res.TimedOut, "stdout_bytes", res.StdoutBytes, "stderr_bytes", res.StderrBytes)
			continue
		}
		d.log.Info("hooks: finished", "hook", h.ID, "event", j.event, "call_id", j.call.ID, "exit", res.ExitCode,
			"timed_out", res.TimedOut, "stdout", res.Stdout, "stderr", res.Stderr, "truncated", res.Truncated)
	}
}
