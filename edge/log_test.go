package edge

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func TestRecover(t *testing.T) {
	log, logs := testLogger()
	h := Recover(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		panic("boom")
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/cards", nil))
	if w.Code != 500 || w.Body.String() != "internal server error\n" || w.Header().Get("ETag") != "" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("panic before writing: %d %q %v", w.Code, w.Body, w.Header())
	}
	for _, want := range []string{"level=ERROR", "msg=panic", "path=/cards", "panic=boom", "log_test.go"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
}

func TestRecoverAbortsAStartedResponse(t *testing.T) {
	log, _ := testLogger()
	for name, handler := range map[string]http.HandlerFunc{
		"panic after writing": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("<html>half a page"))
			panic("boom")
		},
		"ErrAbortHandler": func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) },
	} {
		func() {
			defer func() {
				if v := recover(); v != http.ErrAbortHandler {
					t.Errorf("%s: recovered %v, want http.ErrAbortHandler", name, v)
				}
			}()
			Recover(log, handler).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
		}()
	}
}

func TestLogRequests(t *testing.T) {
	log, logs := testLogger()
	h := LogRequests(log, Recover(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/created":
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte("hello"))
		case "/panic":
			panic("boom")
		case "/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			w.Write([]byte("data: x\n\n"))
		}
	})))
	for _, path := range []string{"/created", "/panic", "/stream", "/empty"} {
		r := httptest.NewRequest("POST", path, nil)
		r.RemoteAddr = "127.0.0.1:4711"
		r.Header.Set("X-Real-IP", "198.51.100.7")
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	var requests []string
	for _, l := range lines {
		if strings.Contains(l, "msg=request") {
			requests = append(requests, l)
		}
	}
	if len(requests) != 4 {
		t.Fatalf("%d request lines:\n%s", len(requests), logs)
	}
	for i, want := range []string{
		"level=INFO msg=request method=POST path=/created status=201 bytes=5",
		"level=ERROR msg=request method=POST path=/panic status=500",
		"level=INFO msg=request method=POST path=/stream status=200 bytes=9",
		"level=INFO msg=request method=POST path=/empty status=200 bytes=0",
	} {
		if !strings.Contains(requests[i], want) || !strings.Contains(requests[i], "ip=198.51.100.7") {
			t.Errorf("line %d: %s\nwant %s", i, requests[i], want)
		}
	}

	// A response a panic cut off is logged as aborted.
	logs.Reset()
	func() {
		defer func() { recover() }()
		LogRequests(log, Recover(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("half"))
			panic("boom")
		}))).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/half", nil))
	}()
	if !strings.Contains(logs.String(), "level=ERROR msg=request method=GET path=/half status=200 bytes=4") || !strings.Contains(logs.String(), "aborted=true") {
		t.Errorf("aborted request:\n%s", logs)
	}
}

// TestQuiet logs a request whose handler called Quiet at Debug, unless it
// failed, and changes nothing for the others or outside LogRequests.
func TestQuiet(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := LogRequests(log, Recover(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("quiet") {
			Quiet(r)
		}
		switch r.URL.Path {
		case "/move":
			w.WriteHeader(http.StatusNoContent)
		case "/bad":
			http.Error(w, "bad request", http.StatusBadRequest)
		case "/fail":
			http.Error(w, "oops", http.StatusInternalServerError)
		case "/panic":
			panic("boom")
		}
	})))
	for _, target := range []string{"/move?quiet", "/bad?quiet", "/fail?quiet", "/panic?quiet", "/move"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", target, nil))
	}
	var requests []string
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.Contains(l, "msg=request") {
			requests = append(requests, l)
		}
	}
	for i, want := range []string{
		"level=DEBUG msg=request method=POST path=/move status=204",
		"level=DEBUG msg=request method=POST path=/bad status=400",
		"level=ERROR msg=request method=POST path=/fail status=500",
		"level=ERROR msg=request method=POST path=/panic status=500",
		"level=INFO msg=request method=POST path=/move status=204",
	} {
		if i >= len(requests) || !strings.Contains(requests[i], want) {
			t.Errorf("line %d: want %s in\n%s", i, want, buf.String())
		}
	}
	Quiet(httptest.NewRequest("GET", "/", nil)) // outside LogRequests: nothing to do, no panic
}

func TestRecorderReachesTheConnection(t *testing.T) {
	log, _ := testLogger()
	srv := httptest.NewServer(LogRequests(log, Recover(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
			t.Errorf("SetWriteDeadline through LogRequests and Recover: %v", err)
		}
	}))))
	defer srv.Close()
	res, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
}
