package hooks

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/config"
)

func TestQueueFullDrops(t *testing.T) {
	var buf bytes.Buffer
	d := NewDispatcher(nil, slog.New(slog.NewTextHandler(&buf, nil))) // no worker: the queue fills
	for i := 0; i < 101; i++ {
		d.Emit("call.ringing", CallInfo{ID: "x"})
	}
	if d.Pending() != 100 {
		t.Fatal(d.Pending())
	}
	if !strings.Contains(buf.String(), "hook_queue_full") || strings.Count(buf.String(), "hook_queue_full") != 1 {
		t.Fatalf("log: %q", buf.String())
	}
	d.EmitError("stun_failed", "x") // must not block either
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("timeout")
}

func TestSequentialOrder(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "order")
	// The first event's hook is slow: a concurrent worker would reorder the lines.
	cmd := `if [ "$SIPTOOTH_EVENT" = call.ringing ]; then sleep 0.3; fi; echo "$SIPTOOTH_EVENT $SIPTOOTH_CALL_ID $SIPTOOTH_CALLER" >> ` + out
	d := NewDispatcher([]config.Hook{{ID: "h", Events: []string{"call.ringing", "call.connected", "call.ended"}, Command: cmd, TimeoutSeconds: 5}}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	c := sample()
	for _, ev := range []string{"call.ringing", "call.connected", "call.ended"} {
		d.Emit(ev, c)
	}
	waitFor(t, func() bool { b, _ := os.ReadFile(out); return strings.Count(string(b), "\n") == 3 })
	b, _ := os.ReadFile(out)
	id := c.ID + " +380501234567\n"
	if want := "call.ringing " + id + "call.connected " + id + "call.ended " + id; string(b) != want {
		t.Fatalf("got %q want %q", b, want)
	}
}

func TestOnlySubscribedHooksRunAndPayloadOnStdin(t *testing.T) {
	dir := t.TempDir()
	hit, miss := filepath.Join(dir, "hit.json"), filepath.Join(dir, "miss")
	d := NewDispatcher([]config.Hook{
		{ID: "a", Events: []string{"system.error"}, Command: "cat > " + hit, TimeoutSeconds: 5},
		{ID: "b", Events: []string{"call.ended"}, Command: "touch " + miss, TimeoutSeconds: 5},
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	d.EmitError("routing", "boom")
	waitFor(t, func() bool { b, _ := os.ReadFile(hit); return bytes.HasSuffix(b, []byte("}\n")) })
	b, _ := os.ReadFile(hit)
	if !strings.Contains(string(b), `"error":{"code":"routing","detail":"boom"}`) {
		t.Fatalf("%s", b)
	}
	if _, err := os.Stat(miss); err == nil {
		t.Fatal("unsubscribed hook ran")
	}
}

func TestWithheldCallerEnvEmpty(t *testing.T) {
	out := filepath.Join(t.TempDir(), "o")
	d := NewDispatcher([]config.Hook{{ID: "h", Events: []string{"call.ringing"}, Command: `echo "[$SIPTOOTH_CALLER]" > ` + out, TimeoutSeconds: 5}}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	d.Emit("call.ringing", CallInfo{ID: "x", CallerWithheld: true})
	waitFor(t, func() bool { b, _ := os.ReadFile(out); return string(b) == "[]\n" })
}

func TestOutstandingCoversQueuedAndInFlight(t *testing.T) {
	dir := t.TempDir()
	d := NewDispatcher([]config.Hook{{ID: "h", Events: []string{"call.ended"}, Command: "sleep 0.2; touch " + filepath.Join(dir, "done"), TimeoutSeconds: 5}}, nil)
	d.Emit("call.ended", sample())
	d.Emit("call.ended", sample())
	if d.Outstanding() != 2 {
		t.Fatalf("queued: %d", d.Outstanding())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	if d.Pending() != 1 || d.Outstanding() != 2 {
		t.Fatalf("one in flight: pending %d outstanding %d", d.Pending(), d.Outstanding())
	}
	waitFor(t, func() bool { return d.Outstanding() == 0 })
}
