package stack

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
)

var (
	// ErrUnhealthy is returned when an app does not pass its health check
	// within its timeout.
	ErrUnhealthy = errors.New("not healthy")
	// ErrExited is returned when an app's process ends while rig still needs
	// it running.
	ErrExited = errors.New("exited")
)

// _healthInterval is the pause between health checks.
const _healthInterval = 200 * time.Millisecond

// _probeTimeout bounds one health check, so a server that accepts the
// connection and never answers is retried rather than waited on.
const _probeTimeout = 2 * time.Second

// healthCheck says how to decide an app is ready.
type healthCheck struct {
	host string
	port int
	// path is an HTTP path. Empty means a TCP connect is enough.
	path string
}

// probe makes one attempt. It returns nil when the app is ready.
func (h healthCheck) probe(ctx context.Context, client *http.Client) error {
	ctx, cancel := context.WithTimeout(ctx, _probeTimeout)
	defer cancel()

	addr := net.JoinHostPort(h.host, strconv.Itoa(h.port))

	if h.path == "" {
		var d net.Dialer

		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}

		return conn.Close()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+h.path, http.NoBody)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("GET %s answered %d", h.path, resp.StatusCode)
	}

	return nil
}

// waitHealthy polls until the check passes, the timeout runs out, or the
// process exits. Watching exited means an app that crashes on boot fails in
// a second, not after the full timeout.
func waitHealthy(ctx context.Context, h healthCheck, timeout time.Duration, exited <-chan struct{}) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Redirects are followed: a health path that redirects to a login page
	// still proves the server is up.
	client := &http.Client{}

	var last error

	for {
		if last = h.probe(ctx, client); last == nil {
			return nil
		}

		select {
		case <-exited:
			return ErrExited
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("%w after %s: %w", ErrUnhealthy, timeout, last)
			}

			return ctx.Err()
		case <-time.After(_healthInterval):
		}
	}
}
