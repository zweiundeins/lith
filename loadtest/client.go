package loadtest

import (
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

// client is the far end of a stream without a network. It takes every
// write at once and keeps only its size, so the stream still compresses,
// flushes and sets its write deadlines as it does for a browser, but no
// client ever makes it wait.
type client struct {
	header  http.Header
	coding  string // the content coding the stream negotiates
	pending int64  // bytes since the last flush; only the stream's goroutine

	deadline atomic.Int64 // unix nanoseconds, 0 for none
	writes   atomic.Int64 // flushes that carried bytes
	bytes    atomic.Int64
}

func newClient(coding string) *client {
	return &client{header: http.Header{}, coding: coding}
}

func (c *client) Header() http.Header { return c.header }
func (c *client) WriteHeader(int)     {}

func (c *client) Write(p []byte) (int, error) {
	if c.expired() {
		return 0, os.ErrDeadlineExceeded
	}
	c.pending += int64(len(p))
	return len(p), nil
}

func (c *client) FlushError() error {
	if c.expired() {
		return os.ErrDeadlineExceeded
	}
	if c.pending > 0 {
		c.writes.Add(1)
		c.bytes.Add(c.pending)
		c.pending = 0
	}
	return nil
}

func (c *client) SetWriteDeadline(t time.Time) error {
	var d int64
	if !t.IsZero() {
		d = t.UnixNano()
	}
	c.deadline.Store(d)
	return nil
}

func (c *client) expired() bool {
	d := c.deadline.Load()
	return d != 0 && time.Now().UnixNano() >= d
}
