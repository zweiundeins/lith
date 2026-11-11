package sse

import (
	"bytes"
	"context"
	"io"
	"sync/atomic"
	"testing"

	"github.com/zweiundeins/lith/internal/shutdown"
)

// lateCtx reports its end only from its second Err, which puts the hub's stop
// exactly between Write's own check and the start of its send.
type lateCtx struct {
	context.Context
	checks atomic.Int32
}

func (c *lateCtx) Err() error {
	if c.checks.Add(1) == 1 {
		return nil
	}
	return c.Context.Err()
}

// TestShutdownAsWriteStartsKeepsEndFrame: a write that passed its check
// before the hub stopped goes out, and the end frame after it too, so the
// stream still sends its notices, Stopping's events and the reconnect.
func TestShutdownAsWriteStartsKeepsEndFrame(t *testing.T) {
	for _, enc := range []string{"", Zstd} {
		parent, cancel := context.WithCancelCause(t.Context())
		ctx := &lateCtx{Context: parent}
		ctx.checks.Store(-1) // Open checks the context too
		w := newRecorder()
		s, err := Open(w, request(ctx, enc), Options{})
		if err != nil {
			t.Fatal(err)
		}
		cancel(shutdown.ErrShutdown)
		ctx.checks.Store(0)
		_ = s.Write(Comment("y"))
		if err := s.CloseWith(Comment("last")); err != nil {
			t.Errorf("%q: CloseWith: %v", enc, err)
		}
		var body io.Reader = bytes.NewReader(w.bytes())
		if enc != "" {
			body = decoder(t, enc, w.bytes())
		}
		got, err := io.ReadAll(body)
		if err != nil || !bytes.HasSuffix(got, []byte(": last\n\n")) {
			t.Errorf("%q: the end frame is missing: got %q, %v", enc, got, err)
		}
	}
}
