package live

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/zweiundeins/lith/sse"
)

// RefreshWait is how long a stream that needs a refresh waits for one of
// Options.RefreshConcurrency's slots. Right after a deploy every open tab
// reconnects at once; a stream that gets no slot in this time goes on
// without its refresh (Stats.RefreshesBusy), and its page tries again on
// its next reconnect.
const RefreshWait = time.Second

// RefreshRequest is what Spec.Refresh gets: what the page reported with
// its stream request, and the request itself, for its language, cookies
// and app values such as an assets version.
type RefreshRequest struct {
	// Page is Spec.Page: the path and query of the page that opened the
	// stream.
	Page string
	// Build is Spec.Build: the build that rendered the page.
	Build string
	// Dirty is Spec.Dirty: the page holds input the person typed.
	Dirty bool
	// Request is the stream's request.
	Request *http.Request
}

// buildOf is the binary's VCS revision, with "+dirty" for a build from a
// modified tree, or "" when the binary carries none (go test, a build
// outside the repository).
func buildOf() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var rev, modified string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if rev != "" && modified == "true" {
		rev += "+dirty"
	}
	return rev
}

// Build is the build the hub runs (Options.Build), "" when unknown.
func (h *Hub) Build() string { return h.build }

// buildPatch patches the build signal to the hub's build.
func buildPatch(signal, build string) []byte {
	b, _ := json.Marshal(map[string]string{signal: build})
	return sse.PatchSignals(b, false)
}

// refresh runs spec.Refresh for a stream whose page reported a build other
// than the hub's. It returns the events to write first and the build
// signal's patch to write after the first frames; both are nil when the
// page needs no refresh or the refresh was skipped, so that the page asks
// again on its next reconnect.
func (h *Hub) refresh(ctx context.Context, r *http.Request, spec Spec) (events [][]byte, tail []byte) {
	if spec.Refresh == nil || spec.Build == "" || h.build == "" || spec.Build == h.build {
		return nil, nil
	}
	wait := time.NewTimer(h.refreshWait)
	defer wait.Stop()
	busy := func() {
		h.refreshesBusy.Add(1)
		h.refreshLog(logBusy, slog.LevelWarn, "live: no slot for a refresh within RefreshWait, the stream goes on without it", "stream", spec.ID, "page", spec.Page)
	}
	// The client's own slot comes first: a client that reports a build of
	// its choosing on every stream holds one of the shared slots at most.
	release := func() {}
	if spec.Client != "" {
		c := h.joinRefresh(spec.Client)
		select {
		case c.slot <- struct{}{}:
			release = func() {
				<-c.slot
				h.leaveRefresh(spec.Client, c)
			}
		case <-wait.C:
			h.leaveRefresh(spec.Client, c)
			busy()
			return nil, nil
		case <-ctx.Done():
			h.leaveRefresh(spec.Client, c)
			return nil, nil
		}
	}
	select {
	case h.refreshSlots <- struct{}{}:
	case <-wait.C:
		release()
		busy()
		return nil, nil
	case <-ctx.Done():
		release()
		return nil, nil
	}
	type result struct {
		events [][]byte
		err    error
	}
	done := make(chan result, 1)
	rctx, cancel := context.WithTimeout(ctx, h.renderTimeout)
	defer cancel()
	go func() {
		// Both slots stay taken until Refresh returns, also past RenderTimeout.
		defer release()
		defer func() { <-h.refreshSlots }()
		defer func() {
			if p := recover(); p != nil {
				done <- result{err: fmt.Errorf("live: Spec.Refresh panicked: %v\n%s", p, debug.Stack())}
			}
		}()
		ev, err := spec.Refresh(rctx, RefreshRequest{Page: spec.Page, Build: spec.Build, Dirty: spec.Dirty, Request: r})
		done <- result{ev, err}
	}()
	var res result
	select {
	case res = <-done:
	case <-rctx.Done():
		res.err = rctx.Err()
	}
	switch {
	case ctx.Err() != nil:
		return nil, nil // the client left, or the hub stops
	case errors.Is(res.err, context.DeadlineExceeded):
		h.refreshTimeouts.Add(1)
		h.refreshLog(logTimeout, slog.LevelWarn, "live: Spec.Refresh ran past RenderTimeout, the stream goes on without it", "stream", spec.ID, "page", spec.Page)
		return nil, nil
	case res.err != nil:
		h.refreshErrors.Add(1)
		h.refreshLog(logError, slog.LevelError, "live: Spec.Refresh failed, the stream goes on without it", "stream", spec.ID, "page", spec.Page, "err", res.err)
		return nil, nil
	}
	if spec.Reconnect != "" {
		for _, ev := range res.events {
			if holdsID(ev, spec.Reconnect) {
				h.refreshesGuarded.Add(1)
				h.refreshLog(logGuarded, slog.LevelWarn, "live: a refresh holds the stream's element, which would open a second stream; it is skipped: morph a container without it", "stream", spec.ID, "page", spec.Page, "element", spec.Reconnect)
				return nil, nil
			}
		}
	}
	if len(res.events) == 0 {
		h.refreshesUnchanged.Add(1)
	} else {
		h.refreshes.Add(1)
	}
	return res.events, h.buildPatch
}

// clientRefresh lets the streams of one Spec.Client refresh one at a time.
type clientRefresh struct {
	slot  chan struct{} // holds one
	users int           // the streams that wait for slot or hold it, under Hub.refreshMu
}

// joinRefresh returns the client's slot, and counts the stream as its user
// until leaveRefresh.
func (h *Hub) joinRefresh(client string) *clientRefresh {
	h.refreshMu.Lock()
	defer h.refreshMu.Unlock()
	c := h.refreshing[client]
	if c == nil {
		c = &clientRefresh{slot: make(chan struct{}, 1)}
		h.refreshing[client] = c
	}
	c.users++
	return c
}

// leaveRefresh forgets the client once no stream of it waits or holds.
func (h *Hub) leaveRefresh(client string, c *clientRefresh) {
	h.refreshMu.Lock()
	defer h.refreshMu.Unlock()
	if c.users--; c.users == 0 {
		delete(h.refreshing, client)
	}
}

// The kinds of skipped refreshes, each logged on its own (refreshLog).
const (
	logBusy = iota
	logTimeout
	logError
	logGuarded
)

// refreshLogEvery is the least time between two log lines of one kind of
// skipped refresh: a client can cause one with every stream it opens, and
// Stats counts each.
const refreshLogEvery = 10 * time.Second

// refreshLog logs a skipped refresh, unless one of the same kind was logged
// less than refreshLogEvery ago.
func (h *Hub) refreshLog(kind int, level slog.Level, msg string, args ...any) {
	last := &h.refreshLogged[kind]
	now, prev := time.Now().UnixNano(), last.Load()
	if prev != 0 && now-prev < int64(refreshLogEvery) || !last.CompareAndSwap(prev, now) {
		return
	}
	h.log.Log(context.Background(), level, msg, args...)
}

// holdsID reports whether ev holds an element with the id.
func holdsID(ev []byte, id string) bool {
	return bytes.Contains(ev, []byte(`id="`+id+`"`)) || bytes.Contains(ev, []byte(`id='`+id+`'`))
}

func eventsLen(events [][]byte) int {
	n := 0
	for _, ev := range events {
		n += len(ev)
	}
	return n
}
