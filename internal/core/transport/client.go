package transport

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

// Call sends req to the daemon listening at path and returns its response.
// Without a deadline in ctx, the exchange is bounded by DefaultTimeout;
// cancelling ctx interrupts it. Connection errors wrap the system error
// (ENOENT, ECONNREFUSED, EACCES), so callers can explain them.
func Call(ctx context.Context, path string, req api.Request) (api.Response, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return api.Response{}, fmt.Errorf("transport: connect to %s: %w", path, err)
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) }) // interrupt pending I/O
	defer stop()
	if err := api.WriteRequest(conn, req); err != nil {
		return api.Response{}, fmt.Errorf("transport: send to %s: %w", path, err)
	}
	resp, err := api.ReadResponse(conn)
	if err != nil {
		return api.Response{}, fmt.Errorf("transport: response from %s: %w", path, err)
	}
	return resp, nil
}
