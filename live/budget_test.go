package live

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zweiundeins/lith/sse"
)

// The hub's streams share the budget of Options.SSE: each holds a tier
// while it is open, a stream without room still gets its frames, and every
// stream gives its tier back when it ends.
func TestBudgetThroughOptions(t *testing.T) {
	b := sse.NewBudget(sse.BudgetOptions{Limit: 4 << 20, MinWindow: 17, MaxWindow: 19})
	h := start(t, Options{SSE: sse.Options{Budget: b}})
	v := text("a", nil, func(context.Context, *sql.Tx) (string, error) { return "a", nil })
	var cs []*client
	for range 3 {
		c := serve(h, Spec{Views: []View{v}}, sse.Zstd)
		waitFor(t, "the first frame", func() bool { return strings.Contains(string(unzstd(t, c.bytes())), `<p id="a">a</p>`) })
		cs = append(cs, c)
	}
	// Shares of 4 MiB, 2 MiB and 1.3 MiB, the last with 0.7 MiB left.
	if got := b.Stats().Streams; got["zstd 2^19"] != 2 || got["zstd asleep"] != 1 {
		t.Fatalf("streams by tier: %v", got)
	}
	for _, c := range cs {
		c.leave(t)
	}
	if st := b.Stats(); st.Reserved != 0 || len(st.Streams) != 0 {
		t.Fatalf("after the streams left: %+v", st)
	}

	// With Refuse, a stream without room is answered 503.
	b = sse.NewBudget(sse.BudgetOptions{Limit: 2 << 20, Refuse: true})
	h = start(t, Options{SSE: sse.Options{Budget: b}})
	first := serve(h, Spec{Views: []View{v}}, sse.Zstd)
	waitFor(t, "the first frame", func() bool { return len(unzstd(t, first.bytes())) > 0 })
	second := serve(h, Spec{Views: []View{v}}, sse.Brotli)
	if err := second.returned(t); !errors.Is(err, sse.ErrNoRoom) || second.status() != http.StatusServiceUnavailable {
		t.Fatalf("a stream without room: %v, status %d", err, second.status())
	}
	if st := b.Stats(); st.Refused != 1 || h.Stats().Streams != 1 {
		t.Fatalf("after the refusal: %+v, %d streams", st, h.Stats().Streams)
	}
	first.leave(t)
}

// A stream that sends no frame for Options.Hibernate hibernates and gives
// its tier back; heartbeats leave it asleep, and its next frame wakes it.
// The body decodes to everything it sent.
func TestHibernateInHub(t *testing.T) {
	b := sse.NewBudget(sse.BudgetOptions{Limit: -1})
	h := start(t, Options{Tick: 2 * time.Millisecond, Heartbeat: 15 * time.Millisecond, Hibernate: 40 * time.Millisecond, SSE: sse.Options{Budget: b}})
	var n atomic.Int64
	v := text("n", []string{"n"}, func(context.Context, *sql.Tx) (string, error) { return strconv.Itoa(int(n.Load())), nil })
	c := serve(h, Spec{Views: []View{v}}, sse.Zstd)
	has := func(html string) func() bool {
		return func() bool { return strings.Contains(string(unzstd(t, c.bytes())), html) }
	}
	asleep := func() bool { return b.Stats().Streams["zstd asleep"] == 1 }
	waitFor(t, "the first frame", has(`<p id="n">0</p>`))
	magic := []byte{0x28, 0xb5, 0x2f, 0xfd}
	for i := 1; i <= 2; i++ {
		waitFor(t, "hibernation", asleep)
		body := c.bytes()
		beats, frames := strings.Count(string(unzstd(t, body)), ":\n\n"), bytes.Count(body, magic)
		waitFor(t, "two heartbeats", func() bool { return strings.Count(string(unzstd(t, c.bytes())), ":\n\n") >= beats+2 })
		// Each went out in a zstd frame of its own, after which the stream
		// sleeps again: had a heartbeat woken it, it would stay awake.
		if got := bytes.Count(c.bytes(), magic); got < frames+2 {
			t.Fatalf("%d zstd frames after two heartbeats, from %d", got, frames)
		}
		waitFor(t, "sleep after the heartbeats", asleep)
		n.Store(int64(i))
		h.Changed([]string{"n"})
		waitFor(t, "the next frame", has(`<p id="n">`+strconv.Itoa(i)+`</p>`))
	}
	c.leave(t)
	if st := b.Stats(); len(st.Streams) != 0 {
		t.Fatalf("after the stream left: %+v", st)
	}
}
