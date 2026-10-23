package edge

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// servers starts h over HTTP/1.1 and HTTP/2 with the server's ReadTimeout
// set, and runs f on each.
func servers(t *testing.T, readTimeout time.Duration, h http.Handler, f func(t *testing.T, srv *httptest.Server)) {
	for _, h2 := range []bool{false, true} {
		t.Run(map[bool]string{false: "HTTP/1.1", true: "HTTP/2"}[h2], func(t *testing.T) {
			srv := httptest.NewUnstartedServer(h)
			srv.Config.ReadTimeout = readTimeout
			srv.EnableHTTP2 = h2
			srv.StartTLS()
			defer srv.Close()
			f(t, srv)
		})
	}
}

// slowBody sends n bytes in pieces of piece, one every gap.
func slowBody(n, piece int, gap time.Duration) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		chunk := bytes.Repeat([]byte("x"), piece)
		for sent := 0; sent < n; sent += piece {
			if _, err := pw.Write(chunk[:min(piece, n-sent)]); err != nil {
				return
			}
			time.Sleep(gap)
		}
		pw.Close()
	}()
	return pr
}

// TestReadTimeoutSparesStreams: the server's ReadTimeout ends a body that
// does not arrive, read or not, and leaves alone a stream whose request has
// no body or whose body is in, over HTTP/1.1 and HTTP/2. Over HTTP/1.1 Go's
// server clears the read deadline at the body's end, or before the handler
// for a request without one; over HTTP/2 the timeout is a timer per stream,
// which does nothing once the body has ended.
func TestReadTimeoutSparesStreams(t *testing.T) {
	const d = 200 * time.Millisecond
	mux := http.NewServeMux()
	mux.HandleFunc("POST /pace", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /read", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		_, err := io.ReadAll(r.Body)
		if took := time.Since(start); !errors.Is(err, os.ErrDeadlineExceeded) || took > d+time.Second {
			t.Errorf("a trickled body: %v after %v, want the deadline", err, took)
		}
		w.WriteHeader(http.StatusBadRequest)
	})
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Errorf("%s stream's body: %v", r.Method, err)
		}
		w.Write([]byte("open\n"))
		http.NewResponseController(w).Flush()
		time.Sleep(5 * d)
		if err := r.Context().Err(); err != nil {
			t.Errorf("%s stream: %v", r.Method, err)
		}
		w.Write([]byte("still\n"))
	})
	servers(t, d, mux, func(t *testing.T, srv *httptest.Server) {
		c := srv.Client()
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
		res, err := c.Post(srv.URL+"/read", "text/plain", slowBody(1000, 1, 100*time.Millisecond))
		if err == nil {
			res.Body.Close()
		}
	})

	// Over HTTP/1.1 a handler that never reads the body still waits for it:
	// the server reads the rest before it answers.
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.ReadTimeout = d
	srv.Start()
	defer srv.Close()
	for _, line := range []string{"POST /pace", "GET /page"} {
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(conn, line+" HTTP/1.1\r\nHost: app\r\nContent-Length: 100000\r\n\r\n{")
		start := time.Now()
		conn.SetReadDeadline(start.Add(10 * time.Second))
		b, err := io.ReadAll(conn)
		took := time.Since(start)
		conn.Close()
		if err != nil || took > d+2*time.Second {
			t.Errorf("%s with a trickled body: after %v: %v\n%.200s", line, took.Round(time.Millisecond), err, b)
		}
	}
}

// uploadHandler reads the body through UploadBody in the way named by the
// path, answers with the bytes it got or the error, and checks that the
// request's context lives on after the body.
func uploadHandler(t *testing.T, minRate, maxSize int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = UploadBody(w, r, minRate, maxSize)
		start := time.Now()
		var n int64
		var err error
		switch r.URL.Path {
		case "/plain":
			n, err = io.Copy(io.Discard, r.Body)
		case "/form":
			if err = r.ParseMultipartForm(1 << 10); err == nil {
				f, _, ferr := r.FormFile("file")
				if err = ferr; err == nil {
					n, err = io.Copy(io.Discard, f)
				}
			}
		case "/parts":
			var mr *multipart.Reader
			if mr, err = r.MultipartReader(); err == nil {
				var p *multipart.Part
				if p, err = mr.NextPart(); err == nil {
					n, err = io.Copy(io.Discard, p)
				}
			}
		}
		took := time.Since(start)
		if err != nil {
			w.Header().Set("Took", took.String())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// A deadline set after the body's end would cancel the request's
		// context over HTTP/1.1 when it passed.
		time.Sleep(3 * uploadWindow)
		if err := r.Context().Err(); err != nil {
			t.Errorf("%s: the request's context after the body: %v", r.URL.Path, err)
		}
		fmt.Fprint(w, n)
	})
}

