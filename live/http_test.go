package live

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/zweiundeins/lith/sse"
)

var protocols = []struct {
	name string
	h2   bool
}{{"HTTP/1.1", false}, {"HTTP/2", true}}

// bigView renders a 1 MB frame that says which change it shows, so a
// client that stops reading fills its socket and flow control quickly.
func bigView(n *atomic.Int64) View {
	pad := strings.Repeat("x", 1<<20)
	return View{Key: "big", Topics: []string{"t"}, Render: func(context.Context, *sql.Tx) (any, error) {
		return sse.PatchElements(fmt.Appendf(nil, `<p id="big" data-n="%d">%s</p>`, n.Load(), pad)), nil
	}}
}

// realServer serves the hub over TLS, with HTTP/2 if h2. Serve's result
// for each path goes to results. Its connections have small send
// buffers: this machine's TCP buffers grow to 16 MB, which no test fills
// quickly.
func realServer(t *testing.T, h *Hub, h2 bool, spec Spec) (*httptest.Server, map[string]chan error) {
	results := map[string]chan error{}
	for _, p := range []string{"/a", "/b", "/c", "/stalled"} {
		results[p] = make(chan error, 1)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		results[r.URL.Path] <- h.Serve(w, r, spec)
	}))
	srv.Listener = smallBuffers{srv.Listener}
	srv.EnableHTTP2 = h2
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, results
}

type smallBuffers struct{ net.Listener }

func (l smallBuffers) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetWriteBuffer(32 << 10)
	}
	return c, err
}

// get opens a stream; the test ends it. The stalled stream gets a
// connection of its own with a small receive buffer.
func get(t *testing.T, ctx context.Context, srv *httptest.Server, path string, h2 bool) *http.Response {
	t.Helper()
	client := srv.Client()
	if path == "/stalled" {
		tr := client.Transport.(*http.Transport).Clone()
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if tc, ok := c.(*net.TCPConn); ok {
				tc.SetReadBuffer(32 << 10)
			}
			return c, err
		}
		client = &http.Client{Transport: tr}
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+path, nil)
	req.Header.Set("Accept-Encoding", "identity")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if (res.ProtoMajor == 2) != h2 {
		t.Fatalf("protocol %s", res.Proto)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

// changes reads a stream of bigView frames and passes on the change each
// one shows; the channel closes when the stream ends.
func changes(body io.Reader) <-chan int {
	out := make(chan int, 100)
	go func() {
		defer close(out)
		r := bufio.NewReader(body)
		for {
			l, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if v, ok := strings.CutPrefix(l, `data: elements <p id="big" data-n="`); ok {
				n, _ := strconv.Atoi(v[:strings.IndexByte(v, '"')])
				out <- n
			}
		}
	}()
	return out
}

func await(t *testing.T, c <-chan int, want int) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case n, ok := <-c:
			if !ok {
				t.Fatalf("the stream ended before change %d", want)
			}
			if n == want {
				return
			}
		case <-timeout:
			t.Fatalf("change %d did not arrive", want)
		}
	}
}

// ended waits for the client to see its stream end.
func ended(t *testing.T, c <-chan int) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-c:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("the client did not see its stream end")
		}
	}
}

