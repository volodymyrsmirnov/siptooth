// Package control is the private HTTP-over-Unix-socket API used by the CLI
// (spec 11): GET /status and nothing else.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/bridge"
	"github.com/volodymyrsmirnov/siptooth/internal/sms"
)

// ErrAlreadyRunning means a live daemon owns the socket.
var ErrAlreadyRunning = errors.New("control: another daemon owns the socket")

// StatusProvider is the coordinator as the server uses it.
type StatusProvider interface {
	Status() bridge.Status
}

// Status is the GET /status response: the coordinator's status plus the SMS service snapshot
// (spec 17.10).
type Status struct {
	bridge.Status
	SMS sms.Status `json:"sms"`
}

// NewServer returns the handler: GET /status. smsStatus may be nil (reported as disabled).
func NewServer(sp StatusProvider, smsStatus func() sms.Status) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		st := Status{Status: sp.Status(), SMS: sms.Status{State: sms.StateDisabled}}
		if smsStatus != nil {
			st.SMS = smsStatus()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	})
	return mux
}

// Probe reports ErrAlreadyRunning when a daemon accepts connections on the
// socket, and nil when the path is free or stale.
func Probe(socketPath string) error {
	c, err := net.DialTimeout("unix", socketPath, 500*time.Millisecond)
	if err == nil {
		c.Close()
		return ErrAlreadyRunning
	}
	if !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.ENOTSOCK) {
		return fmt.Errorf("probe socket %s: %w", socketPath, err)
	}
	return nil
}

// Serve listens on socketPath until ctx ends. A live socket means another
// daemon is running (ErrAlreadyRunning).
func Serve(ctx context.Context, socketPath string, h http.Handler) error {
	if err := Probe(socketPath); err != nil {
		return err
	}
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale socket: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o750); err != nil {
		return err
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", socketPath, err)
	}
	// The listener unlinks the socket file when closed (srv.Shutdown closes it).
	if err := os.Chmod(socketPath, 0o660); err != nil {
		ln.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case err := <-errCh:
		ln.Close()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return ctx.Err()
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			srv.Close()
		}
		<-errCh
		return ctx.Err()
	}
}
