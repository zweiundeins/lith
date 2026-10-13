package budget

import (
	"encoding/csv"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/zweiundeins/lith/sse"
)

// TestHibernation simulates an hour of 200 mostly idle tabs, second by
// second, as libretto's are: each person changes something every three
// minutes on average (a modal, then the list it changed, a second apart),
// and the hub sends a heartbeat after 20 seconds of silence. With
// hibernation, a stream that sent no frame for the given time gives its
// encoder back, as live's pump does with Options.Hibernate. It measures
// the heap the streams hold (sampled every five minutes after the first
// ten), the bytes on the wire per frame, and what the frame that wakes a
// stream costs. It writes hibernate.csv.
func TestHibernation(t *testing.T) {
	request(t)
	const (
		tabs      = 200
		hour      = 3600 // seconds
		heartbeat = 20
		meanGap   = 180
	)
	sessions := make([][][]byte, 10)
	for i := range sessions {
		sessions[i] = pageFrames(80, uint64(i+1))
	}
	// When each tab changes something, the same in every run.
	rnd := rand.New(rand.NewPCG(7, 7))
	updates := make([]map[int]bool, tabs)
	for i := range updates {
		updates[i] = map[int]bool{}
		for at := 0.0; ; {
			at += rnd.ExpFloat64() * meanGap
			if at >= hour-1 {
				break
			}
			updates[i][int(at)] = true
		}
	}
	f, err := os.Create("hibernate.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c := csv.NewWriter(f)
	c.Write([]string{"setting", "hibernate_s", "tabs", "heap_mib_per_tab", "heap_mib_per_tab_max", "awake_share", "frames", "bytes_per_frame",
		"wake_frames", "bytes_per_wake_frame", "bytes_per_other_frame", "heartbeat_bytes_per_tab_hour", "us_per_wake_write", "us_per_other_write"})
	t.Logf("load average %s", loadavg())
	for _, s := range []setting{{coding: sse.Zstd, window: 17, lowMem: true}, {coding: sse.Zstd, window: 19}} {
		for _, hib := range []int{0, 30, 120} {
			before := liveHeap()
			type tab struct {
				st         *sse.Stream
				sink       *sink
				session    [][]byte
				next       int
				lastFrame  int
				lastWrite  int
				asleep     bool
				secondDue  int // the list frame that follows a modal, -1 for none
				wakeFrames int
			}
			ts := make([]*tab, tabs)
			for i := range ts {
				k := &sink{header: http.Header{}}
				st, err := sse.Open(k, s.request(), s.options())
				if err != nil {
					t.Fatal(err)
				}
				ts[i] = &tab{st: st, sink: k, session: sessions[i%len(sessions)], secondDue: -1}
			}
			var frames, wakeFrames, wakeBytes, otherBytes, beatBytes int64
			var wakeTime, otherTime time.Duration
			var heaps []float64
			var awake []float64
			write := func(k *tab, e []byte, now int) {
				wake := k.asleep
				n0 := k.sink.n
				start := time.Now()
				if err := k.st.Write(e); err != nil {
					t.Fatal(err)
				}
				d := time.Since(start)
				k.sink.Flush()
				n := k.sink.n - n0
				frames++
				if wake {
					wakeFrames++
					wakeBytes += n
					wakeTime += d
				} else {
					otherBytes += n
					otherTime += d
				}
				k.asleep = false
				k.lastFrame, k.lastWrite = now, now
			}
			for now := 0; now < hour; now++ {
				for i, k := range ts {
					switch {
					case now == 0 || updates[i][now]:
						write(k, k.session[k.next%len(k.session)], now)
						k.next++
						if now > 0 {
							k.secondDue = now + 1
						}
					case now == k.secondDue:
						write(k, k.session[k.next%len(k.session)], now)
						k.next++
						k.secondDue = -1
					case now-k.lastWrite >= heartbeat:
						n0 := k.sink.n
						if err := k.st.Write(sse.Comment("")); err != nil {
							t.Fatal(err)
						}
						if k.asleep {
							if err := k.st.Hibernate(); err != nil {
								t.Fatal(err)
							}
						}
						k.sink.Flush()
						beatBytes += k.sink.n - n0
						k.lastWrite = now
					}
					if hib > 0 && !k.asleep && now-k.lastFrame >= hib {
						n0 := k.sink.n
						if err := k.st.Hibernate(); err != nil {
							t.Fatal(err)
						}
						k.sink.Flush()
						beatBytes += k.sink.n - n0 // the end of the frame
						k.asleep = true
					}
				}
				if now >= 600 && now%300 == 0 {
					heaps = append(heaps, float64(liveHeap()-before)/tabs/mib)
					n := 0
					for _, k := range ts {
						if !k.asleep {
							n++
						}
					}
					awake = append(awake, float64(n)/tabs)
				}
			}
			for _, k := range ts {
				k.st.Close()
			}
			mean, hi := 0.0, 0.0
			for _, h := range heaps {
				mean += h / float64(len(heaps))
				hi = max(hi, h)
			}
			share := 0.0
			for _, a := range awake {
				share += a / float64(len(awake))
			}
			others := frames - wakeFrames
			per := func(a, b int64) float64 {
				if b == 0 {
					return 0
				}
				return float64(a) / float64(b)
			}
			us := func(d time.Duration, n int64) string {
				if n == 0 {
					return "0"
				}
				return strconv.FormatFloat(float64(d)/float64(n)/1e3, 'f', 1, 64)
			}
			c.Write([]string{s.String(), strconv.Itoa(hib), strconv.Itoa(tabs),
				strconv.FormatFloat(mean, 'f', 3, 64), strconv.FormatFloat(hi, 'f', 3, 64), strconv.FormatFloat(share, 'f', 3, 64),
				strconv.FormatInt(frames, 10), strconv.FormatFloat(per(wakeBytes+otherBytes, frames), 'f', 0, 64),
				strconv.FormatInt(wakeFrames, 10), strconv.FormatFloat(per(wakeBytes, wakeFrames), 'f', 0, 64), strconv.FormatFloat(per(otherBytes, others), 'f', 0, 64),
				strconv.FormatFloat(float64(beatBytes)/tabs, 'f', 0, 64), us(wakeTime, wakeFrames), us(otherTime, others)})
			t.Logf("%-22s hibernate %3ds: %.3f MiB per tab (max %.3f), %2.0f%% awake, %d frames, %.0f B/frame (wake %d at %.0f B, others %.0f B), heartbeats %.0f B per tab, write %s us on wake, %s us otherwise",
				s, hib, mean, hi, share*100, frames, per(wakeBytes+otherBytes, frames), wakeFrames, per(wakeBytes, wakeFrames), per(otherBytes, others),
				float64(beatBytes)/tabs, us(wakeTime, wakeFrames), us(otherTime, others))
			runtime.KeepAlive(ts)
		}
	}
	c.Flush()
	t.Logf("load average %s", loadavg())
}

// TestWakeCost measures a write that wakes a stream, against one to an
// awake stream, for the largest frame of each workload after the frame
// before it: the bytes and the time, with an encoder from the pool and
// with a new one when the pool is empty.
func TestWakeCost(t *testing.T) {
	request(t)
	page := pageFrames(150, 1)
	largest := 1
	for i := 1; i < len(page); i++ {
		if len(page[i]) > len(page[largest]) {
			largest = i
		}
	}
	board := boardFrames(t, 4)
	for _, w := range []struct {
		name       string
		prev, next []byte
		s          setting
	}{
		{"page", page[largest-1], page[largest], setting{coding: sse.Zstd, window: 17, lowMem: true}},
		{"board", board[2], board[3], setting{coding: sse.Zstd, window: 19}},
	} {
		const n = 200
		k := &sink{header: http.Header{}}
		st, err := sse.Open(k, w.s.request(), w.s.options())
		if err != nil {
			t.Fatal(err)
		}
		var awake, pooled, fresh time.Duration
		var awakeBytes, wakeBytes int64
		for range n {
			st.Write(w.prev)
			n0 := k.n
			start := time.Now()
			st.Write(w.next)
			awake += time.Since(start)
			awakeBytes += k.n - n0
		}
		for range n {
			st.Hibernate()
			n0 := k.n
			start := time.Now()
			st.Write(w.next)
			pooled += time.Since(start)
			wakeBytes += k.n - n0
		}
		for range n {
			st.Hibernate()
			runtime.GC()
			runtime.GC() // empties the pool
			start := time.Now()
			st.Write(w.next)
			fresh += time.Since(start)
		}
		st.Close()
		t.Log(fmt.Sprintf("%s (%s, a %d B frame after a %d B one): awake %d B in %.1f us; waking %d B in %.1f us with a pooled encoder, %.1f us with a new one",
			w.name, w.s, len(w.next), len(w.prev), awakeBytes/n, float64(awake)/n/1e3, wakeBytes/n, float64(pooled)/n/1e3, float64(fresh)/n/1e3))
	}
}