// A real client that stops reading: its writes block when the socket or
// HTTP/2's flow control window is full, it fails at the write deadline,
// and the others get every change meanwhile.
func TestStalledRealClient(t *testing.T) {
	for _, p := range protocols {
		t.Run(p.name, func(t *testing.T) {
			var n atomic.Int64
			h := start(t, Options{Tick: 5 * time.Millisecond, SSE: sse.Options{WriteTimeout: 300 * time.Millisecond}})
			srv, results := realServer(t, h, p.h2, Spec{Views: []View{bigView(&n)}})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			get(t, ctx, srv, "/stalled", p.h2)
			healthy := changes(get(t, ctx, srv, "/a", p.h2).Body)
			await(t, healthy, 0)
			for i := 1; i <= 10; i++ {
				n.Store(int64(i))
				h.Changed([]string{"t"})
				await(t, healthy, i)
			}
			select {
			case err := <-results["/stalled"]:
				if !errors.Is(err, os.ErrDeadlineExceeded) {
					t.Fatalf("the stalled stream ended with %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the stalled stream outlived its write deadline")
			}
			if h.Stats().FramesDropped == 0 {
				t.Fatal("the stalled stream dropped no frames")
			}
		})
	}
}

// When the hub stops, a stream whose client reads ends cleanly: with the
// end of its compressed stream, and with the end of the response, so the
// client reads to EOF without an error.
func TestShutdownEndsResponsesCleanly(t *testing.T) {
	for _, p := range protocols {
		for _, enc := range []string{"identity", sse.Zstd} {
			t.Run(p.name+" "+enc, func(t *testing.T) {
				h := New(Options{Tick: 5 * time.Millisecond, Log: slog.New(slog.DiscardHandler)})
				hctx, stop := context.WithCancel(context.Background())
				defer stop()
				ran := make(chan error, 1)
				go func() { ran <- h.Run(hctx) }()
				v := text("v", nil, func(context.Context, *sql.Tx) (string, error) { return "v", nil })
				srv, results := realServer(t, h, p.h2, Spec{Views: []View{v}})
				req, _ := http.NewRequestWithContext(t.Context(), "GET", srv.URL+"/a", nil)
				req.Header.Set("Accept-Encoding", enc)
				res, err := srv.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				var body io.Reader = res.Body
				if enc == sse.Zstd {
					d, err := zstd.NewReader(res.Body, zstd.WithDecoderConcurrency(1))
					if err != nil {
						t.Fatal(err)
					}
					defer d.Close()
					body = d
				}
				r := bufio.NewReader(body)
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						t.Fatalf("before the first frame: %v", err)
					}
					if strings.Contains(l, `<p id="v">v</p>`) {
						break
					}
				}

				stop()
				if _, err := io.ReadAll(r); err != nil {
					t.Fatalf("the response ended with %v", err)
				}
				if err := <-results["/a"]; err != nil {
					t.Fatalf("Serve returned %v", err)
				}
				if err := <-ran; err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

// When the hub stops, Run returns and every stream ends at once, the one
// blocked in a write to a client that stopped reading included.
func TestShutdownWithRealClients(t *testing.T) {
	for _, p := range protocols {
		t.Run(p.name, func(t *testing.T) {
			var n atomic.Int64
			h := New(Options{Tick: 5 * time.Millisecond, Log: slog.New(slog.DiscardHandler), SSE: sse.Options{WriteTimeout: time.Minute}})
			hctx, stop := context.WithCancel(context.Background())
			ran := make(chan error, 1)
			go func() { ran <- h.Run(hctx) }()
			defer stop()
			srv, results := realServer(t, h, p.h2, Spec{Views: []View{bigView(&n)}})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			get(t, ctx, srv, "/stalled", p.h2)
			var healthy []<-chan int
			for _, path := range []string{"/a", "/b", "/c"} {
				healthy = append(healthy, changes(get(t, ctx, srv, path, p.h2).Body))
			}
			// Eight megabytes fill any socket and flow control window.
			for i := 1; i <= 8; i++ {
				n.Store(int64(i))
				h.Changed([]string{"t"})
				for _, c := range healthy {
					await(t, c, i)
				}
			}
			time.Sleep(100 * time.Millisecond)

			begin := time.Now()
			stop()
			select {
			case err := <-ran:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not return")
			}
			for path, res := range results {
				select {
				case err := <-res:
					if err != nil {
						t.Fatalf("%s: Serve returned %v", path, err)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("%s outlived the hub", path)
				}
			}
			for _, c := range healthy {
				ended(t, c)
			}
			if d := time.Since(begin); d > time.Second {
				t.Fatalf("shutdown took %v", d)
			}
		})
	}
}