func setUploadWindow(t *testing.T, d time.Duration) {
	old := uploadWindow
	uploadWindow = d
	t.Cleanup(func() { uploadWindow = old })
}

// multipartBody wraps n bytes sent slowly as a form with one file.
func multipartBody(n, piece int, gap time.Duration) (io.Reader, string) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		fw, _ := mw.CreateFormFile("file", "photo.jpg")
		if _, err := io.Copy(fw, slowBody(n, piece, gap)); err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.CloseWithError(mw.Close())
	}()
	return pr, mw.FormDataContentType()
}

// TestUploadBodyTakesASlowUpload: a body a little faster than minRate runs
// over several windows and past the server's ReadTimeout, in each way of
// reading it, and the request's context lives on after it.
func TestUploadBodyTakesASlowUpload(t *testing.T) {
	const w = 300 * time.Millisecond
	setUploadWindow(t, w)
	// 10 KB/s wanted, 3 KB a window; sent at about 13 KB/s for 4 windows.
	h := uploadHandler(t, 10_000, 1<<20)
	servers(t, w, h, func(t *testing.T, srv *httptest.Server) {
		for _, path := range []string{"/plain", "/form", "/parts"} {
			const n = 16_000
			var body io.Reader
			ct := "application/octet-stream"
			if path == "/plain" {
				body = slowBody(n, 1300, 100*time.Millisecond)
			} else {
				body, ct = multipartBody(n, 1300, 100*time.Millisecond)
			}
			start := time.Now()
			res, err := srv.Client().Post(srv.URL+path, ct, body)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != http.StatusOK || string(b) != fmt.Sprint(n) {
				t.Errorf("%s after %v: %d %s", path, time.Since(start).Round(time.Millisecond), res.StatusCode, b)
			}
		}
	})
}

// TestUploadBodyCutsATrickle: a body under minRate is cut off after about a
// window, over HTTP/1.1 and HTTP/2.
func TestUploadBodyCutsATrickle(t *testing.T) {
	const w = 300 * time.Millisecond
	setUploadWindow(t, w)
	h := uploadHandler(t, 10_000, 1<<20)
	servers(t, 5*time.Second, h, func(t *testing.T, srv *httptest.Server) {
		res, err := srv.Client().Post(srv.URL+"/plain", "application/octet-stream", slowBody(100_000, 100, 50*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		took, _ := time.ParseDuration(res.Header.Get("Took"))
		t.Logf("cut off after %v", took)
		if res.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), "timeout") && !strings.Contains(string(b), "deadline") || took < w || took > 2*w+time.Second {
			t.Errorf("a trickle: %d after %v: %s", res.StatusCode, took, b)
		}
	})
}

// TestUploadBodyBurstThenStall: a burst earns one window ahead at most, so
// a body that stalls after it is cut off within two.
func TestUploadBodyBurstThenStall(t *testing.T) {
	const w = 300 * time.Millisecond
	setUploadWindow(t, w)
	h := uploadHandler(t, 10_000, 1<<20)
	servers(t, 5*time.Second, h, func(t *testing.T, srv *httptest.Server) {
		pr, pw := io.Pipe()
		go func() {
			pw.Write(bytes.Repeat([]byte("x"), 300_000))
			time.Sleep(10 * w)
			pw.Close()
		}()
		res, err := srv.Client().Post(srv.URL+"/plain", "application/octet-stream", pr)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		took, _ := time.ParseDuration(res.Header.Get("Took"))
		if res.StatusCode != http.StatusBadRequest || took > 2*w+time.Second {
			t.Errorf("a stall after a burst: %d after %v: %s", res.StatusCode, took, b)
		}
	})
}

// TestUploadBodyLimitsTheSize: a body over maxSize fails with
// *http.MaxBytesError.
func TestUploadBodyLimitsTheSize(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(UploadBody(w, r, 0, 1000))
		var tooBig *http.MaxBytesError
		if !errors.As(err, &tooBig) {
			t.Errorf("a body over the limit: %v", err)
		}
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat("x", 2000))))
}
