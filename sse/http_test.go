package sse

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// smallBuffers gives accepted connections a small send buffer: this
// machine's TCP buffers grow to 16 MB, which no test fills quickly.
type smallBuffers struct{ net.Listener }

func (l smallBuffers) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetWriteBuffer(32 << 10)
	}
	return c, err
}

// stallingClient is a client for srv on connections of its own with a
// small receive buffer, for a response nobody reads.
func stallingClient(srv *httptest.Server) *http.Client {
	tr := srv.Client().Transport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if tc, ok := c.(*net.TCPConn); ok {
			tc.SetReadBuffer(32 << 10)
		}
		return c, err
	}
	return &http.Client{Transport: tr}
}

// A stream that stays silent for longer than its write timeout lives on,
// over HTTP/1.1 and HTTP/2: the timeout bounds each write, not the silence
// between writes. (HTTP/2 resets a stream when its deadline passes, write
// or no write.)
func TestSilenceOutlivesWriteTimeout(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(map[bool]string{false: "HTTP/1.1", true: "HTTP/2"}[h2], func(t *testing.T) {
			next := make(chan struct{})
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				s, err := Open(w, r, Options{WriteTimeout: 100 * time.Millisecond})
				if err != nil {
					t.Error(err)
					return
				}
				defer s.Close()
				for i := range 2 {
					if i > 0 {
						<-next
					}
					if err := s.Write(Comment("frame")); err != nil {
						t.Errorf("write %d: %v", i, err)
						return
					}
				}
			}))
			srv.EnableHTTP2 = h2
			srv.StartTLS()
			defer srv.Close()

			req, _ := http.NewRequestWithContext(t.Context(), "GET", srv.URL, nil)
			req.Header.Set("Accept-Encoding", "identity")
			res, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if got := res.ProtoMajor == 2; got != h2 {
				t.Fatalf("protocol %s", res.Proto)
			}
			body := bufio.NewReader(res.Body)
			read := func() string {
				var b strings.Builder
				for {
					l, err := body.ReadString('\n')
					b.WriteString(l)
					if err != nil || l == "\n" {
						return b.String()
					}
				}
			}
			if got := read(); got != ": frame\n\n" {
				t.Fatalf("first frame %q", got)
			}
			time.Sleep(300 * time.Millisecond)
			close(next)
			if got := read(); got != ": frame\n\n" {
				t.Fatalf("after the silence: %q", got)
			}
			if _, err := io.ReadAll(body); err != nil {
				t.Fatalf("the response did not end cleanly: %v", err)
			}
		})
	}
}

// A real client that stops reading: the write blocks once the socket (or
// HTTP/2's flow control window) is full, and fails at the deadline with a
// timeout, not as a stream that ended.
func TestStalledRealClientTimesOut(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(map[bool]string{false: "HTTP/1.1", true: "HTTP/2"}[h2], func(t *testing.T) {
			frame := PatchElements([]byte(strings.Repeat("x", 1<<20)))
			result := make(chan error, 1)
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				s, err := Open(w, r, Options{WriteTimeout: 200 * time.Millisecond})
				if err != nil {
					result <- err
					return
				}
				defer s.Close()
				for {
					if err := s.Write(frame); err != nil {
						result <- err
						return
					}
				}
			}))
			srv.Listener = smallBuffers{srv.Listener}
			srv.EnableHTTP2 = h2
			srv.StartTLS()
			defer srv.Close()
			req, _ := http.NewRequestWithContext(t.Context(), "GET", srv.URL, nil)
			req.Header.Set("Accept-Encoding", "identity")
			res, err := stallingClient(srv).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			select {
			case err := <-result:
				if !errors.Is(err, os.ErrDeadlineExceeded) {
					t.Fatalf("the write failed with %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the writes never blocked")
			}
		})
	}
}
