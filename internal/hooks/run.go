package hooks

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/config"
)

// OutputLimit caps captured stdout and stderr each (spec section 10).
const OutputLimit = 8 * 1024

type Result struct {
	ExitCode  int
	TimedOut  bool
	Stdout    string
	Stderr    string
	Truncated bool
	// StdoutBytes and StderrBytes are the totals the command wrote, including
	// bytes beyond the OutputLimit capture.
	StdoutBytes, StderrBytes int64
	StartedAt                time.Time
	FinishedAt               time.Time
}

// cappedBuffer keeps the first limit bytes and discards (but counts) the rest.
type cappedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
	total     int64
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total += int64(len(p))
	room := c.limit - c.buf.Len()
	if room <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		c.truncated = true
		c.buf.Write(p[:room])
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

// Env returns the daemon environment minus the SIP password, with SIPTOOTH_EVENT,
// SIPTOOTH_CALL_ID, SIPTOOTH_CALLER (empty when withheld) and SIPTOOTH_SMS_FROM
// (the sender of an sms.received event; empty otherwise or when unknown) set.
func Env(event, callID, caller, smsFrom string) []string {
	own := map[string]bool{"SIPTOOTH_EVENT": true, "SIPTOOTH_CALL_ID": true, "SIPTOOTH_CALLER": true, "SIPTOOTH_SMS_FROM": true, config.PasswordVar: true}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if own[k] {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "SIPTOOTH_EVENT="+event, "SIPTOOTH_CALL_ID="+callID, "SIPTOOTH_CALLER="+caller, "SIPTOOTH_SMS_FROM="+smsFrom)
}

// Run executes command with /bin/sh -c, payload on stdin, in its own process group.
func Run(ctx context.Context, command string, timeout time.Duration, payload []byte, env []string) Result {
	res := Result{StartedAt: time.Now().UTC()}
	argv := []string{"/bin/sh", "-c", command}
	if hasAmbientCaps() {
		if sp, err := exec.LookPath("setpriv"); err == nil {
			argv = append([]string{sp, "--inh-caps=-all", "--ambient-caps=-all", "--"}, argv...)
		}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(payload)
	out := &cappedBuffer{limit: OutputLimit}
	errb := &cappedBuffer{limit: OutputLimit}
	cmd.Stdout, cmd.Stderr = out, errb
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		res.ExitCode = 127
		res.Stderr = err.Error()
		res.FinishedAt = time.Now().UTC()
		return res
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	pgid := cmd.Process.Pid
	defer syscall.Kill(-pgid, syscall.SIGKILL) // every completion path: no descendant outlives the hook
	select {
	case waitErr = <-done:
	case <-time.After(timeout):
		res.TimedOut = true
		syscall.Kill(-pgid, syscall.SIGTERM)
		select {
		case waitErr = <-done:
		case <-time.After(2 * time.Second):
			syscall.Kill(-pgid, syscall.SIGKILL)
			waitErr = <-done
		}
	case <-ctx.Done():
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		waitErr = <-done
	}
	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
		res.ExitCode = 0
	case errors.Is(waitErr, exec.ErrWaitDelay) && cmd.ProcessState != nil:
		// The shell exited but a descendant held stdout/stderr open past WaitDelay;
		// report the shell's own status (the deferred group kill reaps the descendant).
		res.ExitCode = cmd.ProcessState.ExitCode()
		if res.ExitCode < 0 {
			res.ExitCode = 128
		}
	case errors.As(waitErr, &exitErr):
		res.ExitCode = exitErr.ExitCode()
		if res.ExitCode < 0 {
			res.ExitCode = 128 // killed by signal
		}
	default:
		res.ExitCode = 126
	}
	res.Stdout, res.Stderr = out.buf.String(), errb.buf.String()
	res.StdoutBytes, res.StderrBytes = out.total, errb.total
	res.Truncated = out.truncated || errb.truncated
	res.FinishedAt = time.Now().UTC()
	return res
}
