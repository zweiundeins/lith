package sse

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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
