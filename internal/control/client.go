package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// ErrUnavailable means the daemon does not answer on the socket.
var ErrUnavailable = errors.New("control: daemon not running")

// Client talks to the daemon's control socket.
type Client struct {
	http *http.Client
}

// NewClient returns a client for the socket at socketPath.
func NewClient(socketPath string) *Client {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	}}
	return &Client{http: &http.Client{Transport: tr, Timeout: 10 * time.Second}}
}

// Status fetches GET /status.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var st Status
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://siptooth/status", nil)
	if err != nil {
		return st, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return st, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return st, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if resp.StatusCode/100 != 2 {
		return st, fmt.Errorf("daemon returned %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, &st); err != nil {
		return st, fmt.Errorf("decode status: %w", err)
	}
	return st, nil
}
