package edge

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestBodyDeadline: a trickled body to a handler that never reads it ends
// with the answer and a closed connection once the deadline has passed. A
// handler that read the body, and a stream, with a body or without, run
// on past the deadline, over HTTP/1.1 and HTTP/2.
func TestBodyDeadline(t *testing.T) {
	const d = 200 * time.Millisecond
	log, _ := testLogger()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /pace", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /read", func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read: %v", err)
		}
		time.Sleep(3 * d)
		if err := r.Context().Err(); err != nil {
			t.Errorf("after the body: %v", err)
		}
		w.Write(b)
	})
	// A stream reads no body: its headers going out discard what was sent.
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("open\n"))
		http.NewResponseController(w).Flush()
		time.Sleep(3 * d)
		if err := r.Context().Err(); err != nil {
			t.Errorf("%s stream: %v", r.Method, err)
		}
		w.Write([]byte("still\n"))
	})
	h := BodyDeadline(d, LogRequests(log, Recover(log, mux)))

	t.Run("trickled", func(t *testing.T) {
		srv := httptest.NewServer(h)
		defer srv.Close()
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		io.WriteString(conn, "POST /pace HTTP/1.1\r\nHost: app\r\nContent-Length: 100000\r\n\r\n{")
		start := time.Now()
		conn.SetReadDeadline(start.Add(10 * time.Second))
		b, err := io.ReadAll(conn)
		took := time.Since(start)
		if err != nil || !strings.HasPrefix(string(b), "HTTP/1.1 204") || took < d || took > d+2*time.Second {
			t.Errorf("after %v: %v\n%s", took.Round(time.Millisecond), err, b)
		}
	})
	for _, h2 := range []bool{false, true} {
		t.Run(map[bool]string{false: "HTTP/1.1", true: "HTTP/2"}[h2], func(t *testing.T) {
			srv := httptest.NewUnstartedServer(h)
			srv.EnableHTTP2 = h2
			srv.StartTLS()
			defer srv.Close()
			c := srv.Client()

			res, err := c.Post(srv.URL+"/read", "text/plain", strings.NewReader("hello"))
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if string(b) != "hello" {
				t.Errorf("read: %q", b)
			}
			for _, method := range []string{"GET", "POST"} {
				var body io.Reader
				if method == "POST" {
					body = strings.NewReader(`{"_build":"b"}`)
				}
				req, _ := http.NewRequest(method, srv.URL+"/stream", body)
				res, err := c.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				lines := bufio.NewScanner(res.Body)
				for _, want := range []string{"open", "still"} {
					if !lines.Scan() || lines.Text() != want {
						t.Errorf("%s stream: %q, want %q (%v)", method, lines.Text(), want, lines.Err())
					}
				}
				res.Body.Close()
			}
		})
	}
}

// TestBodyDeadlineWithoutAConnection leaves a writer that takes no
// deadline, such as httptest's recorder, alone.
func TestBodyDeadlineWithoutAConnection(t *testing.T) {
	h := BodyDeadline(0, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Write(b)
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/", strings.NewReader("body")))
	if w.Body.String() != "body" {
		t.Errorf("%q", w.Body)
	}
}
