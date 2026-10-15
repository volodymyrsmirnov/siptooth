package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestEnvStripsPassword(t *testing.T) {
	t.Setenv("SIPTOOTH_SIP_PASSWORD", "hunter2")
	res := Run(context.Background(), "env", time.Second, nil, Env("call.ended", "c42", "+380501234567", ""))
	if strings.Contains(res.Stdout, "hunter2") || strings.Contains(res.Stdout, "SIPTOOTH_SIP_PASSWORD") {
		t.Fatalf("password leaked: %s", res.Stdout)
	}
	for _, want := range []string{"SIPTOOTH_EVENT=call.ended\n", "SIPTOOTH_CALL_ID=c42\n", "SIPTOOTH_CALLER=+380501234567\n", "PATH="} {
		if !strings.Contains(res.Stdout, want) {
			t.Fatalf("missing %q in %s", want, res.Stdout)
		}
	}
}

func TestEnvKeepsCustomVariable(t *testing.T) {
	t.Setenv("MY_HOOK_TOKEN", "abc123")
	res := Run(context.Background(), "echo $MY_HOOK_TOKEN", time.Second, nil, Env("e", "c", "", ""))
	if res.Stdout != "abc123\n" {
		t.Fatalf("%+v", res)
	}
}

func TestEnvOverridesStaleSiptoothVars(t *testing.T) {
	t.Setenv("SIPTOOTH_EVENT", "stale")
	n := 0
	for _, kv := range Env("call.ringing", "id", "", "") {
		if strings.HasPrefix(kv, "SIPTOOTH_EVENT=") {
			n++
			if kv != "SIPTOOTH_EVENT=call.ringing" {
				t.Fatal(kv)
			}
		}
	}
	if n != 1 {
		t.Fatal(n)
	}
}

func TestRunOutputCap8KiB(t *testing.T) {
	res := Run(context.Background(), "yes | head -c 20000; yes | head -c 20000 >&2", 5*time.Second, nil, Env("e", "c", "", ""))
	if OutputLimit != 8*1024 || len(res.Stdout) != 8*1024 || len(res.Stderr) != 8*1024 || !res.Truncated {
		t.Fatalf("%d %d %v", len(res.Stdout), len(res.Stderr), res.Truncated)
	}
}

func TestRunMetacharactersStayData(t *testing.T) {
	payload := []byte(`{"x":"$(touch /tmp/siptooth-pwned); \"; rm -rf /; ` + "`id`" + `"}` + "\n")
	res := Run(context.Background(), "cat", 5*time.Second, payload, Env("call.ended", "c1", "", ""))
	if res.ExitCode != 0 || res.TimedOut || res.Stdout != string(payload) {
		t.Fatalf("%+v", res)
	}
	if _, err := os.Stat("/tmp/siptooth-pwned"); err == nil {
		t.Fatal("payload was interpreted by the shell")
	}
}

func TestRunCapturesStreamsSeparately(t *testing.T) {
	res := Run(context.Background(), "echo out; echo err >&2; exit 3", time.Second, nil, Env("e", "c", "", ""))
	if res.ExitCode != 3 || res.Stdout != "out\n" || res.Stderr != "err\n" {
		t.Fatalf("%+v", res)
	}
	if res.FinishedAt.Before(res.StartedAt) {
		t.Fatal("timestamps")
	}
}

func TestRunOutputCap(t *testing.T) {
	res := Run(context.Background(), "yes | head -c 20000", 5*time.Second, nil, Env("e", "c", "", ""))
	if len(res.Stdout) != OutputLimit || !res.Truncated || res.ExitCode != 0 {
		t.Fatalf("len=%d truncated=%v exit=%d", len(res.Stdout), res.Truncated, res.ExitCode)
	}
}

func TestRunTimeoutKillsProcessGroup(t *testing.T) {
	start := time.Now()
	res := Run(context.Background(), "sleep 30 & sleep 30", 300*time.Millisecond, nil, Env("e", "c", "", ""))
	if !res.TimedOut {
		t.Fatalf("%+v", res)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("grandchild kept the runner waiting")
	}
}

func TestRunTimeoutKillsTermIgnoringDescendant(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	start := time.Now()
	res := Run(context.Background(), "(trap '' TERM; sleep 30 >/dev/null 2>&1 & echo $! > "+pidFile+"; wait) & wait", 300*time.Millisecond, nil, Env("e", "c", "", ""))
	if !res.TimedOut || time.Since(start) > 6*time.Second {
		t.Fatalf("%+v after %s", res, time.Since(start))
	}
	assertDead(t, pidFile)
}

func TestRunNormalExitKillsBackgroundDescendant(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	res := Run(context.Background(), "sleep 30 >/dev/null 2>&1 & echo $! > "+pidFile+"; exit 0", 5*time.Second, nil, Env("e", "c", "", ""))
	if res.ExitCode != 0 || res.TimedOut {
		t.Fatalf("%+v", res)
	}
	assertDead(t, pidFile)
}

// assertDead reads a PID written by the hook and checks the process is gone within a second.
func assertDead(t *testing.T, pidFile string) {
	t.Helper()
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("pid file: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return // ESRCH: gone
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("descendant %d still alive", pid)
}

func TestRunMissingCommand(t *testing.T) {
	res := Run(context.Background(), "/nonexistent/binary", time.Second, nil, Env("e", "c", "", ""))
	if res.ExitCode == 0 {
		t.Fatal("missing command must fail")
	}
}

func TestRunWritesFile(t *testing.T) {
	out := filepath.Join(t.TempDir(), "hook.json")
	res := Run(context.Background(), "cat > "+out, time.Second, []byte("{\"a\":1}\n"), Env("e", "c", "", ""))
	if res.ExitCode != 0 {
		t.Fatalf("%+v", res)
	}
	if b, _ := os.ReadFile(out); string(b) != "{\"a\":1}\n" {
		t.Fatalf("%q", b)
	}
}

func TestRunNormalExitWithDescendantHoldingStdout(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	res := Run(context.Background(), "sleep 30 & echo $! > "+pidFile+"; echo done; exit 0", 5*time.Second, nil, Env("e", "c", "", ""))
	if res.ExitCode != 0 || res.TimedOut || res.Stdout != "done\n" {
		t.Fatalf("%+v", res)
	}
	assertDead(t, pidFile)
}

func TestEnvSMSFrom(t *testing.T) {
	t.Setenv("SIPTOOTH_SMS_FROM", "stale")
	res := Run(context.Background(), "env", time.Second, nil, Env("sms.received", "", "", "+380501234567"))
	if !strings.Contains(res.Stdout, "SIPTOOTH_SMS_FROM=+380501234567\n") || strings.Contains(res.Stdout, "stale") {
		t.Fatalf("%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "SIPTOOTH_CALL_ID=\n") || !strings.Contains(res.Stdout, "SIPTOOTH_CALLER=\n") {
		t.Fatalf("call vars must be empty: %s", res.Stdout)
	}
}

func TestByteCounts(t *testing.T) {
	res := Run(context.Background(), "head -c 20000 /dev/zero | tr '\\0' x; echo hi >&2", 5*time.Second, nil, Env("e", "c", "", ""))
	if res.StdoutBytes != 20000 || res.StderrBytes != 3 || len(res.Stdout) != OutputLimit {
		t.Fatalf("%d %d %d", res.StdoutBytes, res.StderrBytes, len(res.Stdout))
	}
}
