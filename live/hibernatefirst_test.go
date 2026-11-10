package live

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zweiundeins/lith/sse"
)

// With HibernateFirst a stream hibernates that long after its first frames,
// and its rests after the first are Hibernate. It is at most Hibernate, and
// does nothing without it.
func TestHibernateFirst(t *testing.T) {
	const never = 0
	for _, tc := range []struct {
		name             string
		hibernate, first time.Duration
		firstRest, rest  time.Duration // never: the stream stays awake
	}{
		{"short first rest", time.Hour, 200 * time.Millisecond, 200 * time.Millisecond, time.Hour},
		{"without HibernateFirst", time.Second, 0, time.Second, time.Second},
		{"longer than Hibernate", time.Second, time.Hour, time.Second, time.Second},
		{"without Hibernate", 0, 200 * time.Millisecond, never, never},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				b := sse.NewBudget(sse.BudgetOptions{Limit: -1})
				h := start(t, Options{Tick: 10 * time.Millisecond, Hibernate: tc.hibernate, HibernateFirst: tc.first, SSE: sse.Options{Budget: b}})
				var n atomic.Int64
				v := text("n", []string{"n"}, func(context.Context, *sql.Tx) (string, error) { return strconv.Itoa(int(n.Load())), nil })
				c := serve(h, Spec{Views: []View{v}}, sse.Zstd)
				defer c.leave(t)
				asleep := func() bool {
					synctest.Wait()
					return b.Stats().Streams["zstd asleep"] == 1
				}
				// frame waits for the frame of i and returns when it was sent,
				// within the last millisecond.
				frame := func(i int) time.Time {
					t.Helper()
					want := `<p id="n">` + strconv.Itoa(i) + `</p>`
					for !strings.Contains(string(unzstd(t, c.bytes())), want) {
						time.Sleep(time.Millisecond)
						synctest.Wait()
					}
					return time.Now()
				}
				// sleeps checks that the stream sent at hibernates d after it.
				sleeps := func(sent time.Time, d time.Duration, which string) {
					t.Helper()
					if d == never {
						time.Sleep(2*time.Hour - time.Since(sent))
						if asleep() {
							t.Fatalf("the stream hibernated after its %s", which)
						}
						return
					}
					time.Sleep(d - 2*time.Millisecond - time.Since(sent))
					if asleep() {
						t.Fatalf("the stream hibernated %v after its %s, before %v", time.Since(sent), which, d)
					}
					time.Sleep(3 * time.Millisecond)
					if !asleep() {
						t.Fatalf("the stream is awake %v after its %s, want asleep after %v", time.Since(sent), which, d)
					}
				}

				sleeps(frame(0), tc.firstRest, "first frame")
				for i := 1; i <= 2; i++ {
					n.Store(int64(i))
					h.Changed([]string{"n"})
					sleeps(frame(i), tc.rest, "frame "+strconv.Itoa(i))
				}
			})
		})
	}
}
