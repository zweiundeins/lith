package edge

import (
	"bufio"
	"bytes"
	"encoding/json"
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
// and WriteTimeout set, and runs f on each.
func servers(t *testing.T, readTimeout time.Duration, h http.Handler, f func(t *testing.T, srv *httptest.Server)) {
	serversWithWriteTimeout(t, readTimeout, 0, h, f)
}

func serversWithWriteTimeout(t *testing.T, readTimeout, writeTimeout time.Duration, h http.Handler, f func(t *testing.T, srv *httptest.Server)) {
	for _, h2 := range []bool{false, true} {
		t.Run(map[bool]string{false: "HTTP/1.1", true: "HTTP/2"}[h2], func(t *testing.T) {
			srv := httptest.NewUnstartedServer(h)
			srv.Config.ReadTimeout = readTimeout
			srv.Config.WriteTimeout = writeTimeout
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

// testWindow is the tests' Upload window.
const testWindow = 300 * time.Millisecond

// uploadHandler reads the body through Upload in the way named by the
// path, answers with the bytes it got or the error, and checks that the
// request's context lives on linger after the body.
func uploadHandler(t *testing.T, l UploadLimits, linger time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := Upload(w, r, l)
		if err != nil {
			t.Errorf("Upload: %v", err)
		}
		r.Body = body
		start := time.Now()
		var n int64
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
		// A read deadline set after the body's end would cancel the
		// request's context over HTTP/1.1 when it passed.
		time.Sleep(linger)
		if err := r.Context().Err(); err != nil {
			t.Errorf("%s: the request's context after the body: %v", r.URL.Path, err)
		}
		fmt.Fprint(w, n)
	})
}

// slowLimits wants 10 KB/s, 3 KB a window.
var slowLimits = UploadLimits{MinRate: 10_000, Max: 1 << 20, Window: testWindow}

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
	// Sent at about 13 KB/s for 4 windows.
	h := uploadHandler(t, slowLimits, 3*testWindow)
	servers(t, testWindow, h, func(t *testing.T, srv *httptest.Server) {
		for _, path := range []string{"/plain", "/form", "/parts"} {
			const n = 16_000
			var body io.Reader
			ct := "application/octet-stream"
			if path == "/plain" {
				body = slowBody(n, 260, 20*time.Millisecond)
			} else {
				body, ct = multipartBody(n, 260, 20*time.Millisecond)
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
	const w = testWindow
	h := uploadHandler(t, slowLimits, 0)
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

// TestUploadBodyBurstThenStall: a burst earns no time beyond a window from
// its last read, so a body that stalls after it is cut off a window later.
func TestUploadBodyBurstThenStall(t *testing.T) {
	const w = testWindow
	h := uploadHandler(t, slowLimits, 0)
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
		if res.StatusCode != http.StatusBadRequest || took < w || took > 3*w/2 {
			t.Errorf("a stall after a burst: %d after %v: %s", res.StatusCode, took, b)
		}
	})
}

// TestUploadOutlastsTheWriteTimeout: an upload longer than the server's
// WriteTimeout gets its answer, which has the WriteTimeout from the body's
// end.
func TestUploadOutlastsTheWriteTimeout(t *testing.T) {
	h := uploadHandler(t, slowLimits, 100*time.Millisecond)
	serversWithWriteTimeout(t, 5*time.Second, 400*time.Millisecond, h, func(t *testing.T, srv *httptest.Server) {
		const n = 16_000 // about 1.2 s
		res, err := srv.Client().Post(srv.URL+"/plain", "application/octet-stream", slowBody(n, 260, 20*time.Millisecond))
		if err != nil {
			t.Fatalf("the answer after a long upload: %v", err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK || string(b) != fmt.Sprint(n) {
			t.Errorf("%d %s", res.StatusCode, b)
		}
	})
}

// TestTooLarge: a body over Max fails with *http.MaxBytesError, which
// TooLarge finds through each way of reading it; a writer without a
// connection gets no deadline and says so.
func TestTooLarge(t *testing.T) {
	form := func() (*bytes.Buffer, string) {
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		fw, _ := mw.CreateFormFile("file", "photo.jpg")
		fw.Write(bytes.Repeat([]byte("x"), 5000))
		mw.Close()
		return &b, mw.FormDataContentType()
	}
	for name, read := range map[string]func(r *http.Request) error{
		"plain": func(r *http.Request) error { _, err := io.ReadAll(r.Body); return err },
		"json":  func(r *http.Request) error { var v any; return json.NewDecoder(r.Body).Decode(&v) },
		"form":  func(r *http.Request) error { return r.ParseMultipartForm(1 << 10) },
		"parts": func(r *http.Request) error {
			mr, err := r.MultipartReader()
			if err != nil {
				return err
			}
			p, err := mr.NextPart()
			if err != nil {
				return err
			}
			_, err = io.Copy(io.Discard, p)
			return err
		},
	} {
		var req *http.Request
		switch name {
		case "plain":
			req = httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat("x", 2000)))
		case "json":
			req = httptest.NewRequest("POST", "/", strings.NewReader(`"`+strings.Repeat("x", 2000)+`"`))
		default:
			b, ct := form()
			req = httptest.NewRequest("POST", "/", b)
			req.Header.Set("Content-Type", ct)
		}
		w := httptest.NewRecorder()
		body, err := Upload(w, req, UploadLimits{Max: 1000})
		if !errors.Is(err, http.ErrNotSupported) {
			t.Errorf("%s: Upload with a recorder: %v, want ErrNotSupported", name, err)
		}
		req.Body = body
		if err := read(req); !TooLarge(err) {
			t.Errorf("%s: %v, want TooLarge", name, err)
		}
	}
	if TooLarge(errors.New("other")) || TooLarge(nil) {
		t.Error("TooLarge of another error")
	}
}
