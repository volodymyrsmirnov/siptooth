package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/bridge"
	"github.com/volodymyrsmirnov/siptooth/internal/sms"
)

type fakeSP struct{ st bridge.Status }

func (f *fakeSP) Status() bridge.Status { return f.st }

func sockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "st") // short path: unix sockets are limited to ~104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "c.sock")
}

func waitSocket(t *testing.T, p string) {
	t.Helper()
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if c, err := net.Dial("unix", p); err == nil {
			c.Close()
			return
		}
	}
	t.Fatal("server did not start")
}

func startServer(t *testing.T, sp *fakeSP) string {
	t.Helper()
	p := sockPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go Serve(ctx, p, NewServer(sp, nil))
	waitSocket(t, p)
	return p
}

func TestStatusEndpointJSON(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	sp := &fakeSP{st: bridge.Status{Ready: true, PhoneConnected: true, State: "bridged", CallID: "c1", Caller: "+4915",
		CallStartedAt: &now, LastOutcome: "completed",
		Registration: bridge.RegistrationStatus{Enabled: true, Registered: true, Contact: "sip:u@1.2.3.4:5060"}}}
	p := startServer(t, sp)

	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", p)
	}}}
	resp, err := hc.Get("http://siptooth/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	for _, k := range []string{"ready", "phone_connected", "state", "call_id", "caller", "call_started_at", "last_outcome", "registration"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("status JSON lacks %q: %s", k, body)
		}
	}
	reg, _ := raw["registration"].(map[string]any)
	if reg["registered"] != true || reg["enabled"] != true || reg["contact"] != "sip:u@1.2.3.4:5060" {
		t.Errorf("registration: %v", reg)
	}

	st, err := NewClient(p).Status(context.Background())
	if err != nil || !st.Ready || st.State != "bridged" || st.CallID != "c1" || st.Caller != "+4915" || !st.Registration.Registered {
		t.Fatalf("client status %+v %v", st, err)
	}
}

func TestOnlyStatusIsServed(t *testing.T) {
	p := startServer(t, &fakeSP{})
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", p)
	}}}
	for _, path := range []string{"/v1/status", "/calls", "/hangup"} {
		resp, err := hc.Get("http://siptooth" + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
	}
	resp, err := hc.Post("http://siptooth/status", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /status: %d", resp.StatusCode)
	}
}

func TestClientUnavailable(t *testing.T) {
	if _, err := NewClient(sockPath(t)).Status(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func TestServeReplacesStaleSocket(t *testing.T) {
	p := sockPath(t)
	os.WriteFile(p, nil, 0o600) // stale regular file at the path
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- Serve(ctx, p, NewServer(&fakeSP{}, nil)) }()
	waitSocket(t, p)
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o660 {
		t.Fatalf("socket mode: %v %v", fi, err)
	}
	if _, err := NewClient(p).Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("socket file must be removed on shutdown")
	}
}

func TestServeRefusesLiveSocket(t *testing.T) {
	sp := &fakeSP{}
	p := startServer(t, sp)
	if err := Serve(context.Background(), p, NewServer(sp, nil)); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("got %v", err)
	}
	if err := Probe(p); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("probe: %v", err)
	}
}

func getBody(t *testing.T, p string) []byte {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", p)
	}}}
	resp, err := hc.Get("http://siptooth/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b
}

func TestStatusSMSDisabledExactJSON(t *testing.T) {
	p := startServer(t, &fakeSP{})
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(getBody(t, p), &raw); err != nil {
		t.Fatal(err)
	}
	if got := string(raw["sms"]); got != `{"enabled":false,"state":"disabled"}` {
		t.Fatalf("sms = %s", got)
	}
	if _, ok := raw["ready"]; !ok {
		t.Fatal("bridge fields must stay at the top level")
	}
}

func TestStatusSMSEnabled(t *testing.T) {
	yes, n := true, 3
	p := sockPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	smsSt := func() sms.Status {
		return sms.Status{Enabled: true, State: "ready", Notifications: &yes, Received: &n, LastError: "boom"}
	}
	go Serve(ctx, p, NewServer(&fakeSP{}, smsSt))
	waitSocket(t, p)
	st, err := NewClient(p).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s := st.SMS
	if !s.Enabled || s.State != "ready" || s.Notifications == nil || !*s.Notifications || s.Received == nil || *s.Received != 3 || s.LastError != "boom" {
		t.Fatalf("sms = %+v", s)
	}
}
